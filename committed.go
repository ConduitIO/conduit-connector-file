// Copyright © 2026 Meroxa, Inc.
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

// The committed length.
//
// A SIGKILL can stop a write(2) to a regular file part way: Linux copies a
// buffered write one page at a time and gives up at a page boundary when a
// fatal signal is pending. A batch the destination was writing when it was
// killed can therefore end in part of a line. That batch was never
// acknowledged (Write had not returned), so it is delivered again after the
// restart, and appending it to the fragment would glue the two into one line
// that is neither record.
//
// So the destination keeps, next to the output, the length of the output up to
// the end of the last batch it fully wrote: <path>.committed, the length as an
// 8-byte big-endian integer, always written with one pwrite at offset 0. Eight
// bytes at offset 0 lie in one page, so that write is never torn: the file
// holds the old length or the new one. Write records the new length after the
// batch is in the file and before it returns, and Open cuts the output back to
// the recorded length before it appends anything. A kill between the batch and
// its length only makes the length lag: Open then removes complete lines of a
// batch that was never acknowledged, and they are delivered again.
//
// Durability. A kill of the process never loses the page cache, but an
// operating-system crash or a power loss does, and the page cache of the
// output and of the committed-length file reach the disk independently. So
// every commit (Destination.commit and Open, through commitLength) is: fsync
// the output, pwrite the length, fsync the committed-length file, and only
// then acknowledge. The length on disk therefore never covers a byte that is
// not on disk, and every acknowledged byte is on disk. After any crash the
// length is at most one batch behind the durable output, and that batch was
// not acknowledged: cutting it is ordinary at-least-once redelivery. A length
// ahead of the output can only mean the output was changed outside the
// connector, or that the storage lost writes it reported as synced; Open
// refuses it (FILE_COMMITTED_LENGTH_MISMATCH). Open also fsyncs the output's
// directory, so that a newly created output and committed-length file still
// exist after a crash.
//
// A committed-length file of 0 bytes (or none at all) means no length is
// recorded: the output was written by a version without this file, or the
// destination stopped cleanly (Teardown empties the file, so a later run of an
// older version, which appends without updating it, is never cut back to a
// stale length). Open then removes only what follows the last newline, which
// is all a torn write can leave when records have no newline of their own.
//
// The committed length is only kept for a regular file. A device or a FIFO
// (for example /dev/stdout) cannot be truncated and is written as before.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	sdk "github.com/conduitio/conduit-connector-sdk"
)

// committedSuffix is appended to the output path to name the file holding the
// committed length.
const committedSuffix = ".committed"

// committedSize is the size of a committed-length file that holds a length.
// It also identifies the format: a later format must use a different size.
const committedSize = 8

// committedPath returns the path of the committed-length file of output path.
func committedPath(path string) string { return path + committedSuffix }

// openCommitted prepares the output file f (opened for appending at path) for
// writing: if it is a regular file, it cuts it back to its committed length
// (or, without one, to the end of its last line), records that length and
// returns the open committed-length file. For an output that is not a regular
// file it returns nil and changes nothing. On error the output is unchanged
// unless the error is from recording the length after the cut.
//
// newlineFree tells whether the configured record format can never put a
// newline inside a record; it decides what Open may do with an output that has
// no committed length (see lengthToKeep).
//
// It also returns a second, read-write descriptor of the output, which the
// caller keeps to fsync the output (see Destination.data) and closes with it.
// sync makes a file durable (syncFile outside tests).
func openCommitted(ctx context.Context, open Opener, path string, f *os.File, newlineFree bool, sync func(*os.File) error) (_, _ *os.File, err error) {
	info, err := f.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("stat output file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, nil
	}

	// The output is opened write-only for appending (as before this change,
	// so devices and FIFOs behave the same). Reading its tail and cutting it
	// back need a second descriptor, opened read-write without O_APPEND: on
	// Windows a handle opened for appending cannot truncate. It must be the
	// same file; openFlags does not block if the path was replaced by a FIFO
	// in between, and SameFile then refuses it.
	r, err := open(path, openFlags|os.O_RDWR, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open output file %q for reading: %w", path, err)
	}
	defer func() {
		if err != nil {
			_ = r.Close()
		}
	}()
	rinfo, err := r.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("stat output file %q: %w", path, err)
	}
	if !os.SameFile(info, rinfo) {
		return nil, nil, fmt.Errorf("output file %q was replaced while it was being opened; open the destination again", path)
	}

	cpath := committedPath(path)
	perm := info.Mode().Perm()
	c, cinfo, err := openCommittedFile(open, path, perm)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			_ = c.Close()
		}
	}()
	// The committed-length file is part of the output: whoever may read the
	// output may read it, and nobody else.
	if cinfo.Mode().Perm() != perm {
		if err := c.Chmod(perm); err != nil {
			return nil, nil, fmt.Errorf("set the mode of committed-length file %q to that of the output (%o): %w", cpath, perm, err)
		}
	}

	size := info.Size()
	keep, err := lengthToKeep(ctx, path, c, cinfo.Size(), r, size, newlineFree)
	if err != nil {
		return nil, nil, err
	}
	if keep < size {
		// Invariant 6 (and 3): Open never keeps bytes past the committed
		// length. They are the start of a batch that was never acknowledged;
		// appending its redelivery to them would glue a fragment and a record
		// into one malformed line.
		if err := r.Truncate(keep); err != nil {
			return nil, nil, fmt.Errorf("truncate output file %q to %d bytes: %w", path, keep, err)
		}
		sdk.Logger(ctx).Warn().
			Str("path", path).
			Int64("oldSize", size).
			Int64("newSize", keep).
			Int64("bytesRemoved", size-keep).
			Bool("committedLengthRecorded", cinfo.Size() == committedSize).
			Msg("removed the end of the output file that was written by a write that never completed " +
				"(the connector was stopped mid-write); those records were not acknowledged and are delivered again")
	}
	// Invariant 1 and 5: the same durable order as every Write (see
	// commitLength), so the length Open records never covers bytes an older
	// version left only in the page cache, and the cut is durable before the
	// first batch is appended.
	if err := commitLength(path, r, c, keep, sync); err != nil {
		return nil, nil, err
	}
	return c, r, nil
}

