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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/conduitio/conduit-commons/opencdc"
)

func quietFollower(t *testing.T, path string, quiet time.Duration) *follower {
	t.Helper()
	fl := newFollower(path, os.OpenFile, nil, quiet, nil)
	if err := fl.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fl.close() })
	return fl
}

// A line that is written in pieces becomes exactly one record, never the
// pieces, and the position after it is the end of the whole line.
func TestFollower_LineWrittenInPiecesIsOneRecord(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	writeFile(t, p, "")
	fl := quietFollower(t, p, time.Hour)

	go func() {
		for _, piece := range []string{"hel", "lo ", "wor", "ld", "\n"} {
			time.Sleep(40 * time.Millisecond)
			appendFile(t, p, piece)
		}
	}()
	if g := mustNext(t, fl); g != (got{"hello world", 1, 12}) {
		t.Fatalf("got %+v", g)
	}
	// and nothing else follows
	if g, err := nextWithin(t, fl, 200*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("extra record %+v, %v", g, err)
	}
}

// The saved position is never inside a line: while a line is incomplete no
// position is produced at all, and every position is at a line start.
func TestFollower_PositionsAreLineBoundaries(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	writeFile(t, p, "one\ntw")
	fl := quietFollower(t, p, time.Hour)
	if g := mustNext(t, fl); g != (got{"one", 1, 4}) {
		t.Fatalf("got %+v", g)
	}
	if g, err := nextWithin(t, fl, 150*time.Millisecond); err == nil {
		t.Fatalf("incomplete line returned: %+v", g)
	}
	appendFile(t, p, "o\nthree\n")
	if g := mustNext(t, fl); g != (got{"two", 2, 8}) {
		t.Fatalf("got %+v", g)
	}
}

// A last line without a newline is returned once it has not changed for the
// quiet period, not before, and not while it keeps growing.
func TestFollower_QuietPeriodFlushesUnterminatedLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	writeFile(t, p, "a\ntail")
	const quiet = 250 * time.Millisecond
	fl := quietFollower(t, p, quiet)
	if g := mustNext(t, fl); g.line != "a" {
		t.Fatalf("got %+v", g)
	}

	start := time.Now()
	g := mustNext(t, fl)
	if g != (got{"tail", 2, 6}) {
		t.Fatalf("got %+v", g)
	}
	if d := time.Since(start); d < quiet-50*time.Millisecond {
		t.Fatalf("flushed after %v, before the quiet period %v", d, quiet)
	}

	// Data appended later is a line of its own.
	appendFile(t, p, "more\n")
	if g := mustNext(t, fl); g != (got{"more", 3, 11}) {
		t.Fatalf("got %+v", g)
	}
}

func TestFollower_GrowingUnterminatedLineIsNotFlushed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	writeFile(t, p, "")
	const quiet = 300 * time.Millisecond
	fl := quietFollower(t, p, quiet)

	stop := make(chan struct{})
	go func() {
		defer close(stop)
		for range 12 { // 12 * 60ms: far longer than the quiet period
			appendFile(t, p, "x")
			time.Sleep(60 * time.Millisecond)
		}
	}()
	g, err := nextWithin(t, fl, 700*time.Millisecond)
	if err == nil {
		t.Fatalf("flushed while still growing: %+v", g)
	}
	<-stop
	if g := mustNext(t, fl); g.line != strings.Repeat("x", 12) {
		t.Fatalf("got %+v", g)
	}
}

// On rotation or truncation a tail that can never be completed is returned
// before the follower moves on.
func TestFollower_TailIsFlushedOnRotationAndTruncation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	writeFile(t, p, "a\nlast-without-newline")
	fl := quietFollower(t, p, time.Hour)
	mustNext(t, fl)
	// let the follower read the tail, then rotate
	if _, err := nextWithin(t, fl, 100*time.Millisecond); err == nil {
		t.Fatal("tail flushed too early")
	}
	if err := os.Rename(p, p+".1"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, p, "fresh\n")
	if g := mustNext(t, fl); g.line != "last-without-newline" {
		t.Fatalf("got %+v", g)
	}
	if g := mustNext(t, fl); g != (got{"fresh", 1, 6}) {
		t.Fatalf("got %+v", g)
	}

	// truncation
	appendFile(t, p, "tail2")
	if _, err := nextWithin(t, fl, 100*time.Millisecond); err == nil {
		t.Fatal("tail flushed too early")
	}
	if err := os.Truncate(p, 0); err != nil {
		t.Fatal(err)
	}
	if g := mustNext(t, fl); g.line != "tail2" {
		t.Fatalf("got %+v", g)
	}
}

// ---- Source level -------------------------------------------------------

func newTestSource(path string, quiet time.Duration) *Source {
	return &Source{config: SourceConfig{Config: Config{Path: path}, QuietPeriod: quiet}}
}

func readRec(ctx context.Context, t *testing.T, s *Source) opencdc.Record {
	t.Helper()
	rec, err := s.Read(ctx)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return rec
}

