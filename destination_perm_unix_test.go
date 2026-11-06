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

	"github.com/conduitio/conduit-commons/opencdc"
)

// TestDestination_CreatesOwnerOnlyFile is the regression test for
// GHSA-wj57-rcgj-4x67. The umask is cleared so the test proves the mode comes
// from the connector, not from the environment.
func TestDestination_CreatesOwnerOnlyFile(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	path := filepath.Join(t.TempDir(), "out.txt")
	d := newTestDestination(t, path)
	writeAll(t, d, permRecord())

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %o, want 600", got)
	}
}

// An existing file keeps its mode: the connector must not silently chmod
// files an operator prepared.
func TestDestination_KeepsExistingFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.txt")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	d := newTestDestination(t, path)
	writeAll(t, d, permRecord())
	info, _ := os.Stat(path)
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("file mode = %o, want 640", got)
	}
}

// The committed-length file is part of the output, so it has the output's mode:
// on creation, and again on Open when the output's mode was changed.
func TestDestination_CommittedFileHasOutputMode(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	path := filepath.Join(t.TempDir(), "out.txt")
	d := newTestDestination(t, path)
	writeAll(t, d, permRecord())
	assertMode(t, committedPath(path), 0o600)

	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	d2 := newTestDestination(t, path)
	writeAll(t, d2, permRecord())
	assertMode(t, committedPath(path), 0o640)
}

// An output that is not a regular file (a device, a FIFO) cannot be truncated:
// it is written as before, without a committed-length file. /dev/null also
// proves no committed-length file is created next to it (Open would fail).
func TestDestination_NonRegularOutputHasNoCommittedLength(t *testing.T) {
	d := newTestDestination(t, os.DevNull)
	if d.committed != nil {
		t.Fatal("committed-length file opened for a device")
	}
	writeAll(t, d, permRecord())
	if _, err := os.Stat(committedPath(os.DevNull)); err == nil {
		t.Fatal("committed-length file created next to a device")
	}
}

// The committed-length file is never followed through a symlink: Open refuses
// it and leaves the link's target alone.
func TestDestination_RefusesSymlinkedCommittedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, committedPath(path)); err != nil {
		t.Fatal(err)
	}
	d := &Destination{config: DestinationConfig{Config: Config{Path: path}}}
	err := d.Open(context.Background())
	var ce *CodedError
	if !errors.As(err, &ce) || ce.Code != ErrCodeCommittedLengthInvalid || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Open = %v, want %s naming the symlink", err, ErrCodeCommittedLengthInvalid)
	}
	if got, _ := os.ReadFile(target); string(got) != "keep me" {
		t.Fatalf("symlink target changed: %q", got)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}

func permRecord() opencdc.Record {
	return opencdc.Record{
		Position:  opencdc.Position("p"),
		Operation: opencdc.OperationCreate,
		Key:       opencdc.StructuredData{"id": 1},
		Payload:   opencdc.Change{After: opencdc.StructuredData{"id": 1}},
	}
}
