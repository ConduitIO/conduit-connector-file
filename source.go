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
	"strconv"
	"time"

	"github.com/conduitio/conduit-commons/lang"
	"github.com/conduitio/conduit-commons/opencdc"
	sdk "github.com/conduitio/conduit-connector-sdk"
)

const MetadataFilePath = "file.path"

type SourceConfig struct {
	sdk.DefaultSourceMiddleware

	Config // embed the global config

	// QuietPeriod is opt-in and off (0) by default. By default the last line
	// of the file, when it has no trailing newline yet, is held until its
	// newline is written, the file is rotated or truncated, or the connector is
	// stopped gracefully, so a record is never a piece of a line. Set a duration
	// to also read such a line once the file has not changed for that long, for
	// writers that never terminate their last line.
	QuietPeriod time.Duration `json:"quietPeriod" default:"0"`

	// MaxLineSize is the longest line, in bytes, that is read. A longer line
	// stops the source with the error FILE_LINE_TOO_LONG; a line is never
	// truncated.
	MaxLineSize int64 `json:"maxLineSize" default:"16777216" validate:"gt=0"`

	// RotationSettle is how long a rotated file's size must stay unchanged
	// before the source leaves it for the next file. A writer that still holds
	// the old file open (logrotate renames, then signals the program later)
	// keeps appending to it, and those lines are read first. A file that grows
	// again after the source left it is read again for a while. 0 switches at
	// once.
	RotationSettle time.Duration `json:"rotation.settle" default:"1s"`

	// RotatedPattern is a glob that matches the rotated files of the path, for
	// example "app.log.*" (relative to the directory of path unless it is
	// absolute). Wildcards are only allowed in the last component, so the
	// directory is literal; an embedding engine may refuse it
	// (file.WithDirChecker). With it the source finds the rotation chain itself, orders it
	// by modification time and reads every file in turn: the tail of the file
	// it was reading, the files rotated in between from their first byte, then
	// the current file. Without it the source stops with the error
	// FILE_ROTATION_TAIL_UNREADABLE when it cannot account for a rotated file's
	// unread tail. Compressed files are not read.
	RotatedPattern string `json:"rotatedPattern"`

	// RotationAcceptLoss makes the source continue, with a warning that names
	// the code and the size of what is skipped, when the unread tail of a
	// rotated file cannot be read, instead of stopping.
	RotationAcceptLoss bool `json:"rotation.acceptLoss" default:"false"`
}

func (c SourceConfig) Validate(ctx context.Context) error {
	if c.QuietPeriod < 0 {
		return fmt.Errorf(`config value "quietPeriod" must not be negative, got %v`, c.QuietPeriod)
	}
	if c.RotatedPattern != "" {
		dir, _, err := PatternDir(c.Path, c.RotatedPattern)
		if err != nil {
			return err
		}
		if check := dirCheckerFrom(ctx); check != nil {
			if err := check(dir); err != nil {
				return &CodedError{
					Code: ErrCodeRotatedPatternDenied, Path: c.Path,
					Msg: fmt.Sprintf("rotatedPattern lists the directory %q, which is not allowed", dir),
					Fix: patternDeniedFix,
					Err: err,
				}
			}
		}
	}
	return c.Config.Validate()
}

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
	pos, err := parsePosition(s.config.Path, position)
	if err != nil {
		return err
	}
	if s.config.RotatedPattern != "" {
		// Refused here too, not only in Validate: the engine's policy decides.
		if err := s.config.Validate(ctx); err != nil {
			return err
		}
	}
	var offset int64
	if pos != nil {
		offset = pos.offset
	}

	sdk.Logger(ctx).Info().
		Int64("position", offset).
		Msgf("seeking...")

	// The follower opens the path through the engine's opener when there is
	// one (WithOpener), also whenever it re-opens the file later.
	logger := sdk.Logger(ctx)
	fol := newFollower(s.config.Path, openerFrom(ctx), pos, s.config.QuietPeriod, func(format string, args ...any) {
		logger.Info().Msgf(format, args...)
	}).withMaxLine(s.config.MaxLineSize).withSettle(s.config.RotationSettle).withRotation(rotationOptions{
		pattern:    s.config.RotatedPattern,
		acceptLoss: s.config.RotationAcceptLoss,
		warnf: func(format string, args ...any) {
			logger.Warn().Msgf(format, args...)
		},
		checkDir: dirCheckerFrom(ctx),
		listDir:  dirListerFrom(ctx),
	})
	if err := fol.start(); err != nil {
		return fmt.Errorf("could not follow file: %w", err)
	}
	s.fol = fol
	return nil
}

func (s *Source) Read(ctx context.Context) (opencdc.Record, error) {
	// A stop (cancelled ctx) never emits the unterminated last line: a record
	// is never a piece of a line, so the position stays at the start of that
	// line and the whole line is read after the restart (at-least-once).
	line, num, _, err := s.fol.next(ctx)
	if err != nil {
		return opencdc.Record{}, err
	}
	return sdk.Util.Source.NewRecordCreate(
		opencdc.Position(s.fol.position()),
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
