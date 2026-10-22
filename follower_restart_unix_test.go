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
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func savedPosition(t *testing.T, path string, fl *follower) *position {
	t.Helper()
	p, err := parsePosition(path, []byte(fl.position()))
	if err != nil || p == nil {
		t.Fatalf("position %q: %v", fl.position(), err)
	}
	return p
}

// A restart after a rotation that happened while the connector was stopped:
// the rotated-away file is found next to the path by inode and finished, then
// the new file is read from its first byte.
func TestFollower_RestartAfterRotation_FindsOldFileThenNewHead(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	writeFile(t, p, "a\nb\n")
	fl := newTestFollower(t, p, 0)
	readAll(t, fl, 2)
	pos := savedPosition(t, p, fl)
	_ = fl.close()

	appendFile(t, p, "c\nd\n") // written after the last checkpoint
	if err := os.Rename(p, p+".1"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, p, "n1\nn2\n")

	fl = newFollower(p, os.OpenFile, pos, 0, nil)
	if err := fl.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fl.close() })
	want := []string{"c", "d", "n1", "n2"}
	for i, w := range want {
		if g := mustNext(t, fl); g.line != w {
			t.Fatalf("line %d = %+v, want %q", i, g, w)
		}
	}
}

// Same file, appended to while stopped: resumes at the offset.
func TestFollower_RestartSameFileResumes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	writeFile(t, p, "a\nb\n")
	fl := newTestFollower(t, p, 0)
	readAll(t, fl, 2)
	pos := savedPosition(t, p, fl)
	_ = fl.close()
	appendFile(t, p, "c\n")

	fl = newFollower(p, os.OpenFile, pos, 0, nil)
	if err := fl.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fl.close() })
	if g := mustNext(t, fl); g != (got{"c", 1, 6}) {
		t.Fatalf("got %+v", g)
	}
}

// copytruncate while stopped: same inode, new content. The content the saved
// position belongs to is gone, so the source stops, unless the loss is accepted.
func TestFollower_RestartAfterCopytruncate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	writeFile(t, p, "alpha\nbeta\n")
	fl := newTestFollower(t, p, 0)
	readAll(t, fl, 2)
	pos := savedPosition(t, p, fl)
	_ = fl.close()

	if err := os.Truncate(p, 0); err != nil {
		t.Fatal(err)
	}
	appendFile(t, p, "gamma\ndelta\nepsilon\n")

	fl = newFollower(p, os.OpenFile, pos, 0, nil)
	_ = wantCode(t, fl.start(), ErrCodeRotationTailUnreadable)

	var warned []string
	fl = newFollower(p, os.OpenFile, pos, 0, nil).withRotation(rotationOptions{
		acceptLoss: true,
		warnf:      func(f string, a ...any) { warned = append(warned, f) },
	})
	if err := fl.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fl.close() })
	if g := mustNext(t, fl); g.line != "gamma" {
		t.Fatalf("got %+v", g)
	}
	if len(warned) != 1 {
		t.Fatalf("warnings = %v", warned)
	}
}

// The source is slow and the file is rotated twice before it has finished the
// first one. Every rotation is noticed while it is still at the path and the
// file is held open, so nothing is skipped.
func TestFollower_RotatedTwiceWhileBehindReadsEveryFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	var want []string
	var sb strings.Builder
	for i := range 400 {
		l := fmt.Sprintf("a-%d", i)
		want = append(want, l)
		sb.WriteString(l + "\n")
	}
	writeFile(t, p, sb.String())
	fl := newTestFollower(t, p, 0)

	var got []string
	read := func(n int) {
		for range n {
			got = append(got, mustNext(t, fl).line)
			time.Sleep(time.Millisecond)
		}
	}
	rotateTo := func(prefix string, k int) {
		if err := os.Rename(p, fmt.Sprintf("%s.%d", p, k)); err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for i := range 5 {
			l := fmt.Sprintf("%s-%d", prefix, i)
			want = append(want, l)
			b.WriteString(l + "\n")
		}
		writeFile(t, p, b.String())
	}

	read(50)
	rotateTo("b", 1)
	time.Sleep(10 * time.Millisecond)
	read(150) // longer than the path poll interval, still inside the first file
	rotateTo("c", 2)
	read(len(want) - len(got))

	if !slices.Equal(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}
