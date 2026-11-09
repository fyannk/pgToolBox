// Copyright 2026 The pgToolBox Authors.
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

package logstream

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// FuzzMatcherObserve feeds arbitrary log lines to the matcher's JSON
// line decoding and asserts its invariants: one observation at most per
// rule and container, the kept line a bounded prefix of what was said,
// a substring rule firing exactly when its needle is in the line, and a
// field rule firing only when the line is a JSON object whose field
// holds the value (never on text that merely quotes it).
func FuzzMatcherObserve(f *testing.F) {
	for _, seed := range []struct{ line, want string }{
		{`{"level":"error","msg":"no free disk space","logger":"instance-manager"}`, "no free disk space"},
		{`{"msg":"x","record":{"error_severity":"FATAL","message":"the database system is shutting down"}}`, "x"},
		{`plain text quoting {"msg":"no free disk space"}`, "no free disk space"},
		{`{"msg":{"nested":"no free disk space"}}`, "no free disk space"},
		{`{"msg":"a"}{"msg":"b"}`, "a"},
		{`[{"msg":"a"}]`, "a"},
		{strings.Repeat("x", 3*maxObservedLine), "x"},
	} {
		f.Add(seed.line, seed.want)
	}
	f.Fuzz(func(t *testing.T, line, want string) {
		if want == "" {
			want = "x"
		}
		const needle = "disk"
		rules := []Rule{
			{ID: "msg-equals", Summary: "s", Fields: []FieldTest{{Path: "msg", Equals: want}}},
			{ID: "severity", Summary: "s", Fields: []FieldTest{{Path: "record.error_severity", Equals: "FATAL"}}},
			{ID: "contains", Summary: "s", Contains: []string{needle}},
		}
		at := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
		m := NewMatcher(rules, 0, nil)
		for range 2 {
			m.Observe(Line{Pod: "orders-1", Container: "postgres", Text: line, At: at})
		}
		var doc map[string]any
		if json.Unmarshal([]byte(line), &doc) != nil {
			doc = nil
		}
		fired := map[string]bool{}
		observations := m.Observations()
		if len(observations) > len(rules) {
			t.Fatalf("%d observations from %d rules", len(observations), len(rules))
		}
		for _, o := range observations {
			if fired[o.RuleID] {
				t.Fatalf("rule %s observed twice on one container", o.RuleID)
			}
			fired[o.RuleID] = true
			if len(o.Line) > maxObservedLine || !strings.HasPrefix(line, o.Line) {
				t.Fatalf("rule %s kept %d bytes that are not a bounded prefix of the line", o.RuleID, len(o.Line))
			}
			if o.Count != 2 {
				t.Fatalf("rule %s counted %d of 2 lines", o.RuleID, o.Count)
			}
		}
		if got, wantFired := fired["contains"], strings.Contains(line, needle); got != wantFired {
			t.Fatalf("substring rule fired %v, want %v", got, wantFired)
		}
		msg, isString := doc["msg"].(string)
		if got, wantFired := fired["msg-equals"], doc != nil && isString && msg == want; got != wantFired {
			t.Fatalf("field rule on msg fired %v, want %v (decoded %v)", got, wantFired, doc != nil)
		}
		record, _ := doc["record"].(map[string]any)
		severity, _ := record["error_severity"].(string)
		if got, wantFired := fired["severity"], severity == "FATAL"; got != wantFired {
			t.Fatalf("nested field rule fired %v, want %v", got, wantFired)
		}
	})
}
