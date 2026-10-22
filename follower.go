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
	// cont marks the continuation of a file that was being read when an older
	// file turned out to have grown (see checkLate): it is entered at start,
	// not at its head, and without accounting for files in between.
	cont  bool
	start int64
}

// lateFile is a rotated-away file the follower finished and still holds open: a
// writer that kept its descriptor (logrotate renames, then signals the program
// later) can append to it after the follower switched away.
type lateFile struct {
	f     *os.File
	fi    os.FileInfo
	off   int64
	until time.Time
}

const (
	// lateWindow is how long a finished file is kept open and re-checked.
	lateWindow = 5 * time.Minute
	// maxLateFiles bounds the descriptors held for finished files.
	maxLateFiles = 16
)

// finishedFile is a file the follower read to its end. It is remembered by
// identity and by content (the first bytes and the size), because file systems
// reuse inode numbers: a new file that got the number of a finished one must
// not be taken for it.
type finishedFile struct {
	fi      os.FileInfo
	size    int64
	headLen int64
	head    [sha256.Size]byte
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
//     newline: it is held until its newline is written, the file is rotated or
//     truncated. A graceful stop does not return it: the position stays at the
//     start of that line and the whole line is read after the restart. Only when the quiet
//     period is set (opt-in; 0 holds) is it also returned after the file has not
//     changed for that long. Data appended to such a line later is returned as a
//     separate line.
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
	// maxLine is the longest line (bytes, newline included) that is read;
	// longer is an error, never a truncation.
	maxLine int64
	logf    func(format string, args ...any)

	// mu serializes next, start and close: Teardown may run while a
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

	settle     time.Duration // a rotated file is left only after its size was stable this long
	settleSize int64
	settleAt   time.Time
	late       []lateFile
	finished   []finishedFile // files of the chain already read to their end
	rot        rotationOptions
	hopFrom    os.FileInfo // the file just finished, until the next one is entered
	warnedFull bool

	queue    []queuedFile // files seen at the path after the followed one, oldest first
	lastPoll time.Time

	partial   []byte    // bytes read past offset that do not yet end in "\n"
	partialAt time.Time // when partial last grew
	switching switchKind

	lastSize    int64 // size of the file at the last check
	truncUnread int64 // bytes that were unread when a truncation was found

	headLen int64             // bytes covered by head
	head    [sha256.Size]byte // fingerprint of the first headLen bytes
	suffix  string            // position suffix for the current head
	lastPos string            // v1 position after the last line returned
}

// withSettle sets how long a rotated file's size must stay unchanged before the
// follower leaves it (0: at once). A writer that still holds the old file open
// keeps appending until it is told to reopen; those lines are read first.
func (fl *follower) withSettle(d time.Duration) *follower {
	fl.settle = d
	return fl
}

// withMaxLine sets the longest line that is read (0: the default).
func (fl *follower) withMaxLine(n int64) *follower {
	if n > 0 {
		fl.maxLine = n
	}
	return fl
}

// withRotation sets the operator's rotation options.
func (fl *follower) withRotation(o rotationOptions) *follower {
	fl.rot = o
	if fl.rot.warnf == nil {
		fl.rot.warnf = fl.logf
	}
	return fl
}

