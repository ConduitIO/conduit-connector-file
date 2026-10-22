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
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/conduitio/conduit-commons/config"
	"github.com/conduitio/conduit-commons/opencdc"
	sdk "github.com/conduitio/conduit-connector-sdk"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func touch(t *testing.T, path string, d time.Duration) {
	t.Helper()
	if err := os.Chtimes(path, t0.Add(d), t0.Add(d)); err != nil {
		t.Fatal(err)
	}
}

type rotEnv struct {
	t      *testing.T
	dir    string
	path   string
	warned []string
}

func newRotEnv(t *testing.T) *rotEnv {
	dir := t.TempDir()
	return &rotEnv{t: t, dir: dir, path: filepath.Join(dir, "f.txt")}
}

// follower returns a started follower resuming at pos (nil: from the start).
func (e *rotEnv) follower(pos *position, o rotationOptions) (*follower, error) {
	o.warnf = func(f string, a ...any) { e.warned = append(e.warned, fmt.Sprintf(f, a...)) }
	fl := newFollower(e.path, os.OpenFile, pos, 0, nil).withRotation(o)
	if err := fl.start(); err != nil {
		return nil, err
	}
	e.t.Cleanup(func() { _ = fl.close() })
	return fl, nil
}

func (e *rotEnv) must(pos *position, o rotationOptions) *follower {
	e.t.Helper()
	fl, err := e.follower(pos, o)
	if err != nil {
		e.t.Fatal(err)
	}
	return fl
}

func (e *rotEnv) rename(from, to string) {
	e.t.Helper()
	if err := os.Rename(from, to); err != nil {
		e.t.Fatal(err)
	}
}

func (e *rotEnv) gzipTo(dst string, content string) {
	e.t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	_, _ = w.Write([]byte(content))
	_ = w.Close()
	writeFile(e.t, dst, b.String())
}

func wantCode(t *testing.T, err error, code string) *CodedError {
	t.Helper()
	var ce *CodedError
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("err = %v, want code %s", err, code)
	}
	if ce.Path == "" || ce.Fix == "" {
		t.Fatalf("coded error lacks path or fix: %+v", ce)
	}
	return ce
}

func lines(t *testing.T, fl *follower, n int) []string {
	t.Helper()
	var out []string
	for range n {
		out = append(out, mustNext(t, fl).line)
	}
	return out
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// ---- restart after a rotation the source cannot account for ----------------

// A position taken in f.txt after "a\nb\n" was read, then the connector stops,
// "c\n" is appended (the unread tail), and f.txt is rotated away by how.
func (e *rotEnv) stoppedWithTail(how func(e *rotEnv)) *position {
	e.t.Helper()
	writeFile(e.t, e.path, "a\nb\n")
	fl := e.must(nil, rotationOptions{})
	lines(e.t, fl, 2)
	pos := savedPosition(e.t, e.path, fl)
	_ = fl.close()
	appendFile(e.t, e.path, "c\n")
	touch(e.t, e.path, time.Hour)
	how(e)
	writeFile(e.t, e.path, "n1\nn2\n")
	touch(e.t, e.path, 2*time.Hour)
	e.warned = nil
	return pos
}

func TestFollower_RestartAfterRotation_UnreadableTail(t *testing.T) {
	other := t.TempDir()
	cases := []struct {
		name    string
		how     func(e *rotEnv)
		pattern string // a pattern that matches wherever the file went
		byInode bool   // found next to the path without any pattern
		found   bool   // found with the pattern
	}{
		{"deleted", func(e *rotEnv) { _ = os.Remove(e.path) }, "f.txt.*", false, false},
		{"compressed", func(e *rotEnv) {
			e.gzipTo(e.path+".1.gz", "a\nb\nc\n")
			_ = os.Remove(e.path)
		}, "f.txt.*", false, false},
		{"moved elsewhere", func(e *rotEnv) { e.rename(e.path, filepath.Join(other, "f.txt.1")) }, filepath.Join(other, "f.txt.*"), false, true},
		{"renamed next to it", func(e *rotEnv) { e.rename(e.path, e.path+".1") }, "f.txt.*", true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// no pattern
			e := newRotEnv(t)
			fl, err := e.follower(e.stoppedWithTail(c.how), rotationOptions{})
			if c.byInode {
				if err != nil {
					t.Fatal(err)
				}
				eq(t, lines(t, fl, 3), []string{"c", "n1", "n2"})
			} else {
				_ = wantCode(t, err, ErrCodeRotationTailUnreadable)
			}

			// with the pattern
			e = newRotEnv(t)
			fl, err = e.follower(e.stoppedWithTail(c.how), rotationOptions{pattern: c.pattern})
			if c.found {
				if err != nil {
					t.Fatal(err)
				}
				eq(t, lines(t, fl, 3), []string{"c", "n1", "n2"})
			} else {
				_ = wantCode(t, err, ErrCodeRotationTailUnreadable)
			}

			// acceptLoss without a pattern: continue at the head of the new file
			if c.byInode {
				return
			}
			e = newRotEnv(t)
			fl, err = e.follower(e.stoppedWithTail(c.how), rotationOptions{acceptLoss: true})
			if err != nil {
				t.Fatal(err)
			}
			eq(t, lines(t, fl, 2), []string{"n1", "n2"})
			if len(e.warned) != 1 || !strings.Contains(e.warned[0], ErrCodeRotationTailUnreadable) {
				t.Fatalf("warnings = %v", e.warned)
			}
		})
	}
}

