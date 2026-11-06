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

//go:build !unix

package file

import "os"

const openFlags = os.O_RDONLY

// syncDir does nothing here. On Windows os.File.Sync is FlushFileBuffers,
// which needs a handle opened for writing, and os.Open of a directory does not
// give one; NTFS records the creation of a file in its metadata journal
// instead. The output and the committed length themselves are flushed on
// every commit. This path is compile-checked only.
func syncDir(string) error { return nil }