// commitLength durably records n as the committed length of the output at
// path: it fsyncs the output through data, then writes n to c, then fsyncs c.
//
// Invariant 1 and 5: the length is written only after every byte it covers is
// on disk, and the caller acknowledges only after the length is on disk too.
// Reordering these steps lets an operating-system crash leave a length that
// covers bytes that never reached the disk (the next Open then refuses the
// output, or keeps a fragment), or leave acknowledged records only in the
// page cache.
func commitLength(path string, data, c *os.File, n int64, sync func(*os.File) error) error {
	cpath := committedPath(path)
	if err := sync(data); err != nil {
		return fmt.Errorf("fsync output file %q: %w", path, err)
	}
	if err := writeCommitted(c, n); err != nil {
		return fmt.Errorf("record committed length in %q: %w", cpath, err)
	}
	if err := sync(c); err != nil {
		return fmt.Errorf("fsync committed-length file %q: %w", cpath, err)
	}
	return nil
}

// openCommittedFile opens (or creates, with perm) the committed-length file of
// the output at path and returns it with its FileInfo.
func openCommittedFile(open Opener, path string, perm os.FileMode) (_ *os.File, _ os.FileInfo, err error) {
	cpath := committedPath(path)
	// Defense in depth: the committed-length file is the connector's own and
	// is never a link or anything but a regular file. Whoever can write the
	// output's directory can write the output too, so this guards against
	// mistakes more than against anyone.
	notOwn := func(what string) error {
		return &CodedError{
			Code: ErrCodeCommittedLengthInvalid,
			Path: path,
			Msg:  fmt.Sprintf("committed-length file %q is %s: it must be a regular file the destination created", cpath, what),
			Fix:  fmt.Sprintf("remove %q (the destination creates it again); the output is kept up to its last complete line", cpath),
		}
	}
	// Refuse a symlink before the open too: where the open cannot refuse one
	// (no O_NOFOLLOW, on Windows), creating through a dangling link would
	// create its target.
	if li, lerr := os.Lstat(cpath); lerr == nil && li.Mode()&os.ModeSymlink != 0 {
		return nil, nil, notOwn("a symlink")
	}
	c, err := open(cpath, committedOpenFlags, perm)
	if err != nil {
		if li, lerr := os.Lstat(cpath); lerr == nil && li.Mode()&os.ModeSymlink != 0 {
			return nil, nil, notOwn("a symlink")
		}
		return nil, nil, fmt.Errorf("open committed-length file %q: %w", cpath, err)
	}
	defer func() {
		if err != nil {
			_ = c.Close()
		}
	}()
	cinfo, err := c.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("stat committed-length file %q: %w", cpath, err)
	}
	if !cinfo.Mode().IsRegular() {
		return nil, nil, notOwn("not a regular file")
	}
	// Where the open cannot refuse a symlink (no O_NOFOLLOW), check the path.
	if li, err := os.Lstat(cpath); err == nil && (li.Mode()&os.ModeSymlink != 0 || !os.SameFile(li, cinfo)) {
		return nil, nil, notOwn("a symlink or was replaced while it was opened")
	}
	return c, cinfo, nil
}

