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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A filesystem that does not support fsync on a directory (EINVAL, ENOTSUP or
// EOPNOTSUPP, as some network and FUSE filesystems answer) does not stop the
// destination: Open warns once, with a stable code, and writes work. Any other
// directory sync error still fails Open.
func TestDestination_OpenWhenDirectorySyncUnsupported(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     syscall.Errno
		wantErr bool
	}{
		{"EINVAL", syscall.EINVAL, false},
		{"ENOTSUP", syscall.ENOTSUP, false},
		{"EOPNOTSUPP", syscall.EOPNOTSUPP, false},
		{"EIO", syscall.EIO, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "out.jsonl")
			ctx, logs := logCtx()
			d := &Destination{config: DestinationConfig{Config: Config{Path: path}}}
			calls := 0
			d.syncDir = func(dir string) error {
				calls++
				return &os.PathError{Op: "sync", Path: dir, Err: tc.err}
			}
			err := d.Open(ctx)
			if calls != 1 {
				t.Fatalf("directory synced %d times, want 1", calls)
			}
			if tc.wantErr {
				if !errors.Is(err, tc.err) {
					t.Fatalf("Open = %v, want the %s directory sync error", err, tc.name)
				}
				if err := d.Teardown(ctx); err != nil {
					t.Fatalf("Teardown after a failed Open = %v, want nil", err)
				}
				if w := warnings(t, logs); len(w) != 0 {
					t.Fatalf("warnings = %v, want none", w)
				}
				return
			}
			if err != nil {
				t.Fatalf("Open = %v, want nil", err)
			}
			t.Cleanup(func() { _ = d.Teardown(ctx) })

			w := warnings(t, logs)
			if len(w) != 1 {
				t.Fatalf("warnings = %v, want exactly one", w)
			}
			msg, _ := w[0]["message"].(string)
			if w[0]["code"] != ErrCodeDirSyncUnsupported || !strings.HasPrefix(msg, "["+ErrCodeDirSyncUnsupported+"]") ||
				!strings.Contains(msg, "may not survive an operating-system crash") {
				t.Fatalf("warning = %v, want code %s and the residual risk", w[0], ErrCodeDirSyncUnsupported)
			}
			if w[0]["dir"] != filepath.Dir(path) || !strings.Contains(w[0]["error"].(string), tc.err.Error()) {
				t.Fatalf("warning = %v, want the directory %q and the error", w[0], filepath.Dir(path))
			}

			writeAll(t, d, benchmarkRecord(1), benchmarkRecord(2))
			assertWholeRecords(t, path, benchmarkRecord(1), benchmarkRecord(2))
		})
	}
}
