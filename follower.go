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
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// errNotRegular is returned when the path names something other than a regular
// file (a FIFO would block the open forever, a device never ends).
var errNotRegular = errors.New("not a regular file")

var errFollowerClosed = errors.New("follower is closed")

const (
	// followBufferSize is the size of the read buffer.
	followBufferSize = 64 * 1024
	// pollInitial and pollCeiling bound the wait between checks once the end of
	// the file is reached. The wait starts short and doubles while the file
	// stays idle, and resets as soon as a line is read.
	pollInitial = 2 * time.Millisecond
	pollCeiling = 50 * time.Millisecond
	// verifyEvery is how many idle waits pass between checks that the path
	// still refers to the file being followed (one open through the opener).
	verifyEvery = 4
	// pathPollEvery is how often the path is looked at while lines keep
	// coming, so that a file rotated in while we are still behind in the
	// previous one is noticed (and kept open) before it is rotated away again.
	pathPollEvery = 200 * time.Millisecond
	// maxQueuedFiles bounds the files held open for rotations seen while behind.
	maxQueuedFiles = 64
)

// queuedFile is a file that appeared at the path while the followed file was
// still being read. It is held open so it can be read even if it is renamed
// again before the follower gets to it.
type queuedFile struct {
	f  *os.File
	fi os.FileInfo
}

type switchKind int

const (
	switchNone switchKind = iota
	switchTruncated
	switchRotated
)

// follower reads a file line by line and keeps following it as it grows, like
// `tail -f`. It replaces github.com/nxadm/tail so that every open of the path
// goes through an [Opener], which an embedding engine uses to enforce its path
// policy at open(2): at the first open, at every re-open after a truncation or
// rotation, and for the periodic check that the path still names the same file.
// A symlink swapped in after the path was checked is refused by the opener and
// surfaces as an error; it is never read.
//
// Semantics:
//
//   - A line is returned without its trailing "\n" (only "\n"; "\r" is kept).
//   - A line is returned once its newline is written. The position after a line
//     is the byte offset just past it, "\n" included, so a position never points
//     into a line. The exception is the last line of a file that has no
//     newline: it is returned after the file has not changed for the quiet
//     period (0 returns it at once, as v0.10.x did), when the file is rotated
//     or truncated, or by takeTail on a graceful stop. Data appended to such a
//     line later is returned as a separate line.
//   - Line numbers count lines returned since the file was opened or re-opened
//     (not file line numbers): they start at 1 and restart at 1 after a
//     truncation or rotation.
//   - A file that does not exist yet is waited for.
//   - A truncated file (size below what was read, or its first bytes changed:
//     copytruncate) is re-opened and read from its start.
//   - A file that is renamed, removed or replaced (rotation) is read to its end
//     and then the file now at the path, once there is one, is read from its
//     start.
//   - Resuming from a saved position first checks that it still refers to the
//     file at the path (see [position]).
type follower struct {
	path  string
	open  Opener
	quiet time.Duration
	logf  func(format string, args ...any)

	// mu serializes next, takeTail, start and close: Teardown may run while a
	// Read is still waiting for data. It is never held while sleeping.
	mu     sync.Mutex
	closed bool

	resume  *position // applied once, to the first file that is opened
	resumed bool

	f      *os.File
	r      *bufio.Reader
	id     os.FileInfo // identity of f
	dev    uint64
	ino    uint64
	offset int64 // position after the last line returned
	lineNo int64

	queue    []queuedFile // files seen at the path after the followed one, oldest first
	lastPoll time.Time

	partial   []byte    // bytes read past offset that do not yet end in "\n"
	partialAt time.Time // when partial last grew
	switching switchKind

	headLen int64             // bytes covered by head
	head    [sha256.Size]byte // fingerprint of the first headLen bytes
	suffix  string            // position suffix for the current head
	lastPos string            // v1 position after the last line returned
}

func newFollower(path string, open Opener, resume *position, quiet time.Duration, logf func(string, ...any)) *follower {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &follower{path: path, open: open, resume: resume, quiet: quiet, logf: logf}
}

// start opens the file if it exists. A missing file is not an error: next waits
// for it. Any other failure, including a refusal by the opener or a position
// that cannot be verified, is.
func (fl *follower) start() error {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	_, err := fl.tryOpen()
	return err
}

func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

