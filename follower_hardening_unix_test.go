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

//go:build unix

package file

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/conduitio/conduit-commons/config"
	"github.com/conduitio/conduit-commons/opencdc"
	sdk "github.com/conduitio/conduit-connector-sdk"
)

// By default an unterminated last line is held, however long it takes: a record
// is never a piece of a line.
func TestSource_DefaultHoldsUnterminatedLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	writeFile(t, p, "a\nhalf")
	src := NewSource()
	ctx := context.Background()
	if err := sdk.Util.ParseConfig(ctx, config.Config{"path": p}, src.Config(), Connector.NewSpecification().SourceParams); err != nil {
		t.Fatal(err)
	}
	if err := src.Open(ctx, nil); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Teardown(ctx) }()

	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	recs, err := readOne(rctx, src)
	if err != nil || string(recs[0].Payload.After.Bytes()) != "a" {
		t.Fatalf("got %v, %v", recs, err)
	}
	// 2 s of silence while a Read waits: nothing is returned (the old 1 s
	// default would have returned "half"). The context is never cancelled, as
	// a cancelled one means a graceful stop.
	type result struct {
		recs []opencdc.Record
		err  error
	}
	res := make(chan result, 1)
	go func() {
		r, err := readOne(ctx, src)
		res <- result{r, err}
	}()
	select {
	case r := <-res:
		t.Fatalf("the unterminated line was returned: %v, %v", r.recs, r.err)
	case <-time.After(2 * time.Second):
	}
	// completed in two more pieces: exactly one record, whole
	appendFile(t, p, "-more")
	appendFile(t, p, "-done\n")
	select {
	case r := <-res:
		if r.err != nil || string(r.recs[0].Payload.After.Bytes()) != "half-more-done" {
			t.Fatalf("got %v, %v", r.recs, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the completed line was not returned")
	}
}

func TestFollower_MaxLineSize(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	long := strings.Repeat("x", 300*1024) // longer than the read buffer
	writeFile(t, p, "ok\n"+long+"\nafter\n")

	fl := nf(p, os.OpenFile, 0).withMaxLine(100 * 1024)
	if err := fl.start(); err != nil {
		t.Fatal(err)
	}
	defer fl.close()
	if g := mustNext(t, fl); g.line != "ok" {
		t.Fatalf("got %+v", g)
	}
	_, err := nextWithin(t, fl, 5*time.Second)
	ce := wantCode(t, err, ErrCodeLineTooLong)
	if !strings.Contains(ce.Fix, "maxLineSize") {
		t.Fatalf("fix = %q", ce.Fix)
	}
	if fl.position() == "" || !strings.HasPrefix(fl.position(), "v1:3:") {
		t.Fatalf("position moved past the long line: %q", fl.position())
	}

	// a line that fits is never cut
	fl2 := nf(p, os.OpenFile, 0).withMaxLine(400 * 1024)
	if err := fl2.start(); err != nil {
		t.Fatal(err)
	}
	defer fl2.close()
	eq(t, lines2(t, fl2, 3), []string{"ok", long, "after"})

	// an unterminated tail that outgrows the limit is an error too
	p2 := filepath.Join(t.TempDir(), "g.txt")
	writeFile(t, p2, "")
	fl3 := newFollower(p2, os.OpenFile, nil, 0, nil).withMaxLine(1000)
	if err := fl3.start(); err != nil {
		t.Fatal(err)
	}
	defer fl3.close()
	appendFile(t, p2, strings.Repeat("y", 600))
	if _, err := nextWithin(t, fl3, 200*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	appendFile(t, p2, strings.Repeat("y", 600))
	_, err = nextWithin(t, fl3, 5*time.Second)
	_ = wantCode(t, err, ErrCodeLineTooLong)
}

func lines2(t *testing.T, fl *follower, n int) []string { return lines(t, fl, n) }

func TestSourceConfig_MaxLineSizeMustBePositive(t *testing.T) {
	src := NewSource()
	err := sdk.Util.ParseConfig(context.Background(), config.Config{"path": "/x", "maxLineSize": "0"}, src.Config(), Connector.NewSpecification().SourceParams)
	if err == nil {
		t.Fatal("maxLineSize 0 accepted")
	}
}

// A FIFO at the path is refused at once: opening it for reading would block
// until a writer shows up, so it is opened without blocking and refused.
func TestFollower_FIFOIsRefusedWithoutBlocking(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	done := make(chan error, 1)
	go func() { done <- nf(p, os.OpenFile, 0).start() }()
	select {
	case err := <-done:
		if !errors.Is(err, errNotRegular) {
			t.Fatalf("err = %v, want errNotRegular", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("opening a FIFO blocked")
	}
}

// ---- copytruncate with an unread tail ---------------------------------------

func bigFile(t *testing.T, p string, n int) string {
	t.Helper()
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&sb, "line-%d\n", i)
	}
	writeFile(t, p, sb.String())
	return sb.String()
}

func copytruncate(t *testing.T, p, copyTo string) {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, copyTo, string(data))
	if err := os.Truncate(p, 0); err != nil {
		t.Fatal(err)
	}
}

// 30000 lines; the follower has read a few thousand (its buffer holds 64 KiB),
// then logrotate copies and truncates the file. The rest of the old content is
// only in the copy.
func TestFollower_CopytruncateWithUnreadTail(t *testing.T) {
	const total = 30000
	setup := func(t *testing.T, o rotationOptions) (*rotEnv, *follower) {
		e := newRotEnv(t)
		bigFile(t, e.path, total)
		fl := e.must(nil, o)
		for i := 1; i <= 10; i++ {
			if g := mustNext(t, fl); g.line != fmt.Sprintf("line-%d", i) {
				t.Fatalf("got %+v", g)
			}
		}
		copytruncate(t, e.path, e.path+".1")
		appendFile(t, e.path, "fresh-1\nfresh-2\n")
		return e, fl
	}
	drain := func(fl *follower) ([]string, error) {
		var got []string
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			l, _, _, err := fl.next(ctx)
			cancel()
			if err != nil {
				return got, err
			}
			got = append(got, l)
			if l == "fresh-2" {
				return got, nil
			}
		}
	}

	t.Run("fails loudly", func(t *testing.T) {
		_, fl := setup(t, rotationOptions{})
		_, err := drain(fl)
		ce := wantCode(t, err, ErrCodeRotationTailUnreadable)
		if !strings.Contains(ce.Msg, "copytruncate") || !strings.Contains(ce.Fix, "rotatedPattern") {
			t.Fatalf("error = %v", ce)
		}
	})
	t.Run("acceptLoss warns with the size and continues", func(t *testing.T) {
		e, fl := setup(t, rotationOptions{acceptLoss: true})
		got, err := drain(fl)
		if err != nil || got[len(got)-1] != "fresh-2" || len(got) > total {
			t.Fatalf("got %d lines, %v", len(got), err)
		}
		if len(e.warned) != 1 || !strings.Contains(e.warned[0], ErrCodeRotationTailUnreadable) || !strings.Contains(e.warned[0], "bytes") {
			t.Fatalf("warnings = %v", e.warned)
		}
	})
	t.Run("pattern recovers the remainder from the copy", func(t *testing.T) {
		e, fl := setup(t, rotationOptions{pattern: "f.txt.*"})
		got, err := drain(fl)
		if err != nil {
			t.Fatal(err)
		}
		// lines 11..total from the buffer and the copy, no gap, then the new file
		want := total - 10 + 2
		if len(got) != want {
			t.Fatalf("got %d lines, want %d", len(got), want)
		}
		for i := 0; i < total-10; i++ {
			if got[i] != fmt.Sprintf("line-%d", i+11) {
				t.Fatalf("line %d = %q", i, got[i])
			}
		}
		if got[want-2] != "fresh-1" || len(e.warned) != 0 {
			t.Fatalf("tail %v, warnings %v", got[want-2:], e.warned)
		}
	})
}

