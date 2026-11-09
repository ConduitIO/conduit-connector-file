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
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/conduitio/conduit-commons/opencdc"
	sdk "github.com/conduitio/conduit-connector-sdk"
)

// writerBufferSize is the capacity of the bufio.Writer that sits between the
// destination and the file. Buffering amortizes the write(2) syscall across
// many records: when records are batched (sdk.batch.size, 1000 by default)
// the whole batch is drained in one syscall instead of one syscall per
// record. One CDC record
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
	// data is a second, read-write descriptor of the output, kept open to
	// fsync it (nil when the output is not a regular file). The output itself
	// is opened for appending, and on Windows such a handle cannot be
	// flushed; fsync acts on the file, not the descriptor, so syncing through
	// data makes durable what was written through file.
	data *os.File
	// sync makes a file's written bytes durable, and syncDir a directory's
	// entries. Nil means syncFile and syncDir; tests replace them to observe
	// the order of the durability steps.
	sync    func(*os.File) error
	syncDir func(dir string) error
	// failed is set once a Write or Teardown failed after it may have written
	// part of a batch. The output may then hold bytes past the committed
	// length, so Teardown keeps the length for the next Open to cut back to,
	// and that Open, once it has cut, clears failed.
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

	// DirSyncAcceptUnsupported makes Open continue, with a warning, when the
	// filesystem rejects an fsync of the output's directory as unsupported
	// (EINVAL, ENOTSUP or EOPNOTSUPP; some network and FUSE filesystems do).
	// By default Open then fails with FILE_DIR_SYNC_UNSUPPORTED. Without that
	// fsync a newly created output file may not survive an operating-system
	// crash or a power loss, and the records acknowledged into it are lost
	// with it. Set it only after creating the output file and
	// <path>.committed and making their directory entries durable by other
	// means (see the connector's description).
	DirSyncAcceptUnsupported bool `json:"dirSync.acceptUnsupported" default:"false"`
}

// dirSyncUnsupportedError is the error of syncDir when the filesystem does not
// support fsync on a directory (see dirSyncResult). Errno names the error, for
// example EINVAL.
type dirSyncUnsupportedError struct {
	Dir   string
	Errno string
	Err   error
}

func (e *dirSyncUnsupportedError) Error() string { return e.Err.Error() }

func (e *dirSyncUnsupportedError) Unwrap() error { return e.Err }

func (c DestinationConfig) Validate(context.Context) error { return c.Config.Validate() }

// Default destination batching. Every Write ends with two fsyncs (see
// commit), a fixed cost that a batch shares among its records, so the
// destination asks for batches of up to defaultBatchSize records. The delay
// bounds how long a partial batch waits for more records. A pipeline that sets
// both sdk.batch.size and sdk.batch.delay to 0 gets the earlier behavior, one
// Write per batch the engine passes. Setting only one of them is not enough:
// with only the size at 0 the SDK still batches (at its own maximum size),
// and with only the delay at 0 a partial batch waits until it is full.
const (
	defaultBatchSize  = 1000
	defaultBatchDelay = 100 * time.Millisecond
)