func (fl *follower) coded(code, msg, fix string, err error) error {
	return &CodedError{Code: code, Path: fl.path, Msg: msg, Fix: fix, Err: err}
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
	var start int64
	rotatedAway := false
	if !fl.resumed {
		fl.resumed = true
		if fl.resume != nil {
			var old *os.File
			if start, old, err = fl.resolveResume(f, fi); err != nil {
				f.Close()
				return false, err
			}
			if old != nil {
				// The saved position belongs to a file that was rotated away
				// while we were down: finish it first, then the file at the path.
				f.Close()
				f, rotatedAway = old, true
				if fi, err = f.Stat(); err != nil {
					f.Close()
					return false, fmt.Errorf("could not stat file: %w", err)
				}
			}
		}
		if start > 0 {
			if _, err := f.Seek(start, io.SeekStart); err != nil {
				f.Close()
				return false, fmt.Errorf("could not seek to position %d: %w", start, err)
			}
		}
	}
	fl.install(f, fi, start)
	if rotatedAway {
		fl.switching = switchRotated
	}
	return true, nil
}

// install makes f the followed file, positioned at start.
func (fl *follower) install(f *os.File, fi os.FileInfo, start int64) {
	fl.offset, fl.lineNo, fl.partial, fl.headLen, fl.suffix = start, 0, nil, 0, ""
	fl.switching = switchNone
	fl.f, fl.id = f, fi
	fl.dev, fl.ino, _ = fileID(fi)
	if fl.r == nil {
		fl.r = bufio.NewReaderSize(f, followBufferSize)
	} else {
		fl.r.Reset(f)
	}
}

// resolveResume decides where in f the saved position resumes.
//
// A v1 position resumes at its offset when f is the file it was taken from
// (same device and inode where known, at least offset bytes long, same first
// bytes). Otherwise the file at the path is a different one (rotated or
// replaced while the connector was down) and is read from its start; data that
// was left unread in the file that was rotated away is not reachable from here.
//
// A legacy offset has no identity, so it is only accepted when it is within
// the file and follows a newline; anything else would be a guess.
func (fl *follower) resolveResume(f *os.File, fi os.FileInfo) (start int64, rotated *os.File, err error) {
	p := fl.resume
	size := fi.Size()
	if p.legacy {
		if p.offset > size {
			return 0, nil, fl.coded(ErrCodePositionUnverifiable,
				fmt.Sprintf("saved offset %d is beyond the end of the file (%d bytes)", p.offset, size),
				"if the file was rotated or truncated while the connector was stopped, reset the pipeline's position; otherwise restore the file", nil)
		}
		if p.offset > 0 {
			var b [1]byte
			if _, err := f.ReadAt(b[:], p.offset-1); err != nil {
				return 0, nil, fl.coded(ErrCodeIdentityUnknown, "could not read the file at the saved offset", "check that the file is readable", err)
			}
			if b[0] != '\n' {
				return 0, nil, fl.coded(ErrCodePositionUnverifiable,
					fmt.Sprintf("saved offset %d is not at the start of a line in the current file", p.offset),
					"the file was probably rotated or replaced; reset the pipeline's position to read the current file from its start", nil)
			}
		}
		return p.offset, nil, nil
	}

	dev, ino, known := fileID(fi)
	same := size >= p.offset
	if same && known && p.idKnown() && (dev != p.dev || ino != p.ino) {
		same = false
	}
	if same && p.fpLen > 0 {
		fp, err := fingerprint(f, p.fpLen)
		switch {
		case errors.Is(err, io.ErrUnexpectedEOF):
			same = false
		case err != nil:
			return 0, nil, fl.coded(ErrCodeIdentityUnknown, "could not read the start of the file to check its identity", "check that the file is readable", err)
		default:
			same = bytes.Equal(fp[:], p.fp[:])
		}
	}
	if !same {
		if old := fl.findRotated(p); old != nil {
			fl.logf("the file position %d belongs to was rotated away from %q while the connector was stopped: reading the rest of it, then the file now at the path from its start", p.offset, fl.path)
			return p.offset, old, nil
		}
		fl.logf("the file at %q is not the file position %d belongs to (rotated or replaced); reading it from its start", fl.path, p.offset)
		return 0, nil, nil
	}
	return p.offset, nil, nil
}

// maxRotatedScan bounds how many directory entries are examined when looking
// for a rotated-away file.
const maxRotatedScan = 100000

