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

func TestDestination_UsesContextOpener(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.txt")
	var gotPath string
	var gotFlag int
	var gotPerm os.FileMode
	ctx := WithOpener(context.Background(), func(p string, flag int, perm os.FileMode) (*os.File, error) {
		gotPath, gotFlag, gotPerm = p, flag, perm
		return os.OpenFile(p, flag, perm) //nolint:gosec // test
	})

	d := &Destination{config: DestinationConfig{Config: Config{Path: path}}}
	if err := d.Open(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Teardown(ctx) })

	if gotPath != path || gotFlag != os.O_CREATE|os.O_APPEND|os.O_WRONLY || gotPerm != 0o600 {
		t.Fatalf("opener got (%q, %#x, %o)", gotPath, gotFlag, gotPerm)
	}
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
