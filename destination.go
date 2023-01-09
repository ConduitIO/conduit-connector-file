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
	"bytes"
	"context"
	"fmt"
	"os"

	sdk "github.com/conduitio/conduit-connector-sdk"
)

// Destination connector
type Destination struct {
	sdk.UnimplementedDestination

	config map[string]string

	buf  bytes.Buffer
	file *os.File
}

func NewDestination() sdk.Destination {
	return sdk.DestinationWithMiddleware(&Destination{}, sdk.DefaultDestinationMiddleware()...)
}

func (d *Destination) Parameters() map[string]sdk.Parameter {
	return map[string]sdk.Parameter{
		"path": {
			Default:     "",
			Description: "the file path where the file destination writes messages",
			Required:    true,
		},
	}
}

func (d *Destination) Configure(ctx context.Context, m map[string]string) error {
	err := d.validateConfig(m)
	if err != nil {
		return err
	}
	d.config = m
	return nil
}

func (d *Destination) Open(ctx context.Context) error {
	file, err := d.openOrCreate(d.config[ConfigPath])
	if err != nil {
		return err
	}

	d.file = file
	return nil
}

func (d *Destination) Write(ctx context.Context, recs []sdk.Record) (int, error) {
	defer d.buf.Reset() // always reset buffer after write
	for _, r := range recs {
		d.buf.Write(r.Bytes())
		d.buf.WriteRune('\n')
	}
	_, err := d.buf.WriteTo(d.file)
	if err != nil {
		return 0, err
	}
	return len(recs), nil
}

func (d *Destination) Teardown(ctx context.Context) error {
	if d.file != nil {
		return d.file.Close()
	}
	return nil
}

func (d *Destination) openOrCreate(path string) (*os.File, error) {
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		file, err := os.Create(path)
		if err != nil {
			return nil, err
		}

		return file, nil
	}
	if err != nil {
		return nil, err
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}

	return file, nil
}

func (d *Destination) validateConfig(cfg map[string]string) error {
	path, ok := cfg[ConfigPath]
	if !ok {
		return requiredConfigErr(ConfigPath)
	}

	// make sure we can stat the file, we don't care if it doesn't exist though
	_, err := os.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf(
			"%q config value %q does not contain a valid path: %w",
			ConfigPath, path, err,
		)
	}

	return nil
}
