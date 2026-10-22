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
	"context"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type got struct {
	line     string
	num, pos int64
}

// nf builds a follower that resumes at a legacy offset (0: from the start) and
// returns partial lines at once.
func nf(path string, open Opener, offset int64) *follower {
	var p *position
	if offset > 0 {
		p = &position{offset: offset, legacy: true}
	}
	return newFollower(path, open, p, 0, nil)
}

func newTestFollower(t *testing.T, path string, offset int64) *follower {
	t.Helper()
	fl := nf(path, os.OpenFile, offset)
	if err := fl.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fl.close() })
	return fl
}

func nextWithin(t *testing.T, fl *follower, d time.Duration) (got, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	l, n, p, err := fl.next(ctx)
	return got{l, n, p}, err
}

func mustNext(t *testing.T, fl *follower) got {
	t.Helper()
	g, err := nextWithin(t, fl, 5*time.Second)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	return g
}

func readAll(t *testing.T, fl *follower, n int) []got {
	t.Helper()
	out := make([]got, 0, n)
	for range n {
		out = append(out, mustNext(t, fl))
	}
	return out
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func TestFollower_LinesNumbersAndPositions(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	writeFile(t, p, "one\n\ntwo\r\nthree\n")
	fl := newTestFollower(t, p, 0)

	want := []got{{"one", 1, 4}, {"", 2, 5}, {"two\r", 3, 10}, {"three", 4, 16}}
	for i, w := range want {
		if g := mustNext(t, fl); g != w {
			t.Fatalf("line %d = %+v, want %+v", i, g, w)
		}
	}

	// Waits for data appended later.
	go func() {
		time.Sleep(30 * time.Millisecond)
		appendFile(t, p, "four\n")
	}()
	if g := mustNext(t, fl); g != (got{"four", 5, 21}) {
		t.Fatalf("appended line = %+v", g)
	}
}

func TestFollower_PartialLastLine_QuietZeroReturnsAtOnce(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	writeFile(t, p, "ab\ncd")
	fl := newTestFollower(t, p, 0)

	// quietPeriod 0 keeps the v0.10.x behavior: the partial line is returned at
	// once, the rest of it later as a line of its own.
	if g := mustNext(t, fl); g != (got{"ab", 1, 3}) {
		t.Fatalf("got %+v", g)
	}
	if g := mustNext(t, fl); g != (got{"cd", 2, 5}) {
		t.Fatalf("partial: got %+v", g)
	}
	appendFile(t, p, "ef\ngh\n")
	if g := mustNext(t, fl); g != (got{"ef", 3, 8}) {
		t.Fatalf("remainder: got %+v", g)
	}
	if g := mustNext(t, fl); g != (got{"gh", 4, 11}) {
		t.Fatalf("got %+v", g)
	}
}

func TestFollower_Truncation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	writeFile(t, p, "aaaa\nbbbb\n")
	fl := newTestFollower(t, p, 0)
	readAll(t, fl, 2)

	if err := os.Truncate(p, 0); err != nil {
		t.Fatal(err)
	}
	appendFile(t, p, "x\ny\n")
	// Re-read from the start; numbering and positions restart.
	if g := mustNext(t, fl); g != (got{"x", 1, 2}) {
		t.Fatalf("after truncation: %+v", g)
	}
	if g := mustNext(t, fl); g != (got{"y", 2, 4}) {
		t.Fatalf("after truncation: %+v", g)
	}
}

func TestFollower_RotationDrainsOldFileThenFollowsNewOne(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	writeFile(t, p, "a\n")
	fl := newTestFollower(t, p, 0)
	mustNext(t, fl)

	// Data written to the old file just before it is rotated is not lost, and
	// the new file is read from its first byte.
	appendFile(t, p, "b\n")
	if err := os.Rename(p, p+".1"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, p, "new1\nnew2\n")

	if g := mustNext(t, fl); g != (got{"b", 2, 4}) {
		t.Fatalf("old file's last line = %+v", g)
	}
	if g := mustNext(t, fl); g != (got{"new1", 1, 5}) {
		t.Fatalf("new file's first line = %+v", g)
	}
	if g := mustNext(t, fl); g != (got{"new2", 2, 10}) {
		t.Fatalf("got %+v", g)
	}
}

func TestFollower_RemovedFileThenRecreated(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	writeFile(t, p, "a\n")
	fl := newTestFollower(t, p, 0)
	mustNext(t, fl)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	// Nothing to read while the path is gone.
	if _, err := nextWithin(t, fl, 300*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a wait", err)
	}
	writeFile(t, p, "again\n")
	if g := mustNext(t, fl); g != (got{"again", 1, 6}) {
		t.Fatalf("got %+v", g)
	}
}

func TestFollower_WaitsForMissingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "later.txt")
	fl := newTestFollower(t, p, 0)
	go func() {
		time.Sleep(60 * time.Millisecond)
		writeFile(t, p, "hello\n")
	}()
	if g := mustNext(t, fl); g != (got{"hello", 1, 6}) {
		t.Fatalf("got %+v", g)
	}
}

func TestFollower_RefusesNonRegularFile(t *testing.T) {
	fl := nf(t.TempDir(), os.OpenFile, 0) // a directory
	if err := fl.start(); !errors.Is(err, errNotRegular) {
		t.Fatalf("start = %v, want errNotRegular", err)
	}
}

func TestFollower_CtxCancelAndClose(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	writeFile(t, p, "")
	fl := newTestFollower(t, p, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, _, err := fl.next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}

	// close while a next is waiting (run with -race).
	var wg sync.WaitGroup
	wg.Add(1)
	var nerr error
	go func() {
		defer wg.Done()
		_, _, _, nerr = fl.next(context.Background())
	}()
	time.Sleep(20 * time.Millisecond)
	if err := fl.close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if !errors.Is(nerr, errFollowerClosed) {
		t.Fatalf("next after close = %v", nerr)
	}
	if err := fl.close(); err != nil {
		t.Fatalf("second close = %v", err)
	}
}

