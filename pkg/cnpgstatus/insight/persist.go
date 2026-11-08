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
	"math"
	"sort"
	"time"
)

// Persistence carries the histories only: the transaction id samples,
// the table samples and the hourly statement snapshots, which take
// hours to rebuild (a statement regression needs 25). The latest
// readings are not carried: the first sweep after a restart reads them
// again, and until then the screen says it has none.
//
// The persisted types hold no pointers. A gob encoder flattens a
// pointer and omits a zero value, so a *float64 holding 0 dead tuples
// would come back as "not reported"; each optional value is a value and
// a flag instead.

// PersistedState is one tracker's histories.
type PersistedState struct {
	XID        []PersistedXID
	MXID       []PersistedMXIDSeries
	Tables     []PersistedTableSeries
	Statements []PersistedStatementSnapshot
}

// PersistedXID is one transaction id sample.
type PersistedXID struct {
	At   int64
	Next float64
}

// PersistedMXIDSeries is one database's multixact samples.
type PersistedMXIDSeries struct {
	Database string
	Samples  []PersistedMXID
}

// PersistedMXID is one multixact sample.
type PersistedMXID struct {
	At       int64
	Age, Min float64
}

// PersistedTableSeries is one table's samples.
type PersistedTableSeries struct {
	Key     string
	Samples []PersistedTableSample
}

// PersistedTableSample is one table sample; each Has* says whether
// the value beside it was reported.
type PersistedTableSample struct {
	At                             int64
	Dead, Threshold, Runs          float64
	HasDead, HasThreshold, HasRuns bool
}

// PersistedStatementSnapshot is one hourly pg_stat_statements snapshot.
type PersistedStatementSnapshot struct {
	At         int64
	Instance   string
	Reset      float64
	Dealloc    float64
	Statements []PersistedStatement
}

// PersistedStatement is one statement's counters.
type PersistedStatement struct {
	Key          string
	Calls, Total float64
}

// Export copies the histories out, in a stable order.
func (t *Tracker) Export() PersistedState {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := PersistedState{}
	for _, s := range t.xid {
		out.XID = append(out.XID, PersistedXID{At: s.at.UnixNano(), Next: s.next})
	}
	for _, name := range sortedKeys(t.mxid) {
		series := PersistedMXIDSeries{Database: name}
		for _, s := range t.mxid[name] {
			series.Samples = append(series.Samples, PersistedMXID{At: s.at.UnixNano(), Age: s.age, Min: s.min})
		}
		out.MXID = append(out.MXID, series)
	}
	for _, key := range sortedKeys(t.tables) {
		series := PersistedTableSeries{Key: key}
		for _, s := range t.tables[key] {
			p := PersistedTableSample{At: s.at.UnixNano()}
			p.Dead, p.HasDead = unpointer(s.dead)
			p.Threshold, p.HasThreshold = unpointer(s.threshold)
			p.Runs, p.HasRuns = unpointer(s.runs)
			series.Samples = append(series.Samples, p)
		}
		out.Tables = append(out.Tables, series)
	}
	for _, snap := range t.stmts {
		p := PersistedStatementSnapshot{At: snap.at.UnixNano(), Instance: snap.instance, Reset: snap.reset, Dealloc: snap.dealloc}
		for _, key := range sortedKeys(snap.stmts) {
			c := snap.stmts[key]
			p.Statements = append(p.Statements, PersistedStatement{Key: key, Calls: c.calls, Total: c.total})
		}
		out.Statements = append(out.Statements, p)
	}
	return out
}

// Import replaces the histories with a snapshot's, dropping what the
// tracker would no longer keep at now (two hours of transaction ids,
// one of tables, statementKeep hourly snapshots) and anything stamped
// in the future, past the tracked bounds, or not a finite number: the
// snapshot is a file on a volume, not a trusted source.
func (t *Tracker) Import(state PersistedState, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	xidFrom, tableFrom := now.Add(-xidKeep), now.Add(-tableKeep)
	stmtFrom := now.Add(-statementEvery * statementKeep)
	inWindow := func(at, from time.Time) bool { return !at.Before(from) && !at.After(now) }

	t.xid = t.xid[:0]
	for _, s := range state.XID {
		at := time.Unix(0, s.At).UTC()
		if inWindow(at, xidFrom) && finite(s.Next) && (len(t.xid) == 0 || at.After(t.xid[len(t.xid)-1].at)) {
			t.xid = append(t.xid, xidSample{at: at, next: s.Next})
		}
	}
	t.mxid = map[string][]mxidSample{}
	for _, series := range state.MXID {
		if len(t.mxid) >= maxTracked || series.Database == "" {
			break
		}
		var kept []mxidSample
		for _, s := range series.Samples {
			at := time.Unix(0, s.At).UTC()
			if inWindow(at, xidFrom) && finite(s.Age) && finite(s.Min) && (len(kept) == 0 || at.After(kept[len(kept)-1].at)) {
				kept = append(kept, mxidSample{at: at, age: s.Age, min: s.Min})
			}
		}
		if len(kept) > 0 {
			t.mxid[series.Database] = kept
		}
	}
	t.tables = map[string][]tableSample{}
	for _, series := range state.Tables {
		if len(t.tables) >= maxTracked || series.Key == "" {
			break
		}
		var kept []tableSample
		for _, s := range series.Samples {
			at := time.Unix(0, s.At).UTC()
			if !inWindow(at, tableFrom) || (len(kept) > 0 && !at.After(kept[len(kept)-1].at)) {
				continue
			}
			kept = append(kept, tableSample{
				at: at, dead: repointer(s.Dead, s.HasDead), threshold: repointer(s.Threshold, s.HasThreshold), runs: repointer(s.Runs, s.HasRuns),
			})
		}
		if len(kept) > 0 {
			t.tables[series.Key] = kept
		}
	}
	t.stmts = t.stmts[:0]
	for _, p := range state.Statements {
		at := time.Unix(0, p.At).UTC()
		if !inWindow(at, stmtFrom) || (len(t.stmts) > 0 && !at.After(t.stmts[len(t.stmts)-1].at)) {
			continue
		}
		snap := statementSnapshot{at: at, instance: p.Instance, reset: p.Reset, dealloc: p.Dealloc, stmts: make(map[string]statementCounters, len(p.Statements))}
		for _, s := range p.Statements {
			if finite(s.Calls) && finite(s.Total) {
				snap.stmts[s.Key] = statementCounters{calls: s.Calls, total: s.Total}
			}
		}
		t.stmts = append(t.stmts, snap)
	}
	if len(t.stmts) > statementKeep {
		t.stmts = append(t.stmts[:0], t.stmts[len(t.stmts)-statementKeep:]...)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func unpointer(p *float64) (float64, bool) {
	if p == nil {
		return 0, false
	}
	return *p, true
}

// repointer rebuilds an optional value; a value that is not a finite
// number reads as not reported.
func repointer(v float64, ok bool) *float64 {
	if !ok || !finite(v) {
		return nil
	}
	return &v
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
