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

import "fmt"

// Stable error codes of the source and the destination. They are part of the
// connector's public contract: tooling and agents match on them.
const (
	// ErrCodePositionInvalid: the saved position is not a position this
	// connector wrote (neither an offset nor a versioned position).
	ErrCodePositionInvalid = "FILE_POSITION_INVALID"
	// ErrCodePositionUnverifiable: an offset-only position (v0.10.x) does not
	// match the current file, so resuming would guess.
	ErrCodePositionUnverifiable = "FILE_POSITION_UNVERIFIABLE"
	// ErrCodeIdentityUnknown: the identity of the file at the path could not be
	// determined, so the saved position cannot be checked against it.
	ErrCodeIdentityUnknown = "FILE_IDENTITY_UNKNOWN"
	// ErrCodeRotationTailUnreadable: data of a rotated file was never read and
	// cannot be read now (the file is gone, moved, compressed, or an unknown
	// file sits between the rotated one and the current one).
	ErrCodeRotationTailUnreadable = "FILE_ROTATION_TAIL_UNREADABLE"
	// ErrCodeLineTooLong: a line is longer than maxLineSize. It is never
	// truncated; raise the setting or fix the writer.
	ErrCodeLineTooLong = "FILE_LINE_TOO_LONG"
	// ErrCodeRotatedPatternDenied: the engine's policy does not allow listing
	// the directory rotatedPattern points into.
	ErrCodeRotatedPatternDenied = "FILE_ROTATED_PATTERN_DENIED"
	// ErrCodeRotatedPatternInvalid: rotatedPattern is not a valid glob, or has
	// a wildcard in its directory part.
	ErrCodeRotatedPatternInvalid = "FILE_ROTATED_PATTERN_INVALID"
	// ErrCodeCommittedLengthMismatch: the destination's committed-length file
	// (<path>.committed) does not describe the output file: the output is
	// shorter than the committed length (and not empty), or the committed
	// length does not end a line. The destination fsyncs the output before it
	// records a length, so a kill or an operating-system crash cannot cause
	// this. The output was truncated, replaced or restored from a backup
	// without its committed-length file, or the storage lost writes it had
	// reported as synced. The destination does not guess which bytes to keep.
	ErrCodeCommittedLengthMismatch = "FILE_COMMITTED_LENGTH_MISMATCH"
	// ErrCodeCommittedLengthInvalid: the destination's committed-length file is
	// not 0 or 8 bytes long, holds a length that is out of range, or is a
	// symlink or not a regular file.
	ErrCodeCommittedLengthInvalid = "FILE_COMMITTED_LENGTH_INVALID"
	// ErrCodeOutputTailUnterminated: the destination's output does not end
	// with a newline, no committed length is recorded, and the configured
	// record format can put a newline inside a record, so the end of the last
	// complete record cannot be found. Only after a crash of v0.12.0 or
	// earlier, or, with dirSync.acceptUnsupported set, after an
	// operating-system crash that lost a new committed-length file.
	ErrCodeOutputTailUnterminated = "FILE_OUTPUT_TAIL_UNTERMINATED"
	// ErrCodeDirSyncUnsupported: the filesystem of the destination's output
	// rejected an fsync of the output's directory as unsupported (EINVAL,
	// ENOTSUP or EOPNOTSUPP; some network and FUSE filesystems do), so a
	// newly created output file, and the records acknowledged into it, may not
	// survive an operating-system crash or a power loss. Open fails with it,
	// unless dirSync.acceptUnsupported is set: Open then logs it as a warning
	// and continues.
	ErrCodeDirSyncUnsupported = "FILE_DIR_SYNC_UNSUPPORTED"
)

// CodedError is an error with a stable code, the failing setting and a
// suggested fix.
type CodedError struct {
	Code string
	// Path is the configured path ("path") of the source or destination.
	Path string
	Msg  string
	Fix  string
	Err  error
}

func (e *CodedError) Error() string {
	s := fmt.Sprintf("[%s] %s (path %q)", e.Code, e.Msg, e.Path)
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	if e.Fix != "" {
		s += ". Fix: " + e.Fix
	}
	return s
}

func (e *CodedError) Unwrap() error { return e.Err }
