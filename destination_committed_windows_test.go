// Copyright © 2026 Meroxa, Inc.
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

//go:build windows

package file

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// On Windows the committed-length file is opened with
// FILE_FLAG_OPEN_REPARSE_POINT and refused by the handle's attributes, so a
// symlink at <path>.committed fails Open without touching its target, whether
// the target exists or not (a dangling link must not create it).
func TestDestination_RefusesReparsePointCommittedFile(t *testing.T) {
	for _, targetExists := range []bool{true, false} {
		name := "dangling link"
		if targetExists {
			name = "link to a file"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "out.jsonl")
			target := filepath.Join(dir, "target")
			const content = "not the destination's"
			if targetExists {
				if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, committedPath(path)); err != nil {
				// Creating a symlink needs a privilege or developer mode.
				t.Skipf("cannot create a symlink here: %v", err)
			}

			d := &Destination{config: DestinationConfig{Config: Config{Path: path}}}
			err := d.Open(context.Background())
			var ce *CodedError
			if !errors.As(err, &ce) || ce.Code != ErrCodeCommittedLengthInvalid {
				_ = d.Teardown(context.Background())
				t.Fatalf("Open = %v, want %s", err, ErrCodeCommittedLengthInvalid)
			}
			got, rerr := os.ReadFile(target)
			switch {
			case targetExists && (rerr != nil || string(got) != content):
				t.Fatalf("the link's target changed: %q, %v", got, rerr)
			case !targetExists && !errors.Is(rerr, os.ErrNotExist):
				t.Fatalf("the dangling link's target was created (%v)", rerr)
			}
		})
	}
}
