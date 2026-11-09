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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/conduitio/conduit-commons/opencdc"
)

// syncEvent is one call of the destination's sync function: which file, and
// what the committed-length file and the output held on disk at that moment.
type syncEvent struct {
	file      string // "output" or "committed"
	committed int64  // readCommitted at the time of the call
	size      int64  // size of the output at the time of the call
}

func (e syncEvent) String() string {
	return fmt.Sprintf("sync %s (committed %d, output %d bytes)", e.file, e.committed, e.size)
}

// recordSyncs returns a sync function for a destination on path that records
// every call in *events and then fails with fail(file) (nil: syncs for real).
func recordSyncs(t *testing.T, path string, events *[]syncEvent, fail func(file string) error) func(*os.File) error {
	t.Helper()
	return func(f *os.File) error {
		var which string
		switch f.Name() {
		case path:
			which = "output"
		case committedPath(path):
			which = "committed"
		default:
			t.Fatalf("sync of unexpected file %q", f.Name())
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		*events = append(*events, syncEvent{which, readCommitted(t, path), info.Size()})
		if fail != nil {
			if err := fail(which); err != nil {
				return err
			}
		}
		return f.Sync()
	}
}

// TestDestination_WriteSyncsOutputBeforeCommittedLength pins the durable
// order of a Write: fsync the output, then write the committed length, then
// fsync the committed-length file, then return (the ack). After an
// operating-system crash the page cache of the two files reaches the disk
// independently, so any other order can leave a length that covers bytes
// that were lost, or an acknowledged batch only in memory.
func TestDestination_WriteSyncsOutputBeforeCommittedLength(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	var events []syncEvent
	d := &Destination{config: DestinationConfig{Config: Config{Path: path}}}
	d.sync = recordSyncs(t, path, &events, nil)
	d.syncDir = func(dir string) error {
		if dir != filepath.Dir(path) {
			t.Fatalf("synced directory %q, want %q", dir, filepath.Dir(path))
		}
		events = append(events, syncEvent{"dir", readCommitted(t, path), 0})
		return syncDir(dir)
	}
	if err := d.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Teardown(context.Background()) })

	// Open of a new output: the output (empty) is synced while no length is
	// recorded yet, then the length 0 is written and synced, then the
	// directory holding both new files.
	wantOpen := []syncEvent{{"output", -1, 0}, {"committed", 0, 0}, {"dir", 0, 0}}
	assertSyncs(t, "Open", events, wantOpen)

	for i, recs := range [][]opencdc.Record{
		{benchmarkRecord(1)},
		{benchmarkRecord(2), benchmarkRecord(3), benchmarkRecord(4)},
	} {
		before := readCommitted(t, path)
		events = nil
		if n, err := d.Write(context.Background(), recs); err != nil || n != len(recs) {
			t.Fatalf("Write %d = (%d, %v)", i, n, err)
		}
		size := int64(len(readFile(t, path)))
		// The output is synced with all of the batch in it while the length
		// still ends at the previous batch; the committed-length file is
		// synced once it holds the new length; both before Write returned.
		assertSyncs(t, fmt.Sprintf("Write %d", i), events, []syncEvent{
			{"output", before, size},
			{"committed", size, size},
		})
	}
}

// TestDestination_OpenSyncsCutBeforeCommittedLength: the cut on Open follows
// the same order, so the length Open records is never ahead of the output on
// disk.
func TestDestination_OpenSyncsCutBeforeCommittedLength(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	d := openDest(context.Background(), t, path)
	writeAll(t, d, benchmarkRecord(1))
	committed := readCommitted(t, path)
	kill(d)
	appendBytes(t, path, `{"torn":`)

	var events []syncEvent
	d = &Destination{config: DestinationConfig{Config: Config{Path: path}}}
	d.sync = recordSyncs(t, path, &events, nil)
	if err := d.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Teardown(context.Background()) })
	assertSyncs(t, "Open", events, []syncEvent{
		{"output", committed, committed}, // already cut, length not rewritten yet
		{"committed", committed, committed},
	})
}

