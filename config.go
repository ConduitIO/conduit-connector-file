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
	"fmt"
	"os"
)

type Config struct {
	// Path is the file path used by the connector to read/write records.
	Path string `json:"path" validate:"required"`
}

func (c Config) Validate() error {
	// make sure we can stat the file, we don't care if it doesn't exist though
	_, err := os.Stat(c.Path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf(`config value "path" does not contain a valid path: %w`, err)
	}
	return nil
}