func NewDestination() sdk.Destination {
	return sdk.DestinationWithMiddleware(&Destination{
		config: DestinationConfig{
			DefaultDestinationMiddleware: sdk.DefaultDestinationMiddleware{
				// Read by conn-sdk-cli specgen, which writes them into
				// connector.yaml as the parameters' defaults.
				DestinationWithBatch: sdk.DestinationWithBatch{
					BatchSize:  defaultBatchSize,
					BatchDelay: defaultBatchDelay,
				},
			},
		},
	})
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
	committed, data, err := openCommitted(ctx, openerFrom(ctx), d.config.Path, file, d.newlineFreeFormat(), d.syncer())
	if err != nil {
		_ = file.Close()
		return err
	}
	if committed != nil {
		// Invariant 1: a new output (or committed-length file) still exists
		// after an operating-system crash only once its directory entry is on
		// disk, so the directory is synced before the first ack (an
		// operator can accept a filesystem that does not support that; see
		// below).
		dirSync := d.syncDir
		if dirSync == nil {
			dirSync = syncDir
		}
		dir := filepath.Dir(d.config.Path)
		if err := dirSync(dir); err != nil {
			closeAll := func() { _ = errors.Join(file.Close(), committed.Close(), data.Close()) }
			var unsupported *dirSyncUnsupportedError
			if !errors.As(err, &unsupported) {
				closeAll()
				return fmt.Errorf("fsync the directory of output file %q: %w", d.config.Path, err)
			}
			// Some filesystems (some network and FUSE filesystems) reject an
			// fsync of a directory as unsupported. Every Write still fsyncs
			// the output and then the committed-length file before the ack,
			// but if Open just created the output, its directory entry may
			// not be on disk yet, and an operating-system crash or a power
			// loss before the filesystem persists it loses the whole file,
			// with every record acknowledged into it. So Open refuses unless
			// the operator accepted that with dirSync.acceptUnsupported.
			coded := &CodedError{
				Code: ErrCodeDirSyncUnsupported,
				Path: d.config.Path,
				Msg: fmt.Sprintf("the filesystem does not support fsync on the output's directory %q (%s), so a newly "+
					"created output file, and the records acknowledged into it, may not survive an operating-system "+
					"crash or a power loss", dir, unsupported.Errno),
				Fix: fmt.Sprintf("move the output to a filesystem that supports fsync on a directory, or create the "+
					"output file and %q and make their directory entries durable by other means (for example "+
					"`touch <path> <path>.committed && sync`, or on Linux `sync -f %s` after creating them), then set "+
					"dirSync.acceptUnsupported to true", committedPath(d.config.Path), dir),
				Err: err,
			}
			if !d.config.DirSyncAcceptUnsupported {
				// Invariant 1: without the opt-in, a destination that cannot
				// make a new output durable never acknowledges into it.
				closeAll()
				return coded
			}
			sdk.Logger(ctx).Warn().
				Str("code", ErrCodeDirSyncUnsupported).
				Str("path", d.config.Path).
				Str("dir", dir).
				Err(err).
				Msg("[" + ErrCodeDirSyncUnsupported + "] " + coded.Msg + "; continuing because dirSync.acceptUnsupported is set")
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.file = file
	d.committed = committed
	d.data = data
	d.writer = bufio.NewWriterSize(file, writerBufferSize)
	// Invariant 3 and 6: a failed Write is forgotten only here, after Open
	// cut the output back to its committed length and made the cut durable,
	// so the next batch is appended to the end of the last acknowledged one
	// and never to a fragment of the failed one. An Open that fails keeps it.
	d.failed = false
	return nil
}

func (d *Destination) Write(_ context.Context, recs []opencdc.Record) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.file == nil {
		return 0, fmt.Errorf("write to output file %q: the destination is not open", d.config.Path)
	}
	// Invariant 3 and 6: after a failed Write the output may end in part of a
	// batch, and after a failed fsync Linux may have dropped the batch's
	// pages while a later fsync reports success. Appending another batch
	// would glue it to the fragment and commit a length that covers bytes
	// that are not on disk. Only Open can cut back to the committed length,
	// so the destination refuses to write until it is opened again.
	if d.failed {
		return 0, fmt.Errorf("write to output file %q: an earlier write failed; the destination must be opened again "+
			"(Open removes the unacknowledged part of the failed batch)", d.config.Path)
	}
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
	// buffered bytes must be flushed to the file before we return. Flushing
	// once here batches the flush across the whole batch instead of per
	// record; commit then makes the batch and its length durable.
	if err := d.writer.Flush(); err != nil {
		d.failed = true
		return 0, err
	}
	// Invariant 1 (and 3, 6): ack only after the batch and the committed
	// length that ends it are both on disk, in that order (see commit). Until
	// the length is durable, the next Open treats the batch as never written
	// and removes it, which is correct only because it was not acknowledged
	// yet.
	if err := d.commit(); err != nil {
		d.failed = true
		return 0, err
	}

	return len(recs), nil
}

// newlineFreeFormat tells whether the configured record format can never put a
// newline inside a record. The JSON formats (the default opencdc/json, and
// every <converter>/json) cannot: encoding/json escapes control characters in
// strings and emits no whitespace. Any other format may.
func (d *Destination) newlineFreeFormat() bool {
	f := d.config.RecordFormat
	// The SDK's middleware sets the default before Open; nil is that default.
	return f == nil || strings.HasSuffix(*f, "/json")
}

// commit makes the output durable and records its current length as the
// committed length, durably. The order is what makes the length safe to cut
// back to after an operating-system crash or a power loss, where the page
// cache of the output and of the committed-length file reach the disk
// independently:
//
//  1. fsync the output: every byte the length will cover is on disk;
//  2. pwrite the length;
//  3. fsync the committed-length file;
//  4. return, and the SDK acknowledges the batch.
//
// So the committed length on disk never covers bytes that are not on disk,
// and every acknowledged byte is on disk and covered by it. After any crash
// the length on disk is at most one batch behind the durable output, and
// that batch was not acknowledged.
//
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
	return commitLength(d.config.Path, d.data, d.committed, info.Size(), d.syncer())
}

// syncer returns the function that makes a file durable.
func (d *Destination) syncer() func(*os.File) error {
	if d.sync != nil {
		return d.sync
	}
	return syncFile
}

// Teardown flushes and closes the output and the committed-length file. It is
// safe to call more than once and on a destination that was never opened; a
// Write after Teardown fails without touching the files.
func (d *Destination) Teardown(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.file == nil {
		return nil
	}

	// Flush before close so a graceful shutdown never drops bytes that were
	// buffered but not yet written to the file. Every Write ends with a
	// flush and a commit, so the buffer is empty here unless a Write failed,
	// and then failed is set and the length is kept anyway.
	flushErr := d.writer.Flush()
	closeErr := d.file.Close()
	if flushErr != nil || closeErr != nil {
		// The output may not have been finished cleanly: keep the committed
		// length so the next Open cuts back to it.
		d.failed = true
	}
	committedErr := d.closeCommitted()
	var dataErr error
	if d.data != nil {
		dataErr = d.data.Close()
	}
	d.file, d.writer, d.committed, d.data = nil, nil, nil, nil
	return errors.Join(flushErr, closeErr, committedErr, dataErr)
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
		// Durable, so that an older version run after a power loss that
		// followed this clean stop is not cut back to a stale length either.
		truncErr = d.committed.Truncate(0)
		if truncErr == nil {
			truncErr = d.syncer()(d.committed)
		}
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
