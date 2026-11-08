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
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)

func f(v float64) *float64 { return &v }

func TestLimitsFrom(t *testing.T) {
	l := LimitsFrom(map[string]float64{"autovacuum_freeze_max_age": 1.6e9}, map[string]string{"autovacuum_multixact_freeze_max_age": "500000000", "vacuum_failsafe_age": "bad"})
	if l.FreezeMaxAge != 1.6e9 || l.Source["autovacuum_freeze_max_age"] != "instance" {
		t.Fatalf("%+v", l)
	}
	// vacuum_failsafe_age is raised to 105% of autovacuum_freeze_max_age.
	if l.FailsafeAge != 1.05*1.6e9 {
		t.Fatalf("failsafe = %v", l.FailsafeAge)
	}
	if l.MultixactFreezeMaxAge != 5e8 || l.Source["autovacuum_multixact_freeze_max_age"] != "spec" {
		t.Fatalf("%+v", l)
	}
	if got := strings.Join(l.Assumed(), ","); got != "vacuum_failsafe_age,vacuum_multixact_failsafe_age" {
		t.Fatalf("assumed = %s", got)
	}
}

func TestForecast(t *testing.T) {
	l := LimitsFrom(nil, nil)
	rate := 1000.0 // transactions per second
	fc := ForecastXID(150e6, l, 0, &rate)
	if fc.Autovacuum.Past || fc.Autovacuum.In == nil || *fc.Autovacuum.In != 50_000*time.Second {
		t.Fatalf("autovacuum = %+v", fc.Autovacuum)
	}
	if fc.Stop.Threshold != StopLimitAge || StopLimitAge != 2_144_483_647 {
		t.Fatalf("stop = %v", fc.Stop.Threshold)
	}
	// A table's own lower freeze age wins; a higher one is ignored, as
	// PostgreSQL ignores it.
	if got := ForecastXID(1, l, 1e8, nil).Autovacuum.Threshold; got != 1e8 {
		t.Fatalf("table setting = %v", got)
	}
	if got := ForecastXID(1, l, 9e8, nil).Autovacuum.Threshold; got != DefaultFreezeMaxAge {
		t.Fatalf("higher table setting = %v", got)
	}
	past := ForecastXID(2.2e9, l, 0, &rate)
	if !past.Stop.Past || past.Stop.In != nil {
		t.Fatalf("past = %+v", past.Stop)
	}
	if ForecastXID(1, l, 0, nil).Stop.In != nil {
		t.Fatal("no rate, no time")
	}
	zero := 0.0
	if ForecastXID(1, l, 0, &zero).Stop.In != nil {
		t.Fatal("a zero rate never reaches the limit")
	}
}

func primaryReading(next float64, dbs ...Database) Reading {
	return Reading{Version: "1", NextXID: &next, Databases: dbs}
}

func TestXIDRate(t *testing.T) {
	tr := NewTracker()
	if s := tr.Snapshot(t0); s.XIDRate != nil || s.Present() {
		t.Fatal("empty tracker has no rate")
	}
	for i := 0; i <= 90; i++ { // 90 minutes, 600 xid/min
		tr.Observe("db-1", t0.Add(time.Duration(i)*time.Minute), primaryReading(1000+float64(i)*600), true)
	}
	s := tr.Snapshot(t0.Add(90 * time.Minute))
	if s.XIDRate == nil || math.Abs(s.XIDRate.PerSecond-10) > 1e-9 || s.XIDRate.Window != RateWindow {
		t.Fatalf("rate = %+v", s.XIDRate)
	}
	if s.Primary != "db-1" {
		t.Fatalf("primary = %q", s.Primary)
	}
	// A replica's reading feeds no history.
	tr.Observe("db-2", t0.Add(91*time.Minute), primaryReading(1), false)
	if s := tr.Snapshot(t0.Add(91 * time.Minute)); s.XIDRate == nil || s.Primary != "db-1" {
		t.Fatal("a replica must not reset the counter history")
	}
	// The counter going back (a restore) drops the history.
	tr.Observe("db-1", t0.Add(92*time.Minute), primaryReading(5), true)
	if s := tr.Snapshot(t0.Add(92 * time.Minute)); s.XIDRate != nil {
		t.Fatal("history must restart after the counter went back")
	}
}

