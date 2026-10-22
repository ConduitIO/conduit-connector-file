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
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// errFileGone is returned by the source when the path no longer refers to the
// file it was following: the file was removed, renamed (rotated) or replaced.
// github.com/nxadm/tail stopped silently in that case and the source then
// returned an empty record with a nil error; the follower reports it.
var errFileGone = errors.New("file was removed, renamed or replaced")

// errNotRegular is returned when the path names something other than a regular
// file (a FIFO would block the open forever, a device never ends).
var errNotRegular = errors.New("not a regular file")

const (
	// followBufferSize is the size of the read buffer.
	followBufferSize = 64 * 1024
	// pollInitial and pollCeiling bound the wait between checks once the end of the
	// file is reached. The wait starts short and doubles while the file stays
	// idle, and resets as soon as a line is read.
	pollInitial = 2 * time.Millisecond
	pollCeiling = 50 * time.Millisecond
	// verifyEvery is how many idle waits pass between checks that the path
	// still refers to the file being followed (one open through the opener).
	verifyEvery = 4
)

// follower reads a file line by line and keeps following it as it grows, like
// `tail -f` without re-opening on rotation. It replaces github.com/nxadm/tail
// so that every open of the path goes through an [Opener], which an embedding
// engine uses to enforce its path policy at open(2): at the first open and at
// every re-open after a truncation, and for the periodic check that the path
// still names the same file. A symlink swapped in after the path was checked is
// refused by the opener and surfaces as an error; it is never read.
//
// Semantics deliberately match nxadm/tail as the file source used it
// (Follow, no ReOpen, no MustExist):
//
//   - A line is returned without its trailing "\n" (only "\n"; "\r" is kept).
//   - The position after a line is the byte offset just past it, "\n" included.
//     That is the position format of earlier versions, unchanged.
//   - A partial last line (no "\n" yet) is returned immediately, and the rest of
//     that line, once written, is returned as a separate line.
//   - Line numbers count lines returned since the file was opened or re-opened
//     (not file line numbers): they start at 1 and restart at 1 after a
//     truncation.
//   - A file that does not exist yet is waited for.
//   - A truncated file (size below the current offset) is re-opened and read
//     from its start.
//   - A file that is removed, renamed or replaced ends the follow.
type follower struct {
	path string
	open Opener

	// mu serializes next, start and close: Teardown may run while a Read is
	// still waiting for data. It is never held while sleeping.
	mu     sync.Mutex
	closed bool

	startOffset int64 // applied once, to the first file that is opened
	seeked      bool

	f       *os.File
	r       *bufio.Reader
	id      os.FileInfo // identity of f
	offset  int64       // bytes consumed from the start of f; the position after the last line
	lineNum int64
}

func newFollower(path string, open Opener, startOffset int64) *follower {
	return &follower{path: path, open: open, startOffset: startOffset}
}

// start opens the file if it exists. A missing file is not an error: next waits
// for it. Any other failure, including a refusal by the opener, is.
func (fl *follower) start() error {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	_, err := fl.tryOpen()
	return err
}