// Everything read before the truncation: nothing is lost, nothing to report.
func TestFollower_CopytruncateWithoutUnreadTailIsQuiet(t *testing.T) {
	e := newRotEnv(t)
	bigFile(t, e.path, 50)
	fl := e.must(nil, rotationOptions{})
	lines(t, fl, 50)
	copytruncate(t, e.path, e.path+".1")
	appendFile(t, e.path, "fresh\n")
	eq(t, lines(t, fl, 1), []string{"fresh"})
	if len(e.warned) != 0 {
		t.Fatalf("warnings = %v", e.warned)
	}
}

// ---- identity ------------------------------------------------------------------

// The device number is not identity: the inode, size and first bytes decide.
func TestFollower_DeviceNumberChangeStillResumes(t *testing.T) {
	e := newRotEnv(t)
	writeFile(t, e.path, "a\nb\n")
	fl := e.must(nil, rotationOptions{})
	lines(t, fl, 2)
	pos := savedPosition(t, e.path, fl)
	_ = fl.close()
	appendFile(t, e.path, "c\n")

	pos.dev += 7 // e.g. after a reboot or a remount
	var logged []string
	fl2 := newFollower(e.path, os.OpenFile, pos, 0, func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) })
	if err := fl2.start(); err != nil {
		t.Fatal(err)
	}
	defer fl2.close()
	if g := mustNext(t, fl2); g.line != "c" {
		t.Fatalf("got %+v", g)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "device number") {
		t.Fatalf("log = %v", logged)
	}

	// a different inode with the same content is still a different file
	pos.dev -= 7
	pos.ino++
	fl3 := newFollower(e.path, os.OpenFile, pos, 0, nil)
	_ = wantCode(t, fl3.start(), ErrCodeRotationTailUnreadable)
}