func TestXIDRateNeedsAWindow(t *testing.T) {
	tr := NewTracker()
	tr.Observe("db-1", t0, primaryReading(100), true)
	tr.Observe("db-1", t0.Add(2*time.Minute), primaryReading(200), true)
	if s := tr.Snapshot(t0.Add(2 * time.Minute)); s.XIDRate != nil {
		t.Fatal("two minutes of samples is too short to extrapolate weeks from")
	}
}

func TestMXIDRate(t *testing.T) {
	tr := NewTracker()
	for i := 0; i <= 30; i++ {
		at := t0.Add(time.Duration(i) * time.Minute)
		minA := 10.0
		if i == 20 {
			minA = 11 // app was frozen at minute 20: its window breaks
		}
		if i > 20 {
			minA = 11
		}
		tr.Observe("db-1", at, primaryReading(1,
			Database{Name: "app", MXIDAge: f(float64(i) * 60), MinMXID: f(minA)},
			Database{Name: "other", MXIDAge: f(float64(i) * 120), MinMXID: f(3)},
		), true)
	}
	s := tr.Snapshot(t0.Add(30 * time.Minute))
	if s.MXIDRate == nil || s.MXIDRate.PerSecond != 2 || s.MXIDRate.Window != 30*time.Minute {
		t.Fatalf("mxid rate = %+v (the database whose datminmxid did not move wins)", s.MXIDRate)
	}
}

func table(dead, threshold, runs float64) Table {
	return Table{Database: "app", Schema: "public", Name: "orders", Kind: "table", DeadTuples: f(dead), AutovacuumThreshold: f(threshold), AutovacuumCount: f(runs), AutovacuumEnabled: f(1)}
}

func TestKeepUp(t *testing.T) {
	cases := []struct {
		name   string
		dead   []float64
		runs   []float64
		thresh float64
		want   KeepUp
	}{
		{"growing past threshold, no run", []float64{100, 200, 300, 400}, []float64{5, 5, 5, 5}, 50, KeepUpNotStarted},
		{"growing although autovacuum ran", []float64{100, 200, 150, 400}, []float64{5, 5, 6, 6}, 50, KeepUpFallingBehind},
		{"growing under threshold", []float64{1, 2, 3, 4}, []float64{0, 0, 0, 0}, 50, KeepUpOK},
		{"sawtooth", []float64{100, 10, 120, 15}, []float64{1, 2, 3, 4}, 50, KeepUpOK},
		{"too few samples", []float64{100, 400}, []float64{1, 1}, 50, KeepUpUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := NewTracker()
			var last time.Time
			for i := range tc.dead {
				last = t0.Add(time.Duration(i) * 10 * time.Minute)
				tr.Observe("db-1", last, Reading{Version: "1", Tables: []Table{table(tc.dead[i], tc.thresh, tc.runs[i])}}, true)
			}
			got := tr.Snapshot(last).Trends["app\x00public\x00orders"]
			if got.Verdict != tc.want {
				t.Fatalf("verdict = %s, want %s (%+v)", got.Verdict, tc.want, got)
			}
		})
	}
	t.Run("disabled", func(t *testing.T) {
		tr := NewTracker()
		tb := table(100, 50, 0)
		tb.AutovacuumEnabled = f(0)
		tr.Observe("db-1", t0, Reading{Version: "1", Tables: []Table{tb}}, true)
		if got := tr.Snapshot(t0).Trends[tb.Key()].Verdict; got != KeepUpDisabled {
			t.Fatalf("verdict = %s", got)
		}
	})
	t.Run("temporary", func(t *testing.T) {
		tr := NewTracker()
		tb := table(100, 50, 0)
		tb.Kind = "temporary"
		tr.Observe("db-1", t0, Reading{Version: "1", Tables: []Table{tb}}, true)
		if got := tr.Snapshot(t0).Trends[tb.Key()].Verdict; got != KeepUpNever {
			t.Fatalf("verdict = %s", got)
		}
	})
}

