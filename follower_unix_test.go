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
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/conduitio/conduit-commons/opencdc"
)

// noFollow stands in for the engine's path policy opener: it refuses a final
// path component that is a symlink, the way openat2 with RESOLVE_NO_SYMLINKS or
// O_NOFOLLOW does.
func noFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flag|syscall.O_NOFOLLOW, perm) //nolint:gosec // test opener
}

const secret = "TOP-SECRET-FROM-OTHER-FILE\n"

func secretFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	writeFile(t, p, secret)
	return p
}

func TestFollower_SymlinkRefusedAtOpen(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "f.txt")
	if err := os.Symlink(secretFile(t), link); err != nil {
		t.Fatal(err)
	}
	fl := newFollower(link, noFollow, 0)
	err := fl.start()
	if err == nil {
		t.Fatal("symlink was opened")
	}
	if !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("err = %v, want ELOOP", err)
	}
}

// The path is checked (opened) fine, then swapped for a symlink. The periodic
// re-verification goes through the opener again and refuses it; the target's
// content is never returned.
func TestFollower_SymlinkSwapAfterOpenRefusedAtReverify(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	writeFile(t, p, "ok\n")
	fl := newFollower(p, noFollow, 0)
	if err := fl.start(); err != nil {
		t.Fatal(err)
	}
	defer fl.close()
	if g := mustNext(t, fl); g.line != "ok" {
		t.Fatal(g)
	}

	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secretFile(t), p); err != nil {
		t.Fatal(err)
	}
	g, err := nextWithin(t, fl, 5*time.Second)
	if !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("got %+v, %v; want ELOOP", g, err)
	}
	if strings.Contains(g.line, "SECRET") {
		t.Fatal("read through the swapped symlink")
	}
}

// Same swap, observed at the re-open that follows a truncation.
func TestFollower_SymlinkSwapRefusedAtTruncationReopen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	writeFile(t, p, "aaaa\nbbbb\n")
	fl := newFollower(p, noFollow, 0)
	if err := fl.start(); err != nil {
		t.Fatal(err)
	}
	defer fl.close()
	readAll(t, fl, 2)

	if err := os.Truncate(p, 0); err != nil { // the open inode shrinks ...
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil { // ... and the path becomes a link
		t.Fatal(err)
	}
	if err := os.Symlink(secretFile(t), p); err != nil {
		t.Fatal(err)
	}
	g, err := nextWithin(t, fl, 5*time.Second)
	if !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("got %+v, %v; want ELOOP", g, err)
	}
}

// With the default opener (no engine policy) a swapped path is not refused, but
// the inode check still ends the follow instead of reading the new target.
func TestFollower_SwappedInodeEndsFollow(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	writeFile(t, p, "ok\n")
	fl := newTestFollower(t, p, 0)
	mustNext(t, fl)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secretFile(t), p); err != nil {
		t.Fatal(err)
	}
	// Different inode: the follow ends (errFileGone), nothing is read from the link.
	if _, err := nextWithin(t, fl, 5*time.Second); !errors.Is(err, errFileGone) {
		t.Fatalf("err = %v", err)
	}
}

func TestSource_UsesContextOpenerForFollowing(t *testing.T) {
	link := filepath.Join(t.TempDir(), "f.txt")
	if err := os.Symlink(secretFile(t), link); err != nil {
		t.Fatal(err)
	}
	ctx := WithOpener(context.Background(), noFollow)
	s := &Source{config: SourceConfig{Config: Config{Path: link}}}
	if err := s.Open(ctx, nil); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("Open = %v, want ELOOP", err)
	}
}

func TestSource_PositionAndRecord(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	writeFile(t, p, "alpha\nbeta\ngamma\n")
	s := &Source{config: SourceConfig{Config: Config{Path: p}}}
	ctx := context.Background()
	if err := s.Open(ctx, opencdc.Position("6")); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Teardown(ctx) }()
	rec, err := s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(rec.Position) != "11" || string(rec.Key.Bytes()) != "1" || string(rec.Payload.After.Bytes()) != "beta" {
		t.Fatalf("record = pos %q key %q payload %q", rec.Position, rec.Key.Bytes(), rec.Payload.After.Bytes())
	}
	if err := (&Source{config: s.config}).Open(ctx, opencdc.Position("nope")); err == nil {
		t.Fatal("bad position accepted")
	}
}

