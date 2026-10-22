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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// fingerprintSize is how many bytes at the start of a file are hashed to
// recognize it again.
const fingerprintSize = 4096

// position is a decoded source position.
//
// Two formats exist. Up to v0.10.x a position was the decimal byte offset just
// past the last line read ("legacy"). From v0.11.0 it is
//
//	v1:<offset>:<dev>:<inode>:<n>:<sha256 of the first n bytes, hex>
//
// where n = min(offset, 4096). Offset semantics are the same in both. The
// extra fields let a restart tell whether the file at the path is still the
// file the offset belongs to. dev and inode are 0 where the platform has none.
// v0.10.x cannot read v1 positions: after a resume with v0.11.0 a downgrade
// needs the pipeline's position reset.
type position struct {
	offset int64
	legacy bool
	dev    uint64
	ino    uint64
	fpLen  int64
	fp     [sha256.Size]byte
}

func (p position) idKnown() bool { return p.dev != 0 || p.ino != 0 }

func (p position) String() string {
	if p.legacy {
		return strconv.FormatInt(p.offset, 10)
	}
	return "v1:" + strconv.FormatInt(p.offset, 10) + posSuffix(p.dev, p.ino, p.fpLen, p.fp)
}

func posSuffix(dev, ino uint64, n int64, fp [sha256.Size]byte) string {
	h := ""
	if n > 0 {
		h = hex.EncodeToString(fp[:])
	}
	return ":" + strconv.FormatUint(dev, 10) + ":" + strconv.FormatUint(ino, 10) + ":" + strconv.FormatInt(n, 10) + ":" + h
}

// parsePosition decodes a saved position. An empty position is the start.
func parsePosition(path string, b []byte) (*position, error) {
	if len(b) == 0 {
		return nil, nil //nolint:nilnil // no position: start of the file
	}
	invalid := func(err error) error {
		return &CodedError{
			Code: ErrCodePositionInvalid, Path: path,
			Msg: fmt.Sprintf("invalid position %q, expected a byte offset or a v1 position", b),
			Fix: "reset the pipeline's position, or restore the position the connector last wrote",
			Err: err,
		}
	}
	s := string(b)
	if !strings.HasPrefix(s, "v1:") {
		off, err := strconv.ParseInt(s, 10, 64)
		if err != nil || off < 0 {
			return nil, invalid(err)
		}
		return &position{offset: off, legacy: true}, nil
	}
	f := strings.Split(s[3:], ":")
	if len(f) != 5 {
		return nil, invalid(nil)
	}
	var p position
	var err error
	if p.offset, err = strconv.ParseInt(f[0], 10, 64); err != nil || p.offset < 0 {
		return nil, invalid(err)
	}
	if p.dev, err = strconv.ParseUint(f[1], 10, 64); err != nil {
		return nil, invalid(err)
	}
	if p.ino, err = strconv.ParseUint(f[2], 10, 64); err != nil {
		return nil, invalid(err)
	}
	if p.fpLen, err = strconv.ParseInt(f[3], 10, 64); err != nil || p.fpLen != min(p.offset, fingerprintSize) {
		return nil, invalid(err)
	}
	if p.fpLen > 0 {
		raw, err := hex.DecodeString(f[4])
		if err != nil || len(raw) != sha256.Size {
			return nil, invalid(err)
		}
		copy(p.fp[:], raw)
	} else if f[4] != "" {
		return nil, invalid(nil)
	}
	return &p, nil
}

// fingerprint hashes the first n bytes of f without moving its offset.
func fingerprint(f *os.File, n int64) ([sha256.Size]byte, error) {
	var out [sha256.Size]byte
	h := sha256.New()
	got, err := io.Copy(h, io.NewSectionReader(f, 0, n))
	if err != nil {
		return out, err
	}
	if got != n {
		return out, io.ErrUnexpectedEOF
	}
	copy(out[:], h.Sum(nil))
	return out, nil
}
