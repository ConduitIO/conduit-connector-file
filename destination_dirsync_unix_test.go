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
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/conduitio/conduit-commons/config"
	"github.com/conduitio/conduit-commons/opencdc"
	sdk "github.com/conduitio/conduit-connector-sdk"
)

// A filesystem that does not support fsync on a directory (EINVAL, ENOTSUP or
// EOPNOTSUPP, as some network and FUSE filesystems answer) fails Open by
// default, with a stable code, and closes what Open opened: a new output
// there may not survive an operating-system crash, and neither may the
// records acknowledged into it. With dirSync.acceptUnsupported Open warns
// once, with the same code, and writes work. Any other directory sync error
// fails Open either way.
func TestDestination_OpenWhenDirectorySyncUnsupported(t *testing.T) {
	for _, accept := range []bool{false, true} {
		for _, tc := range []struct {
			name        string
			err         syscall.Errno
			unsupported bool
		}{
			{"EINVAL", syscall.EINVAL, true},
			{"ENOTSUP", syscall.ENOTSUP, true},
			{"EOPNOTSUPP", syscall.EOPNOTSUPP, true},
			{"EIO", syscall.EIO, false},
			{"ENOSPC", syscall.ENOSPC, false},
			{"EROFS", syscall.EROFS, false},
		} {
			t.Run(fmt.Sprintf("%s/acceptUnsupported=%v", tc.name, accept), func(t *testing.T) {
				o := openWithDirSyncError(t, accept, tc.err)
				if tc.unsupported && accept {
					o.assertAccepted(t, tc.name, tc.err)
				} else {
					o.assertRefused(t, tc.name, tc.err, tc.unsupported)
				}
			})
		}
	}
}

// errnoName is how Open names errno: ENOTSUP and EOPNOTSUPP are one value on
// Linux and FreeBSD, named after both.
func errnoName(name string, errno syscall.Errno) string {
	if syscall.ENOTSUP == syscall.EOPNOTSUPP && errno == syscall.ENOTSUP {
		return "ENOTSUP/EOPNOTSUPP"
	}
	return name
}

// dirSyncOpen is a destination Open whose directory sync failed.
type dirSyncOpen struct {
	d      *Destination
	ctx    context.Context
	logs   *bytes.Buffer
	opened []*os.File // every file Open opened, through the opener
	path   string
	err    error // of Open
}

// openWithDirSyncError opens a destination whose directory fsync fails with
// errno, as syncDir reports it.
func openWithDirSyncError(t *testing.T, accept bool, errno syscall.Errno) *dirSyncOpen {
	t.Helper()
	o := &dirSyncOpen{path: filepath.Join(t.TempDir(), "out.jsonl")}
	o.ctx, o.logs = logCtx()
	o.ctx = WithOpener(o.ctx, func(p string, flag int, perm os.FileMode) (*os.File, error) {
		f, err := os.OpenFile(p, flag, perm) //nolint:gosec // test
		if err == nil {
			o.opened = append(o.opened, f)
		}
		return f, err
	})
	o.d = &Destination{config: DestinationConfig{Config: Config{Path: o.path}, DirSyncAcceptUnsupported: accept}}
	calls := 0
	o.d.syncDir = func(dir string) error {
		calls++
		return dirSyncResult(dir, &os.PathError{Op: "sync", Path: dir, Err: errno}, nil)
	}
	o.err = o.d.Open(o.ctx)
	if calls != 1 {
		t.Fatalf("directory synced %d times, want 1", calls)
	}
	return o
}

// assertRefused checks that Open failed with the directory sync error (coded
// FILE_DIR_SYNC_UNSUPPORTED when unsupported), closed every file it opened,
// warned nothing and left the destination unopened.
func (o *dirSyncOpen) assertRefused(t *testing.T, name string, errno syscall.Errno, unsupported bool) {
	t.Helper()
	if !errors.Is(o.err, errno) {
		t.Fatalf("Open = %v, want the %s directory sync error", o.err, name)
	}
	var coded *CodedError
	if isCoded := errors.As(o.err, &coded) && coded.Code == ErrCodeDirSyncUnsupported; isCoded != unsupported {
		t.Fatalf("Open = %v, coded %s = %v, want %v", o.err, ErrCodeDirSyncUnsupported, isCoded, unsupported)
	}
	if unsupported {
		for _, want := range []string{
			fmt.Sprintf("%q (%s)", filepath.Dir(o.path), errnoName(name, errno)),
			"may not survive an operating-system crash",
			"set dirSync.acceptUnsupported to true",
			"sync -f",
		} {
			if !strings.Contains(o.err.Error(), want) {
				t.Fatalf("Open = %q, want it to contain %q", o.err, want)
			}
		}
	}
	if len(o.opened) == 0 {
		t.Fatal("Open opened nothing through the opener")
	}
	for _, f := range o.opened {
		if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("%s left open by the failed Open (Stat = %v)", f.Name(), err)
		}
	}
	if n, err := o.d.Write(o.ctx, []opencdc.Record{benchmarkRecord(1)}); err == nil || n != 0 {
		t.Fatalf("Write after a failed Open = (%d, %v), want (0, error)", n, err)
	}
	if err := o.d.Teardown(o.ctx); err != nil {
		t.Fatalf("Teardown after a failed Open = %v, want nil", err)
	}
	if w := warnings(t, o.logs); len(w) != 0 {
		t.Fatalf("warnings = %v, want none", w)
	}
}

