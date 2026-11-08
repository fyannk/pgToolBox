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

package insight

import (
	"bytes"
	"encoding/gob"
	"math"
	"reflect"
	"testing"
	"time"
)

// gobRoundTrip is what the console's snapshot file does to the state.
func gobRoundTrip(t *testing.T, in PersistedState) PersistedState {
	t.Helper()
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(in); err != nil {
		t.Fatal(err)
	}
	var out PersistedState
	if err := gob.NewDecoder(&buf).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPersistRoundTripKeepsTheRegressionHistory(t *testing.T) {
	t.Parallel()
	// 26 hours of statements, the last one 3x slower: the regression a
	// restart used to hide for a whole day survives it.
	tr := NewTracker()
	feed(tr, 26, func(h int) float64 {
		if h == 26 {
			return 30
		}
		return 10
	})
	now := t0.Add(26 * time.Hour)
	restored := NewTracker()
	restored.Import(gobRoundTrip(t, tr.Export()), now)
	// The latest readings are not persisted: the first sweep after the
	// restart reads the primary again.
	restored.Observe("db-1", now, tr.instances["db-1"].Reading, true)
	got := restored.Snapshot(now).Statements
	want := tr.Snapshot(now).Statements
	if !got.Comparable || len(got.Regressions) != 1 || !reflect.DeepEqual(got.Regressions, want.Regressions) {
		t.Fatalf("restored comparison = %+v, want %+v", got, want)
	}
}

func TestPersistKeepsAZeroAsAValue(t *testing.T) {
	t.Parallel()
	// Zero dead tuples is a reading, not an absence: gob would turn a
	// *float64 holding 0 into nil, which the persisted flags prevent.
	tr := NewTracker()
	for i := range 4 {
		tr.Observe("db-1", t0.Add(time.Duration(i)*10*time.Minute), Reading{Version: "1", Tables: []Table{table(0, 50, 0)}}, true)
	}
	now := t0.Add(30 * time.Minute)
	restored := NewTracker()
	restored.Import(gobRoundTrip(t, tr.Export()), now)
	samples := restored.tables["app\x00public\x00orders"]
	if len(samples) != 4 {
		t.Fatalf("samples = %d, want 4", len(samples))
	}
	for _, s := range samples {
		if s.dead == nil || *s.dead != 0 || s.runs == nil || *s.runs != 0 {
			t.Fatalf("sample = %+v, want dead and runs reported as 0", s)
		}
	}
}

func TestPersistDropsWhatTheTrackerNoLongerKeeps(t *testing.T) {
	t.Parallel()
	tr := NewTracker()
	for i := 0; i <= 90; i++ {
		tr.Observe("db-1", t0.Add(time.Duration(i)*time.Minute), primaryReading(1000+float64(i)*600,
			Database{Name: "app", MXIDAge: f(float64(i)), MinMXID: f(1)}), true)
	}
	state := tr.Export()
	// A sample from the future and one that is not a number: a file on a
	// volume is not a trusted source.
	state.XID = append(state.XID, PersistedXID{At: t0.Add(5 * time.Hour).UnixNano(), Next: 1e9}, PersistedXID{At: t0.Add(91 * time.Minute).UnixNano(), Next: math.NaN()})

	// Read back 3 hours after the last sample: past the two-hour window.
	late := NewTracker()
	late.Import(state, t0.Add(90*time.Minute+3*time.Hour))
	if len(late.xid) != 0 || len(late.mxid) != 0 {
		t.Fatalf("xid = %d, mxid = %d samples, want none past the window", len(late.xid), len(late.mxid))
	}
	// Read back 30 minutes later: everything younger than two hours,
	// nothing from the future, nothing that is not a number.
	soon := NewTracker()
	soon.Import(state, t0.Add(2*time.Hour))
	for _, s := range soon.xid {
		if s.at.Before(t0) || s.at.After(t0.Add(2*time.Hour)) || math.IsNaN(s.next) {
			t.Fatalf("kept sample %+v", s)
		}
	}
	if s := soon.Snapshot(t0.Add(2 * time.Hour)); s.XIDRate == nil || math.Abs(s.XIDRate.PerSecond-10) > 1e-9 {
		t.Fatalf("restored rate = %+v, want 10/s", s.XIDRate)
	}
}