// Restarting at the position of any line yields exactly the lines after it,
// with the same numbers-since-open restarting at 1 and the same positions: no
// gap and no duplicate.
func TestFollower_RestartMidFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	var sb strings.Builder
	for i := range 50 {
		sb.WriteString(strings.Repeat("x", i%7))
		sb.WriteString("\n")
	}
	writeFile(t, p, sb.String())

	all := readAll(t, newTestFollower(t, p, 0), 50)
	for k := range all {
		fl := newTestFollower(t, p, all[k].pos)
		rest := readAll(t, fl, len(all)-k-1)
		for i, g := range rest {
			w := all[k+1+i]
			if g.line != w.line || g.pos != w.pos || g.num != int64(i+1) {
				t.Fatalf("restart after line %d: got %+v, want line=%q pos=%d num=%d", k+1, g, w.line, w.pos, i+1)
			}
		}
		_ = fl.close()
	}
}

// A legacy offset that is past the end of the file, or not at the start of a
// line, cannot be verified and is refused instead of guessed at.
func TestFollower_LegacyOffsetRefusedWhenUnverifiable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	writeFile(t, p, "new-1\nnew-2\n")
	for name, off := range map[string]int64{"mid-line": 10, "past end": 1000} {
		fl := nf(p, os.OpenFile, off)
		err := fl.start()
		var ce *CodedError
		if !errors.As(err, &ce) || ce.Code != ErrCodePositionUnverifiable {
			t.Fatalf("%s: err = %v, want %s", name, err, ErrCodePositionUnverifiable)
		}
		if ce.Path != p || ce.Fix == "" {
			t.Fatalf("%s: error lacks path or fix: %+v", name, ce)
		}
	}
	// A line boundary is verified and accepted.
	fl := newTestFollower(t, p, 6)
	if g := mustNext(t, fl); g != (got{"new-2", 1, 12}) {
		t.Fatalf("got %+v", g)
	}
}

func randContent(r *rand.Rand) string {
	var sb strings.Builder
	n := r.Intn(40)
	alphabet := []string{"a", "bc", "\r", "é世", "\x00", " ", "xyz"}
	for range n {
		for range r.Intn(6) {
			sb.WriteString(alphabet[r.Intn(len(alphabet))])
		}
		sb.WriteByte('\n')
	}
	if r.Intn(2) == 0 { // partial last line
		for range 1 + r.Intn(4) {
			sb.WriteString(alphabet[r.Intn(len(alphabet))])
		}
	}
	return sb.String()
}

// checkPositionRoundTrip asserts the position contract for one file content:
// reading everything reconstructs the file byte for byte, offsets strictly
// increase and end at the file size, every saved position survives
// parsePosition/String unchanged, and resuming from any of them (v1, and the
// legacy offset where it follows a newline) yields exactly the lines that
// follow, with identical positions.
//
//nolint:gocyclo // one linear scenario, split would hide the contract
func checkPositionRoundTrip(t *testing.T, content string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f.txt")
	writeFile(t, p, content)

	want := 0
	for _, l := range strings.SplitAfter(content, "\n") {
		if l != "" {
			want++
		}
	}
	fl := newTestFollower(t, p, 0)
	var all []got
	var saved []string
	for range want {
		all = append(all, mustNext(t, fl))
		saved = append(saved, fl.position())
	}
	_ = fl.close()

	var prev int64
	for i, g := range all {
		if g.pos <= prev || g.num != int64(i+1) {
			t.Fatalf("line %d: %+v after offset %d", i, g, prev)
		}
		if seg := content[prev:g.pos]; seg != g.line && seg != g.line+"\n" {
			t.Fatalf("line %d %q does not match bytes %q", i, g.line, seg)
		}
		prev = g.pos
	}
	if prev != int64(len(content)) {
		t.Fatalf("offsets end at %d, file has %d bytes", prev, len(content))
	}

	for k := range all {
		pp, err := parsePosition(p, []byte(saved[k]))
		if err != nil || pp.legacy || pp.offset != all[k].pos || pp.String() != saved[k] {
			t.Fatalf("position %q does not round-trip: %+v, %v", saved[k], pp, err)
		}
		resumes := []*position{pp}
		if content[all[k].pos-1] == '\n' {
			resumes = append(resumes, &position{offset: all[k].pos, legacy: true})
		}
		for _, rp := range resumes {
			fl := newFollower(p, os.OpenFile, rp, 0, nil)
			if err := fl.start(); err != nil {
				t.Fatalf("resume %q: %v", rp, err)
			}
			for i := k + 1; i < len(all); i++ {
				g := mustNext(t, fl)
				if g.line != all[i].line || g.pos != all[i].pos || fl.position() != saved[i] {
					t.Fatalf("resume %q: got %+v (%s), want %+v (%s)", rp, g, fl.position(), all[i], saved[i])
				}
			}
			_ = fl.close()
		}
	}
}

func TestFollower_PositionRoundTripProperty(t *testing.T) {
	r := rand.New(rand.NewSource(1)) //nolint:gosec // deterministic test data
	for range 150 {
		checkPositionRoundTrip(t, randContent(r))
	}
}

func FuzzFollowerPositionRoundTrip(f *testing.F) {
	f.Add("a\nb\n")
	f.Add("a\r\nb")
	f.Add("\n\n\n")
	f.Add("x")
	f.Fuzz(func(t *testing.T, content string) {
		if len(content) > 4096 {
			t.Skip()
		}
		checkPositionRoundTrip(t, content)
	})
}