func TestFollower_RestartAfterRotation_OldFileGone_AcceptLoss(t *testing.T) {
	e := newRotEnv(t)
	writeFile(t, e.path, "a\nb\n")
	fl := e.must(nil, rotationOptions{})
	lines(t, fl, 2)
	pos := savedPosition(t, e.path, fl)
	_ = fl.close()
	if err := os.Remove(e.path); err != nil {
		t.Fatal(err)
	}
	writeFile(t, e.path, "new-head-line\nsecond\n")

	_, err := e.follower(pos, rotationOptions{})
	ce := wantCode(t, err, ErrCodeRotationTailUnreadable)
	if ce.Path != e.path {
		t.Fatalf("path = %q", ce.Path)
	}

	fl = e.must(pos, rotationOptions{acceptLoss: true})
	if g := mustNext(t, fl); g != (got{"new-head-line", 1, 14}) {
		t.Fatalf("got %+v", g)
	}
	if len(e.warned) != 1 || !strings.Contains(e.warned[0], "FILE_ROTATION_TAIL_UNREADABLE") || !strings.Contains(e.warned[0], "acceptLoss") {
		t.Fatalf("warnings = %v", e.warned)
	}
}

// ---- two rotations before the source catches up --------------------------

// a1 is read; then f.txt is rotated twice before the source looks again:
// a1 stays in f.txt.1 (nothing unread), b1 in f.txt.2, c1 is the current file.
func (e *rotEnv) doubleRotationWhileRunning(o rotationOptions) *follower {
	e.t.Helper()
	writeFile(e.t, e.path, "a1\n")
	touch(e.t, e.path, 0)
	fl := e.must(nil, o)
	lines(e.t, fl, 1)
	e.rename(e.path, e.path+".1")
	writeFile(e.t, e.path, "b1\n")
	touch(e.t, e.path, 10*time.Second)
	e.rename(e.path, e.path+".2")
	writeFile(e.t, e.path, "c1\n")
	touch(e.t, e.path, 20*time.Second)
	return fl
}

func TestFollower_DoubleRotationWhileRunning_FailsLoudlyWithoutPattern(t *testing.T) {
	e := newRotEnv(t)
	fl := e.doubleRotationWhileRunning(rotationOptions{})
	_, err := nextWithin(t, fl, 5*time.Second)
	ce := wantCode(t, err, ErrCodeRotationTailUnreadable)
	if !strings.Contains(ce.Msg, "f.txt.2") || !strings.Contains(ce.Fix, "rotatedPattern") {
		t.Fatalf("error does not name the file or the fix: %v", ce)
	}
}

func TestFollower_DoubleRotationWhileRunning_AcceptLoss(t *testing.T) {
	e := newRotEnv(t)
	fl := e.doubleRotationWhileRunning(rotationOptions{acceptLoss: true})
	eq(t, lines(t, fl, 1), []string{"c1"})
	if len(e.warned) != 1 || !strings.Contains(e.warned[0], ErrCodeRotationTailUnreadable) || !strings.Contains(e.warned[0], "3 bytes") {
		t.Fatalf("warnings = %v", e.warned)
	}
}

func TestFollower_DoubleRotationWhileRunning_PatternDrainsTheChain(t *testing.T) {
	e := newRotEnv(t)
	fl := e.doubleRotationWhileRunning(rotationOptions{pattern: "f.txt.*"})
	eq(t, lines(t, fl, 2), []string{"b1", "c1"})
	if len(e.warned) != 0 {
		t.Fatalf("warnings = %v", e.warned)
	}
}

