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

//go:build linux

package file

import (
	"errors"
	"os"
	"syscall"
)

// syncFile makes the written bytes of f durable with fdatasync. That is enough
// for the destination: fdatasync writes the data and every piece of metadata
// needed to read it back, including a changed file size (an append, a
// truncate, the first 8 bytes of the committed-length file); it skips only
// timestamps.
func syncFile(f *os.File) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var syncErr error
	if err := rc.Control(func(fd uintptr) {
		for {
			syncErr = syscall.Fdatasync(int(fd))
			if !errors.Is(syncErr, syscall.EINTR) {
				return
			}
		}
	}); err != nil {
		return err
	}
	if syncErr != nil {
		return &os.PathError{Op: "fdatasync", Path: f.Name(), Err: syncErr}
	}
	return nil
}