// assertAccepted checks that Open succeeded with exactly one coded warning
// that names the errno, the directory and the risk, and that writes work.
func (o *dirSyncOpen) assertAccepted(t *testing.T, name string, errno syscall.Errno) {
	t.Helper()
	if o.err != nil {
		t.Fatalf("Open = %v, want nil", o.err)
	}
	t.Cleanup(func() { _ = o.d.Teardown(o.ctx) })

	w := warnings(t, o.logs)
	if len(w) != 1 {
		t.Fatalf("warnings = %v, want exactly one", w)
	}
	msg, _ := w[0]["message"].(string)
	if w[0]["code"] != ErrCodeDirSyncUnsupported || !strings.HasPrefix(msg, "["+ErrCodeDirSyncUnsupported+"]") ||
		!strings.Contains(msg, "may not survive an operating-system crash") ||
		!strings.Contains(msg, "("+errnoName(name, errno)+")") {
		t.Fatalf("warning = %v, want code %s, the errno and the residual risk", w[0], ErrCodeDirSyncUnsupported)
	}
	if e, _ := w[0]["error"].(string); w[0]["dir"] != filepath.Dir(o.path) || !strings.Contains(e, errno.Error()) {
		t.Fatalf("warning = %v, want the directory %q and the error", w[0], filepath.Dir(o.path))
	}

	writeAll(t, o.d, benchmarkRecord(1), benchmarkRecord(2))
	assertWholeRecords(t, o.path, benchmarkRecord(1), benchmarkRecord(2))
}

// Only the fsync's own error decides that the filesystem does not support
// fsync on a directory. An error closing the directory, whatever it looks
// like, is never mistaken for that, and it fails Open even when the fsync
// was unsupported.
func TestDirSyncResult(t *testing.T) {
	pathErr := func(op string, errno syscall.Errno) error {
		return &os.PathError{Op: op, Path: "dir", Err: errno}
	}
	for _, tc := range []struct {
		name     string
		syncErr  error
		closeErr error
		wantErr  bool
		// wantErrno is the errno of the *dirSyncUnsupportedError, 0 for none.
		wantErrno syscall.Errno
	}{
		{"ok", nil, nil, false, 0},
		{"sync EINVAL", pathErr("sync", syscall.EINVAL), nil, true, syscall.EINVAL},
		{"sync ENOTSUP", pathErr("sync", syscall.ENOTSUP), nil, true, syscall.ENOTSUP},
		{"sync EOPNOTSUPP", pathErr("sync", syscall.EOPNOTSUPP), nil, true, syscall.EOPNOTSUPP},
		{"sync EIO", pathErr("sync", syscall.EIO), nil, true, 0},
		{"close EINVAL after a good sync", nil, pathErr("close", syscall.EINVAL), true, 0},
		{"close EOPNOTSUPP after a good sync", nil, pathErr("close", syscall.EOPNOTSUPP), true, 0},
		{"sync EIO, close EINVAL", pathErr("sync", syscall.EIO), pathErr("close", syscall.EINVAL), true, 0},
		{"sync EINVAL, close EIO", pathErr("sync", syscall.EINVAL), pathErr("close", syscall.EIO), true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := dirSyncResult("dir", tc.syncErr, tc.closeErr)
			if (err != nil) != tc.wantErr {
				t.Fatalf("dirSyncResult = %v, want error %v", err, tc.wantErr)
			}
			var unsupported *dirSyncUnsupportedError
			if got := errors.As(err, &unsupported); got != (tc.wantErrno != 0) {
				t.Fatalf("dirSyncResult = %v, unsupported %v, want %v", err, got, tc.wantErrno != 0)
			}
			if tc.wantErrno != 0 {
				want := errnoName(strings.TrimPrefix(tc.name, "sync "), tc.wantErrno)
				if unsupported.Dir != "dir" || unsupported.Errno != want || !errors.Is(unsupported, tc.wantErrno) {
					t.Fatalf("dirSyncResult = %+v, want directory %q and errno %s", unsupported, "dir", want)
				}
			}
			for _, e := range []error{tc.syncErr, tc.closeErr} {
				if e != nil && !errors.Is(err, e) {
					t.Fatalf("dirSyncResult = %v, want it to wrap %v", err, e)
				}
			}
		})
	}
}

// The option exists in the spec, parses, and is off by default.
func TestDestinationConfig_DirSyncAcceptUnsupported(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "out.jsonl")
	for _, tc := range []struct {
		cfg  config.Config
		want bool
	}{
		{config.Config{"path": p}, false},
		{config.Config{"path": p, "dirSync.acceptUnsupported": "true"}, true},
	} {
		dst := NewDestination()
		if err := sdk.Util.ParseConfig(ctx, tc.cfg, dst.Config(), Connector.NewSpecification().DestinationParams); err != nil {
			t.Fatal(err)
		}
		if got := dst.Config().(*DestinationConfig).DirSyncAcceptUnsupported; got != tc.want {
			t.Fatalf("config %v: DirSyncAcceptUnsupported = %v, want %v", tc.cfg, got, tc.want)
		}
	}
}