// A v0.10.x position cannot be confirmed to belong to the file: say so, with the
// numbers an operator needs.
func TestFollower_LegacyPositionResumeWarns(t *testing.T) {
	e := newRotEnv(t)
	writeFile(t, e.path, "first\nsecond\n")
	fl := e.must(&position{offset: 6, legacy: true}, rotationOptions{})
	defer fl.close()
	if g := mustNext(t, fl); g.line != "second" {
		t.Fatalf("got %+v", g)
	}
	if len(e.warned) != 1 {
		t.Fatalf("warnings = %v", e.warned)
	}
	w := e.warned[0]
	for _, want := range []string{ErrCodePositionUnverifiable, "offset 6", "13 bytes", "inode", "modified", "skipped"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning lacks %q: %s", want, w)
		}
	}
}

// ---- rotatedPattern ordering ----------------------------------------------------

func TestNameBefore(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"app.log.3", "app.log.2", true},  // logrotate index: larger is older
		{"app.log.2", "app.log.3", false}, //
		{"app.log.10", "app.log.9", true}, // numeric, not lexical
		{"app.log-20261020", "app.log-20261021", true},
		{"app.log-20261021", "app.log-20261020", false},
		{"app.log.a", "app.log.b", true},
		{"x.1", "y.1", true},
	}
	for _, c := range cases {
		if got := nameBefore(c.a, c.b); got != c.want {
			t.Errorf("nameBefore(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// Files written in the same tick (equal modification times) are still found and
// are ordered by the numeric suffix of their names.
func TestFollower_PatternChainWithEqualModificationTimes(t *testing.T) {
	e := newRotEnv(t)
	writeFile(t, e.path, "a1\n")
	touch(t, e.path, 0)
	fl := e.must(nil, rotationOptions{pattern: "f.txt.*"})
	lines(t, fl, 1)

	// three rotations; logrotate numbering: .3 is the oldest, .1 the newest
	e.rename(e.path, e.path+".3")
	writeFile(t, e.path, "b1\n")
	touch(t, e.path, 0)
	e.rename(e.path, e.path+".2")
	writeFile(t, e.path, "c1\n")
	touch(t, e.path, 0)
	e.rename(e.path, e.path+".1")
	writeFile(t, e.path, "d1\n")
	touch(t, e.path, 0)
	// a.1 itself was f.txt.3's content: A is f.txt.3 -> known; b1 and c1 are
	// intermediates with the same mtime as A; the compressed twin is ignored
	e.gzipTo(e.path+".3.gz", "a1\n")
	touch(t, e.path+".3.gz", 0)
	eq(t, lines(t, fl, 3), []string{"b1", "c1", "d1"})
}

// The engine can list the directory itself; its lister replaces os.ReadDir and
// its refusal is a coded error.
func TestFollower_DirListerIsUsedAndPoliced(t *testing.T) {
	e := newRotEnv(t)
	writeFile(t, e.path, "a1\n")
	touch(t, e.path, 0)
	var listed []string
	lister := func(dir string) ([]fs.DirEntry, error) {
		listed = append(listed, dir)
		return os.ReadDir(dir)
	}
	fl := e.must(nil, rotationOptions{listDir: lister})
	lines(t, fl, 1)
	e.rename(e.path, e.path+".1")
	writeFile(t, e.path, "c1\n")
	touch(t, e.path, 20*time.Second)
	eq(t, lines(t, fl, 1), []string{"c1"})
	if len(listed) == 0 || listed[0] != e.dir {
		t.Fatalf("lister calls = %v", listed)
	}

	e2 := newRotEnv(t)
	writeFile(t, e2.path, "a1\n")
	touch(t, e2.path, 0)
	deny := func(string) ([]fs.DirEntry, error) { return nil, errPolicy }
	fl2 := e2.must(nil, rotationOptions{listDir: deny})
	lines(t, fl2, 1)
	e2.rename(e2.path, e2.path+".1")
	writeFile(t, e2.path, "c1\n")
	_, err := nextWithin(t, fl2, 5*time.Second)
	ce := wantCode(t, err, ErrCodeRotatedPatternDenied)
	if !errors.Is(err, errPolicy) {
		t.Fatalf("does not wrap the engine's error: %v", ce)
	}
}

var _ = opencdc.Position(nil)