func newFollower(path string, open Opener, resume *position, quiet time.Duration, logf func(string, ...any)) *follower {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	fl := &follower{path: path, open: open, resume: resume, quiet: quiet, logf: logf}
	fl.rot.warnf = logf
	fl.maxLine = defaultMaxLineSize
	return fl
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
	f, err := fl.open(fl.path, openFlags, 0)
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
	if fl.hopFrom != nil {
		// The first file at the path after the followed one was finished.
		return true, fl.enter(f)
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
	fl.lastSize, fl.truncUnread = 0, 0
	fl.settleAt, fl.settleSize = time.Time{}, 0
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
//
//nolint:gocyclo // one decision table over the saved position, split would hide it
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
		dev, ino, _ := fileID(fi)
		fl.rot.warnf("[%s] resuming from an offset-only position written by v0.10.x: offset %d in %q (inode %d on device %d, %d bytes, modified %s). The position carries no file identity, so this cannot be confirmed to be the file it was taken from; if it was rotated or replaced while upgrading, the first %d bytes of the current file are skipped. Positions written from now on carry the identity",
			ErrCodePositionUnverifiable, p.offset, fl.path, ino, dev, size, fi.ModTime().UTC().Format(time.RFC3339), p.offset)
		return p.offset, nil, nil
	}

	dev, ino, known := fileID(fi)
	same := size >= p.offset
	// The inode decides, not the device number: st_dev changes across reboots
	// and remounts of the same file system (device-mapper and network volumes,
	// containers), while the fingerprint below confirms the content.
	if same && known && p.idKnown() && ino != p.ino {
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
	if same && known && p.idKnown() && dev != p.dev {
		fl.logf("the device number of %q changed (%d, was %d) but the inode, size and first bytes still match: resuming at offset %d", fl.path, dev, p.dev, p.offset)
	}
	if !same {
		old, err := fl.findRotated(p)
		if err != nil {
			return 0, nil, err
		}
		if old != nil {
			fl.logf("the file position %d belongs to was rotated away from %q while the connector was stopped: reading the rest of it, then the file now at the path from its start", p.offset, fl.path)
			return p.offset, old, nil
		}
		found := fmt.Sprintf("the file at the path has inode %d, %d bytes", ino, size)
		var msg string
		switch {
		case known && p.idKnown() && ino == p.ino:
			// Same inode, different content: truncated or rewritten in place
			// (copytruncate), or deleted and the inode reused.
			msg = fmt.Sprintf("the file at the path was truncated or rewritten while the connector was stopped (same inode %d, but its size or first bytes differ from position %d); the content the position belongs to is gone and its unread tail (size unknown) is lost", ino, p.offset)
		default:
			msg = fmt.Sprintf("position %d belongs to a file (inode %d, at least %d bytes) that is no longer at the path and was not found; %s; its unread tail (size unknown) is lost", p.offset, p.ino, p.offset, found)
		}
		if err := fl.tailUnreadable(msg); err != nil {
			return 0, nil, err
		}
		return 0, nil, nil
	}
	return p.offset, nil, nil
}

// findRotated looks for the file p was taken from among the files next to the
// configured path (any name) or, with rotatedPattern, among its matches. Log
// rotation usually only renames it. It matches device and inode and then
// checks size and fingerprint, and opens it through the opener like any other
// file. It returns nil when there is no such file (deleted, compressed, moved
// elsewhere, or the platform has no inode numbers).
func (fl *follower) findRotated(p *position) (*os.File, error) {
	if !p.idKnown() {
		return nil, nil
	}
	cands, err := fl.family(true)
	if err != nil {
		return nil, err
	}
	for _, c := range cands {
		info := c.info
		if dev, ino, ok := fileID(info); !ok || dev != p.dev || ino != p.ino {
			continue
		}
		f, err := fl.open(c.path, openFlags, 0)
		if err != nil {
			continue
		}
		fi, err := f.Stat()
		if err == nil && fi.Mode().IsRegular() && os.SameFile(info, fi) && fi.Size() >= p.offset {
			if p.fpLen == 0 {
				return f, nil
			}
			if fp, err := fingerprint(f, p.fpLen); err == nil && fp == p.fp {
				return f, nil
			}
		}
		f.Close()
	}
	return nil, nil
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

// defaultMaxLineSize is the longest line read when none is configured.
const defaultMaxLineSize = 16 << 20

// readLine reads up to and including the next newline. A line longer than the
// limit (with the bytes already pending) is an error: it is never cut. The
// bytes read so far are not consumed from the follower's accounting, so after
// the error the position is still at the start of the line.
func (fl *follower) readLine() (line string, rerr, tooLong error) {
	var buf []byte
	for {
		chunk, err := fl.r.ReadSlice('\n')
		if len(buf) == 0 && err == nil && int64(len(chunk))+int64(len(fl.partial)) <= fl.maxLine {
			return string(chunk), nil, nil // the common case: one slice, no copy of its own
		}
		buf = append(buf, chunk...)
		if int64(len(buf))+int64(len(fl.partial)) > fl.maxLine {
			return "", nil, fl.coded(ErrCodeLineTooLong,
				fmt.Sprintf("a line at offset %d is longer than maxLineSize (%d bytes)", fl.offset, fl.maxLine),
				"raise maxLineSize, or fix the writer; the source does not truncate lines", nil)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return string(buf), err, nil
	}
}

// position returns the saved position after the line most recently returned.
func (fl *follower) position() string {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	return fl.lastPos
}

// emit returns the line made of the pending partial bytes and raw.
func (fl *follower) emit(raw string, complete bool) (line string, num, offset int64, ok bool, err error) {
	if len(fl.partial) > 0 {
		raw = string(fl.partial) + raw
	}
	prevOffset := fl.offset
	fl.offset += int64(len(raw))
	if err := fl.updateHead(); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			// The first bytes are gone: the file was truncated under us (the
			// next refill of the buffer notices). Return the line anyway, with a
			// fingerprint that matches nothing, so a restart from this position
			// stops instead of resuming in a different file.
			// fl.head stays the fingerprint of the old content (the copy of a
			// copytruncate is recognized by it).
			n := min(fl.offset, fingerprintSize)
			fl.suffix = posSuffix(fl.dev, fl.ino, n, [sha256.Size]byte{})
		} else {
			fl.offset = prevOffset // nothing was consumed: the bytes stay pending
			return "", 0, 0, false, err
		}
	}
	fl.partial = nil
	fl.settleAt = time.Time{}
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
//
//nolint:gocyclo // one sequence of cases: poll, check, read, flush, switch
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
		if err := fl.checkLate(); err != nil {
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
		var tooLong error
		if raw, rerr, tooLong = fl.readLine(); tooLong != nil {
			return "", 0, 0, false, tooLong
		}
	}
	switch {
	case rerr == nil:
		return fl.emit(raw, true)
	case errors.Is(rerr, io.EOF):
		if raw != "" {
			fl.partial = append(fl.partial, raw...)
			fl.partialAt = time.Now()
			fl.settleAt = time.Time{}
		}
	default:
		return "", 0, 0, false, fmt.Errorf("error reading file: %w", rerr)
	}

	// At the end of the file. A tail without a newline is held back until the
	// file has been quiet, or until it can never be completed.
	// Not when the file was truncated with unread data: then the partial bytes
	// are the head of a line whose rest only exists in the copy (or is lost),
	// and a piece of a line is never a record.
	if fl.switching == switchRotated && fl.settle > 0 {
		settled, err := fl.settled()
		if err != nil {
			return "", 0, 0, false, err
		}
		if !settled {
			return "", 0, 0, false, nil // keep reading the old file until it stops growing
		}
	}
	unreadLoss := fl.switching == switchTruncated && fl.truncUnread > 0
	if len(fl.partial) > 0 && !unreadLoss && (fl.switching != switchNone || (fl.quiet > 0 && time.Since(fl.partialAt) >= fl.quiet)) {
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

// settled reports whether the rotated file's size has been stable for the settle
// period.
func (fl *follower) settled() (bool, error) {
	fi, err := fl.f.Stat()
	if err != nil {
		return false, fmt.Errorf("could not stat file: %w", err)
	}
	if fl.settleAt.IsZero() || fi.Size() != fl.settleSize {
		fl.settleAt, fl.settleSize = time.Now(), fi.Size()
		return false, nil
	}
	return time.Since(fl.settleAt) >= fl.settle, nil
}

// checkLate looks at the finished files still held open. If one has grown (a
// writer kept appending after the rotation) the follower goes back to it,
// reads the new data, and continues with the file it was in. It only does so
// between lines, with nothing buffered.
func (fl *follower) checkLate() error {
	now := time.Now()
	kept := fl.late[:0]
	for _, l := range fl.late {
		if now.After(l.until) {
			_ = l.f.Close()
			continue
		}
		kept = append(kept, l)
	}
	fl.late = kept
	if fl.switching != switchNone || fl.f == nil || fl.r.Buffered() != 0 || len(fl.partial) != 0 {
		return nil
	}
	for i, l := range fl.late {
		fi, err := l.f.Stat()
		if err != nil || fi.Size() <= l.off {
			continue
		}
		fl.late = append(fl.late[:i], fl.late[i+1:]...)
		cont := queuedFile{f: fl.f, fi: fl.id, cont: true, start: fl.offset}
		fl.queue = append([]queuedFile{cont}, fl.queue...)
		fl.install(l.f, l.fi, l.off)
		if _, err := l.f.Seek(l.off, io.SeekStart); err != nil {
			return fmt.Errorf("could not seek to %d: %w", l.off, err)
		}
		fl.switching = switchRotated // after it settles, back to the file we were in
		fl.logf("file %q kept growing after it was rotated: reading the new data, then continuing", fl.path)
		return nil
	}
	return nil
}

// checkTruncated marks the file as truncated when it is shorter than what was
// read from it or its first bytes are not the ones the position was taken from.
func (fl *follower) checkTruncated() error {
	fi, err := fl.f.Stat()
	if err != nil {
		return fmt.Errorf("could not stat file: %w", err)
	}
	consumed := fl.offset + int64(len(fl.partial))
	last := fl.lastSize
	fl.lastSize = fi.Size()
	truncated := fi.Size() < consumed
	if !truncated && fl.headLen > 0 {
		fp, err := fingerprint(fl.f, fl.headLen)
		switch {
		case errors.Is(err, io.ErrUnexpectedEOF), err == nil && fp != fl.head:
			truncated = true
		case err != nil:
			return fmt.Errorf("could not fingerprint file: %w", err)
		}
	}
	if truncated {
		fl.switching = switchTruncated
		// What was in the file past our position when we last looked is gone
		// with the truncation: lines written but never read.
		if last > consumed {
			fl.truncUnread = last - consumed
		}
	}
	return nil
}

// pollPath looks at what the path names now. If it is not the last file we know
// of (the followed one, or the newest queued one) it is a rotation: the new file
// is opened through the opener and queued, so it is read even if it is rotated
// away again before the followed file is finished. step first reads whatever the
// old file still has and then calls doSwitch.
func (fl *follower) pollPath() error {
	f, err := fl.open(fl.path, openFlags, 0)
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
		if !fl.warnedFull {
			fl.warnedFull = true
			msg := fmt.Sprintf("more than %d rotated files are waiting to be read; newer ones in between would be skipped", maxQueuedFiles)
			return fl.tailUnreadable(msg)
		}
		return nil
	}
	fl.queue = append(fl.queue, queuedFile{f: f, fi: fi})
	fl.switching = switchRotated
	return nil
}

// doSwitch leaves the followed file and follows the next one from its start: the
// oldest file queued by pollPath, else the file now at the path (through the
// opener). If there is none yet it waits for one. Before entering the next file
// it accounts for the files written in between (see enter).
func (fl *follower) doSwitch() error {
	kind := fl.switching
	fl.switching = switchNone
	prev, err := fl.f.Stat() // fresh size and mtime of the file just finished
	if err != nil {
		prev = fl.id
	}
	old := fl.f
	fl.f = nil
	fl.rememberFinished(old, prev)
	if kind == switchRotated && !fl.keepLate(old, prev) {
		_ = old.Close()
	} else if kind != switchRotated {
		_ = old.Close()
	}
	if kind != switchRotated {
		fl.hopFrom = nil
		if unread := fl.truncUnread; unread > 0 {
			// The pending bytes are the head of a line that was cut by the
			// truncation: drop them, the line is read whole from the copy or
			// counted as lost.
			unread += int64(len(fl.partial))
			fl.partial = nil
			// copytruncate with an unread tail: the data is only in the copy.
			copyF, err := fl.findCopy()
			if err != nil {
				return err
			}
			if copyF != nil {
				fl.logf("file %q was truncated with %d bytes unread: reading them from its copy, then the file from its start", fl.path, unread)
				fi, err := copyF.Stat()
				if err != nil {
					copyF.Close()
					return fmt.Errorf("could not stat file: %w", err)
				}
				start := fl.offset
				fl.install(copyF, fi, start)
				if _, err := copyF.Seek(start, io.SeekStart); err != nil {
					copyF.Close()
					return fmt.Errorf("could not seek to %d in the copy: %w", start, err)
				}
				fl.switching = switchRotated
				return nil
			}
			msg := fmt.Sprintf("the file was truncated (copytruncate) while at least %d bytes past offset %d had been written but not read; they are lost", unread, fl.offset)
			if err := fl.tailUnreadable(msg); err != nil {
				return err
			}
		}
		fl.logf("file %q was truncated: reading it again from its start", fl.path)
		_, err := fl.tryOpen()
		return err
	}
	fl.logf("file %q was rotated, removed or replaced: following the next file from its start", fl.path)
	fl.hopFrom = prev
	if len(fl.queue) > 0 {
		q := fl.queue[0]
		fl.queue = fl.queue[1:]
		if q.cont {
			fl.hopFrom = nil
			fl.install(q.f, q.fi, q.start)
			if _, err := q.f.Seek(q.start, io.SeekStart); err != nil {
				return fmt.Errorf("could not seek to %d: %w", q.start, err)
			}
			if len(fl.queue) > 0 {
				fl.switching = switchRotated
			}
			return nil
		}
		return fl.enter(q.f)
	}
	_, err = fl.tryOpen()
	return err
}

// rememberFinished records f as read to its end.
func (fl *follower) rememberFinished(f *os.File, fi os.FileInfo) {
	e := finishedFile{fi: fi, size: fi.Size(), headLen: min(fi.Size(), fingerprintSize)}
	if e.headLen > 0 {
		if fp, err := fingerprint(f, e.headLen); err == nil {
			e.head = fp
		} else {
			e.headLen = -1 // unknown content: never matches by content
		}
	}
	fl.finished = append(fl.finished, e)
	if len(fl.finished) > 1024 {
		fl.finished = fl.finished[len(fl.finished)-1024:]
	}
}

// isFinished reports whether the candidate is a file already read to its end:
// same identity and still the same content (it may have grown).
func (fl *follower) isFinished(c famFile) bool {
	for _, e := range fl.finished {
		if !os.SameFile(c.info, e.fi) || e.headLen < 0 || c.info.Size() < e.size {
			continue
		}
		if e.headLen == 0 {
			return true
		}
		f, err := fl.open(c.path, openFlags, 0)
		if err != nil {
			continue
		}
		fp, err := fingerprint(f, e.headLen)
		f.Close()
		if err == nil && fp == e.head {
			return true
		}
	}
	return false
}

// keepLate holds a finished, rotated file open so lines a writer appends to it
// later are still read (see checkLate). It reports whether it kept it.
func (fl *follower) keepLate(f *os.File, fi os.FileInfo) bool {
	if len(fl.late) >= maxLateFiles {
		_ = fl.late[0].f.Close()
		fl.late = fl.late[1:]
	}
	fl.late = append(fl.late, lateFile{f: f, fi: fi, off: fl.offset, until: time.Now().Add(lateWindow)})
	return true
}

func (fl *follower) close() error {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	fl.closed = true
	for _, q := range fl.queue {
		_ = q.f.Close()
	}
	fl.queue = nil
	for _, l := range fl.late {
		_ = l.f.Close()
	}
	fl.late = nil
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
