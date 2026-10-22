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

import (
	"context"
	"os"
)

// Opener opens a file for the connector. The engine that embeds the connector
// can supply one through [WithOpener] to enforce its own path policy at the
// moment the file is opened (for example by refusing to follow a symlink that
// appeared after the path was checked). The default is os.OpenFile. The source
// calls it for every open of the path, including the periodic check that the
// path still names the file being followed and the re-open after a truncation.
type Opener func(path string, flag int, perm os.FileMode) (*os.File, error)

type openerKey struct{}

// WithOpener returns a context carrying o. The destination uses it to open its
// output file and the source to open (and re-open) the file it follows. The
// source reads o from the context of Open. A nil o is ignored.
func WithOpener(ctx context.Context, o Opener) context.Context {
	if o == nil {
		return ctx
	}
	return context.WithValue(ctx, openerKey{}, o)
}

func openerFrom(ctx context.Context) Opener {
	if o, ok := ctx.Value(openerKey{}).(Opener); ok {
		return o
	}
	return os.OpenFile
}

// DirChecker tells whether the connector may list a directory (to find rotated
// files). An embedding engine supplies one through [WithDirChecker] to apply its
// path policy to rotatedPattern, the same way [WithOpener] polices opens. It
// returns an error to refuse. Without one, listing is not restricted.
type DirChecker func(dir string) error

type dirCheckerKey struct{}

// WithDirChecker returns a context carrying c. The source calls it with the
// literal directory of rotatedPattern (or, without a pattern, the directory of
// path) before every listing: at configuration, at open, when it looks for the
// file a saved position belongs to, and when it accounts for files between a
// rotated file and the current one. A nil c is ignored.
func WithDirChecker(ctx context.Context, c DirChecker) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, dirCheckerKey{}, c)
}

func dirCheckerFrom(ctx context.Context) DirChecker {
	c, _ := ctx.Value(dirCheckerKey{}).(DirChecker)
	return c
}
