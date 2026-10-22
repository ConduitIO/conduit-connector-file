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
	"os"
	"path/filepath"
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
	fl := nf(link, noFollow, 0)
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
	fl := nf(p, noFollow, 0)
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
	fl := nf(p, noFollow, 0)
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

// With the default opener (no engine policy) a swapped path is not refused: it
// is a rotation to a new file. This is what the engine-supplied opener prevents.
func TestFollower_SwappedPathIsFollowedWithDefaultOpener(t *testing.T) {
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
	// Different inode: the follow switches to what is at the path, from its
	// start. Only an engine opener can refuse the link.
	if g := mustNext(t, fl); g != (got{"TOP-SECRET-FROM-OTHER-FILE", 1, 27}) {
		t.Fatalf("got %+v", g)
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
	if !strings.HasPrefix(string(rec.Position), "v1:11:") || string(rec.Key.Bytes()) != "1" || string(rec.Payload.After.Bytes()) != "beta" {
		t.Fatalf("record = pos %q key %q payload %q", rec.Position, rec.Key.Bytes(), rec.Payload.After.Bytes())
	}
	if err := (&Source{config: s.config}).Open(ctx, opencdc.Position("nope")); err == nil {
		t.Fatal("bad position accepted")
	}
}