// findRotated looks next to the configured path for the file p was taken from,
// which log rotation usually only renames. It matches device and inode and then
// checks size and fingerprint, and opens it through the opener like any other
// file. It returns nil when there is no such file (deleted, compressed, moved
// elsewhere, or the platform has no inode numbers).
func (fl *follower) findRotated(p *position) *os.File {
	if !p.idKnown() {
		return nil
	}
	dir := filepath.Dir(fl.path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for i, e := range entries {
		if i >= maxRotatedScan {
			break
		}
		if !e.Type().IsRegular() || e.Name() == filepath.Base(fl.path) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if dev, ino, ok := fileID(info); !ok || dev != p.dev || ino != p.ino {
			continue
		}
		f, err := fl.open(filepath.Join(dir, e.Name()), os.O_RDONLY, 0)
		if err != nil {
			continue
		}
		fi, err := f.Stat()
		if err == nil && fi.Mode().IsRegular() && os.SameFile(info, fi) && fi.Size() >= p.offset {
			if p.fpLen == 0 {
				return f
			}
			if fp, err := fingerprint(f, p.fpLen); err == nil && fp == p.fp {
				return f
			}
		}
		f.Close()
	}
	return nil
}

// next returns the next line, waiting for it if the file has none. It returns
// the line, its number, and the offset after it; position gives the matching
// saved position.
func (fl *follower) next(ctx context.Context) (line string, num, offset int64, err error) {
	wait := pollInitial
	idle := 0
	for {
		if err := ctx.Err(); err != nil {
			return "", 0, 0, err
		}
		line, num, offset, ok, err := fl.step(idle)
		if err != nil || ok {
			return line, num, offset, err
		}
		// Nothing to return yet: wait, then look again.
		if err := sleep(ctx, wait); err != nil {
			return "", 0, 0, err
		}
		wait = min(wait*2, pollCeiling)
		idle++
	}
}

// position returns the saved position after the line most recently returned.
func (fl *follower) position() string {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	return fl.lastPos
}

// takeTail returns the bytes read past the last newline as a line, if there
// are any. It is the graceful-stop flush: the caller emits the record and its
// position is checkpointed like any other. It reads nothing new, so it stays
// bounded; a tail that was not read yet is replayed after the next start.
func (fl *follower) takeTail() (line string, num, offset int64, ok bool) {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if fl.closed || fl.f == nil || len(fl.partial) == 0 {
		return "", 0, 0, false
	}
	line, num, offset, _, err := fl.emit("", false)
	return line, num, offset, err == nil
}

// emit returns the line made of the pending partial bytes and raw.
func (fl *follower) emit(raw string, complete bool) (line string, num, offset int64, ok bool, err error) {
	if len(fl.partial) > 0 {
		raw = string(fl.partial) + raw
	}
	prevOffset := fl.offset
	fl.offset += int64(len(raw))
	if err := fl.updateHead(); err != nil {
		if fl.switching == switchTruncated && errors.Is(err, io.ErrUnexpectedEOF) {
			// The bytes of this tail are gone (the file was truncated under
			// us). Return it anyway, with a fingerprint that matches nothing,
			// so a restart from this position reads the file from its start.
			n := min(fl.offset, fingerprintSize)
			fl.headLen, fl.head = n, [sha256.Size]byte{}
			fl.suffix = posSuffix(fl.dev, fl.ino, n, fl.head)
		} else {
			fl.offset = prevOffset // nothing was consumed: the bytes stay pending
			return "", 0, 0, false, err
		}
	}
	fl.partial = nil
	fl.lineNo++
	fl.lastPos = "v1:" + strconv.FormatInt(fl.offset, 10) + fl.suffix
	if complete {
		raw = strings.TrimSuffix(raw, "\n")
	}
	return raw, fl.lineNo, fl.offset, true, nil
}

// updateHead keeps the fingerprint of the first min(offset, 4096) bytes current.
// It only changes while the offset is below 4096.
func (fl *follower) updateHead() error {
	n := min(fl.offset, fingerprintSize)
	if n == fl.headLen && fl.suffix != "" {
		return nil
	}
	fp, err := fingerprint(fl.f, n)
	if err != nil {
		return fmt.Errorf("could not fingerprint file (it changed while being read): %w", err)
	}
	fl.headLen, fl.head = n, fp
	fl.suffix = posSuffix(fl.dev, fl.ino, n, fp)
	return nil
}

// step makes one attempt to produce a line. ok is false when there is nothing
// to return yet. idle counts the waits since the last line.
func (fl *follower) step(idle int) (line string, num, offset int64, ok bool, err error) {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if fl.closed {
		return "", 0, 0, false, errFollowerClosed
	}
	if fl.f == nil {
		_, err := fl.tryOpen()
		return "", 0, 0, false, err
	}

	if now := time.Now(); now.Sub(fl.lastPoll) >= pathPollEvery {
		fl.lastPoll = now
		if err := fl.pollPath(); err != nil {
			return "", 0, 0, false, err
		}
	}

	// Before the buffer is refilled from the file, make sure it is still the
	// file we have been reading: a copytruncate that regrew past our offset
	// within one poll leaves the size alone but changes the first bytes.
	if fl.switching == switchNone && fl.r.Buffered() == 0 {
		if err := fl.checkTruncated(); err != nil {
			return "", 0, 0, false, err
		}
	}
	raw, rerr := "", io.EOF
	if fl.switching != switchTruncated { // what the old content held is gone
		raw, rerr = fl.r.ReadString('\n')
	}
	switch {
	case rerr == nil:
		return fl.emit(raw, true)
	case errors.Is(rerr, io.EOF):
		if raw != "" {
			fl.partial = append(fl.partial, raw...)
			fl.partialAt = time.Now()
		}
	default:
		return "", 0, 0, false, fmt.Errorf("error reading file: %w", rerr)
	}

	// At the end of the file. A tail without a newline is held back until the
	// file has been quiet, or until it can never be completed.
	if len(fl.partial) > 0 && (fl.switching != switchNone || time.Since(fl.partialAt) >= fl.quiet) {
		return fl.emit("", false)
	}
	if fl.switching != switchNone {
		// Everything the old file held has been read and returned.
		return "", 0, 0, false, fl.doSwitch()
	}
	if idle == 0 {
		return "", 0, 0, false, nil
	}
	if idle%verifyEvery != 0 {
		return "", 0, 0, false, nil
	}
	fl.lastPoll = time.Now()
	return "", 0, 0, false, fl.pollPath()
}

// checkTruncated marks the file as truncated when it is shorter than what was
// read from it or its first bytes are not the ones the position was taken from.
func (fl *follower) checkTruncated() error {
	fi, err := fl.f.Stat()
	if err != nil {
		return fmt.Errorf("could not stat file: %w", err)
	}
	if fi.Size() < fl.offset+int64(len(fl.partial)) {
		fl.switching = switchTruncated
		return nil
	}
	if fl.headLen == 0 {
		return nil
	}
	fp, err := fingerprint(fl.f, fl.headLen)
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF), err == nil && fp != fl.head:
		fl.switching = switchTruncated
	case err != nil:
		return fmt.Errorf("could not fingerprint file: %w", err)
	}
	return nil
}

