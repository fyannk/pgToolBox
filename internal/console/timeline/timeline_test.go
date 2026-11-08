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

package timeline

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/cache"
)

var t0 = time.Date(2026, 11, 3, 3, 0, 0, 0, time.UTC)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *clock) Wait(ctx context.Context, _ time.Duration) error {
	<-ctx.Done()
	return ctx.Err()
}

func actions(events []Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, string(e.Kind)+":"+e.Action)
	}
	return out
}

func equal(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestJournalBoundsAndDedup(t *testing.T) {
	c := &clock{now: t0}
	j := New(Limits{MaxEvents: 5, PerCluster: 3}, c)
	for i := range 4 {
		c.advance(time.Minute)
		j.Record(Event{Kind: KindPhase, Action: ActionChanged, At: c.Now(), Namespace: "a", Cluster: "orders", To: string(rune('A' + i))})
	}
	if got := j.Query(Query{Namespace: "a", Cluster: "orders"}); len(got.Events) != 3 || !got.Evicted || got.Events[0].To != "D" {
		t.Fatalf("per-Cluster cap: %+v", got)
	}
	// A keyed event is recorded once.
	b := Event{Kind: KindBackup, Action: ActionCompleted, At: c.Now(), Namespace: "a", Cluster: "billing", Key: "backup/u1/completed"}
	if !j.Record(b) || j.Record(b) {
		t.Fatal("the second record of a key must be refused")
	}
	// Below the horizon of a full scope: dropped, not re-added.
	old := Event{Kind: KindBackup, Action: ActionCompleted, At: t0, Namespace: "a", Cluster: "orders", Key: "backup/u0/completed"}
	if j.Record(old) {
		t.Fatal("a keyed event older than the full scope's oldest must be dropped")
	}
}

func TestQueryScope(t *testing.T) {
	c := &clock{now: t0}
	j := New(Limits{}, c)
	j.Record(Event{Kind: KindConsole, Action: ActionStarted, At: t0})
	j.Record(Event{Kind: KindGap, Action: ActionBegan, At: t0.Add(time.Minute), Namespace: "a", Ref: "pods"})
	j.Record(Event{Kind: KindPhase, Action: ActionChanged, At: t0.Add(2 * time.Minute), Namespace: "a", Cluster: "orders"})
	j.Record(Event{Kind: KindPhase, Action: ActionChanged, At: t0.Add(3 * time.Minute), Namespace: "a", Cluster: "billing"})
	j.Record(Event{Kind: KindPhase, Action: ActionChanged, At: t0.Add(4 * time.Minute), Namespace: "b", Cluster: "orders"})
	got := j.Query(Query{Namespace: "a", Cluster: "orders"})
	equal(t, actions(got.Events), []string{"phase:changed", "gap:began", "console:started"})
	got = j.Query(Query{Namespace: "a", Cluster: "orders", Kinds: map[Kind]bool{KindGap: true}})
	equal(t, actions(got.Events), []string{"gap:began"})
	got = j.Query(Query{From: t0.Add(2 * time.Minute), To: t0.Add(4 * time.Minute)})
	if len(got.Events) != 2 {
		t.Fatalf("window: %+v", got.Events)
	}
	if got := j.Query(Query{Limit: 2}); !got.Truncated || len(got.Events) != 2 {
		t.Fatalf("limit: %+v", got)
	}
}

func facts(phase, primary string) ClusterFacts {
	return ClusterFacts{Namespace: "a", Name: "orders", UID: "c1", Phase: phase, CurrentPrimary: primary}
}

func TestRecorderClusterTransitions(t *testing.T) {
	c := &clock{now: t0}
	j := New(Limits{}, c)
	r := NewRecorder(j, time.Time{})
	r.Observe(facts("Cluster in healthy state", "orders-1"))
	c.advance(time.Minute)
	r.Observe(facts("Failing over", "orders-1"))
	c.advance(time.Minute)
	f := facts("Cluster in healthy state", "orders-2")
	f.CurrentPrimaryTimestamp = c.Now().Add(-30 * time.Second)
	f.Pods = []PodFacts{{Name: "orders-1", UID: "p1", Restarts: 1, LastReason: "OOMKilled", LastExitCode: 137, LastFinishedAt: c.Now().Add(-40 * time.Second)}}
	f.Backups = []BackupFacts{{Name: "b1", UID: "b1", Phase: "failed", StoppedAt: c.Now().Add(-time.Hour), Error: "no space"}}
	r.Observe(f)
	r.Observe(f) // a second identical pass adds nothing
	got := j.Query(Query{Namespace: "a", Cluster: "orders"})
	equal(t, actions(got.Events), []string{
		"phase:changed", "primary:failover", "restart:restarted", "phase:changed",
		"primary:firstObserved", "phase:firstObserved", "console:started",
		"backup:failed", // stopped an hour before: ordered by the operator's time
	})
	if p := got.Events[1]; p.Timing != TimingReported || p.From != "orders-1" || p.To != "orders-2" {
		t.Fatalf("primary change: %+v", p)
	}
	if rs := got.Events[2]; rs.Timing != TimingReported || rs.Detail != "OOMKilled (exit code 137)" {
		t.Fatalf("restart: %+v", rs)
	}
}

func TestRecorderFindings(t *testing.T) {
	c := &clock{now: t0}
	j := New(Limits{}, c)
	r := NewRecorder(j, time.Time{})
	key := cache.ClusterKey{Namespace: "a", Name: "orders"}
	lag := FindingFacts{ID: "f1", Check: "lag", Severity: "warning", Summary: "replica lags"}
	arch := FindingFacts{ID: "f2", Check: "archive", Severity: "high"}
	r.Findings(key, t0, []FindingFacts{lag}, nil)                                                 // first run: first observed
	r.Findings(key, t0.Add(time.Minute), []FindingFacts{lag, arch}, nil)                          // f2 appears
	lag.Severity = "critical"                                                                     //
	r.Findings(key, t0.Add(2*time.Minute), []FindingFacts{lag, arch}, nil)                        // f1 worsens
	r.Findings(key, t0.Add(3*time.Minute), []FindingFacts{lag}, map[string]bool{"archive": true}) // f2's check unavailable: not cleared
	r.Findings(key, t0.Add(4*time.Minute), nil, nil)                                              // both clear
	got := j.Query(Query{Namespace: "a", Cluster: "orders", Kinds: map[Kind]bool{KindFinding: true}})
	equal(t, actions(got.Events), []string{"finding:cleared", "finding:cleared", "finding:worsened", "finding:appeared", "finding:firstObserved"})
}

func TestRecorderSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	c := &clock{now: t0}
	file, err := Open(filepath.Join(dir, "timeline.db"), c, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	j, err := NewPersisted(Limits{}, c, file)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRecorder(j, file.PreviousAlive())
	key := cache.ClusterKey{Namespace: "a", Name: "orders"}
	r.Observe(facts("Cluster in healthy state", "orders-1"))
	r.Findings(key, t0, []FindingFacts{{ID: "f1", Check: "lag", Severity: "warning"}}, nil)
	r.Sources([]SourceFacts{{Namespace: "a", Source: "pods", State: "stale", Reason: "watch lost"}})
	if err := file.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := file.db.Close(); err != nil {
		t.Fatal(err)
	}

	// The console is gone for an hour; meanwhile a switchover happened
	// and the finding cleared.
	c.advance(time.Hour)
	file, err = Open(filepath.Join(dir, "timeline.db"), c, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !file.PreviousAlive().Equal(t0) {
		t.Fatalf("previous life last alive %v, want %v", file.PreviousAlive(), t0)
	}
	j, err = NewPersisted(Limits{}, c, file)
	if err != nil {
		t.Fatal(err)
	}
	r = NewRecorder(j, file.PreviousAlive())
	r.Observe(facts("Cluster in healthy state", "orders-2"))
	r.Findings(key, c.Now(), nil, nil)
	r.Sources([]SourceFacts{{Namespace: "a", Source: "pods", State: "live"}})
	got := j.Query(Query{Namespace: "a", Cluster: "orders", From: t0.Add(time.Minute)})
	equal(t, actions(got.Events), []string{"gap:ended", "finding:cleared", "primary:changed", "console:started"})
	for _, e := range got.Events[1:3] {
		if !e.AfterGap || !e.Since.Equal(t0) {
			t.Fatalf("a change found after the restart must carry the window: %+v", e)
		}
	}
	if start := got.Events[3]; !start.Since.Equal(t0) {
		t.Fatalf("the start must name the previous life's last heartbeat: %+v", start)
	}
	// Nothing about the phase: it did not change while the console was gone.
	if n := len(j.Query(Query{Kinds: map[Kind]bool{KindPhase: true}}).Events); n != 1 {
		t.Fatalf("phase events %d, want 1", n)
	}
}

func TestRecorderRecreatedCluster(t *testing.T) {
	c := &clock{now: t0}
	j := New(Limits{}, c)
	r := NewRecorder(j, time.Time{})
	r.Observe(facts("Cluster in healthy state", "orders-1"))
	f := facts("Setting up primary", "orders-1")
	f.UID = "c2"
	r.Observe(f)
	got := j.Query(Query{Namespace: "a", Cluster: "orders", Kinds: map[Kind]bool{KindPhase: true}})
	equal(t, actions(got.Events), []string{"phase:firstObserved", "phase:firstObserved"})
}

// TestPrimaryCauseIsTheLatestSinceTheLastChange: the cause of a primary
// change is the most recent failover/switchover phase seen since the
// previous change; with none seen, or both, the change is uncertain.
func TestPrimaryCauseIsTheLatestSinceTheLastChange(t *testing.T) {
	c := &clock{now: t0}
	j := New(Limits{}, c)
	r := NewRecorder(j, time.Time{})
	step := func(phase, primary string) {
		c.advance(time.Minute)
		r.Observe(facts(phase, primary))
	}
	step("Cluster in healthy state", "orders-1")
	step("Failing over", "orders-1")
	step("Switchover in progress", "orders-1")
	step("Cluster in healthy state", "orders-2") // latest cause: switchover, but both seen
	step("Cluster in healthy state", "orders-3") // nothing seen since the last change
	step("Failing over", "orders-3")
	step("Cluster in healthy state", "orders-1") // failover only
	got := j.Query(Query{Kinds: map[Kind]bool{KindPrimary: true}})
	equal(t, actions(got.Events), []string{"primary:failover", "primary:changed", "primary:switchover", "primary:firstObserved"})
	if !got.Events[2].Uncertain || !got.Events[1].Uncertain || got.Events[0].Uncertain {
		t.Fatalf("uncertainty: %+v", got.Events)
	}
}

// TestHorizonIsByTime: an operator-dated event older than what a scope
// already evicted is dropped even when its sequence is new.
func TestHorizonIsByTime(t *testing.T) {
	c := &clock{now: t0}
	j := New(Limits{PerCluster: 2}, c)
	rec := func(key string, at time.Time) bool {
		return j.Record(Event{Kind: KindBackup, Action: ActionCompleted, At: at, Namespace: "a", Cluster: "orders", Key: key})
	}
	rec("b/1", t0.Add(10*time.Minute))
	rec("b/2", t0.Add(time.Minute)) // reported late: an older At, a newer Seq
	rec("b/3", t0.Add(20*time.Minute))
	// b/1 was evicted first (oldest Seq); the horizon is its time, 10m.
	if rec("b/4", t0.Add(5*time.Minute)) {
		t.Fatal("an event dated before the evicted horizon must be dropped")
	}
	if !rec("b/5", t0.Add(15*time.Minute)) {
		t.Fatal("an event after the horizon is kept")
	}
	if !j.ScopeEvicted("a", "orders") || j.ScopeEvicted("a", "billing") {
		t.Fatal("ScopeEvicted")
	}
}

// TestPrimingAfterEvictionInventsNothing: a journal that lost a
// Cluster's phase and the appearance of an open finding must not, after
// a restart, report the phase as changed or other open findings as
// having appeared while the console was gone.
func TestPrimingAfterEvictionInventsNothing(t *testing.T) {
	c := &clock{now: t0}
	j := New(Limits{PerCluster: 2}, c)
	r := NewRecorder(j, time.Time{})
	key := cache.ClusterKey{Namespace: "a", Name: "orders"}
	r.Observe(facts("Cluster in healthy state", "orders-1"))
	c.advance(time.Minute)
	r.Findings(key, c.Now(), []FindingFacts{{ID: "f1", Check: "lag", Severity: "warning"}}, nil)
	c.advance(time.Minute)
	r.Findings(key, c.Now(), []FindingFacts{{ID: "f1", Check: "lag", Severity: "warning"}, {ID: "f2", Check: "arch", Severity: "high"}}, nil)
	// The cap (2) evicted the phase and primary events.
	c.advance(time.Hour)
	r = NewRecorder(j, t0.Add(2*time.Minute))
	noGap := func() {
		t.Helper()
		for _, e := range j.All() {
			if e.AfterGap {
				t.Fatalf("invented a change found after the gap: %+v", e)
			}
		}
	}
	scoped := Query{Namespace: "a", Cluster: "orders", Kinds: map[Kind]bool{KindPhase: true, KindPrimary: true, KindFinding: true}, From: t0.Add(time.Hour)}
	r.Observe(facts("Cluster in healthy state", "orders-1"))
	noGap()
	equal(t, actions(j.Query(scoped).Events), []string{"primary:firstObserved", "phase:firstObserved"})
	r.Findings(key, c.Now(), []FindingFacts{{ID: "f1", Check: "lag", Severity: "warning"}, {ID: "f2", Check: "arch", Severity: "high"}, {ID: "f3", Check: "x", Severity: "note"}}, nil)
	noGap()
	// f1 and f2 are still known from the journal; f3 is new, but the scope
	// lost history, so it is only first observed.
	equal(t, actions(j.Query(scoped).Events), []string{"finding:firstObserved", "primary:firstObserved"})
}

// TestPrimedUIDRecognisesARecreatedCluster: after a restart, a Cluster
// with the same name and a new UID starts from nothing.
func TestPrimedUIDRecognisesARecreatedCluster(t *testing.T) {
	c := &clock{now: t0}
	j := New(Limits{}, c)
	r := NewRecorder(j, time.Time{})
	r.Observe(facts("Cluster in healthy state", "orders-1"))
	c.advance(time.Hour)
	r = NewRecorder(j, t0)
	f := facts("Setting up primary", "orders-1")
	f.UID = "c2"
	r.Observe(f)
	got := j.Query(Query{Namespace: "a", Cluster: "orders", From: t0.Add(time.Minute)})
	equal(t, actions(got.Events), []string{"primary:firstObserved", "phase:firstObserved", "console:started"})
}

// TestConsoleGapKeepsFixedText: a console-wide source's reason (any
// error text) never reaches the journal; a namespace's does.
func TestConsoleGapKeepsFixedText(t *testing.T) {
	c := &clock{now: t0}
	j := New(Limits{}, c)
	r := NewRecorder(j, time.Time{})
	r.Sources([]SourceFacts{
		{Source: "diagnostics", State: "unavailable", Reason: "panic: runtime error at 10.0.0.7 with token abc"},
		{Namespace: "a", Source: "pods", State: "forbidden", Reason: "pods is forbidden"},
	})
	for _, e := range j.Query(Query{Kinds: map[Kind]bool{KindGap: true}}).Events {
		switch e.Namespace {
		case "":
			if e.Detail != "could not be read or run" {
				t.Fatalf("console-wide gap detail %q", e.Detail)
			}
		case "a":
			if e.Detail != "pods is forbidden" {
				t.Fatalf("namespace gap detail %q", e.Detail)
			}
		}
	}
}
