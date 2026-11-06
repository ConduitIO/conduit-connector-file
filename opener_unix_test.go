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

import "os"

// wantCommittedOpen returns the Opener call the destination makes for its
// committed-length file at cpath (on unix it goes through the Opener).
func wantCommittedOpen(cpath string, perm os.FileMode) (openCall, bool) {
	return openCall{cpath, committedOpenFlags, perm}, committedViaOpener
}
