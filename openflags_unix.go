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

// syncDir fsyncs the directory dir, so that the entries of files created in
// it survive an operating-system crash or a power loss.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
