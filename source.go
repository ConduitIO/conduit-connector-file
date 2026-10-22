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
	"errors"
	"fmt"
	"strconv"

	"github.com/conduitio/conduit-commons/lang"
	"github.com/conduitio/conduit-commons/opencdc"
	sdk "github.com/conduitio/conduit-connector-sdk"
)

const MetadataFilePath = "file.path"

type SourceConfig struct {
	sdk.DefaultSourceMiddleware

	Config // embed the global config
}

func (c SourceConfig) Validate(context.Context) error { return c.Config.Validate() }

type Source struct {
	sdk.UnimplementedSource

	config SourceConfig
	fol    *follower
}

func NewSource() sdk.Source {
	return sdk.SourceWithMiddleware(
		&Source{
			config: SourceConfig{
				DefaultSourceMiddleware: sdk.DefaultSourceMiddleware{
					SourceWithSchemaExtraction: sdk.SourceWithSchemaExtraction{
						PayloadEnabled: lang.Ptr(false),
						KeyEnabled:     lang.Ptr(false),
					},
				},
			},
		},
	)
}

func (s *Source) Config() sdk.SourceConfig {
	return &s.config
}

func (s *Source) Open(ctx context.Context, position opencdc.Position) error {
	var offset int64
	if position != nil {
		var err error
		offset, err = strconv.ParseInt(string(position), 10, 64)
		if err != nil {
			return fmt.Errorf("invalid position %v, expected a number", position)
		}
	}

	sdk.Logger(ctx).Info().
		Int64("position", offset).
		Msgf("seeking...")

	// The follower opens the path through the engine's opener when there is
	// one (WithOpener), also whenever it re-opens the file later.
	fol := newFollower(s.config.Path, openerFrom(ctx), offset)
	if err := fol.start(); err != nil {
		return fmt.Errorf("could not follow file: %w", err)
	}
	s.fol = fol
	return nil
}

func (s *Source) Read(ctx context.Context) (opencdc.Record, error) {
	line, num, pos, err := s.fol.next(ctx)
	if err != nil {
		if errors.Is(err, errFileGone) {
			return opencdc.Record{}, fmt.Errorf("%q: %w", s.config.Path, err)
		}
		return opencdc.Record{}, err
	}
	return sdk.Util.Source.NewRecordCreate(
		opencdc.Position(strconv.FormatInt(pos, 10)),
		map[string]string{
			MetadataFilePath: s.config.Path,
		},
		opencdc.RawData(strconv.FormatInt(num, 10)), // use line number as key
		opencdc.RawData(line),                       // use line content as payload
	), nil
}

func (s *Source) Ack(context.Context, opencdc.Position) error {
	return nil // no ack needed
}

func (s *Source) Teardown(context.Context) error {
	if s.fol != nil {
		return s.fol.close()
	}
	return nil
}
