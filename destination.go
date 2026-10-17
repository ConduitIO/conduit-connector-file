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

	// mu serializes access to writer. The SDK calls Write from a single
	// goroutine, but a bufio.Writer is not safe for concurrent use, and a
	// batching strategy (or a future caller) can invoke Write from more than one
	// goroutine. The mutex is uncontended on the hot path.
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

func (d *Destination) Open(context.Context) error {
	file, err := d.openOrCreate(d.config.Path)
	if err != nil {
		return err
	}

	d.file = file
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
			return 0, err
		}
		if err := d.writer.WriteByte('\n'); err != nil {
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
		return 0, err
	}

	return len(recs), nil
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
	closeErr := d.file.Close()
	if flushErr != nil {
		return flushErr
	}
	return closeErr
}

func (d *Destination) openOrCreate(path string) (*os.File, error) {
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		file, err := os.Create(path)
		if err != nil {
			return nil, err
		}

		return file, nil
	}
	if err != nil {
		return nil, err
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}

	return file, nil
}