// A Write whose output or committed length cannot be made durable is not
// acknowledged, and an output that could not be synced is never covered by a
// new length.
func TestDestination_WriteFailsWhenSyncFails(t *testing.T) {
	for _, failing := range []string{"output", "committed"} {
		t.Run(failing, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "out.jsonl")
			var events []syncEvent
			fail := false
			d := &Destination{config: DestinationConfig{Config: Config{Path: path}}}
			d.sync = recordSyncs(t, path, &events, func(file string) error {
				if fail && file == failing {
					return errors.New("injected sync failure")
				}
				return nil
			})
			if err := d.Open(context.Background()); err != nil {
				t.Fatal(err)
			}
			writeAll(t, d, benchmarkRecord(1))
			committed := readCommitted(t, path)

			fail = true
			n, err := d.Write(context.Background(), []opencdc.Record{benchmarkRecord(2)})
			if err == nil || n != 0 || !strings.Contains(err.Error(), "injected sync failure") {
				t.Fatalf("Write = (%d, %v), want (0, the sync error)", n, err)
			}
			if failing == "output" {
				if got := readCommitted(t, path); got != committed {
					t.Fatalf("committed length after a failed output sync = %d, want %d (unchanged)", got, committed)
				}
			}
			fail = false
			_ = d.Teardown(context.Background())
			if got := readCommitted(t, path); got == -1 {
				t.Fatal("Teardown after a failed Write cleared the committed length")
			}

			// The record that was not acknowledged is removed on Open when
			// its length was never written, and delivered again.
			d = newTestDestination(t, path)
			writeAll(t, d, benchmarkRecord(2))
			if failing == "output" {
				assertWholeRecords(t, path, benchmarkRecord(1), benchmarkRecord(2))
			} else {
				// The length reached the page cache, so the record is kept
				// and the redelivery duplicates it: at-least-once.
				assertWholeRecords(t, path, benchmarkRecord(1), benchmarkRecord(2), benchmarkRecord(2))
			}
		})
	}
}

// Open fails, and closes what it opened, when the directory cannot be synced.
func TestDestination_OpenFailsWhenDirectorySyncFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	d := &Destination{config: DestinationConfig{Config: Config{Path: path}}}
	d.syncDir = func(string) error { return errors.New("injected directory sync failure") }
	err := d.Open(context.Background())
	if err == nil || !strings.Contains(err.Error(), "injected directory sync failure") {
		t.Fatalf("Open = %v, want the directory sync error", err)
	}
	if err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown after a failed Open = %v, want nil", err)
	}
}

// After a failed Write the destination refuses to write again until it is
// reopened: the output may end in part of the failed batch, which only Open
// can cut, and a later fsync may succeed although the failed batch's pages
// were dropped.
func TestDestination_WriteAfterFailedWriteIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	fail := false
	var events []syncEvent
	d := &Destination{config: DestinationConfig{Config: Config{Path: path}}}
	d.sync = recordSyncs(t, path, &events, func(file string) error {
		if fail && file == "output" {
			return errors.New("injected sync failure")
		}
		return nil
	})
	if err := d.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeAll(t, d, benchmarkRecord(1))
	committed := readCommitted(t, path)

	fail = true
	if n, err := d.Write(context.Background(), []opencdc.Record{benchmarkRecord(2)}); err == nil || n != 0 {
		t.Fatalf("Write = (%d, %v), want (0, error)", n, err)
	}
	fail = false // the storage "recovers": a retry would now sync fine
	sizeAfterFailure := len(readFile(t, path))
	n, err := d.Write(context.Background(), []opencdc.Record{benchmarkRecord(2)})
	if err == nil || n != 0 || !strings.Contains(err.Error(), "opened again") {
		t.Fatalf("Write after a failed Write = (%d, %v), want (0, an error asking to reopen)", n, err)
	}
	if got := len(readFile(t, path)); got != sizeAfterFailure {
		t.Fatalf("the refused Write changed the output: %d bytes, was %d", got, sizeAfterFailure)
	}
	if got := readCommitted(t, path); got != committed {
		t.Fatalf("committed length = %d, want %d", got, committed)
	}
	_ = d.Teardown(context.Background())

	d = newTestDestination(t, path)
	writeAll(t, d, benchmarkRecord(2))
	assertWholeRecords(t, path, benchmarkRecord(1), benchmarkRecord(2))
}

