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
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/conduitio/conduit-commons/opencdc"
	"github.com/rs/zerolog"
)

// logCtx returns a context whose sdk.Logger writes JSON lines to the returned
// buffer.
func logCtx() (context.Context, *bytes.Buffer) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	return logger.WithContext(context.Background()), &buf
}

// warnings returns the warn-level log entries in buf.
func warnings(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if e["level"] == "warn" {
			out = append(out, e)
		}
	}
	return out
}

// openDest opens a destination on path without registering a Teardown, for
// tests that end the destination the way a kill does.
func openDest(ctx context.Context, t *testing.T, path string) *Destination {
	t.Helper()
	d := &Destination{config: DestinationConfig{Config: Config{Path: path}}}
	if err := d.Open(ctx); err != nil {
		t.Fatalf("open: %v", err)
	}
	return d
}

// kill ends d like a SIGKILL: the descriptors close, nothing else runs.
func kill(d *Destination) {
	_ = d.file.Close()
	if d.committed != nil {
		_ = d.committed.Close()
	}
	if d.data != nil {
		_ = d.data.Close()
	}
}

func appendBytes(t *testing.T, path string, b string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(b); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func setCommitted(t *testing.T, path string, n uint64) {
	t.Helper()
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], n)
	if err := os.WriteFile(committedPath(path), b[:], 0o600); err != nil {
		t.Fatal(err)
	}
}

// readCommitted returns the committed length recorded for path, or -1 if
// none is (the file is missing or empty).
func readCommitted(t *testing.T, path string) int64 {
	t.Helper()
	b, err := os.ReadFile(committedPath(path))
	if errors.Is(err, os.ErrNotExist) || len(b) == 0 {
		return -1
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 8 {
		t.Fatalf("committed-length file is %d bytes", len(b))
	}
	return int64(binary.BigEndian.Uint64(b)) //nolint:gosec // test
}

// assertWholeRecords asserts that path holds exactly want, one JSON record per
// line.
func assertWholeRecords(t *testing.T, path string, want ...opencdc.Record) {
	t.Helper()
	var expected strings.Builder
	for _, r := range want {
		expected.Write(r.Bytes())
		expected.WriteByte('\n')
	}
	got := readFile(t, path)
	if got != expected.String() {
		t.Fatalf("output mismatch:\n got: %q\nwant: %q", got, expected.String())
	}
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if !json.Valid([]byte(line)) {
			t.Fatalf("line is not a record: %q", line)
		}
	}
}

// TestDestination_OpenCutsBackToCommittedLength is the deterministic
// regression test for the torn tail: a kill mid-batch leaves part of a batch
// past the committed length. Open must remove it before the redelivered batch
// is appended, so no line is a fragment glued to a record. The fragment has a
// newline of its own (a record format whose output contains a newline, or a
// tear after the first lines of a batch): cutting after the last newline would
// keep it.
func TestDestination_OpenCutsBackToCommittedLength(t *testing.T) {
	cases := map[string]string{
		"partial line":                    `{"position":"cGFy`,
		"complete lines and partial line": string(benchmarkRecord(3).Bytes()) + "\n" + `{"posi`,
		"fragment ending in a newline":    "{\"payload\":\n",
		// A kill after the batch was written but before its length was:
		// the batch was not acknowledged, so removing it is correct.
		"whole unacknowledged batch": string(benchmarkRecord(3).Bytes()) + "\n",
	}
	for name, tail := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "out.jsonl")
			ctx, logs := logCtx()

			d := openDest(ctx, t, path)
			writeAll(t, d, benchmarkRecord(1))
			writeAll(t, d, benchmarkRecord(2))
			kill(d)
			committed := readCommitted(t, path)
			if size := int64(len(readFile(t, path))); committed != size {
				t.Fatalf("committed length %d after the last acknowledged Write, want the file size %d", committed, size)
			}
			appendBytes(t, path, tail)

			d = openDest(ctx, t, path)
			writeAll(t, d, benchmarkRecord(3)) // the redelivery
			if err := d.Teardown(ctx); err != nil {
				t.Fatal(err)
			}

			assertWholeRecords(t, path, benchmarkRecord(1), benchmarkRecord(2), benchmarkRecord(3))
			w := warnings(t, logs)
			if len(w) != 1 {
				t.Fatalf("got %d warnings, want 1: %v", len(w), w)
			}
			if w[0]["path"] != path || w[0]["oldSize"] != float64(committed+int64(len(tail))) ||
				w[0]["newSize"] != float64(committed) || w[0]["bytesRemoved"] != float64(len(tail)) ||
				w[0]["committedLengthRecorded"] != true {
				t.Fatalf("warning = %v", w[0])
			}
		})
	}
}