// ---- two rotations while stopped -----------------------------------------

func (e *rotEnv) doubleRotationWhileStopped(o rotationOptions) (*follower, error) {
	e.t.Helper()
	writeFile(e.t, e.path, "a1\n")
	touch(e.t, e.path, 0)
	fl := e.must(nil, rotationOptions{})
	lines(e.t, fl, 1)
	pos := savedPosition(e.t, e.path, fl)
	_ = fl.close()

	appendFile(e.t, e.path, "a2\n") // unread tail of the first file
	touch(e.t, e.path, 5*time.Second)
	e.rename(e.path, e.path+".1")
	writeFile(e.t, e.path, "b1\n")
	touch(e.t, e.path, 10*time.Second)
	e.rename(e.path, e.path+".2")
	writeFile(e.t, e.path, "c1\n")
	touch(e.t, e.path, 20*time.Second)
	e.warned = nil
	return e.follower(pos, o)
}

func TestFollower_DoubleRotationWhileStopped(t *testing.T) {
	t.Run("without pattern: the unread tail is read, then it stops", func(t *testing.T) {
		e := newRotEnv(t)
		fl, err := e.doubleRotationWhileStopped(rotationOptions{})
		if err != nil {
			t.Fatal(err)
		}
		eq(t, lines(t, fl, 1), []string{"a2"})
		_, err = nextWithin(t, fl, 5*time.Second)
		_ = wantCode(t, err, ErrCodeRotationTailUnreadable)
	})
	t.Run("acceptLoss", func(t *testing.T) {
		e := newRotEnv(t)
		fl, err := e.doubleRotationWhileStopped(rotationOptions{acceptLoss: true})
		if err != nil {
			t.Fatal(err)
		}
		eq(t, lines(t, fl, 2), []string{"a2", "c1"})
		if len(e.warned) != 1 || !strings.Contains(e.warned[0], "FILE_ROTATION_TAIL_UNREADABLE") {
			t.Fatalf("warnings = %v", e.warned)
		}
	})
	t.Run("pattern drains the chain in order", func(t *testing.T) {
		e := newRotEnv(t)
		fl, err := e.doubleRotationWhileStopped(rotationOptions{pattern: "f.txt.*"})
		if err != nil {
			t.Fatal(err)
		}
		eq(t, lines(t, fl, 3), []string{"a2", "b1", "c1"})
	})
}

// A compressed file in the chain cannot be read, with a pattern too.
func TestFollower_CompressedIntermediateStopsOrIsSkipped(t *testing.T) {
	setup := func(e *rotEnv) *position {
		writeFile(t, e.path, "a1\n")
		touch(t, e.path, 0)
		fl := e.must(nil, rotationOptions{})
		lines(t, fl, 1)
		pos := savedPosition(t, e.path, fl)
		_ = fl.close()
		e.rename(e.path, e.path+".1")
		e.gzipTo(e.path+".2.gz", "b1\n")
		touch(t, e.path+".2.gz", 10*time.Second)
		writeFile(t, e.path, "c1\n")
		touch(t, e.path, 20*time.Second)
		e.warned = nil
		return pos
	}
	e := newRotEnv(t)
	fl := e.must(setup(e), rotationOptions{pattern: "f.txt.*"})
	_, err := nextWithin(t, fl, 5*time.Second)
	ce := wantCode(t, err, ErrCodeRotationTailUnreadable)
	if !strings.Contains(ce.Msg, "compressed") {
		t.Fatalf("msg = %q", ce.Msg)
	}

	e = newRotEnv(t)
	fl = e.must(setup(e), rotationOptions{pattern: "f.txt.*", acceptLoss: true})
	eq(t, lines(t, fl, 1), []string{"c1"})
	if len(e.warned) != 1 || !strings.Contains(e.warned[0], "compressed") {
		t.Fatalf("warnings = %v", e.warned)
	}
}

// The compressed twin of the file just finished (same mtime, new inode) is not
// an unknown intermediate: logrotate's compress does exactly this.
func TestFollower_CompressedTwinOfFinishedFileIsNotAnIntermediate(t *testing.T) {
	e := newRotEnv(t)
	writeFile(t, e.path, "a1\n")
	touch(t, e.path, 0)
	fl := e.must(nil, rotationOptions{})
	lines(t, fl, 1)
	e.rename(e.path, e.path+".1")
	e.gzipTo(e.path+".1.gz", "a1\n")
	touch(t, e.path+".1.gz", 0)
	writeFile(t, e.path, "c1\n")
	touch(t, e.path, 20*time.Second)
	eq(t, lines(t, fl, 1), []string{"c1"})
}

