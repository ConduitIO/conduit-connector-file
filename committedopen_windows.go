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
	"os"

	"golang.org/x/sys/windows"
)

// committedViaOpener is false: the engine's Opener (os.OpenFile semantics)
// follows reparse points, so on Windows the committed-length file is opened
// with CreateFile directly. It lies next to the output, which was opened
// through the Opener, under a fixed name.
const committedViaOpener = false

// openCommittedNoFollow opens (or creates) the committed-length file at cpath
// without following a reparse point (a symlink, a junction, any other) and
// refuses one by the attributes of the opened handle, so there is no window
// between a check and the open. perm is not applied: Windows has no mode bits
// beyond read-only, and the file must be writable.
func openCommittedNoFollow(_ Opener, cpath string, _ os.FileMode) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(cpath)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: cpath, Err: err}
	}
	// FILE_FLAG_OPEN_REPARSE_POINT opens a link itself instead of its
	// target, and OPEN_ALWAYS then never creates the target of a dangling
	// link. No FILE_FLAG_BACKUP_SEMANTICS: a directory then fails to open,
	// which is right, it is not the destination's file.
	h, err := windows.CreateFile(p,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: cpath, Err: err}
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		_ = windows.CloseHandle(h)
		return nil, &os.PathError{Op: "GetFileInformationByHandle", Path: cpath, Err: err}
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(h)
		return nil, errCommittedIsLink
	}
	return os.NewFile(uintptr(h), cpath), nil
}
