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

//go:generate paramgen -output destination_paramgen.go DestinationConfig

package file

import (
	"context"
	"fmt"
	"github.com/conduitio/conduit-connector-sdk/schema"
	"os"
	"strconv"

	"github.com/conduitio/conduit-commons/config"
	"github.com/conduitio/conduit-commons/opencdc"
	sdk "github.com/conduitio/conduit-connector-sdk"
)

type Destination struct {
	sdk.UnimplementedDestination

	config DestinationConfig

	file *os.File
}

type DestinationConfig struct {
	Config // embed the global config
}

func (c DestinationConfig) Validate() error { return c.Config.Validate() }

func NewDestination() sdk.Destination {
	return sdk.DestinationWithMiddleware(&Destination{}, sdk.DefaultDestinationMiddleware()...)
}

func (d *Destination) Parameters() config.Parameters {
	return d.config.Parameters()
}

func (d *Destination) Configure(ctx context.Context, cfg config.Config) error {
	err := sdk.Util.ParseConfig(ctx, cfg, &d.config, NewDestination().Parameters())
	if err != nil {
		return err
	}
	err = d.config.Validate()
	if err != nil {
		return err
	}
	return nil
}

func (d *Destination) Open(context.Context) error {
	file, err := d.openOrCreate(d.config.Path)
	if err != nil {
		return err
	}

	d.file = file
	return nil
}

func (d *Destination) Write(ctx context.Context, recs []opencdc.Record) (int, error) {
	version, err := strconv.ParseInt(recs[0].Metadata[opencdc.MetadataPayloadSchemaVersion], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid schema version: %w", err)
	}

	recSchema, err := schema.Get(
		ctx,
		recs[0].Metadata[opencdc.MetadataPayloadSchemaSubject],
		int(version),
	)
	if err != nil {
		return 0, fmt.Errorf("invalid schema: %w", err)
	}

	sdk.Logger(ctx).Info().Any("schema", recSchema).Msg("got schema")
	for i, r := range recs {
		_, err := d.file.Write(append(r.Bytes(), '\n'))
		if err != nil {
			return i, err
		}
	}
	return len(recs), nil
}

func (d *Destination) Teardown(context.Context) error {
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
