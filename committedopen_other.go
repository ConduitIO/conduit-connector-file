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

//go:build !unix && !windows

package file

import (
	"fmt"
	"os"
	"runtime"
)

const committedViaOpener = false

// openCommittedNoFollow fails closed: this platform has neither O_NOFOLLOW nor
// a handle-based way the connector knows to refuse a link, and opening the
// committed-length file through a link would let it read and write another
// file. A destination writing a regular file therefore cannot be opened here.
func openCommittedNoFollow(_ Opener, cpath string, _ os.FileMode) (*os.File, error) {
	return nil, fmt.Errorf("open committed-length file %q: not supported on %s, which cannot open a file "+
		"without following a symlink; run the destination on a unix system or Windows, or write to a device or FIFO", cpath, runtime.GOOS)
}
