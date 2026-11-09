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
	"errors"
	"os"
	"syscall"
)

// openFlags opens without blocking: opening a FIFO for reading with no writer
// would otherwise block forever before the regular-file check can refuse it.
const openFlags = os.O_RDONLY | syscall.O_NONBLOCK

// committedOpenFlags opens (or creates) the destination's committed-length
// file without following a symlink in its last component: the file is the
// connector's own and is never a link.
const committedOpenFlags = os.O_CREATE | os.O_RDWR | syscall.O_NOFOLLOW

// committedViaOpener tells whether the committed-length file is opened
// through the engine's Opener (on unix it is, with committedOpenFlags).
const committedViaOpener = true

// openCommittedNoFollow opens (or creates, with perm) the committed-length
// file at cpath through open, refusing a symlink in the open itself.
func openCommittedNoFollow(open Opener, cpath string, perm os.FileMode) (*os.File, error) {
	c, err := open(cpath, committedOpenFlags, perm)
	if err != nil {
		// The open already refused; the Lstat only names why (ELOOP on
		// Linux and macOS, EMLINK on FreeBSD).
		if li, lerr := os.Lstat(cpath); lerr == nil && li.Mode()&os.ModeSymlink != 0 {
			return nil, errCommittedIsLink
		}
		return nil, err
	}
	return c, nil
}

// syncDir fsyncs the directory dir, so that the entries of files created in
// it survive an operating-system crash or a power loss. An fsync the
// filesystem does not support gives a *dirSyncUnsupportedError (see
// dirSyncResult).
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	return dirSyncResult(dir, f.Sync(), f.Close())
}

// dirSyncResult is the result of syncDir from the errors of the fsync and of
// the close of dir. Only the fsync's own error is classified: when it is
// EINVAL, ENOTSUP or EOPNOTSUPP (as some network and FUSE filesystems answer)
// and the close succeeded, the filesystem does not support fsync on a
// directory, and the result is a *dirSyncUnsupportedError. Any other error,
// and any error of the close, is returned as it is, so it fails Open. ENOTSUP
// and EOPNOTSUPP are one value on Linux and FreeBSD, named after both there,
// and differ on macOS, OpenBSD and NetBSD.
func dirSyncResult(dir string, syncErr, closeErr error) error {
	if syncErr == nil || closeErr != nil {
		return errors.Join(syncErr, closeErr)
	}
	notsup := "ENOTSUP"
	if syscall.ENOTSUP == syscall.EOPNOTSUPP {
		notsup = "ENOTSUP/EOPNOTSUPP"
	}
	for _, e := range []struct {
		name  string
		errno syscall.Errno
	}{
		{"EINVAL", syscall.EINVAL},
		{notsup, syscall.ENOTSUP},
		{"EOPNOTSUPP", syscall.EOPNOTSUPP},
	} {
		if errors.Is(syncErr, e.errno) {
			return &dirSyncUnsupportedError{Dir: dir, Errno: e.name, Err: syncErr}
		}
	}
	return syncErr
}