// lengthToKeep returns how many bytes of the output (r, size bytes) are kept,
// from the committed-length file c (csize bytes).
func lengthToKeep(ctx context.Context, path string, c *os.File, csize int64, r io.ReaderAt, size int64, newlineFree bool) (int64, error) {
	cpath := committedPath(path)
	switch csize {
	case 0:
		// No length recorded: an output written by an earlier version, or
		// one the destination closed cleanly. A clean output ends with a
		// newline and nothing is cut. An output that does not was torn by a
		// crash of v0.12.0 or earlier. When records cannot contain a newline,
		// all a torn write can leave is a partial last line, so cutting after
		// the last newline removes exactly the unacknowledged part. When they
		// can, the last newline may be inside the torn record, and the
		// destination does not guess.
		keep, err := lastLineEnd(r, size)
		if err != nil || keep == size || newlineFree {
			return keep, err
		}
		return 0, &CodedError{
			Code: ErrCodeOutputTailUnterminated,
			Path: path,
			Msg: fmt.Sprintf("the output file does not end with a newline and no committed length is recorded; "+
				"the configured record format can put a newline inside a record, so the end of the last complete "+
				"record is unknown (%d bytes follow the last newline). This only happens on the first start after "+
				"upgrading from v0.12.0 or earlier following a crash", size-keep),
			Fix: "inspect the end of the output file, remove the partial record (or truncate the file), then restart",
		}
	case committedSize:
	default:
		return 0, &CodedError{
			Code: ErrCodeCommittedLengthInvalid,
			Path: path,
			Msg:  fmt.Sprintf("committed-length file %q is %d bytes long, want 0 or %d", cpath, csize, committedSize),
			Fix:  "this file is written only by the destination; restore it from the same backup as the output, or delete it to keep the output up to its last complete line",
		}
	}

	var buf [committedSize]byte
	if _, err := c.ReadAt(buf[:], 0); err != nil {
		return 0, fmt.Errorf("read committed-length file %q: %w", cpath, err)
	}
	u := binary.BigEndian.Uint64(buf[:])
	if u > math.MaxInt64 {
		return 0, &CodedError{
			Code: ErrCodeCommittedLengthInvalid,
			Path: path,
			Msg:  fmt.Sprintf("committed-length file %q holds %d, which is not a file length", cpath, u),
			Fix:  "this file is written only by the destination; restore it from the same backup as the output, or delete it to keep the output up to its last complete line",
		}
	}
	committed := int64(u)

	// The destination fsyncs the output before it records a length that
	// covers it (commitLength), so neither a kill nor an operating-system
	// crash can leave a length that the output does not reach or that does
	// not end a line. What can: the output was truncated, replaced or
	// restored from a backup without its committed-length file, or the
	// storage lost writes it reported as synced (a disk write cache without
	// power-loss protection, a filesystem mounted without write barriers).
	mismatch := func(msg string) error {
		return &CodedError{
			Code: ErrCodeCommittedLengthMismatch,
			Path: path,
			Msg: msg + "; the output was changed outside the connector (truncated, replaced, or restored without " +
				"its committed-length file), or the storage lost writes it had reported as synced",
			Fix: fmt.Sprintf("check the output file. If it holds what you expect, delete %q and restart: the output is "+
				"kept up to its last newline (with a record format other than */json, an output that does not end with "+
				"a newline is then refused with %s until its partial record is removed). Otherwise restore the output "+
				"and %q together from the same backup", cpath, ErrCodeOutputTailUnterminated, cpath),
		}
	}
	switch {
	case committed > size && size == 0:
		// The output was deleted (and created again by Open) or emptied.
		// There is nothing to keep and nothing to guess.
		sdk.Logger(ctx).Warn().
			Str("path", path).
			Int64("committedLength", committed).
			Msg("the output file is empty but the destination had written more to it: it was deleted or emptied " +
				"outside the connector; starting it again from empty")
		return 0, nil
	case committed > size:
		return 0, mismatch(fmt.Sprintf("the output file is %d bytes long, shorter than the %d bytes the destination committed", size, committed))
	case committed > 0:
		// Every batch ends with a newline, so a committed length that does
		// not end a line belongs to another file.
		var last [1]byte
		if _, err := r.ReadAt(last[:], committed-1); err != nil {
			return 0, fmt.Errorf("read output file %q: %w", path, err)
		}
		if last[0] != '\n' {
			return 0, mismatch(fmt.Sprintf("the committed length %d of the output file does not end a line", committed))
		}
	}
	return committed, nil
}

// lastLineEnd returns the offset just past the last newline in the first size
// bytes of r, or 0 if there is none.
func lastLineEnd(r io.ReaderAt, size int64) (int64, error) {
	const chunk = 64 * 1024
	buf := make([]byte, chunk)
	for end := size; end > 0; {
		start := max(end-chunk, 0)
		b := buf[:end-start]
		if _, err := r.ReadAt(b, start); err != nil && !errors.Is(err, io.EOF) {
			return 0, fmt.Errorf("read the end of the output file: %w", err)
		}
		for i := len(b) - 1; i >= 0; i-- {
			if b[i] == '\n' {
				return start + int64(i) + 1, nil
			}
		}
		end = start
	}
	return 0, nil
}

// writeCommitted records n as the committed length in c, with one pwrite of
// committedSize bytes at offset 0. That write lies within one page and is
// never torn by a kill.
func writeCommitted(c *os.File, n int64) error {
	var buf [committedSize]byte
	binary.BigEndian.PutUint64(buf[:], uint64(n)) //nolint:gosec // n is a file size, never negative
	_, err := c.WriteAt(buf[:], 0)
	return err
}
