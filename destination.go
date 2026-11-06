// Copyright © 2022 Meroxa, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package file

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/conduitio/conduit-commons/opencdc"
	sdk "github.com/conduitio/conduit-connector-sdk"
)

// writerBufferSize is the capacity of the bufio.Writer that sits between the
// destination and the file. Buffering amortizes the write(2) syscall across
// many records: with the default single-record delivery each flush writes one
// line, but when the SDK batches records (sdk.batch.size) the whole batch is
// drained in one syscall instead of one syscall per record. One CDC record
// line is typically a few hundred bytes, so a full buffer holds on the order of
// a hundred records.
const writerBufferSize = 64 * 1024

type Destination struct {
	sdk.UnimplementedDestination

	config DestinationConfig

	file   *os.File
	writer *bufio.Writer
	// committed is the committed-length file (see committed.go), nil when the
	// output is not a regular file.
	committed *os.File
	// failed is set once a Write or Teardown failed after it may have written
	// part of a batch. The output may then hold bytes past the committed
	// length, so Teardown keeps the length for the next Open to cut back to.
	failed bool

	// mu serializes access to writer, committed and failed. The SDK calls
	// Write from a single goroutine, but a bufio.Writer is not safe for
	// concurrent use, and a batching strategy (or a future caller) can invoke
	// Write from more than one goroutine. The mutex is uncontended on the hot
	// path.
	mu sync.Mutex
}

func (d *Destination) Config() sdk.DestinationConfig {
	return &d.config
}

type DestinationConfig struct {
	sdk.DefaultDestinationMiddleware

	Config // embed the global config
}

func (c DestinationConfig) Validate(context.Context) error { return c.Config.Validate() }

func NewDestination() sdk.Destination {
	return sdk.DestinationWithMiddleware(&Destination{})
}

// Open opens the output file for appending, creating it if needed. A regular
// output file is first cut back to its committed length (see committed.go), so
// that a batch a kill left half-written is removed before it is delivered
// again; a warning names the bytes removed.
func (d *Destination) Open(ctx context.Context) error {
	file, err := d.openOrCreate(ctx, d.config.Path)
	if err != nil {
		return err
	}
	committed, err := openCommitted(ctx, openerFrom(ctx), d.config.Path, file)
	if err != nil {
		_ = file.Close()
		return err
	}

	d.file = file
	d.committed = committed
	d.writer = bufio.NewWriterSize(file, writerBufferSize)
	return nil
}

func (d *Destination) Write(_ context.Context, recs []opencdc.Record) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, r := range recs {
		// Marshal the record once and write it plus its line terminator into the
		// buffer. Writing the bytes and the newline separately avoids the extra
		// allocation and copy that append(r.Bytes(), '\n') used to do per record.
		if _, err := d.writer.Write(r.Bytes()); err != nil {
			// A buffered writer can fail here if a buffer-full flush (or a direct
			// write of an oversized record) hits the file error. At that point we
			// can no longer attribute which records reached the file, so we report
			// zero written and let the whole batch be redelivered (at-least-once).
			d.failed = true
			return 0, err
		}
		if err := d.writer.WriteByte('\n'); err != nil {
			d.failed = true
			return 0, err
		}
	}

	// Invariant 1: ack only after the destination confirms the durable write.
	// The SDK acks every record in this batch as soon as Write returns, so the
	// buffered bytes must be flushed to the file (the OS page cache) before we
	// return — otherwise a crash after ack would lose records the source already
	// considers delivered. Flushing once here batches the flush across the whole
	// batch instead of per record.
	if err := d.writer.Flush(); err != nil {
		d.failed = true
		return 0, err
	}
	// Invariant 1 (and 6): ack only after both the batch and the committed
	// length that ends it are written. Until the length is recorded, the next
	// Open treats the batch as never written and removes it, which is correct
	// only because it was not acknowledged yet.
	if err := d.commit(); err != nil {
		d.failed = true
		return 0, err
	}

	return len(recs), nil
}

// commit records the current length of the output as its committed length.
// The length is taken from the file, not counted, so it stays right if the
// output is truncated outside the connector while it runs.
func (d *Destination) commit() error {
	if d.committed == nil {
		return nil
	}
	info, err := d.file.Stat()
	if err != nil {
		return fmt.Errorf("stat output file %q: %w", d.config.Path, err)
	}
	if err := writeCommitted(d.committed, info.Size()); err != nil {
		return fmt.Errorf("record committed length in %q: %w", committedPath(d.config.Path), err)
	}
	return nil
}

func (d *Destination) Teardown(context.Context) error {
	if d.file == nil {
		return nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// Flush before close so a graceful shutdown never drops bytes that were
	// buffered but not yet written to the file.
	flushErr := d.writer.Flush()
	if flushErr != nil {
		d.failed = true
	}
	closeErr := d.file.Close()
	committedErr := d.closeCommitted()
	return errors.Join(flushErr, closeErr, committedErr)
}

// closeCommitted closes the committed-length file. After a clean run the output
// ends with the last acknowledged batch, so the length is cleared (the file is
// emptied, not removed): an older version that appends without updating the
// length cannot have its records cut off by a stale one later. After a failed
// Write the output may end in part of a batch, so the length is kept for the
// next Open.
func (d *Destination) closeCommitted() error {
	if d.committed == nil {
		return nil
	}
	var truncErr error
	if !d.failed {
		truncErr = d.committed.Truncate(0)
	}
	return errors.Join(truncErr, d.committed.Close())
}

// destinationFileMode is the mode used when the connector creates the output
// file. Records may contain sensitive data, so the file is owner-only
// (GHSA-wj57-rcgj-4x67). The process umask can only make it stricter.
const destinationFileMode = 0o600

// openOrCreate opens the file for appending, creating it with
// destinationFileMode if it does not exist. Creation and open are one atomic
// syscall, so there is no window between a stat and a create. The mode of an
// already existing file is never changed.
func (d *Destination) openOrCreate(ctx context.Context, path string) (*os.File, error) {
	return openerFrom(ctx)(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, destinationFileMode)
}
