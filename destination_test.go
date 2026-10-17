// Copyright © 2026 Meroxa, Inc.
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
	"strings"
	"sync"
	"testing"

	"github.com/conduitio/conduit-commons/opencdc"
)

func newTestDestination(t *testing.T, path string) *Destination {
	t.Helper()
	d := &Destination{config: DestinationConfig{Config: Config{Path: path}}}
	if err := d.Open(context.Background()); err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Teardown(context.Background()); err != nil {
			t.Errorf("teardown: %v", err)
		}
	})
	return d
}

// writeAll is a helper that writes records and fails the test on any error or
// short write.
func writeAll(t *testing.T, d *Destination, recs ...opencdc.Record) {
	t.Helper()
	n, err := d.Write(context.Background(), recs)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(recs) {
		t.Fatalf("write returned %d, want %d", n, len(recs))
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sink file: %v", err)
	}
	return string(data)
}

// TestDestinationWriteGroundTruth asserts the benchmark's ground-truth
// contract: every record becomes exactly one line, byte-identical to
// opencdc.Record.Bytes(), so wc -l on the sink is the record count and the
// bytes match the previous (unbuffered) output for a wide range of record
// shapes, including ones that stress the JSON serializer.
func TestDestinationWriteGroundTruth(t *testing.T) {
	recs := []opencdc.Record{
		// simple structured record
		{
			Position:  opencdc.Position(`{"last_lsn":"0/1C3F380","tx_seq":1}`),
			Operation: opencdc.OperationCreate,
			Metadata:  opencdc.Metadata{"k": "v"},
			Key:       opencdc.StructuredData{"id": 1},
			Payload: opencdc.Change{
				After: opencdc.StructuredData{"id": 1, "name": "alice"},
			},
		},
		// unicode + HTML-sensitive characters + control characters
		{
			Position:  opencdc.Position("position-é"),
			Operation: opencdc.OperationUpdate,
			Metadata:  opencdc.Metadata{"emoji": "🎉", "html": "<script>&\"'</script>"},
			Key:       opencdc.RawData("raw-key"),
			Payload: opencdc.Change{
				Before: opencdc.StructuredData{"note": "line1\nline2\ttab", "f": 1.5},
				After:  opencdc.StructuredData{"note": "更新"},
			},
		},
		// tombstone: nil key, nil payload
		{
			Position:  opencdc.Position("tombstone"),
			Operation: opencdc.OperationDelete,
			Key:       opencdc.StructuredData{"id": 2},
		},
		// raw data payload + empty metadata + empty string values
		{
			Position:  nil,
			Operation: opencdc.OperationSnapshot,
			Metadata:  opencdc.Metadata{},
			Key:       nil,
			Payload: opencdc.Change{
				After: opencdc.RawData(`{"raw":"json"}`),
			},
		},
	}

	d := newTestDestination(t, t.TempDir()+"/out.txt")
	writeAll(t, d, recs...)

	var expected strings.Builder
	for _, r := range recs {
		expected.Write(r.Bytes())
		expected.WriteByte('\n')
	}

	if got := readFile(t, d.config.Path); got != expected.String() {
		t.Fatalf("sink file mismatch:\n got: %q\nwant: %q", got, expected.String())
	}
}

// TestDestinationWriteAppend verifies a reopened destination appends to an
// existing file instead of truncating it (O_APPEND), and that the buffered
// writer does not reorder or drop prior content.
func TestDestinationWriteAppend(t *testing.T) {
	path := t.TempDir() + "/out.txt"

	d := newTestDestination(t, path)
	writeAll(t, d, benchmarkRecord(1))
	first := readFile(t, path)

	// Reopen the same path: prior lines must survive and new lines append.
	d2 := newTestDestination(t, path)
	writeAll(t, d2, benchmarkRecord(2))

	got := readFile(t, path)
	if !strings.HasPrefix(got, first) {
		t.Fatalf("prior content lost on append:\n got: %q\nfirst: %q", got, first)
	}

	var expected strings.Builder
	expected.WriteString(first)
	expected.Write(benchmarkRecord(2).Bytes())
	expected.WriteByte('\n')
	if got != expected.String() {
		t.Fatalf("append mismatch:\n got: %q\nwant: %q", got, expected.String())
	}
}

