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

//go:generate paramgen -output source_paramgen.go SourceConfig

package file

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/conduitio/conduit-commons/config"
	"github.com/conduitio/conduit-commons/lang"
	"github.com/conduitio/conduit-commons/opencdc"
	sdk "github.com/conduitio/conduit-connector-sdk"
	"github.com/nxadm/tail"
)

const MetadataFilePath = "file.path"

type Source struct {
	sdk.UnimplementedSource

	config SourceConfig
	tail   *tail.Tail
}

type SourceConfig struct {
	Config // embed the global config
}

func (c SourceConfig) Validate() error { return c.Config.Validate() }

func NewSource() sdk.Source {
	return sdk.SourceWithMiddleware(
		&Source{},
		sdk.DefaultSourceMiddleware(
			// disable schema extraction by default, because the source produces raw data
			sdk.SourceWithSchemaExtractionConfig{
				PayloadEnabled: lang.Ptr(false),
				KeyEnabled:     lang.Ptr(false),
			}.Apply,
		)...,
	)
}

func (s *Source) Parameters() config.Parameters {
	return s.config.Parameters()
}

func (s *Source) Configure(ctx context.Context, cfg config.Config) error {
	err := sdk.Util.ParseConfig(ctx, cfg, &s.config, NewSource().Parameters())
	if err != nil {
		return err
	}
	err = s.config.Validate()
	if err != nil {
		return err
	}
	return nil
}

func (s *Source) Open(ctx context.Context, position opencdc.Position) error {
	return s.seek(ctx, position)
}

func (s *Source) Read(ctx context.Context) (opencdc.Record, error) {
	select {
	case line, ok := <-s.tail.Lines:
		if !ok {
			return opencdc.Record{}, s.tail.Err()
		}
		return sdk.Util.Source.NewRecordCreate(
			opencdc.Position(strconv.FormatInt(line.SeekInfo.Offset, 10)),
			map[string]string{
				MetadataFilePath: s.config.Path,
			},
			opencdc.RawData(strconv.Itoa(line.Num)), // use line number as key
			opencdc.RawData(line.Text),              // use line content as payload
		), nil
	case <-ctx.Done():
		return opencdc.Record{}, ctx.Err()
	}
}

func (s *Source) Ack(context.Context, opencdc.Position) error {
	return nil // no ack needed
}

func (s *Source) Teardown(context.Context) error {
	if s.tail != nil {
		return s.tail.Stop()
	}
	return nil
}

func (s *Source) seek(ctx context.Context, p opencdc.Position) error {
	var offset int64
	if p != nil {
		var err error
		offset, err = strconv.ParseInt(string(p), 10, 64)
		if err != nil {
			return fmt.Errorf("invalid position %v, expected a number", p)
		}
	}

	sdk.Logger(ctx).Info().
		Int64("position", offset).
		Msgf("seeking...")

	t, err := tail.TailFile(
		s.config.Path,
		tail.Config{
			Follow: true,
			Location: &tail.SeekInfo{
				Offset: offset,
				Whence: io.SeekStart,
			},
			Logger: tail.DiscardingLogger,
		},
	)
	if err != nil {
		return fmt.Errorf("could not tail file: %w", err)
	}

	s.tail = t
	return nil
}
