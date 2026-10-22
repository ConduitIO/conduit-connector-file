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
	"testing"

	"github.com/conduitio/conduit-commons/opencdc"
)

var errPolicy = errors.New("outside the allowed directories")

// allowOnly returns a DirChecker that allows exactly the given directories.
func allowOnly(calls *[]string, dirs ...string) DirChecker {
	return func(dir string) error {
		*calls = append(*calls, dir)
		for _, d := range dirs {
			if filepath.Clean(dir) == filepath.Clean(d) {
				return nil
			}
		}
		return errPolicy
	}
}

func TestPatternDir(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "app", "f.log")
	cases := []struct {
		pattern, dir, name string
		code               string
	}{
		{"f.log.*", filepath.Join(root, "app"), "f.log.*", ""},
		{"old/f.log.*", filepath.Join(root, "app", "old"), "f.log.*", ""},
		{filepath.Join(root, "archive", "f.log-[0-9]*"), filepath.Join(root, "archive"), "f.log-[0-9]*", ""},
		{"../x/f.log.*", filepath.Join(root, "x"), "f.log.*", ""},
		{filepath.Join(root, "*", "f.log.*"), "", "", ErrCodeRotatedPatternInvalid},
		{"o?d/f.log.*", "", "", ErrCodeRotatedPatternInvalid},
		{"[ab]/f.log.*", "", "", ErrCodeRotatedPatternInvalid},
		{filepath.Join(root, "dir") + "/", "", "", ErrCodeRotatedPatternInvalid},
		{"f.log.[", "", "", ErrCodeRotatedPatternInvalid},
	}
	for _, c := range cases {
		dir, name, err := PatternDir(p, c.pattern)
		if c.code != "" {
			var ce *CodedError
			if !errors.As(err, &ce) || ce.Code != c.code || ce.Fix == "" {
				t.Errorf("%q: err = %v, want %s", c.pattern, err, c.code)
			}
			continue
		}
		if err != nil || dir != c.dir || name != c.name {
			t.Errorf("%q: got (%q, %q, %v), want (%q, %q)", c.pattern, dir, name, err, c.dir, c.name)
		}
	}
}

// An absolute pattern outside the allowed directories is refused at
// configuration (Validate) and again at Open, with a coded error that carries
// the engine's own refusal.
func TestSource_RotatedPatternOutsideAllowedDirsIsRefused(t *testing.T) {
	root := t.TempDir()
	allowed := filepath.Join(root, "allowed")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{allowed, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(allowed, "f.log")
	var calls []string
	ctx := WithDirChecker(context.Background(), allowOnly(&calls, allowed))

	ok := SourceConfig{Config: Config{Path: p}, RotatedPattern: "f.log.*"}
	if err := ok.Validate(ctx); err != nil {
		t.Fatalf("relative pattern in the allowed directory: %v", err)
	}
	ok.RotatedPattern = filepath.Join(allowed, "f.log.*")
	if err := ok.Validate(ctx); err != nil {
		t.Fatalf("absolute pattern in the allowed directory: %v", err)
	}

	bad := SourceConfig{Config: Config{Path: p}, RotatedPattern: filepath.Join(outside, "f.log.*")}
	err := bad.Validate(ctx)
	var ce *CodedError
	if !errors.As(err, &ce) || ce.Code != ErrCodeRotatedPatternDenied || !errors.Is(err, errPolicy) || ce.Fix == "" {
		t.Fatalf("Validate = %v, want %s wrapping the policy error", err, ErrCodeRotatedPatternDenied)
	}
	if got := calls[len(calls)-1]; got != outside {
		t.Fatalf("the policy was asked about %q, want %q", got, outside)
	}

	s := &Source{config: bad}
	writeFile(t, p, "x\n")
	if err := s.Open(ctx, opencdc.Position(nil)); !errors.As(err, &ce) || ce.Code != ErrCodeRotatedPatternDenied {
		t.Fatalf("Open = %v", err)
	}

	// a wildcard in the directory is invalid whatever the policy says
	wild := SourceConfig{Config: Config{Path: p}, RotatedPattern: filepath.Join(allowed, "*", "f.log.*")}
	if err := wild.Validate(ctx); !errors.As(err, &ce) || ce.Code != ErrCodeRotatedPatternInvalid {
		t.Fatalf("wildcard directory: %v", err)
	}

	// without a checker (standalone use) nothing is restricted
	if err := bad.Validate(context.Background()); err != nil {
		t.Fatalf("no checker: %v", err)
	}
}

// Every listing goes through the checker: the restart search and the
// accounting between rotated files. A refusal stops the source with the code.
func TestFollower_ListingsAreChecked(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	writeFile(t, p, "a\nb\n")
	fl := newTestFollower(t, p, 0)
	readAll(t, fl, 2)
	pos := savedPosition2(t, p, fl)
	_ = fl.close()

	// the saved file is replaced: the restart must look for it (a listing)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	writeFile(t, p, "new\n")

	var calls []string
	deny := func(string) error { return errPolicy }
	fl2 := newFollower(p, os.OpenFile, pos, 0, nil).withRotation(rotationOptions{
		pattern:  "f.txt.*",
		checkDir: func(d string) error { calls = append(calls, d); return deny(d) },
	})
	err := fl2.start()
	var ce *CodedError
	if !errors.As(err, &ce) || ce.Code != ErrCodeRotatedPatternDenied || !errors.Is(err, errPolicy) {
		t.Fatalf("start = %v", err)
	}
	if len(calls) != 1 || calls[0] != dir {
		t.Fatalf("checker calls = %v", calls)
	}

	// without a pattern the directory of the path is what is listed
	calls = nil
	fl3 := newFollower(p, os.OpenFile, pos, 0, nil).withRotation(rotationOptions{
		checkDir: func(d string) error { calls = append(calls, d); return errPolicy },
	})
	if err := fl3.start(); !errors.As(err, &ce) || ce.Code != ErrCodeRotatedPatternDenied {
		t.Fatalf("start = %v", err)
	}
	if len(calls) != 1 || calls[0] != dir {
		t.Fatalf("checker calls = %v", calls)
	}
}

func savedPosition2(t *testing.T, path string, fl *follower) *position {
	t.Helper()
	p, err := parsePosition(path, []byte(fl.position()))
	if err != nil || p == nil {
		t.Fatalf("position %q: %v", fl.position(), err)
	}
	return p
}
