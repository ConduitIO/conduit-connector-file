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
// appeared after the path was checked). The default is os.OpenFile.
type Opener func(path string, flag int, perm os.FileMode) (*os.File, error)

type openerKey struct{}

// WithOpener returns a context carrying o. The destination uses it to open its
// output file. A nil o is ignored.
//
// The source is not affected: it follows the file with github.com/nxadm/tail,
// which opens the path itself.
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