// An output written by v0.12.0 (or closed cleanly) has no committed length.
// Open then removes what follows the last newline, and records a length.
func TestDestination_OpenWithoutCommittedLength(t *testing.T) {
	cases := map[string]struct {
		content string
		keep    int
	}{
		"partial last line":   {"{\"a\":1}\n{\"b\":2}\n{\"c\"", len("{\"a\":1}\n{\"b\":2}\n")},
		"only a partial line": {"{\"c\"", 0},
		"complete lines":      {"{\"a\":1}\n", len("{\"a\":1}\n")},
		"empty":               {"", 0},
		// Longer than the backwards scan's chunk, so the scan crosses chunks.
		"partial line longer than a chunk": {"{\"a\":1}\n" + strings.Repeat("x", 3*64*1024+7), len("{\"a\":1}\n")},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "out.jsonl")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, logs := logCtx()
			d := openDest(ctx, t, path)
			if got := readCommitted(t, path); got != int64(tc.keep) {
				t.Fatalf("committed length after Open = %d, want %d", got, tc.keep)
			}
			writeAll(t, d, benchmarkRecord(1))
			if err := d.Teardown(ctx); err != nil {
				t.Fatal(err)
			}

			want := tc.content[:tc.keep] + string(benchmarkRecord(1).Bytes()) + "\n"
			if got := readFile(t, path); got != want {
				t.Fatalf("output = %q, want %q", got, want)
			}
			w := warnings(t, logs)
			removed := len(tc.content) - tc.keep
			switch {
			case removed == 0 && len(w) != 0:
				t.Fatalf("warned without removing anything: %v", w)
			case removed > 0 && (len(w) != 1 || w[0]["bytesRemoved"] != float64(removed) || w[0]["committedLengthRecorded"] != false):
				t.Fatalf("warnings = %v, want one with bytesRemoved %d", w, removed)
			}
		})
	}
}

// A committed length longer than the (non-empty) output means the output was
// truncated or replaced outside the connector. Open refuses rather than guess,
// and changes nothing.
func TestDestination_OpenRefusesCommittedLengthMismatch(t *testing.T) {
	cases := map[string]struct {
		content   string
		committed uint64
	}{
		"output shorter than committed":  {"{\"a\":1}\n", 100},
		"committed does not end a line":  {"{\"a\":1}\n{\"b\":2}\n", 3},
		"committed ends in partial line": {"{\"a\":1}\n{\"b\"", 10},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "out.jsonl")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			setCommitted(t, path, tc.committed)

			d := &Destination{config: DestinationConfig{Config: Config{Path: path}}}
			err := d.Open(context.Background())
			var ce *CodedError
			if !errors.As(err, &ce) || ce.Code != ErrCodeCommittedLengthMismatch || ce.Path != path {
				t.Fatalf("Open = %v, want %s", err, ErrCodeCommittedLengthMismatch)
			}
			if !strings.Contains(err.Error(), committedPath(path)) {
				t.Fatalf("error does not name the committed-length file: %v", err)
			}
			if got := readFile(t, path); got != tc.content {
				t.Fatalf("Open changed the output: %q", got)
			}
			if got := readCommitted(t, path); got != int64(tc.committed) { //nolint:gosec // test
				t.Fatalf("Open changed the committed length: %d", got)
			}
			if err := d.Teardown(context.Background()); err != nil {
				t.Fatalf("Teardown after a failed Open: %v", err)
			}
		})
	}
}

func TestDestination_OpenRefusesInvalidCommittedLength(t *testing.T) {
	cases := map[string][]byte{
		"short":        {1, 2, 3},
		"long":         make([]byte, 16),
		"out of range": {0xff, 0, 0, 0, 0, 0, 0, 0},
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "out.jsonl")
			if err := os.WriteFile(committedPath(path), content, 0o600); err != nil {
				t.Fatal(err)
			}
			d := &Destination{config: DestinationConfig{Config: Config{Path: path}}}
			err := d.Open(context.Background())
			var ce *CodedError
			if !errors.As(err, &ce) || ce.Code != ErrCodeCommittedLengthInvalid {
				t.Fatalf("Open = %v, want %s", err, ErrCodeCommittedLengthInvalid)
			}
		})
	}
}

// An output deleted (or emptied) behind a committed length starts again from
// empty, with a warning: there is nothing to keep and nothing to guess.
func TestDestination_OpenAfterOutputDeleted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	ctx, logs := logCtx()
	d := openDest(ctx, t, path)
	writeAll(t, d, benchmarkRecord(1))
	kill(d)
	committed := readCommitted(t, path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	d = openDest(ctx, t, path)
	writeAll(t, d, benchmarkRecord(2))
	if got := readCommitted(t, path); got != int64(len(readFile(t, path))) {
		t.Fatalf("committed length = %d, want the output size", got)
	}
	if err := d.Teardown(ctx); err != nil {
		t.Fatal(err)
	}
	assertWholeRecords(t, path, benchmarkRecord(2))
	if w := warnings(t, logs); len(w) != 1 || w[0]["committedLength"] != float64(committed) {
		t.Fatalf("warnings = %v, want one naming committedLength %d", w, committed)
	}
}