// The engine may open the same Destination again after a Teardown. That Open
// cuts the failed batch, so it also lifts the refusal: the redelivered batch is
// written once, after the last acknowledged one.
func TestDestination_ReopenSameInstanceAfterFailedWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	fail := false
	var events []syncEvent
	d := &Destination{config: DestinationConfig{Config: Config{Path: path}}}
	d.sync = recordSyncs(t, path, &events, func(file string) error {
		if fail && file == "output" {
			return errors.New("injected sync failure")
		}
		return nil
	})
	if err := d.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeAll(t, d, benchmarkRecord(1))

	fail = true
	if n, err := d.Write(context.Background(), []opencdc.Record{benchmarkRecord(2)}); err == nil || n != 0 {
		t.Fatalf("Write = (%d, %v), want (0, error)", n, err)
	}
	fail = false
	_ = d.Teardown(context.Background())
	// The failed batch is in the output, past the committed length.
	if got, committed := int64(len(readFile(t, path))), readCommitted(t, path); got <= committed {
		t.Fatalf("output is %d bytes, want more than the committed %d (the failed batch)", got, committed)
	}

	if err := d.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Teardown(context.Background()) })
	if n, err := d.Write(context.Background(), []opencdc.Record{benchmarkRecord(2)}); err != nil || n != 1 {
		t.Fatalf("Write after reopening = (%d, %v), want (1, nil)", n, err)
	}
	assertWholeRecords(t, path, benchmarkRecord(1), benchmarkRecord(2))
}

// An Open that fails does not lift the refusal after a failed Write: only an
// Open that cut the output back to its committed length may. The second Open
// here comes without a Teardown and fails, so the handles that hold the
// fragment of the failed batch are still installed.
func TestDestination_FailedOpenKeepsWriteRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	fail := false
	var events []syncEvent
	d := &Destination{config: DestinationConfig{Config: Config{Path: path}}}
	d.sync = recordSyncs(t, path, &events, func(file string) error {
		if fail && file == "output" {
			return errors.New("injected sync failure")
		}
		return nil
	})
	if err := d.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Teardown(context.Background()) })
	writeAll(t, d, benchmarkRecord(1))

	fail = true
	if n, err := d.Write(context.Background(), []opencdc.Record{benchmarkRecord(2)}); err == nil || n != 0 {
		t.Fatalf("Write = (%d, %v), want (0, error)", n, err)
	}
	fail = false

	d.syncDir = func(dir string) error { return &os.PathError{Op: "sync", Path: dir, Err: syscall.EIO} }
	if err := d.Open(context.Background()); err == nil {
		t.Fatal("Open = nil, want an error")
	}
	sizeAfterFailure := len(readFile(t, path))
	n, err := d.Write(context.Background(), []opencdc.Record{benchmarkRecord(2)})
	if err == nil || n != 0 || !strings.Contains(err.Error(), "an earlier write failed") {
		t.Fatalf("Write after a failed Open = (%d, %v), want (0, the earlier-write-failed refusal)", n, err)
	}
	if got := len(readFile(t, path)); got != sizeAfterFailure {
		t.Fatalf("the refused Write changed the output: %d bytes, was %d", got, sizeAfterFailure)
	}
}

