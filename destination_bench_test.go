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
	"testing"

	"github.com/conduitio/conduit-commons/opencdc"
)

// benchmarkRecord builds a record shaped like a typical builtin:postgres CDC
// row (structured key + payload, positional metadata), so the benchmark
// measures the real marshaling + write cost of the parity pipeline rather than
// a synthetic tiny record.
func benchmarkRecord(id int) opencdc.Record {
	return opencdc.Record{
		Position:  opencdc.Position(fmt.Sprintf(`{"last_lsn":"0/1C3F380","tx_commit_lsn":"0/1C3F380","tx_seq":%d}`, id)),
		Operation: opencdc.OperationCreate,
		Metadata: opencdc.Metadata{
			"conduit.source.connector.id": "cdc-parity:pg-src",
			"opencdc.createdAt":           "2026-10-17T09:00:00Z",
		},
		Key: opencdc.StructuredData{"id": id},
		Payload: opencdc.Change{
			After: opencdc.StructuredData{
				"id":        id,
				"name":      "benchmark-row-name-abcdefghijklmnopqrstuvwxyz",
				"email":     "user@example.com",
				"age":       42,
				"is_active": true,
				"balance":   1234.56,
				"created":   "2026-10-17T09:00:00.000Z",
			},
		},
	}
}

// newBenchDestination opens a destination writing into a fresh temporary file.
func newBenchDestination(b *testing.B) (*Destination, string) {
	b.Helper()
	dir := b.TempDir()
	path := dir + "/out.txt"

	d := &Destination{config: DestinationConfig{Config: Config{Path: path}}}
	if err := d.Open(context.Background()); err != nil {
		b.Fatalf("open destination: %v", err)
	}
	b.Cleanup(func() { _ = d.Teardown(context.Background()) })
	return d, path
}

// benchmarkWrite measures the destination Write hot path for a given number of
// records per Write call. batchSize 1 mirrors the engine's default (the SDK
// calls Write with one record per call); larger batches mirror a destination
// configured with sdk.batch.size.
func benchmarkWrite(b *testing.B, batchSize int) {
	b.Helper()
	d, _ := newBenchDestination(b)

	// Pre-build the records and pre-compute their byte size so the timed loop
	// measures only the sink's marshal + write path, not record construction.
	recs := make([]opencdc.Record, batchSize)
	var bytesPerOp int64
	for i := range recs {
		recs[i] = benchmarkRecord(i)
		bytesPerOp += int64(len(recs[i].Bytes()))
	}

	b.ReportAllocs()
	b.SetBytes(bytesPerOp)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n, err := d.Write(context.Background(), recs)
		if err != nil {
			b.Fatalf("write: %v", err)
		}
		if n != len(recs) {
			b.Fatalf("write returned %d, want %d", n, len(recs))
		}
	}
}

func BenchmarkDestinationWrite1(b *testing.B)   { benchmarkWrite(b, 1) }
func BenchmarkDestinationWrite10(b *testing.B)  { benchmarkWrite(b, 10) }
func BenchmarkDestinationWrite100(b *testing.B) { benchmarkWrite(b, 100) }
