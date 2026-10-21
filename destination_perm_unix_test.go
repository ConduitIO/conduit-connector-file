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
	"os"
	"path/filepath"
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

func permRecord() opencdc.Record {
	return opencdc.Record{
		Position:  opencdc.Position("p"),
		Operation: opencdc.OperationCreate,
		Key:       opencdc.StructuredData{"id": 1},
		Payload:   opencdc.Change{After: opencdc.StructuredData{"id": 1}},
	}
}
