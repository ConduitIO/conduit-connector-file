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
	"encoding/json"
	"strings"
	"testing"

	"github.com/conduitio/conduit-commons/opencdc"
	sdk "github.com/conduitio/conduit-connector-sdk"
)

// TestNewlineFreeFormat_MatchesSDKSerializers pins the rule newlineFreeFormat
// relies on to the SDK's encoders, not to the format's name alone: every record
// format the SDK offers whose name ends in "/json" must encode records with
// newlines in every place a record can hold one (strings, keys, raw bytes,
// embedded pretty-printed JSON) without a raw newline. A new or changed
// "*/json" serializer that can emit one fails here, before the fallback on Open
// could cut inside a record.
func TestNewlineFreeFormat_MatchesSDKSerializers(t *testing.T) {
	nl := "line one\nline two\r\n "
	recs := []opencdc.Record{
		{
			Position:  opencdc.Position("pos\n1"),
			Operation: opencdc.OperationUpdate,
			Metadata:  opencdc.Metadata{"meta\nkey": nl},
			Key:       opencdc.RawData(nl),
			Payload: opencdc.Change{
				Before: opencdc.RawData(nl),
				After: opencdc.StructuredData{
					nl:       nl,
					"nested": map[string]any{"list": []any{nl, map[string]any{nl: nl}}},
					"bytes":  []byte(nl),
					"raw":    json.RawMessage("{\n  \"pretty\": [\n    1,\n    2\n  ]\n}"),
				},
			},
		},
		{
			Operation: opencdc.OperationCreate,
			Key:       opencdc.StructuredData{"id\n": nl},
			Payload:   opencdc.Change{After: opencdc.RawData(nl)},
		},
	}

	serializers := (&sdk.DestinationWithRecordFormat{}).DefaultRecordSerializers()
	nJSON := 0
	for _, s := range serializers {
		name := s.Name()
		d := &Destination{}
		d.config.RecordFormat = &name
		newlineFree := d.newlineFreeFormat()
		if newlineFree != strings.HasSuffix(name, "/json") {
			t.Fatalf("newlineFreeFormat(%q) = %v", name, newlineFree)
		}
		if !newlineFree {
			continue
		}
		nJSON++
		s, err := s.Configure("")
		if err != nil {
			t.Fatalf("configure %q: %v", name, err)
		}
		for i, r := range recs {
			b, err := s.Serialize(r)
			if err != nil {
				t.Fatalf("%s: serialize record %d: %v", name, i, err)
			}
			if bytes.ContainsAny(b, "\n") {
				t.Fatalf("%s: record %d encodes with a raw newline, so the */json rule in newlineFreeFormat "+
					"no longer holds:\n%s", name, i, b)
			}
		}
	}
	if nJSON == 0 {
		t.Fatal("the SDK offers no */json record format; the test checks nothing")
	}
	// The default (no format set yet) is opencdc/json.
	if !(&Destination{}).newlineFreeFormat() {
		t.Fatal("newlineFreeFormat() with no format set = false, want true (the default is opencdc/json)")
	}
}