// ---- kill -9 recovery -------------------------------------------------------

const (
	envKillChild = "FILE_FOLLOWER_KILL_CHILD"
	totalLines   = 30000
)

// TestMain-free child: when re-executed with envKillChild set, the test binary
// acts as a connector: it follows the file from the checkpoint, "delivers" each
// line to the out file, and checkpoints the position after delivery (never
// before: at-least-once). It is killed with SIGKILL by the parent.
func TestKillChildHelper(t *testing.T) {
	dir := os.Getenv(envKillChild)
	if dir == "" {
		t.Skip("helper process")
	}
	src := filepath.Join(dir, "src.txt")
	ckpt := filepath.Join(dir, "ckpt")
	out, err := os.OpenFile(filepath.Join(dir, "out.txt"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // test
	if err != nil {
		t.Fatal(err)
	}
	var off int64
	if b, err := os.ReadFile(ckpt); err == nil { //nolint:gosec // test
		off, _ = strconv.ParseInt(string(b), 10, 64)
	}
	fl := newFollower(src, os.OpenFile, off)
	if err := fl.start(); err != nil {
		t.Fatal(err)
	}
	for {
		line, _, pos, err := fl.next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// Deliver (to the OS; a SIGKILL does not lose data written to the kernel) ...
		if _, err := fmt.Fprintf(out, "%s\n", line); err != nil {
			t.Fatal(err)
		}
		// ... then checkpoint atomically: write temp, rename.
		tmp := ckpt + ".tmp"
		if err := os.WriteFile(tmp, []byte(strconv.FormatInt(pos, 10)), 0o600); err != nil { //nolint:gosec // test
			t.Fatal(err)
		}
		if err := os.Rename(tmp, ckpt); err != nil { //nolint:gosec // test
			t.Fatal(err)
		}
	}
}

func startChild(t *testing.T, dir string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), os.Args[0], //nolint:gosec // re-executes the test binary
		"-test.run=^TestKillChildHelper$", "-test.v")
	cmd.Env = append(os.Environ(), envKillChild+"="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func TestFollower_Kill9RecoveryNoGaps(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	writeFile(t, src, "")

	// The writer appends numbered lines in bursts, also while the child runs.
	done := make(chan struct{})
	go func() {
		defer close(done)
		f, err := os.OpenFile(src, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		for i := 1; i <= totalLines; {
			var sb strings.Builder
			for range 1 + rand.Intn(200) { //nolint:gosec // test
				if i > totalLines {
					break
				}
				fmt.Fprintf(&sb, "line-%d\n", i)
				i++
			}
			if _, err := f.WriteString(sb.String()); err != nil {
				t.Error(err)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	kills := 0
	for kills < 5 {
		cmd := startChild(t, dir)
		time.Sleep(time.Duration(100+rand.Intn(300)) * time.Millisecond) //nolint:gosec // test
		if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait()
		kills++
	}
	<-done

	// Final run: let it drain to the end, then stop it.
	cmd := startChild(t, dir)
	deadline := time.Now().Add(60 * time.Second)
	for {
		b, _ := os.ReadFile(filepath.Join(dir, "ckpt"))
		if n, _ := strconv.ParseInt(string(b), 10, 64); n == fileSize(t, src) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not catch up")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = cmd.Process.Signal(syscall.SIGKILL)
	_ = cmd.Wait()

	// Every line must have been delivered at least once; replays are allowed
	// (and, since the checkpoint follows delivery, expected).
	data, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]int, totalLines)
	for _, l := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		seen[l]++
	}
	dups := 0
	for i := 1; i <= totalLines; i++ {
		c := seen[fmt.Sprintf("line-%d", i)]
		if c == 0 {
			t.Fatalf("line-%d was never delivered: gap after kill -9", i)
		}
		dups += c - 1
	}
	t.Logf("%d lines, %d kills, %d replayed lines, 0 gaps", totalLines, kills, dups)
}

func fileSize(t *testing.T, p string) int64 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}
