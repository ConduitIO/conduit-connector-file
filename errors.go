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

// Stable error codes of the source. They are part of the connector's public
// contract: tooling and agents match on them.
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
	// ErrCodeRotatedPatternDenied: the engine's policy does not allow listing
	// the directory rotatedPattern points into.
	ErrCodeRotatedPatternDenied = "FILE_ROTATED_PATTERN_DENIED"
	// ErrCodeRotatedPatternInvalid: rotatedPattern is not a valid glob, or has
	// a wildcard in its directory part.
	ErrCodeRotatedPatternInvalid = "FILE_ROTATED_PATTERN_INVALID"
)

// CodedError is an error with a stable code, the failing setting and a
// suggested fix.
type CodedError struct {
	Code string
	// Path is the configured source path ("path").
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
