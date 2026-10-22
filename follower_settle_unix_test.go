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
	"os"
	"path/filepath"
	"testing"
	"time"
)

func appendTo(t *testing.T, w *os.File, s string) {
	t.Helper()
	if _, err := w.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func openWriter(t *testing.T, p string) *os.File {
	t.Helper()
	w, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// logrotate renames the file and only later tells the program to reopen it: until
// then the writer keeps appending through its old descriptor. Those lines are
// read before the follower moves on to the new file.
func TestFollower_RotatedFileStillWrittenThroughOldDescriptor(t *testing.T) {
	e := newRotEnv(t)
	writeFile(t, e.path, "a\n")
	w := openWriter(t, e.path)
	fl := newFollower(e.path, os.OpenFile, nil, 0, nil).withSettle(1500 * time.Millisecond)
	if err := fl.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fl.close() })
	eq(t, lines(t, fl, 1), []string{"a"})

	e.rename(e.path, e.path+".1")
	writeFile(t, e.path, "n1\n") // the program has not reopened yet: w still writes to .1

	got := make(chan []string, 1)
	go func() {
		var out []string
		for range 3 {
			g, err := nextWithin(t, fl, 10*time.Second)
			if err != nil {
				break
			}
			out = append(out, g.line)
		}
		got <- out
	}()
	time.Sleep(400 * time.Millisecond) // the rotation has been noticed by now
	appendTo(t, w, "late1\n")
	time.Sleep(100 * time.Millisecond)
	appendTo(t, w, "late2\n")

	eq(t, <-got, []string{"late1", "late2", "n1"})
}

// And after the follower settled and moved on, a writer that is told to reopen
// late appends once more: the finished file is still held open and re-read, and
// the follower then continues with the file it was in.
func TestFollower_FinishedFileThatGrowsAgainIsReadAgain(t *testing.T) {
	e := newRotEnv(t)
	writeFile(t, e.path, "a\n")
	w := openWriter(t, e.path)
	fl := newFollower(e.path, os.OpenFile, nil, 0, nil).withSettle(50 * time.Millisecond)
	if err := fl.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fl.close() })
	eq(t, lines(t, fl, 1), []string{"a"})

	e.rename(e.path, e.path+".1")
	writeFile(t, e.path, "n1\n")
	eq(t, lines(t, fl, 1), []string{"n1"}) // settled, moved on

	appendTo(t, w, "late-after-settle\n")
	eq(t, lines(t, fl, 1), []string{"late-after-settle"})

	// the follower is back in the new file
	appendFile(t, e.path, "n2\n")
	eq(t, lines(t, fl, 1), []string{"n2"})
}

// Inode numbers are reused. A finished file is remembered by its content as
// well, so a different file that got the same number is not taken for it.
func TestFollower_FinishedFileIsRememberedByContentToo(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.log")
	writeFile(t, p, "aaaa\nbbbb\n")
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	fl := newFollower(filepath.Join(dir, "current"), os.OpenFile, nil, 0, nil)
	fl.rememberFinished(f, fi)

	cand := func() famFile {
		info, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		return famFile{path: p, info: info}
	}
	if !fl.isFinished(cand()) {
		t.Fatal("the finished file is not recognized")
	}
	appendFile(t, p, "cccc\n") // it grew: still the same file
	if !fl.isFinished(cand()) {
		t.Fatal("a finished file that grew is not recognized")
	}
	// same inode, different content: a reused number
	if err := os.WriteFile(p, []byte("zzzzzzzzzzzz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fl.isFinished(cand()) {
		t.Fatal("a different file with the same inode number was taken for the finished one")
	}
}
