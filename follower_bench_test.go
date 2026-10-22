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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const benchFollowerLines = 500000

func BenchmarkFollowerRead(b *testing.B) {
	p := filepath.Join(b.TempDir(), "f.txt")
	var sb strings.Builder
	for i := range benchFollowerLines {
		fmt.Fprintf(&sb, "2026-10-21T10:00:00Z INFO request id=%d path=/api/v1/items status=200 dur=12ms\n", i)
	}
	if err := os.WriteFile(p, []byte(sb.String()), 0o600); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for range b.N {
		fl := newFollower(p, os.OpenFile, 0)
		if err := fl.start(); err != nil {
			b.Fatal(err)
		}
		for range benchFollowerLines {
			if _, _, _, err := fl.next(context.Background()); err != nil {
				b.Fatal(err)
			}
		}
		_ = fl.close()
	}
	b.ReportMetric(float64(benchFollowerLines)*float64(b.N)/b.Elapsed().Seconds(), "lines/s")
}
