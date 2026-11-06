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

// Linux only: a SIGKILL stops a buffered write(2) to a regular file at a page
// boundary only on Linux (generic_perform_write checks for a fatal signal
// before each page). Elsewhere the kill never tears a write and the test would
// prove nothing.

//go:build linux

package file

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/conduitio/conduit-commons/opencdc"
)

const (
	envDestKillChild = "FILE_DESTINATION_KILL_CHILD"
	destKillBatch    = 100
)

// killRecord is a record of the size a CDC record has (about 600 bytes), so a
// batch of destKillBatch spans many pages and a kill can tear it.
func killRecord(seq int) opencdc.Record {
	return opencdc.Record{
		Position:  opencdc.Position(fmt.Sprintf("v1:%d:16777234:416544082:7:2f901bcafc0341672d6bb8861391562829b8873341d5af8192c68c136723b3f7", seq*7)),
		Operation: opencdc.OperationCreate,
		Key:       opencdc.RawData(strconv.Itoa(seq)),
		Metadata: opencdc.Metadata{
			"opencdc.readAt":              strconv.FormatInt(time.Now().UnixNano(), 10),
			"conduit.source.connector.id": "kill:src",
			"opencdc.version":             "v1",
		},
		Payload: opencdc.Change{After: opencdc.StructuredData{
			"seq":    seq,
			"items":  []any{map[string]any{"sku": "A", "qty": 1, "tags": []any{"x", "y"}}, map[string]any{"sku": "B", "qty": 2}},
			"nested": map[string]any{"x": 1, "deeper": map[string]any{"y": 2, "list": []any{1, 2, 3}}},
			"blob":   strings.Repeat("b", 200),
		}},
	}
}

// TestDestinationKillChildHelper is not a test: re-executed with
// envDestKillChild set to an output path, the test binary writes batches of
// destKillBatch records to it through the destination until it is killed, and
// prints "acked <seq>" after every Write returned (the SDK acks the batch
// then).
func TestDestinationKillChildHelper(t *testing.T) {
	path := os.Getenv(envDestKillChild)
	if path == "" {
		t.Skip("helper process")
	}
	ctx := context.Background()
	d := &Destination{config: DestinationConfig{Config: Config{Path: path}}}
	if err := d.Open(ctx); err != nil {
		t.Fatal(err)
	}
	for seq := 1; ; {
		recs := make([]opencdc.Record, destKillBatch)
		for i := range recs {
			recs[i] = killRecord(seq)
			seq++
		}
		if _, err := d.Write(ctx, recs); err != nil {
			t.Fatal(err)
		}
		fmt.Printf("acked %d\n", seq-1)
	}
}

// TestDestination_SIGKILLThenReopen is the end-to-end regression test for the
// torn tail: a child writes batches through the destination, the parent kills
// it with SIGKILL at a random moment, then reopens the destination on the
// same file (as a restart does) and writes one more record. Every line must
// be a whole record, the records must be 1..k in order with k a whole number
// of batches (the cut is at the committed length) and at least every record
// the child had acknowledged, followed by the new record.
//
// Without the committed length, a kill that tears a batch leaves a partial
// line and the reopened destination appends onto it. The test stops at the
// first failure, after maxKills kills or after budget, whichever comes first.
func TestDestination_SIGKILLThenReopen(t *testing.T) {
	const maxKills = 300
	budget := 60 * time.Second
	if testing.Short() {
		budget = 10 * time.Second
	}
	deadline := time.Now().Add(budget)
	marker := killRecord(1 << 30)

	kills, torn := 0, 0
	for ; kills < maxKills && time.Now().Before(deadline); kills++ {
		path := filepath.Join(t.TempDir(), "out.jsonl")
		var stdout bytes.Buffer
		cmd := exec.CommandContext(context.Background(), os.Args[0], //nolint:gosec // re-executes the test binary
			"-test.run=^TestDestinationKillChildHelper$")
		cmd.Env = append(os.Environ(), envDestKillChild+"="+path)
		cmd.Stdout = &stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		waitForOutput(t, path)
		randSleep(0, 60)
		kill9(t, cmd)

		acked := lastAcked(stdout.String())
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > 0 && raw[len(raw)-1] != '\n' {
			torn++
		}

		// The restart: reopen and deliver one more record.
		d := &Destination{config: DestinationConfig{Config: Config{Path: path}}}
		if err := d.Open(context.Background()); err != nil {
			t.Fatalf("kill %d: reopen: %v", kills, err)
		}
		if n, err := d.Write(context.Background(), []opencdc.Record{marker}); err != nil || n != 1 {
			t.Fatalf("kill %d: write after reopen = (%d, %v)", kills, n, err)
		}
		if err := d.Teardown(context.Background()); err != nil {
			t.Fatal(err)
		}

		checkAfterKill(t, kills, path, acked, len(raw))
	}
	t.Logf("%d kills (batches of %d records), %d left a torn last line; every reopened file held whole records only", kills, destKillBatch, torn)
}

// checkAfterKill asserts the output of one kill-and-reopen round.
func checkAfterKill(t *testing.T, kill int, path string, acked, sizeAtKill int) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	for i, line := range lines {
		var rec struct {
			Payload struct {
				After struct {
					Seq *int `json:"seq"`
				} `json:"after"`
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec.Payload.After.Seq == nil {
			t.Fatalf("kill %d (file was %d bytes, size %% 4096 = %d): line %d of %d is not a whole record (%v): %.120q...",
				kill, sizeAtKill, sizeAtKill%4096, i+1, len(lines), err, line)
		}
		want := i + 1
		if i == len(lines)-1 {
			want = 1 << 30
		}
		if *rec.Payload.After.Seq != want {
			t.Fatalf("kill %d: line %d holds record %d, want %d", kill, i+1, *rec.Payload.After.Seq, want)
		}
	}
	k := len(lines) - 1
	if k%destKillBatch != 0 {
		t.Fatalf("kill %d: %d records kept, not a whole number of batches of %d", kill, k, destKillBatch)
	}
	if k < acked {
		t.Fatalf("kill %d: %d records kept, but %d were acknowledged", kill, k, acked)
	}
}

// waitForOutput waits until the child wrote its first bytes, so the kill lands
// while it is writing rather than while it starts.
func waitForOutput(t *testing.T, path string) {
	t.Helper()
	for start := time.Now(); time.Since(start) < 10*time.Second; time.Sleep(time.Millisecond) {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return
		}
	}
	t.Fatal("the child wrote nothing within 10s")
}

// lastAcked returns the highest record number the child printed as acked. A
// line the kill cut short (the text after the last newline) is ignored.
func lastAcked(out string) int {
	acked := 0
	lines := strings.Split(out, "\n")
	for _, line := range lines[:len(lines)-1] {
		if s, ok := strings.CutPrefix(line, "acked "); ok {
			if n, err := strconv.Atoi(s); err == nil && n > acked {
				acked = n
			}
		}
	}
	return acked
}