// pollPath looks at what the path names now. If it is not the last file we know
// of (the followed one, or the newest queued one) it is a rotation: the new file
// is opened through the opener and queued, so it is read even if it is rotated
// away again before the followed file is finished. step first reads whatever the
// old file still has and then calls doSwitch.
func (fl *follower) pollPath() error {
	f, err := fl.open(fl.path, os.O_RDONLY, 0)
	if err != nil {
		if !isNotExist(err) {
			return fmt.Errorf("could not re-open file: %w", err) // includes a refused symlink swap
		}
		if len(fl.queue) == 0 && fl.switching == switchNone {
			fl.switching = switchRotated // removed or renamed; wait for a new one
		}
		return nil
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("could not stat file: %w", err)
	}
	last := fl.id
	if n := len(fl.queue); n > 0 {
		last = fl.queue[n-1].fi
	}
	if os.SameFile(last, fi) || (fl.switching == switchTruncated && len(fl.queue) == 0) {
		f.Close()
		return nil
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return fmt.Errorf("%w: %s", errNotRegular, fi.Mode().Type())
	}
	if len(fl.queue) >= maxQueuedFiles {
		f.Close() // the file at the path is picked up when the queue drains
		return nil
	}
	fl.queue = append(fl.queue, queuedFile{f, fi})
	fl.switching = switchRotated
	return nil
}

// doSwitch leaves the followed file and follows the next one from its start: the
// oldest file queued by pollPath, else the file now at the path (through the
// opener). If there is none yet it waits for one.
func (fl *follower) doSwitch() error {
	kind := fl.switching
	fl.switching = switchNone
	_ = fl.f.Close()
	fl.f = nil
	if kind != switchRotated {
		fl.logf("file %q was truncated: reading it again from its start", fl.path)
	} else {
		fl.logf("file %q was rotated, removed or replaced: following the next file at the path from its start", fl.path)
	}
	if kind == switchRotated && len(fl.queue) > 0 {
		q := fl.queue[0]
		fl.queue = fl.queue[1:]
		fl.install(q.f, q.fi, 0)
		if len(fl.queue) > 0 {
			fl.switching = switchRotated
		}
		return nil
	}
	_, err := fl.tryOpen()
	return err
}

func (fl *follower) close() error {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	fl.closed = true
	for _, q := range fl.queue {
		_ = q.f.Close()
	}
	fl.queue = nil
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