func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// tryOpen opens the path if there is a file and installs it as the followed
// file. It reports false, nil when the path does not exist.
func (fl *follower) tryOpen() (bool, error) {
	if fl.f != nil {
		return true, nil
	}
	f, err := fl.open(fl.path, os.O_RDONLY, 0)
	if err != nil {
		if isNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("could not open file: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return false, fmt.Errorf("could not stat file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return false, fmt.Errorf("%w: %s", errNotRegular, fi.Mode().Type())
	}
	if !fl.seeked {
		if _, err := f.Seek(fl.startOffset, io.SeekStart); err != nil {
			f.Close()
			return false, fmt.Errorf("could not seek to position %d: %w", fl.startOffset, err)
		}
		fl.offset = fl.startOffset
		fl.seeked = true
	} else {
		fl.offset = 0
	}
	fl.lineNum = 0
	fl.f, fl.id = f, fi
	if fl.r == nil {
		fl.r = bufio.NewReaderSize(f, followBufferSize)
	} else {
		fl.r.Reset(f)
	}
	return true, nil
}

// next returns the next line, waiting for it if the file has none. It returns
// the line, its number, and the position after it.
func (fl *follower) next(ctx context.Context) (line string, num, pos int64, err error) {
	wait := pollInitial
	idle := 0
	for {
		if err := ctx.Err(); err != nil {
			return "", 0, 0, err
		}
		line, num, pos, ok, err := fl.step(idle)
		if err != nil || ok {
			return line, num, pos, err
		}
		// Nothing to return yet: wait, then look again.
		if err := sleep(ctx, wait); err != nil {
			return "", 0, 0, err
		}
		wait = min(wait*2, pollCeiling)
		idle++
	}
}

// step makes one attempt to produce a line. ok is false when there is nothing
// to return yet. idle counts the waits since the last line.
func (fl *follower) step(idle int) (line string, num, pos int64, ok bool, err error) {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if fl.closed {
		return "", 0, 0, false, errFollowerClosed
	}
	if fl.f == nil {
		_, err := fl.tryOpen()
		return "", 0, 0, false, err
	}

	raw, err := fl.r.ReadString('\n')
	switch {
	case err == nil:
	case errors.Is(err, io.EOF):
		if raw == "" {
			// At the end of the file: see what happened to it. The first
			// idle step only reads; checks start with the second.
			if idle == 0 {
				return "", 0, 0, false, nil
			}
			return "", 0, 0, false, fl.checkFile(idle%verifyEvery == 0)
		}
		// A partial last line is returned at once, as nxadm/tail did.
	default:
		return "", 0, 0, false, fmt.Errorf("error reading file: %w", err)
	}
	fl.offset += int64(len(raw))
	fl.lineNum++
	return strings.TrimRight(raw, "\n"), fl.lineNum, fl.offset, true, nil
}

// checkFile looks for a truncation of the followed file and, when verifyPath is
// set, checks that the path still names the same file.
func (fl *follower) checkFile(verifyPath bool) error {
	fi, err := fl.f.Stat()
	if err != nil {
		return fmt.Errorf("could not stat file: %w", err)
	}
	if fi.Size() < fl.offset {
		return fl.reopenTruncated()
	}
	if !verifyPath {
		return nil
	}
	cur, err := fl.openPath()
	if err != nil {
		return err
	}
	if !os.SameFile(fl.id, cur) {
		return errFileGone
	}
	return nil
}

// openPath opens the path through the opener, returns the identity of what it
// found and closes it again. An absent path ends the follow.
func (fl *follower) openPath() (os.FileInfo, error) {
	f, err := fl.open(fl.path, os.O_RDONLY, 0)
	if err != nil {
		if isNotExist(err) {
			return nil, errFileGone
		}
		return nil, fmt.Errorf("could not re-open file: %w", err) // includes a refused symlink swap
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("could not stat file: %w", err)
	}
	return fi, nil
}

// reopenTruncated re-opens the path after a truncation and reads from the start.
// If the path now names a different file the follow ends instead: that is a
// rotation, not a truncation.
func (fl *follower) reopenTruncated() error {
	f, err := fl.open(fl.path, os.O_RDONLY, 0)
	if err != nil {
		if isNotExist(err) {
			return errFileGone
		}
		return fmt.Errorf("could not re-open file: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("could not stat file: %w", err)
	}
	if !os.SameFile(fl.id, fi) {
		f.Close()
		return errFileGone
	}
	_ = fl.f.Close()
	fl.f, fl.id = f, fi
	fl.r.Reset(f)
	fl.offset = 0
	fl.lineNum = 0
	return nil
}

var errFollowerClosed = errors.New("follower is closed")

func (fl *follower) close() error {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	fl.closed = true
	if fl.f == nil {
		return nil
	}
	err := fl.f.Close()
	fl.f = nil
	return err
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
