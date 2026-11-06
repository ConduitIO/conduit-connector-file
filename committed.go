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
func openCommitted(ctx context.Context, open Opener, path string, f *os.File) (*os.File, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat output file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil
	}

	// The output is opened write-only for appending (as before this change,
	// so devices and FIFOs behave the same). Reading its tail and cutting it
	// back need a second descriptor, opened read-write without O_APPEND: on
	// Windows a handle opened for appending cannot truncate. It must be the
	// same file; openFlags does not block if the path was replaced by a FIFO
	// in between, and SameFile then refuses it.
	r, err := open(path, openFlags|os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open output file %q for reading: %w", path, err)
	}
	defer r.Close()
	rinfo, err := r.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat output file %q: %w", path, err)
	}
	if !os.SameFile(info, rinfo) {
		return nil, fmt.Errorf("output file %q was replaced while it was being opened; open the destination again", path)
	}

	cpath := committedPath(path)
	perm := info.Mode().Perm()
	c, err := open(cpath, os.O_CREATE|os.O_RDWR, perm)
	if err != nil {
		return nil, fmt.Errorf("open committed-length file %q: %w", cpath, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = c.Close()
		}
	}()
	cinfo, err := c.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat committed-length file %q: %w", cpath, err)
	}
	if !cinfo.Mode().IsRegular() {
		return nil, fmt.Errorf("committed-length file %q is not a regular file", cpath)
	}
	// The committed-length file is part of the output: whoever may read the
	// output may read it, and nobody else.
	if cinfo.Mode().Perm() != perm {
		if err := c.Chmod(perm); err != nil {
			return nil, fmt.Errorf("set the mode of committed-length file %q to that of the output (%o): %w", cpath, perm, err)
		}
	}

	size := info.Size()
	keep, err := lengthToKeep(ctx, path, c, cinfo.Size(), r, size)
	if err != nil {
		return nil, err
	}
	if keep < size {
		// Invariant 6 (and 3): Open never keeps bytes past the committed
		// length. They are the start of a batch that was never acknowledged;
		// appending its redelivery to them would glue a fragment and a record
		// into one malformed line.
		if err := r.Truncate(keep); err != nil {
			return nil, fmt.Errorf("truncate output file %q to %d bytes: %w", path, keep, err)
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
	if err := writeCommitted(c, keep); err != nil {
		return nil, fmt.Errorf("record committed length in %q: %w", cpath, err)
	}
	ok = true
	return c, nil
}

// lengthToKeep returns how many bytes of the output (r, size bytes) are kept,
// from the committed-length file c (csize bytes).
func lengthToKeep(ctx context.Context, path string, c *os.File, csize int64, r io.ReaderAt, size int64) (int64, error) {
	cpath := committedPath(path)
	switch csize {
	case 0:
		// No length recorded: an output written by an earlier version, or
		// one the destination closed cleanly. All a torn write of
		// newline-terminated records can leave is a partial last line.
		return lastLineEnd(r, size)
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

	mismatch := func(msg string) error {
		return &CodedError{
			Code: ErrCodeCommittedLengthMismatch,
			Path: path,
			Msg:  msg,
			Fix: fmt.Sprintf("the output file was truncated or replaced outside the connector; check it, then delete %q "+
				"to keep the output as it is up to its last complete line", cpath),
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