func stmts(reset float64, list ...Statement) Reading {
	return Reading{Version: "1", Statements: list, StatementsInfo: &StatementsInfo{StatsResetUnix: f(reset)}}
}

func stmt(id string, calls, total float64) Statement {
	return Statement{QueryID: id, Database: "app", Role: "app", Calls: f(calls), TotalMillis: f(total)}
}

// feed observes hourly snapshots for hours, each hour adding calls with
// the given mean, through until.
func feed(tr *Tracker, hours int, mean func(h int) float64) {
	var calls, total float64
	for h := 0; h <= hours; h++ {
		if h > 0 {
			calls += 100
			total += 100 * mean(h)
		}
		tr.Observe("db-1", t0.Add(time.Duration(h)*time.Hour), stmts(1, stmt("42", calls, total), stmt("7", calls, calls*1)), true)
	}
}

func TestRegressions(t *testing.T) {
	tr := NewTracker()
	feed(tr, 26, func(h int) float64 {
		if h == 26 {
			return 30 // the last hour: 3x slower
		}
		return 10
	})
	c := tr.Snapshot(t0.Add(26 * time.Hour)).Statements
	if !c.Comparable || c.Compared != 2 {
		t.Fatalf("comparison = %+v", c)
	}
	if len(c.Regressions) != 1 || c.Regressions[0].QueryID != "42" {
		t.Fatalf("regressions = %+v", c.Regressions)
	}
	r := c.Regressions[0]
	if r.BaselineMean != 10 || r.RecentMean != 30 || r.Ratio() != 3 {
		t.Fatalf("regression = %+v", r)
	}
	if r.BaselineTo.Sub(r.BaselineFrom) != time.Hour || t0.Add(26*time.Hour).Sub(r.BaselineTo) < 24*time.Hour {
		t.Fatalf("baseline window %v..%v", r.BaselineFrom, r.BaselineTo)
	}
}

func TestRegressionsNeedHistory(t *testing.T) {
	tr := NewTracker()
	feed(tr, 5, func(int) float64 { return 10 })
	c := tr.Snapshot(t0.Add(5 * time.Hour)).Statements
	if c.Comparable || !strings.Contains(c.Reason, "needs 25 hours") || !strings.Contains(c.Reason, "kept 5 h 00 min") {
		t.Fatalf("comparison = %+v", c)
	}
}

func TestRegressionsAfterResetOrFailover(t *testing.T) {
	t.Run("reset", func(t *testing.T) {
		tr := NewTracker()
		feed(tr, 25, func(int) float64 { return 10 })
		tr.Observe("db-1", t0.Add(26*time.Hour), stmts(2, stmt("42", 5, 5)), true)
		if c := tr.Snapshot(t0.Add(26 * time.Hour)).Statements; c.Comparable || !strings.Contains(c.Reason, "reset") {
			t.Fatalf("comparison = %+v", c)
		}
	})
	t.Run("failover", func(t *testing.T) {
		tr := NewTracker()
		feed(tr, 25, func(int) float64 { return 10 })
		tr.Observe("db-2", t0.Add(26*time.Hour), stmts(1, stmt("42", 5, 5)), true)
		if c := tr.Snapshot(t0.Add(26 * time.Hour)).Statements; c.Comparable || !strings.Contains(c.Reason, "primary changed") {
			t.Fatalf("comparison = %+v", c)
		}
	})
}