// A clean Teardown clears the committed length (the output ends with the last
// acknowledged batch), so a later run of a version that does not maintain it
// cannot be cut back by a stale length.
func TestDestination_TeardownClearsCommittedLength(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	ctx, logs := logCtx()
	d := openDest(ctx, t, path)
	writeAll(t, d, benchmarkRecord(1))
	if err := d.Teardown(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readCommitted(t, path); got != -1 {
		t.Fatalf("committed length after a clean Teardown = %d, want none", got)
	}

	// An older version appends whole records without updating the length.
	appendBytes(t, path, string(benchmarkRecord(2).Bytes())+"\n")

	d = openDest(ctx, t, path)
	writeAll(t, d, benchmarkRecord(3))
	if err := d.Teardown(ctx); err != nil {
		t.Fatal(err)
	}
	assertWholeRecords(t, path, benchmarkRecord(1), benchmarkRecord(2), benchmarkRecord(3))
	if w := warnings(t, logs); len(w) != 0 {
		t.Fatalf("unexpected warnings: %v", w)
	}
}

// After a failed Write the output may end in part of a batch, so Teardown
// keeps the committed length for the next Open to cut back to.
func TestDestination_TeardownKeepsCommittedLengthAfterFailedWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	d := openDest(context.Background(), t, path)
	writeAll(t, d, benchmarkRecord(1))
	committed := readCommitted(t, path)

	if err := d.file.Close(); err != nil { // the next write fails
		t.Fatal(err)
	}
	if n, err := d.Write(context.Background(), []opencdc.Record{benchmarkRecord(2)}); err == nil || n != 0 {
		t.Fatalf("Write = (%d, %v), want (0, error)", n, err)
	}
	_ = d.Teardown(context.Background())
	if got := readCommitted(t, path); got != committed {
		t.Fatalf("committed length after a failed Write = %d, want %d", got, committed)
	}
}

// A Write whose committed length cannot be recorded is not acknowledged.
func TestDestination_WriteFailsWhenCommittedLengthFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	d := openDest(context.Background(), t, path)
	writeAll(t, d, benchmarkRecord(1))
	if err := d.committed.Close(); err != nil {
		t.Fatal(err)
	}
	n, err := d.Write(context.Background(), []opencdc.Record{benchmarkRecord(2)})
	if err == nil || n != 0 {
		t.Fatalf("Write = (%d, %v), want (0, error)", n, err)
	}
	if !strings.Contains(err.Error(), committedPath(path)) {
		t.Fatalf("error does not name the committed-length file: %v", err)
	}
	_ = d.Teardown(context.Background())

	// The record reached the file but not the committed length: the next
	// Open removes it, and its redelivery is the only copy.
	d = newTestDestination(t, path)
	writeAll(t, d, benchmarkRecord(2))
	assertWholeRecords(t, path, benchmarkRecord(1), benchmarkRecord(2))
}

// Teardown that cannot close the output cleanly keeps the committed length,
// like a failed flush: the next Open must be able to cut back to it.
func TestDestination_TeardownKeepsCommittedLengthAfterCloseError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	var output *os.File
	ctx := WithOpener(context.Background(), func(p string, flag int, perm os.FileMode) (*os.File, error) {
		f, err := os.OpenFile(p, flag, perm) //nolint:gosec // test
		if output == nil {
			output = f // the first open is the output, for appending
		}
		return f, err
	})
	d := openDest(ctx, t, path)
	writeAll(t, d, benchmarkRecord(1))
	committed := readCommitted(t, path)

	// Close the output under the destination: Teardown's Close then fails.
	// The write buffer is empty, so the flush does not touch the file.
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.Teardown(ctx); err == nil {
		t.Fatal("Teardown = nil, want the close error")
	}
	if got := readCommitted(t, path); got != committed {
		t.Fatalf("committed length after a failed close = %d, want %d", got, committed)
	}
}

// Without a committed length (an output of v0.12.0 after a crash), cutting
// after the last newline is only sound when a record cannot contain one. For
// any other record format Open refuses an output that does not end with a
// newline, and changes nothing.
func TestDestination_OpenWithoutCommittedLengthDependsOnFormat(t *testing.T) {
	const torn = "{\"a\":1}\nline one of a record\nline tw"
	cases := map[string]struct {
		format  *string
		content string
		wantErr bool
		keep    int
	}{
		"json format, torn":               {ptr("debezium/json"), torn, false, len("{\"a\":1}\nline one of a record\n")},
		"other format, torn":              {ptr("other"), torn, true, 0},
		"other format, ends in a newline": {ptr("other"), "rec\nord\n", false, len("rec\nord\n")},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "out.txt")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			d := &Destination{config: DestinationConfig{Config: Config{Path: path}}}
			d.config.RecordFormat = tc.format
			err := d.Open(context.Background())
			t.Cleanup(func() { _ = d.Teardown(context.Background()) })
			if tc.wantErr {
				var ce *CodedError
				if !errors.As(err, &ce) || ce.Code != ErrCodeOutputTailUnterminated || ce.Path != path {
					t.Fatalf("Open = %v, want %s", err, ErrCodeOutputTailUnterminated)
				}
				if got := readFile(t, path); got != tc.content {
					t.Fatalf("Open changed the output: %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if got := readFile(t, path); got != tc.content[:tc.keep] {
				t.Fatalf("output = %q, want %q", got, tc.content[:tc.keep])
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }
