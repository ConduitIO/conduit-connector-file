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
	"fmt"
	"io"
	"os"
	"strconv"

	sdk "github.com/conduitio/conduit-connector-sdk"
	"github.com/nxadm/tail"
)

const MetadataFilePath = "file.path"

// Source connector
type Source struct {
	sdk.UnimplementedSource

	tail   *tail.Tail
	config map[string]string
}

func NewSource() sdk.Source {
	return sdk.SourceWithMiddleware(&Source{}, sdk.DefaultSourceMiddleware()...)
}

func (s *Source) Parameters() map[string]sdk.Parameter {
	return map[string]sdk.Parameter{
		"path": {
			Default:     "",
			Description: "the file path from which the file source reads messages",
			Required:    true,
		},
	}
}

func (s *Source) Configure(ctx context.Context, m map[string]string) error {
	err := s.validateConfig(m)
	if err != nil {
		return err
	}
	s.config = m
	return nil
}

func (s *Source) Open(ctx context.Context, position sdk.Position) error {
	return s.seek(ctx, position)
}

func (s *Source) Read(ctx context.Context) (sdk.Record, error) {
	select {
	case line, ok := <-s.tail.Lines:
		if !ok {
			return sdk.Record{}, s.tail.Err()
		}
		return sdk.Util.Source.NewRecordCreate(
			sdk.Position(strconv.FormatInt(line.SeekInfo.Offset, 10)),
			map[string]string{
				MetadataFilePath: s.config[ConfigPath],
			},
			sdk.RawData(strconv.Itoa(line.Num)), // use line number as key
			sdk.RawData(line.Text),              // use line content as payload
		), nil
	case <-ctx.Done():
		return sdk.Record{}, ctx.Err()
	}
}

func (s *Source) Ack(ctx context.Context, position sdk.Position) error {
	return nil // no ack needed
}

func (s *Source) Teardown(ctx context.Context) error {
	if s.tail != nil {
		return s.tail.Stop()
	}
	return nil
}

func (s *Source) seek(ctx context.Context, p sdk.Position) error {
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
		s.config[ConfigPath],
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

func (s *Source) validateConfig(cfg map[string]string) error {
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
