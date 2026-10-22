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
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	envKillChild = "FILE_FOLLOWER_KILL_CHILD"
	envQuiet     = "FILE_FOLLOWER_QUIET"
)

// TestKillChildHelper is not a test: re-executed with envKillChild set, the test
// binary acts as a connector. It follows src.txt from the saved position,
// "delivers" each line to out.txt, and saves the position after delivery
// (never before: at-least-once). The parent kills it with SIGKILL.
func TestKillChildHelper(t *testing.T) {
	dir := os.Getenv(envKillChild)
	if dir == "" {
		t.Skip("helper process")
	}
	quiet, err := time.ParseDuration(os.Getenv(envQuiet))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "src.txt")
	ckpt := filepath.Join(dir, "ckpt")
	out, err := os.OpenFile(filepath.Join(dir, "out.txt"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // test
	if err != nil {
		t.Fatal(err)
	}
	var pos *position
	if b, err := os.ReadFile(ckpt); err == nil { //nolint:gosec // test
		if pos, err = parsePosition(src, b); err != nil {
			t.Fatal(err)
		}
	}
	fl := newFollower(src, os.OpenFile, pos, quiet, nil)
	if err := fl.start(); err != nil {
		t.Fatal(err)
	}
	for {
		line, _, _, err := fl.next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// Deliver (to the OS; a SIGKILL does not lose data written to the kernel) ...
		if _, err := fmt.Fprintf(out, "%s\n", line); err != nil {
			t.Fatal(err)
		}
		// ... then save the position atomically: write temp, rename.
		tmp := ckpt + ".tmp"
		if err := os.WriteFile(tmp, []byte(fl.position()), 0o600); err != nil { //nolint:gosec // test
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
	// A quiet period of an hour: the unterminated tail is never flushed, so a
	// kill -9 mid-line must not lose or split anything.
	cmd.Env = append(os.Environ(), envKillChild+"="+dir, envQuiet+"=1h")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func kill9(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
}

func randSleep(minMs, spanMs int) {
	time.Sleep(time.Duration(minMs+rand.Intn(spanMs)) * time.Millisecond) //nolint:gosec // test
}

// finishAndCheck runs a last child until line-<last> was delivered, kills it
// and asserts every line 1..last was delivered at least once and nothing else
// (no fragment of a line) was.
func finishAndCheck(t *testing.T, dir string, last, kills int) {
	t.Helper()
	cmd := startChild(t, dir)
	out := filepath.Join(dir, "out.txt")
	needle := fmt.Sprintf("\nline-%d\n", last)
	deadline := time.Now().Add(60 * time.Second)
	for {
		b, _ := os.ReadFile(out)
		if strings.Contains("\n"+string(b), needle) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child did not deliver line-%d", last)
		}
		time.Sleep(20 * time.Millisecond)
	}
	kill9(t, cmd)

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	valid := regexp.MustCompile(`^line-\d+$`)
	seen := make(map[string]int, last)
	for _, l := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if !valid.MatchString(l) {
			t.Fatalf("delivered a fragment or unexpected line %q", l)
		}
		seen[l]++
	}
	dups := 0
	for i := 1; i <= last; i++ {
		c := seen[fmt.Sprintf("line-%d", i)]
		if c == 0 {
			t.Fatalf("line-%d was never delivered: gap after kill -9", i)
		}
		dups += c - 1
	}
	t.Logf("%d lines, %d kills, %d replayed lines, 0 gaps", last, kills, dups)
}

func waitDelivered(t *testing.T, dir string, line int) {
	t.Helper()
	needle := fmt.Sprintf("\nline-%d\n", line)
	deadline := time.Now().Add(60 * time.Second)
	for {
		b, _ := os.ReadFile(filepath.Join(dir, "out.txt"))
		if strings.Contains("\n"+string(b), needle) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child did not deliver line-%d", line)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func appendLines(t *testing.T, path string, from, to int) {
	t.Helper()
	var sb strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&sb, "line-%d\n", i)
	}
	appendFile(t, path, sb.String())
}

func TestFollower_Kill9RecoveryNoGaps(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	const total = 30000
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	writeFile(t, src, "")

	// The writer appends numbered lines in bursts, also while the child runs.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 1; i <= total; {
			j := min(i+rand.Intn(200), total) //nolint:gosec // test
			appendLines(t, src, i, j)
			i = j + 1
			time.Sleep(time.Millisecond)
		}
	}()

	const kills = 5
	for range kills {
		cmd := startChild(t, dir)
		randSleep(100, 300)
		kill9(t, cmd)
	}
	<-done
	finishAndCheck(t, dir, total, kills)
}

// Lines arrive in pieces and the child is killed at random moments, so it dies
// with an unterminated line pending. Nothing of that line may be delivered or
// checkpointed; after the restart it is delivered once, whole.
func TestFollower_Kill9MidLine(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	const total = 3000
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	writeFile(t, src, "")

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 1; i <= total; i++ {
			s := fmt.Sprintf("line-%d\n", i)
			for _, piece := range []string{s[:3], s[3 : len(s)-1], s[len(s)-1:]} {
				appendFile(t, src, piece)
				time.Sleep(500 * time.Microsecond)
			}
		}
	}()

	const kills = 6
	for range kills {
		cmd := startChild(t, dir)
		randSleep(150, 250)
		kill9(t, cmd)
	}
	<-done
	finishAndCheck(t, dir, total, kills)
}

// The file is rotated while the child runs (it drains the old file, then reads
// the new one from its start) and while it is dead (the restart finds the
// rotated-away file next to the path by inode, finishes it, then reads the new
// file from its start). The child is killed with SIGKILL around each rotation.
// It always has caught up before a rotation: being two or more rotations behind
// is the documented limitation, see TestFollower_TwoRotationsBehindSkipsTheMiddleFile.
func TestFollower_Kill9AcrossRotation(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	writeFile(t, src, "")

	next := 1
	write := func(n int) {
		appendLines(t, src, next, next+n-1)
		next += n
	}
	rotate := func(k int) {
		if err := os.Rename(src, fmt.Sprintf("%s.%d", src, k)); err != nil {
			t.Fatal(err)
		}
		writeFile(t, src, "")
	}

	write(2000)
	kills := 0
	for k := 1; k <= 8; k++ {
		cmd := startChild(t, dir)
		waitDelivered(t, dir, next-1)
		if k%2 == 1 {
			// rotate while the child runs, kill it at a random moment after
			write(1500)
			rotate(k)
			write(1500)
			randSleep(0, 60)
			kill9(t, cmd)
		} else {
			kill9(t, cmd)
			// rotate while it is dead, with an unread tail in the old file
			write(1500)
			rotate(k)
			write(1500)
		}
		kills++
	}
	finishAndCheck(t, dir, next-1, kills)
}