// TestDestinationWriteEmptyBatch verifies an empty batch writes nothing and
// returns 0 records.
func TestDestinationWriteEmptyBatch(t *testing.T) {
	d := newTestDestination(t, t.TempDir()+"/out.txt")
	n, err := d.Write(context.Background(), nil)
	if err != nil {
		t.Fatalf("write empty: %v", err)
	}
	if n != 0 {
		t.Fatalf("write empty returned %d, want 0", n)
	}
	if got := readFile(t, d.config.Path); got != "" {
		t.Fatalf("empty write produced bytes: %q", got)
	}
}

// TestDestinationWriteOversizedRecord verifies a single record larger than the
// write buffer (which bufio writes straight through to the file) still becomes
// exactly one line.
func TestDestinationWriteOversizedRecord(t *testing.T) {
	big := opencdc.Record{
		Position:  opencdc.Position("big"),
		Operation: opencdc.OperationCreate,
		Key:       opencdc.StructuredData{"id": 1},
		Payload: opencdc.Change{
			After: opencdc.StructuredData{
				"blob": strings.Repeat("x", writerBufferSize*2),
			},
		},
	}

	d := newTestDestination(t, t.TempDir()+"/out.txt")
	writeAll(t, d, big)

	var expected strings.Builder
	expected.Write(big.Bytes())
	expected.WriteByte('\n')
	if got := readFile(t, d.config.Path); got != expected.String() {
		t.Fatalf("oversized record mismatch (len got=%d want=%d)", len(got), expected.Len())
	}
}

// TestDestinationWriteConcurrent verifies the destination is safe under
// concurrent Write calls (bufio.Writer is not concurrency-safe on its own; the
// mutex serializes it). Run with -race.
func TestDestinationWriteConcurrent(t *testing.T) {
	d := newTestDestination(t, t.TempDir()+"/out.txt")

	const workers = 8
	const perWorker = 200
	var (
		wg   sync.WaitGroup
		errs = make(chan error, workers)
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				n, err := d.Write(context.Background(), []opencdc.Record{benchmarkRecord(w*perWorker + i)})
				if err != nil {
					errs <- err
					return
				}
				if n != 1 {
					errs <- fmt.Errorf("write returned %d, want 1", n)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent write failed: %v", err)
	}

	// Every record became exactly one line: total lines == workers*perWorker.
	lines := strings.Count(readFile(t, d.config.Path), "\n")
	if lines != workers*perWorker {
		t.Fatalf("got %d lines, want %d", lines, workers*perWorker)
	}
}

// TestDestinationWriteErrorOnClosedFile verifies a write to an already-closed
// file reports zero written and an error (so the SDK nacks and redelivers the
// batch rather than acking a record that was never durably written).
func TestDestinationWriteErrorOnClosedFile(t *testing.T) {
	path := t.TempDir() + "/out.txt"
	d := &Destination{config: DestinationConfig{Config: Config{Path: path}}}
	if err := d.Open(context.Background()); err != nil {
		t.Fatalf("open: %v", err)
	}
	// Close the file behind the destination's back; teardown will also error,
	// which is expected here, so don't register the strict cleanup.
	if err := d.file.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}
	t.Cleanup(func() { _ = d.Teardown(context.Background()) })

	n, err := d.Write(context.Background(), []opencdc.Record{benchmarkRecord(1)})
	if err == nil {
		t.Fatal("expected write to a closed file to fail")
	}
	if n != 0 {
		t.Fatalf("write returned %d, want 0", n)
	}
}
