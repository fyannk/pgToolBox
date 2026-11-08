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
	"strings"
	"testing"
	"time"
)

func TestImportKeepsOnlyValidObservations(t *testing.T) {
	rules := []Rule{{ID: "panic", Contains: []string{"PANIC"}, Summary: "a PANIC"}}
	now := base.Add(time.Hour)
	m := NewMatcher(rules, 2*time.Hour, func() time.Time { return now })
	ok := Observation{RuleID: "panic", Pod: "p-1", Container: "postgres", Line: "PANIC: x", FirstSeen: base, LastSeen: base.Add(time.Minute), Count: 2}
	with := func(edit func(*Observation)) Observation {
		o := ok
		edit(&o)
		return o
	}
	in := []Observation{
		ok,
		ok, // the same key twice: the first wins
		with(func(o *Observation) { o.Pod = "p-2"; o.Line = strings.Repeat("x", 3*maxObservedLine) }),
		with(func(o *Observation) { o.Pod = "p-3"; o.RuleID = "not-a-rule" }),
		with(func(o *Observation) { o.Pod = "p-4"; o.LastSeen = now.Add(time.Minute) }),
		with(func(o *Observation) { o.Pod = "p-5"; o.LastSeen = base.Add(-time.Minute) }),
		with(func(o *Observation) {
			o.Pod = "p-6"
			o.FirstSeen, o.LastSeen = base.Add(-3*time.Hour), base.Add(-2*time.Hour)
		}),
		with(func(o *Observation) { o.Pod = "" }),
		with(func(o *Observation) { o.Pod = "p-8"; o.Count = 0 }),
	}
	if got := m.Import(in, now); got != 2 {
		t.Fatalf("imported %d, want 2 (the valid one and the long line, cut)", got)
	}
	obs := m.Observations()
	if len(obs) != 2 || obs[0].Pod != "p-1" || obs[0].Summary != "a PANIC" || len(obs[1].Line) != maxObservedLine {
		t.Fatalf("observations = %+v", obs)
	}
	// A restored observation counts on like a live one.
	m.Observe(Line{Pod: "p-1", Container: "postgres", Text: "PANIC: again", At: now})
	if got := m.Observations()[0]; got.Count != 3 || !got.FirstSeen.Equal(base) || !got.LastSeen.Equal(now) {
		t.Fatalf("after a new line: %+v", got)
	}
}
