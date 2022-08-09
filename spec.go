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
	sdk "github.com/conduitio/conduit-connector-sdk"
)

func Specification() sdk.Specification {
	return sdk.Specification{
		Name:    "file",
		Summary: "A file source and destination plugin for Conduit.",
		Description: `The file source allows you to listen to a local file and
detect any changes happening to it. Each change will create a new record. The
destination allows you to write record payloads to a destination file, each new
record payload is appended to the file in a new line.`,
		Version: "v0.1.0",
		Author:  "Meroxa, Inc.",
	}
}