// A graceful stop never emits the unterminated last line: that would make the
// rest of the line a second record. The position stays at the start of the line
// and the whole line is read once after the restart (at-least-once, no mangling).
func TestSource_GracefulStopNeverEmitsAPartialLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	writeFile(t, p, "a\nunterminated")
	s := newTestSource(p, time.Hour)
	if err := s.Open(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Teardown(context.Background()) }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := readRec(ctx, t, s)
	if string(rec.Payload.After.Bytes()) != "a" {
		t.Fatalf("got %s", rec.Payload.After.Bytes())
	}
	pos := rec.Position
	go func() {
		time.Sleep(150 * time.Millisecond) // the follower has read the tail by now
		cancel()
	}()
	if r, err := s.Read(ctx); err == nil {
		t.Fatalf("the stop emitted %q", r.Payload.After.Bytes())
	}

	// restart from the last checkpoint; the writer finishes the line
	appendFile(t, p, "-done\nnext\n")
	s2 := newTestSource(p, time.Hour)
	if err := s2.Open(context.Background(), pos); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Teardown(context.Background()) }()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if rec := readRec(ctx2, t, s2); string(rec.Payload.After.Bytes()) != "unterminated-done" {
		t.Fatalf("after restart got %q, want the whole line", rec.Payload.After.Bytes())
	}
	if rec := readRec(ctx2, t, s2); string(rec.Payload.After.Bytes()) != "next" {
		t.Fatalf("after restart got %s", rec.Payload.After.Bytes())
	}
}

// Without a graceful stop the tail is not checkpointed: a restart from the last
// position replays it, whole, once its newline is there.
func TestSource_UncleanStopReplaysTail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	writeFile(t, p, "a\nhalf")
	s := newTestSource(p, time.Hour)
	if err := s.Open(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pos := readRec(ctx, t, s).Position // only "a"; the process dies here
	_ = s.Teardown(context.Background())

	appendFile(t, p, "-done\n")
	s2 := newTestSource(p, time.Hour)
	if err := s2.Open(context.Background(), pos); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Teardown(context.Background()) }()
	if rec := readRec(ctx, t, s2); string(rec.Payload.After.Bytes()) != "half-done" {
		t.Fatalf("got %s", rec.Payload.After.Bytes())
	}
}

// A v0.10.10 position is the bare decimal offset. It keeps working (after being
// verified against the file) and from the first record on positions are v1.
func TestSource_UpgradeFromV01010Position(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	writeFile(t, p, "first\nsecond\nthird\n")
	s := newTestSource(p, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// "6" is exactly what v0.10.10 saved after reading "first".
	if err := s.Open(ctx, opencdc.Position("6")); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Teardown(ctx) }()
	rec := readRec(ctx, t, s)
	if string(rec.Payload.After.Bytes()) != "second" || !strings.HasPrefix(string(rec.Position), "v1:13:") {
		t.Fatalf("got %q at %q", rec.Payload.After.Bytes(), rec.Position)
	}

	// An old position that does not match the file is refused with a code.
	bad := newTestSource(p, 0)
	err := bad.Open(ctx, opencdc.Position("8"))
	var ce *CodedError
	if !errors.As(err, &ce) || ce.Code != ErrCodePositionUnverifiable {
		t.Fatalf("err = %v", err)
	}
	// Garbage is refused with a code too.
	err = newTestSource(p, 0).Open(ctx, opencdc.Position("v1:nope"))
	if !errors.As(err, &ce) || ce.Code != ErrCodePositionInvalid {
		t.Fatalf("err = %v", err)
	}
}

// copytruncate while running: the content is copied away and the file is
// truncated in place (same inode). The follower reads the new content from its
// start.
func TestFollower_Copytruncate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	writeFile(t, p, "old1\nold2\n")
	fl := quietFollower(t, p, 0)
	readAll(t, fl, 2)

	data, _ := os.ReadFile(p)
	if err := os.WriteFile(p+".1", data, 0o600); err != nil { //nolint:gosec // test
		t.Fatal(err)
	}
	if err := os.Truncate(p, 0); err != nil {
		t.Fatal(err)
	}
	appendFile(t, p, "fresh1\nfresh2\n")
	if g := mustNext(t, fl); g != (got{"fresh1", 1, 7}) {
		t.Fatalf("got %+v", g)
	}
	if g := mustNext(t, fl); g != (got{"fresh2", 2, 14}) {
		t.Fatalf("got %+v", g)
	}
}

func TestParsePosition(t *testing.T) {
	for _, in := range []string{"abc", "-1", "v1:1", "v1:1:0:0:0:", "v1:5:0:0:9999:aa", "v1:x:0:0:0:", "v1:3:0:0:2:zz", "v1:3:0:0:0:abcd", "1.5"} {
		_, err := parsePosition("p", []byte(in))
		var ce *CodedError
		if !errors.As(err, &ce) || ce.Code != ErrCodePositionInvalid || ce.Fix == "" || ce.Path != "p" {
			t.Errorf("%q: err = %v", in, err)
		}
	}
	if p, err := parsePosition("p", nil); p != nil || err != nil {
		t.Fatalf("empty = %v, %v", p, err)
	}
	p, err := parsePosition("p", []byte("42"))
	if err != nil || !p.legacy || p.offset != 42 || p.String() != "42" {
		t.Fatalf("legacy = %+v, %v", p, err)
	}
}
