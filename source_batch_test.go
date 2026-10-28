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
	"math/rand"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/conduitio/conduit-commons/config"
	"github.com/conduitio/conduit-commons/opencdc"
	sdk "github.com/conduitio/conduit-connector-sdk"
)

// readOne reads like the SDK adapter: ReadN, falling back to Read when the
// source does not implement it.
func readOne(ctx context.Context, src sdk.Source) ([]opencdc.Record, error) {
	got, err := src.ReadN(ctx, 1)
	if errors.Is(err, sdk.ErrUnimplemented) {
		var rec opencdc.Record
		if rec, err = src.Read(ctx); err == nil {
			got = []opencdc.Record{rec}
		}
	}
	return got, err
}

// offsetOf extracts the offset of a v1 position.
func offsetOf(t *testing.T, path string, pos opencdc.Position) int64 {
	t.Helper()
	p, err := parsePosition(path, pos)
	if err != nil || p == nil {
		t.Fatalf("position %q: %v", pos, err)
	}
	return p.offset
}

// checkCoverage asserts the records tile the file contiguously from start: each
// payload is exactly the bytes between consecutive offsets (minus the newline).
// It returns the offset after the last record.
func checkCoverage(t *testing.T, path, content string, start int64, recs []opencdc.Record) int64 {
	t.Helper()
	at := start
	for _, r := range recs {
		end := offsetOf(t, path, r.Position)
		if end <= at || end > int64(len(content)) {
			t.Fatalf("record %q ends at %d after %d (file has %d bytes): gap, repeat or out of range", r.Payload.After.Bytes(), end, at, len(content))
		}
		if got, want := string(r.Payload.After.Bytes()), strings.TrimSuffix(content[at:end], "\n"); got != want {
			t.Fatalf("record payload %q, bytes %d..%d are %q", got, at, end, want)
		}
		at = end
	}
	return at
}

// TestSource_GracefulStopThroughBatchMiddleware drives the real SDK middleware
// stack with sdk.batch.* enabled and stops it the way the SDK adapter's Stop
// does (the open and read contexts are cancelled) at random moments. Whether the
// flushed tail record reaches the caller or not, nothing may be lost or
// delivered twice: what was delivered tiles the file from the start, and a
// restart from the last delivered position tiles it to the end.
func TestSource_GracefulStopThroughBatchMiddleware(t *testing.T) {
	t.Run("batching", func(t *testing.T) {
		gracefulStopThroughMiddleware(t, map[string]string{"sdk.batch.size": "10", "sdk.batch.delay": "150ms"})
	})
	t.Run("no batching", func(t *testing.T) {
		gracefulStopThroughMiddleware(t, map[string]string{"sdk.batch.size": "0", "sdk.batch.delay": "0"})
	})
	// The shipped defaults (SDK v0.18.0: size 100, delay 0, adaptive drain).
	t.Run("default batching", func(t *testing.T) { gracefulStopThroughMiddleware(t, nil) })
}

// gracefulStopThroughMiddleware runs the scenario with the given sdk.batch.*
// settings; nil leaves them to the spec defaults.
func gracefulStopThroughMiddleware(t *testing.T, batch map[string]string) {
	t.Helper()
	const content = "a\nb\nunterminated-tail"
	delivered := map[int]int{} // number of records delivered before the stop -> runs
	for i := range 20 {
		p := filepath.Join(t.TempDir(), "f.txt")
		writeFile(t, p, content)

		src := NewSource()
		// newCfg returns a fresh config each time: sdk.Util.ParseConfig fills
		// the defaults into the map it is given, and a second parse of that
		// map would see them as explicit settings (an explicit size without a
		// delay waits for a full batch).
		newCfg := func(quiet string) config.Config {
			cfg := config.Config{"path": p, "quietPeriod": quiet}
			for k, v := range batch {
				cfg[k] = v
			}
			return cfg
		}
		cfg := newCfg("1h") // only the stop can flush the tail
		base := context.Background()
		if err := sdk.Util.ParseConfig(base, cfg, src.Config(), Connector.NewSpecification().SourceParams); err != nil {
			t.Fatal(err)
		}
		openCtx, openCancel := context.WithCancel(base)
		if err := src.Open(openCtx, nil); err != nil {
			t.Fatal(err)
		}
		readCtx, readCancel := context.WithCancel(base)

		var (
			mu   sync.Mutex
			recs []opencdc.Record
			wg   sync.WaitGroup
		)
		wg.Add(1)
		go func() { // the adapter's runRead
			defer wg.Done()
			for {
				got, err := readOne(readCtx, src)
				if err != nil {
					return
				}
				mu.Lock()
				recs = append(recs, got...)
				mu.Unlock()
			}
		}()
		time.Sleep(time.Duration(rand.Intn(400)) * time.Millisecond) //nolint:gosec // test
		openCancel()                                                 // Stop: cancel open and read contexts ...
		readCancel()
		wg.Wait() // ... and wait for Read to finish
		_ = src.Teardown(base)
		delivered[len(recs)]++

		end := checkCoverage(t, p, content, 0, recs)

		// Restart from the last delivered position; the writer finishes the line.
		var pos opencdc.Position
		if len(recs) > 0 {
			pos = recs[len(recs)-1].Position
		}
		appendFile(t, p, "-end\n")
		full := content + "-end\n"
		restarted := NewSource()
		cfg = newCfg("0s")
		if err := sdk.Util.ParseConfig(base, cfg, restarted.Config(), Connector.NewSpecification().SourceParams); err != nil {
			t.Fatal(err)
		}
		if err := restarted.Open(base, pos); err != nil {
			t.Fatalf("iteration %d: restart from %q: %v", i, pos, err)
		}
		ctx, cancel := context.WithTimeout(base, 5*time.Second)
		var again []opencdc.Record
		for at := end; at < int64(len(full)); {
			got, err := readOne(ctx, restarted)
			if err != nil {
				t.Fatalf("iteration %d: restart read: %v", i, err)
			}
			again = append(again, got...)
			at = offsetOf(t, p, got[len(got)-1].Position)
		}
		cancel()
		_ = restarted.Teardown(base)
		if got := checkCoverage(t, p, full, end, again); got != int64(len(full)) {
			t.Fatalf("iteration %d: restart ended at %d of %d", i, got, len(full))
		}
	}
	t.Logf("batch=%v: records delivered before the stop, by number of runs: %v", batch, delivered)
	if delivered[3] != 0 {
		t.Fatalf("a stop emitted the unterminated tail: %v", delivered)
	}
}
