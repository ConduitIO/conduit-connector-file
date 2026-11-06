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
)

// Every file the destination opens goes through the engine's opener: the
// output (for appending), the output again (to read and cut back its tail) and the
// committed-length file next to it.
func TestDestination_UsesContextOpener(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.txt")
	type call struct {
		path string
		flag int
		perm os.FileMode
	}
	var got []call
	ctx := WithOpener(context.Background(), func(p string, flag int, perm os.FileMode) (*os.File, error) {
		got = append(got, call{p, flag, perm})
		return os.OpenFile(p, flag, perm) //nolint:gosec // test
	})

	d := &Destination{config: DestinationConfig{Config: Config{Path: path}}}
	if err := d.Open(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Teardown(ctx) })

	want := []call{
		{path, os.O_CREATE | os.O_APPEND | os.O_WRONLY, 0o600},
		{path, openFlags | os.O_RDWR, 0},
		{path + ".committed", committedOpenFlags, 0o600 &^ umask(t)},
	}
	if len(got) != len(want) {
		t.Fatalf("opener got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("opener call %d got %+v, want %+v", i, got[i], want[i])
		}
	}
}

// umask returns the mode bits the process umask removed from a new 0o777
// file, so a test can predict the mode of a created output file.
func umask(t *testing.T) os.FileMode {
	t.Helper()
	p := filepath.Join(t.TempDir(), "umask")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o777)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return 0o777 &^ info.Mode().Perm()
}

func TestDestination_OpenerRefusalFailsOpen(t *testing.T) {
	refused := errors.New("refused by the engine")
	ctx := WithOpener(context.Background(), func(string, int, os.FileMode) (*os.File, error) { return nil, refused })

	d := &Destination{config: DestinationConfig{Config: Config{Path: filepath.Join(t.TempDir(), "x")}}}
	if err := d.Open(ctx); !errors.Is(err, refused) {
		t.Fatalf("Open = %v, want the opener's error", err)
	}
}

func TestWithOpener_NilKeepsDefault(t *testing.T) {
	ctx := WithOpener(context.Background(), nil)
	if openerFrom(ctx) == nil {
		t.Fatal("nil opener replaced the default")
	}
	path := filepath.Join(t.TempDir(), "d.txt")
	d := &Destination{config: DestinationConfig{Config: Config{Path: path}}}
	if err := d.Open(ctx); err != nil {
		t.Fatal(err)
	}
	_ = d.Teardown(ctx)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