func TestRetainForgetsInstances(t *testing.T) {
	tr := NewTracker()
	tr.Observe("db-1", t0, Reading{Version: "1"}, true)
	tr.Observe("db-2", t0, Reading{Version: "1"}, false)
	tr.Retain(map[string]bool{"db-2": true})
	s := tr.Snapshot(t0)
	if _, ok := s.Instances["db-1"]; ok || s.Primary != "" {
		t.Fatalf("snapshot = %+v", s)
	}
}

func session(pid int64, xact float64, blockedBy ...int64) Session {
	return Session{PID: pid, BlockedBy: blockedBy, XactAgeSeconds: f(xact)}
}

func TestBlockingTree(t *testing.T) {
	t.Run("chain and fan-out", func(t *testing.T) {
		tree := BlockingTree([]Session{session(30, 1, 20), session(20, 5, 10), session(10, 60), session(40, 2, 10), session(50, 300), session(51, 1, 50)})
		if len(tree) != 2 || tree[0].Session.PID != 10 || tree[0].Blocked != 3 || tree[1].Session.PID != 50 {
			t.Fatalf("roots = %+v", tree)
		}
		if tree[0].Waiters[0].Session.PID != 20 || tree[0].Waiters[0].Waiters[0].Session.PID != 30 {
			t.Fatalf("chain = %+v", tree[0].Waiters)
		}
	})
	t.Run("blocker outside the sample", func(t *testing.T) {
		tree := BlockingTree([]Session{session(30, 1, 99)})
		if len(tree) != 1 || !tree[0].Missing || tree[0].Session.PID != 99 || tree[0].Blocked != 1 {
			t.Fatalf("tree = %+v", tree)
		}
	})
	t.Run("cycle", func(t *testing.T) {
		tree := BlockingTree([]Session{session(1, 1, 2), session(2, 1, 1)})
		if len(tree) != 1 || tree[0].Session.PID != 1 || !tree[0].Waiters[0].Waiters[0].Cycle {
			t.Fatalf("tree = %+v", tree)
		}
	})
	t.Run("two blockers", func(t *testing.T) {
		tree := BlockingTree([]Session{session(3, 1, 1, 2), session(1, 9), session(2, 8)})
		if len(tree) != 2 || tree[0].Waiters[0].Session.PID != 3 || tree[1].Waiters[0].Session.PID != 3 {
			t.Fatalf("tree = %+v", tree)
		}
	})
	t.Run("prepared transaction", func(t *testing.T) {
		// pg_blocking_pids names a prepared transaction 0; the pack keeps
		// it as a node of its own.
		prepared := Session{PID: 0, BackendType: "prepared transaction"}
		tree := BlockingTree([]Session{session(30, 1, 0), prepared, session(31, 1, 30)})
		if len(tree) != 1 || !tree[0].Session.Prepared() || tree[0].Missing || tree[0].Blocked != 2 {
			t.Fatalf("tree = %+v", tree)
		}
		// Cut from the sample, it is still drawn, from its pid.
		tree = BlockingTree([]Session{session(30, 1, 0)})
		if len(tree) != 1 || !tree[0].Missing || !tree[0].Session.Prepared() {
			t.Fatalf("tree = %+v", tree)
		}
	})
	t.Run("nobody blocked", func(t *testing.T) {
		if tree := BlockingTree([]Session{session(1, 1)}); len(tree) != 0 {
			t.Fatalf("tree = %+v", tree)
		}
	})
}