// A clean Teardown clears the committed length durably: the committed-length
// file is synced once it is empty, so an older version run after a power loss
// that followed the clean stop is not cut back to a stale length. After a
// failed Write the length is kept, and Teardown neither empties nor syncs it.
func TestDestination_TeardownSyncsClearedCommittedLength(t *testing.T) {
	for _, failedWrite := range []bool{false, true} {
		t.Run(fmt.Sprintf("failedWrite=%v", failedWrite), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "out.jsonl")
			fail := false
			var events []syncEvent
			d := &Destination{config: DestinationConfig{Config: Config{Path: path}}}
			d.sync = recordSyncs(t, path, &events, func(file string) error {
				if fail && file == "output" {
					return errors.New("injected sync failure")
				}
				return nil
			})
			if err := d.Open(context.Background()); err != nil {
				t.Fatal(err)
			}
			writeAll(t, d, benchmarkRecord(1))
			committed := readCommitted(t, path)
			if failedWrite {
				fail = true
				if n, err := d.Write(context.Background(), []opencdc.Record{benchmarkRecord(2)}); err == nil || n != 0 {
					t.Fatalf("Write = (%d, %v), want (0, error)", n, err)
				}
				fail = false
			}
			size := int64(len(readFile(t, path)))

			events = nil
			_ = d.Teardown(context.Background())
			if failedWrite {
				assertSyncs(t, "Teardown after a failed Write", events, nil)
				if got := readCommitted(t, path); got != committed {
					t.Fatalf("committed length = %d, want %d (kept for the next Open)", got, committed)
				}
				return
			}
			assertSyncs(t, "clean Teardown", events, []syncEvent{{"committed", -1, size}})
		})
	}
}

func assertSyncs(t *testing.T, what string, got, want []syncEvent) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s synced\n  %v\nwant\n  %v", what, got, want)
	}
}

// Teardown is idempotent, and a Write after it fails without touching the
// files.
func TestDestination_TeardownTwiceAndWriteAfterTeardown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	d := openDest(context.Background(), t, path)
	writeAll(t, d, benchmarkRecord(1))
	if err := d.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("second Teardown = %v, want nil", err)
	}
	if n, err := d.Write(context.Background(), []opencdc.Record{benchmarkRecord(2)}); err == nil || n != 0 {
		t.Fatalf("Write after Teardown = (%d, %v), want (0, error)", n, err)
	}
	assertWholeRecords(t, path, benchmarkRecord(1))
	if got := readCommitted(t, path); got != -1 {
		t.Fatalf("committed length = %d, want none after a clean Teardown", got)
	}
}

// Open on an open destination is refused without opening anything, so the
// installed handles are neither leaked nor cut under, and they keep working.
func TestDestination_OpenTwiceWithoutTeardown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	opens := 0
	ctx := WithOpener(context.Background(), func(p string, flag int, perm os.FileMode) (*os.File, error) {
		opens++
		return os.OpenFile(p, flag, perm) //nolint:gosec // test
	})
	d := &Destination{config: DestinationConfig{Config: Config{Path: path}}}
	if err := d.Open(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Teardown(ctx) })
	writeAll(t, d, benchmarkRecord(1))

	opens = 0
	if err := d.Open(ctx); err == nil || !strings.Contains(err.Error(), "already open") {
		t.Fatalf("second Open = %v, want an already-open error", err)
	}
	if opens != 0 {
		t.Fatalf("the refused Open opened %d files, want none", opens)
	}
	writeAll(t, d, benchmarkRecord(2))
	if err := d.Teardown(ctx); err != nil {
		t.Fatalf("Teardown = %v, want nil", err)
	}
	assertWholeRecords(t, path, benchmarkRecord(1), benchmarkRecord(2))
	if got := readCommitted(t, path); got != -1 {
		t.Fatalf("committed length = %d, want none after a clean Teardown", got)
	}
}