// Unrelated files in the directory are not mistaken for rotated files.
func TestFollower_UnrelatedFilesAreIgnored(t *testing.T) {
	e := newRotEnv(t)
	writeFile(t, e.path, "a1\n")
	touch(t, e.path, 0)
	fl := e.must(nil, rotationOptions{})
	lines(t, fl, 1)
	writeFile(t, filepath.Join(e.dir, "f.txtual"), "x\n") // next char is a letter
	writeFile(t, filepath.Join(e.dir, "other.log"), "x\n")
	touch(t, filepath.Join(e.dir, "f.txtual"), 10*time.Second)
	touch(t, filepath.Join(e.dir, "other.log"), 10*time.Second)
	e.rename(e.path, e.path+".1")
	writeFile(t, e.path, "c1\n")
	touch(t, e.path, 20*time.Second)
	eq(t, lines(t, fl, 1), []string{"c1"})
}

func TestSourceConfig_RotationOptions(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "f.txt")

	// the parameters exist and parse
	src := NewSource()
	cfg := config.Config{"path": p, "rotatedPattern": "f.txt.*", "rotation.acceptLoss": "true", "quietPeriod": "250ms"}
	if err := sdk.Util.ParseConfig(ctx, cfg, src.Config(), Connector.NewSpecification().SourceParams); err != nil {
		t.Fatal(err)
	}
	sc := src.Config().(*SourceConfig)
	if sc.RotatedPattern != "f.txt.*" || !sc.RotationAcceptLoss || sc.QuietPeriod != 250*time.Millisecond {
		t.Fatalf("config = %+v", sc)
	}
	// defaults: fail loudly, hold partial lines (quietPeriod off), 16 MiB lines
	src = NewSource()
	if err := sdk.Util.ParseConfig(ctx, config.Config{"path": p}, src.Config(), Connector.NewSpecification().SourceParams); err != nil {
		t.Fatal(err)
	}
	sc = src.Config().(*SourceConfig)
	if sc.RotatedPattern != "" || sc.RotationAcceptLoss || sc.QuietPeriod != 0 || sc.MaxLineSize != 16<<20 {
		t.Fatalf("defaults = %+v", sc)
	}

	if err := (SourceConfig{Config: Config{Path: p}, RotatedPattern: "f.txt.[", QuietPeriod: 0}).Validate(ctx); err == nil {
		t.Fatal("invalid glob accepted")
	}
	if err := (SourceConfig{Config: Config{Path: p}, QuietPeriod: -time.Second}).Validate(ctx); err == nil {
		t.Fatal("negative quietPeriod accepted")
	}
}

// End to end through Source: two rotations while stopped.
func TestSource_RotationPatternAndFailLoudly(t *testing.T) {
	setup := func(t *testing.T) (string, opencdc.Position) {
		e := newRotEnv(t)
		writeFile(t, e.path, "a1\n")
		touch(t, e.path, 0)
		s := &Source{config: SourceConfig{Config: Config{Path: e.path}}}
		ctx := context.Background()
		if err := s.Open(ctx, nil); err != nil {
			t.Fatal(err)
		}
		rec, err := s.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Teardown(ctx)
		e.rename(e.path, e.path+".1")
		writeFile(t, e.path, "b1\n")
		touch(t, e.path, 10*time.Second)
		e.rename(e.path, e.path+".2")
		writeFile(t, e.path, "c1\n")
		touch(t, e.path, 20*time.Second)
		return e.path, rec.Position
	}
	read := func(s *Source, n int) ([]string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var out []string
		for range n {
			rec, err := s.Read(ctx)
			if err != nil {
				return out, err
			}
			out = append(out, string(rec.Payload.After.Bytes()))
		}
		return out, nil
	}

	path, pos := setup(t)
	s := &Source{config: SourceConfig{Config: Config{Path: path}}}
	if err := s.Open(context.Background(), pos); err != nil {
		t.Fatal(err)
	}
	_, err := read(s, 1)
	_ = wantCode(t, err, ErrCodeRotationTailUnreadable)
	_ = s.Teardown(context.Background())

	path, pos = setup(t)
	s = &Source{config: SourceConfig{Config: Config{Path: path}, RotatedPattern: "f.txt.*"}}
	if err := s.Open(context.Background(), pos); err != nil {
		t.Fatal(err)
	}
	got, err := read(s, 2)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, got, []string{"b1", "c1"})
	_ = s.Teardown(context.Background())
}