// TestObserveUsesExporterTime: the exporter answers from a cache, so the
// console reads one run several times; samples carry the run's time and
// a cached run adds nothing.
func TestObserveUsesExporterTime(t *testing.T) {
	tr := NewTracker()
	for run := 0; run <= 60; run++ {
		ran := t0.Add(time.Duration(run) * time.Minute)
		r := primaryReading(1000 + float64(run)*600)
		r.ExporterUpdatedAt = &ran
		for _, read := range []time.Duration{5 * time.Second, 25 * time.Second, 45 * time.Second} {
			tr.Observe("db-1", ran.Add(read), r, true)
		}
	}
	s := tr.Snapshot(t0.Add(60*time.Minute + 45*time.Second))
	if s.XIDRate == nil || s.XIDRate.PerSecond != 10 || s.XIDRate.Window != time.Hour || !s.XIDRate.From.Equal(t0) {
		t.Fatalf("rate = %+v, want 10/s over the exporter's own hour", s.XIDRate)
	}
	p, _ := s.PrimaryReading()
	if !p.DataAt.Equal(t0.Add(time.Hour)) || !p.At.Equal(t0.Add(60*time.Minute+45*time.Second)) {
		t.Fatalf("read at %v, data at %v", p.At, p.DataAt)
	}
	// An exporter clock ahead of the console's is clamped to the read.
	ahead := t0.Add(5 * time.Hour)
	if got := DataTime(Reading{ExporterUpdatedAt: &ahead}, t0); !got.Equal(t0) {
		t.Fatalf("data time = %v", got)
	}
	if got := DataTime(Reading{}, t0); !got.Equal(t0) {
		t.Fatalf("without the exporter's time: %v", got)
	}
}

// observeHours feeds one snapshot an hour from per-hour statement lists.
func observeHours(tr *Tracker, hours int, at func(h int) Reading) {
	for h := 0; h <= hours; h++ {
		tr.Observe("db-1", t0.Add(time.Duration(h)*time.Hour), at(h), true)
	}
}

func TestRegressionWindowsPerStatement(t *testing.T) {
	// 42 runs all along at 10 ms then 30 ms in the last hour; 99 left
	// the top N at hour 25 (the newest recent-window start lacks it) and
	// came back; 5 entered the top N at hour 20, after the baseline.
	tr := NewTracker()
	observeHours(tr, 26, func(h int) Reading {
		mean := 10.0
		if h == 26 {
			mean = 30
		}
		calls := float64(h) * 100
		total := calls * 10
		if h == 26 {
			total = 25*100*10 + 100*mean
		}
		list := []Statement{stmt("42", calls, total)}
		if h != 25 {
			list = append(list, stmt("99", calls, total))
		}
		if h >= 20 {
			list = append(list, stmt("5", calls, total))
		}
		return stmts(1, list...)
	})
	c := tr.Snapshot(t0.Add(26 * time.Hour)).Statements
	if !c.Comparable || c.Compared != 2 || c.NoBaseline != 1 || len(c.Regressions) != 2 {
		t.Fatalf("comparison = %+v", c)
	}
	for _, r := range c.Regressions {
		if r.QueryID == "99" && (!r.RecentFrom.Equal(t0.Add(24*time.Hour)) || r.RecentMean != 20) {
			t.Fatalf("99 = %+v, want its recent window from hour 24, the newest snapshot that has it", r)
		}
		if r.Uncertain {
			t.Fatal("no eviction reported, nothing uncertain")
		}
	}
}

func TestRegressionsRestartedAndEvicted(t *testing.T) {
	tr := NewTracker()
	observeHours(tr, 26, func(h int) Reading {
		calls := float64(h) * 100
		mean := 10.0
		total := calls * mean
		if h == 26 {
			total = 25*100*10 + 100*30
		}
		r := stmts(1, stmt("42", calls, total), stmt("7", calls, calls))
		if h == 26 {
			// 7 was reset on its own (pg_stat_statements_reset(0, 0, id)):
			// its counters went back.
			r.Statements[1] = stmt("7", 3, 3)
		}
		dealloc := 0.0
		if h == 26 {
			dealloc = 4 // entries evicted in the last hour
		}
		r.StatementsInfo.Dealloc = &dealloc
		return r
	})
	c := tr.Snapshot(t0.Add(26 * time.Hour)).Statements
	if c.Restarted != 1 || c.Compared != 1 || c.Evicted == nil || *c.Evicted != 4 {
		t.Fatalf("comparison = %+v", c)
	}
	if len(c.Regressions) != 1 || !c.Regressions[0].Uncertain {
		t.Fatalf("regressions = %+v, want 42 marked uncertain", c.Regressions)
	}
}
