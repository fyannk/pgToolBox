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

package drills

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fyannk/pgToolBox/internal/console/ops"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

var t0 = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

func i64(v int64) *int64 { return &v }

func running() Record {
	return Record{
		Drill: Key{"team-a", "drill-orders-2611021000"}, Source: SourceKey{"team-a", "orders"},
		Backup: "orders-b1", StartedAt: t0, Deadline: t0.Add(2 * time.Hour), SourceBytes: i64(100 << 30),
	}
}

func healthyObs(bytes *int64) Observation {
	return Observation{Phase: cnpgv1.PhaseHealthy, ReadyInstances: 1, Reading: &Reading{IsPrimary: true, TimelineID: 2}, DatabaseBytes: bytes}
}

func outcome(r Record, n viewmodel.DrillCheckName) viewmodel.DrillCheckOutcome {
	for _, c := range r.Checks {
		if c.Name == n {
			return c.Outcome
		}
	}
	return ""
}

func TestJudge(t *testing.T) {
	t.Parallel()
	// Restoring: nothing decided.
	r := Judge(running(), Observation{Phase: "Setting up primary"}, t0.Add(5*time.Minute))
	if r.Result != viewmodel.DrillRunning || outcome(r, viewmodel.DrillCheckHealthy) != viewmodel.DrillCheckPending || !r.HealthyAt.IsZero() {
		t.Fatalf("restoring = %+v", r)
	}
	// Healthy but still in recovery: the promotion is pending.
	r = Judge(r, Observation{Phase: cnpgv1.PhaseHealthy, ReadyInstances: 1, Reading: &Reading{IsPrimary: false}}, t0.Add(14*time.Minute))
	if r.Result != viewmodel.DrillRunning || !r.HealthyAt.Equal(t0.Add(14*time.Minute)) || outcome(r, viewmodel.DrillCheckPromoted) != viewmodel.DrillCheckPending {
		t.Fatalf("in recovery = %+v", r)
	}
	// No reading because the sweep cannot verify the instance: the check
	// says why rather than waiting silently.
	if blind := Judge(r, Observation{Phase: cnpgv1.PhaseHealthy, ReadyInstances: 1, ReadProblem: "status port not verifiable: CA not in the console's bundle"},
		t0.Add(14*time.Minute)); outcome(blind, viewmodel.DrillCheckPromoted) != viewmodel.DrillCheckPending ||
		checkText(blind, viewmodel.DrillCheckPromoted) != "no /pg/status reading of the instance yet: status port not verifiable: CA not in the console's bundle" {
		t.Errorf("unverifiable = %+v", blind.Checks)
	}
	// Promoted, size within tolerance: passed, duration start → healthy.
	r = Judge(r, healthyObs(i64(95<<30)), t0.Add(15*time.Minute))
	if r.Result != viewmodel.DrillPassed || r.Timeline == nil || *r.Timeline != 2 || *r.RestoredBytes != 95<<30 ||
		outcome(r, viewmodel.DrillCheckSize) != viewmodel.DrillCheckPassed || !r.FinishedAt.Equal(t0.Add(15*time.Minute)) {
		t.Fatalf("passed = %+v", r)
	}
	if v := View(r); *v.DurationSeconds != 14*60 || v.Health != viewmodel.HealthOK || v.HealthyAt == nil {
		t.Errorf("view = %+v", v)
	}
	// A finished record is never re-judged.
	if again := Judge(r, Observation{}, t0.Add(3*time.Hour)); again.Result != viewmodel.DrillPassed {
		t.Errorf("re-judged: %+v", again)
	}
}

func TestJudgeSize(t *testing.T) {
	t.Parallel()
	promoted := func(r Record, bytes *int64, at time.Duration) Record {
		r = Judge(r, healthyObs(nil), t0.Add(10*time.Minute))
		return Judge(r, healthyObs(bytes), t0.Add(at))
	}
	tests := []struct {
		name    string
		src     *int64
		got     *int64
		at      time.Duration
		result  viewmodel.DrillResult
		outcome viewmodel.DrillCheckOutcome
	}{
		{"too small", i64(100 << 30), i64(80 << 30), 11 * time.Minute, viewmodel.DrillFailed, viewmodel.DrillCheckFailed},
		{"too large", i64(100 << 30), i64(111 << 30), 11 * time.Minute, viewmodel.DrillFailed, viewmodel.DrillCheckFailed},
		{"at the edge", i64(100 << 30), i64(110 << 30), 11 * time.Minute, viewmodel.DrillPassed, viewmodel.DrillCheckPassed},
		{"no source size", nil, i64(1), 11 * time.Minute, viewmodel.DrillPassed, viewmodel.DrillCheckNotChecked},
		{"exporter late", i64(1 << 30), nil, 12 * time.Minute, viewmodel.DrillRunning, viewmodel.DrillCheckPending},
		{"exporter silent", i64(1 << 30), nil, 16 * time.Minute, viewmodel.DrillPassed, viewmodel.DrillCheckNotChecked},
	}
	for _, tt := range tests {
		r := running()
		r.SourceBytes = tt.src
		r = promoted(r, tt.got, tt.at)
		if r.Result != tt.result || outcome(r, viewmodel.DrillCheckSize) != tt.outcome {
			t.Errorf("%s: result %s, size %s (%+v)", tt.name, r.Result, outcome(r, viewmodel.DrillCheckSize), r.Checks)
		}
		if tt.result == viewmodel.DrillFailed && !strings.HasPrefix(r.Reason, "size: ") {
			t.Errorf("%s: reason %q", tt.name, r.Reason)
		}
	}
}

func TestJudgeTimeLimit(t *testing.T) {
	t.Parallel()
	r := Judge(running(), Observation{Phase: "Setting up primary"}, t0.Add(2*time.Hour))
	if r.Result != viewmodel.DrillTimedOut || outcome(r, viewmodel.DrillCheckHealthy) != viewmodel.DrillCheckFailed ||
		!strings.Contains(r.Reason, "healthy not reached within the time limit (2h") {
		t.Errorf("timed out = %+v", r)
	}
	if View(r).Health != viewmodel.HealthCritical {
		t.Error("timed out is not critical")
	}
}

func TestVanished(t *testing.T) {
	t.Parallel()
	r := Vanished(running(), t0.Add(time.Minute))
	if r.Result != viewmodel.DrillCancelled || r.TornDownAt.IsZero() {
		t.Errorf("vanished running = %+v", r)
	}
	done := Judge(running(), healthyObs(i64(100<<30)), t0.Add(time.Minute))
	done.TeardownError = "x"
	if v := Vanished(done, t0.Add(2*time.Minute)); v.Result != viewmodel.DrillPassed || v.TornDownAt.IsZero() || v.TeardownError != "" {
		t.Errorf("vanished passed = %+v", v)
	}
}

type memPersist struct {
	mu    sync.Mutex
	data  map[string][]byte
	since time.Time
	// clean leaves out the malformed record LoadDrills otherwise adds.
	clean bool
}

func (m *memPersist) DrillsSince(now time.Time) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.since.IsZero() {
		m.since = now
	}
	return m.since, nil
}

func (m *memPersist) LoadDrills() ([][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out [][]byte
	for _, v := range m.data {
		out = append(out, v)
	}
	if m.clean {
		return out, nil
	}
	return append(out, []byte("{not json")), nil
}

func (m *memPersist) PutDrill(k string, v []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[k] = v
	return nil
}

func (m *memPersist) DeleteDrill(k string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, k)
	return nil
}

func TestStorePersistsAndBounds(t *testing.T) {
	t.Parallel()
	p := &memPersist{data: map[string][]byte{}}
	s := NewStore(p, nil, t0)
	for i := range MaxPerSource + 5 {
		r := running()
		r.Drill.Name = "d" + time.Duration(i).String()
		r.StartedAt = t0.Add(time.Duration(i) * time.Hour)
		r.Result, r.FinishedAt = viewmodel.DrillPassed, r.StartedAt.Add(time.Minute)
		s.Put(r)
	}
	still := running()
	still.Drill.Name, still.StartedAt = "old-but-running", t0.Add(-time.Hour)
	s.Put(still)
	if n := len(s.Source(SourceKey{"team-a", "orders"})); n != MaxPerSource+1 {
		t.Errorf("kept %d", n)
	}
	reloaded := NewStore(p, nil, t0)
	got := reloaded.Source(SourceKey{"team-a", "orders"})
	if len(got) != MaxPerSource+1 || got[0].StartedAt != t0.Add(time.Duration(MaxPerSource+4)*time.Hour) {
		t.Errorf("reloaded %d, newest %v", len(got), got[0].StartedAt)
	}
	if _, ok := reloaded.Get(Key{"team-a", "old-but-running"}); !ok {
		t.Error("a running drill was dropped by the bound")
	}
	if at, ok := reloaded.LastPassed(SourceKey{"team-a", "orders"}); !ok || !at.Equal(t0.Add(time.Duration(MaxPerSource+4)*time.Hour+time.Minute)) {
		t.Errorf("last passed = %v %v", at, ok)
	}
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Wait(ctx context.Context, d time.Duration) error {
	c.now = c.now.Add(d)
	return ctx.Err()
}

type fakeObserver struct {
	clusters []*cnpgv1.Cluster
	obs      map[string]Observation
	err      error
	// others are Clusters that exist but are not in the labelled list.
	others     []*cnpgv1.Cluster
	unreadable bool
}

func (f *fakeObserver) DrillCluster(_ context.Context, namespace, name string) (*cnpgv1.Cluster, error) {
	if f.unreadable {
		return nil, errors.New("namespace not readable")
	}
	for _, c := range append(append([]*cnpgv1.Cluster{}, f.clusters...), f.others...) {
		if c.Namespace == namespace && c.Name == name {
			return c, nil
		}
	}
	return nil, nil
}

func (f *fakeObserver) DrillClusters(context.Context, string) ([]*cnpgv1.Cluster, error) {
	return f.clusters, f.err
}

func (f *fakeObserver) DrillObservation(_ context.Context, _, name string) (Observation, error) {
	return f.obs[name], nil
}

type fakeReaper struct {
	keys    [][]byte
	deleted []string
	fail    error
}

func (f *fakeReaper) Owner() string { return "team-a" }

func (f *fakeReaper) Verify(c *cnpgv1.Cluster) (ops.DrillMeta, error) {
	return ops.VerifyDrill(c, "team-a", f.keys)
}

func (f *fakeReaper) Teardown(_ context.Context, c *cnpgv1.Cluster) error {
	if _, err := f.Verify(c); err != nil {
		return err
	}
	if f.fail != nil {
		return f.fail
	}
	f.deleted = append(f.deleted, c.Name)
	return nil
}

func sealedDrill(t *testing.T, key []byte, at time.Time) *cnpgv1.Cluster {
	t.Helper()
	src := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders"},
		Spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{{
			Name: ops.BarmanCloudPlugin, Parameters: map[string]string{"barmanObjectName": "store"},
		}}},
	}
	rs, err := ops.DrillRecoverySource(src)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ops.DrillCluster(ops.DrillRequest{
		Source: src, Recovery: rs, Namespace: "team-a", Name: ops.DrillName("drill-", "orders", at),
		Meta: ops.DrillMeta{Backup: "orders-b1", StartedAt: at, Deadline: at.Add(2 * time.Hour), SourceBytes: i64(10 << 30), Owner: "team-a"},
	}, [][]byte{key})
	if err != nil {
		t.Fatal(err)
	}
	c.UID = types.UID("uid-" + c.Name)
	c.CreationTimestamp = metav1.NewTime(at.Add(time.Second))
	return c
}

func TestSupervisor(t *testing.T) {
	t.Parallel()
	key := []byte(strings.Repeat("k", 32))
	clock := &fakeClock{now: t0}
	drill := sealedDrill(t, key, t0)
	spoof := drill.DeepCopy()
	spoof.Name, spoof.UID = "drill-lookalike", "uid-lookalike"
	obs := &fakeObserver{clusters: []*cnpgv1.Cluster{drill, spoof}, obs: map[string]Observation{drill.Name: {Phase: "Setting up primary"}}}
	reaper := &fakeReaper{keys: [][]byte{key}}
	store := NewStore(nil, nil, t0)
	sup := NewSupervisor(store, obs, reaper, clock, nil)

	sup.Tick(context.Background())
	rec, ok := store.Get(Key{"team-a", drill.Name})
	if !ok || rec.Result != viewmodel.DrillRunning || rec.UID != string(drill.UID) || rec.Backup != "orders-b1" {
		t.Fatalf("first pass = %+v, %v", rec, ok)
	}
	if _, ok := store.Get(Key{"team-a", spoof.Name}); ok || len(reaper.deleted) != 0 {
		t.Fatal("the unsealed lookalike was recorded or deleted")
	}

	clock.now = t0.Add(20 * time.Minute)
	obs.obs[drill.Name] = healthyObs(i64(10 << 30))
	sup.Tick(context.Background())
	rec, _ = store.Get(Key{"team-a", drill.Name})
	if rec.Result != viewmodel.DrillPassed || len(reaper.deleted) != 1 || reaper.deleted[0] != drill.Name {
		t.Fatalf("passed pass = %+v, deleted %v", rec, reaper.deleted)
	}

	// The accepted delete is the teardown; the Cluster then leaves the
	// cache and nothing changes.
	if !rec.TornDownAt.Equal(t0.Add(20 * time.Minute)) {
		t.Errorf("torn down at %v", rec.TornDownAt)
	}
	obs.clusters = []*cnpgv1.Cluster{spoof}
	clock.now = t0.Add(21 * time.Minute)
	sup.Tick(context.Background())
	rec, _ = store.Get(Key{"team-a", drill.Name})
	if rec.Result != viewmodel.DrillPassed || !rec.TornDownAt.Equal(t0.Add(20*time.Minute)) || len(reaper.deleted) != 1 {
		t.Errorf("after teardown = %+v", rec)
	}
}

func TestSupervisorTimeLimitAndTeardownFailure(t *testing.T) {
	t.Parallel()
	key := []byte(strings.Repeat("k", 32))
	clock := &fakeClock{now: t0.Add(2 * time.Hour)}
	drill := sealedDrill(t, key, t0)
	obs := &fakeObserver{clusters: []*cnpgv1.Cluster{drill}, obs: map[string]Observation{}}
	reaper := &fakeReaper{keys: [][]byte{key}, fail: &ops.WriteError{Status: 403, Text: "the teardown Role is missing"}}
	store := NewStore(nil, nil, t0)
	sup := NewSupervisor(store, obs, reaper, clock, nil)
	sup.Tick(context.Background())
	rec, _ := store.Get(Key{"team-a", drill.Name})
	if rec.Result != viewmodel.DrillTimedOut || rec.TeardownError != "the teardown Role is missing" {
		t.Fatalf("timed out = %+v", rec)
	}
	if !rec.TornDownAt.IsZero() {
		t.Errorf("a refused delete was recorded as a teardown: %+v", rec)
	}
	reaper.fail = nil
	clock.now = clock.now.Add(Interval)
	sup.Tick(context.Background())
	rec, _ = store.Get(Key{"team-a", drill.Name})
	if rec.TeardownError != "" || len(reaper.deleted) != 1 || rec.TornDownAt.IsZero() {
		t.Errorf("retried = %+v", rec)
	}
}

// A record of this replica names the incarnation it created: another
// Cluster that later carries the same name and a copied seal is not it.
func TestSupervisorKeepsToTheRecordedIncarnation(t *testing.T) {
	t.Parallel()
	key := []byte(strings.Repeat("k", 32))
	clock := &fakeClock{now: t0.Add(3 * time.Hour)}
	drill := sealedDrill(t, key, t0)
	store := NewStore(nil, nil, t0)
	store.Put(Record{Drill: Key{"team-a", drill.Name}, Source: SourceKey{"team-a", "orders"}, UID: "the-original", StartedAt: t0, Deadline: t0.Add(2 * time.Hour)})
	reaper := &fakeReaper{keys: [][]byte{key}}
	sup := NewSupervisor(store, &fakeObserver{clusters: []*cnpgv1.Cluster{drill}}, reaper, clock, nil)
	sup.Tick(context.Background())
	if len(reaper.deleted) != 0 {
		t.Errorf("deleted another incarnation: %v", reaper.deleted)
	}
}

func TestSupervisorCacheLagAndListFailure(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: t0.Add(time.Minute)}
	store := NewStore(nil, nil, t0)
	store.Put(running())
	obs := &fakeObserver{}
	sup := NewSupervisor(store, obs, &fakeReaper{}, clock, nil)
	sup.Tick(context.Background())
	if r, _ := store.Get(running().Drill); r.Finished() {
		t.Error("a just-created drill missing from the cache was cancelled")
	}
	obs.err = errors.New("cache not readable")
	clock.now = t0.Add(time.Hour)
	sup.Tick(context.Background())
	if r, _ := store.Get(running().Drill); r.Finished() {
		t.Error("a failed list cancelled a drill")
	}
	obs.err = nil
	sup.Tick(context.Background())
	if r, _ := store.Get(running().Drill); r.Result != viewmodel.DrillCancelled {
		t.Errorf("gone after the lag = %+v", r)
	}
}

// A drill that stops verifying (relabelled, re-owned, seal changed) is
// left in place and said so; it is never recorded as deleted.
func TestSupervisorLeavesAnUnverifiedDrillInPlace(t *testing.T) {
	t.Parallel()
	key := []byte(strings.Repeat("k", 32))
	clock := &fakeClock{now: t0.Add(time.Minute)}
	drill := sealedDrill(t, key, t0)
	obs := &fakeObserver{clusters: []*cnpgv1.Cluster{drill}, obs: map[string]Observation{}}
	reaper := &fakeReaper{keys: [][]byte{key}}
	store := NewStore(nil, nil, t0)
	sup := NewSupervisor(store, obs, reaper, clock, nil)
	sup.Tick(context.Background())

	relabelled := drill.DeepCopy()
	relabelled.Labels[ops.LabelDrillOwner] = "someone-else"
	obs.clusters, obs.others = nil, []*cnpgv1.Cluster{relabelled}
	clock.now = t0.Add(5 * time.Minute)
	sup.Tick(context.Background())
	rec, _ := store.Get(Key{"team-a", drill.Name})
	if !rec.LeftInPlace || !rec.TornDownAt.IsZero() || rec.Result != viewmodel.DrillCancelled || !strings.HasPrefix(rec.TeardownError, "left in place") {
		t.Errorf("relabelled drill = %+v", rec)
	}
	if v := View(rec); v.TornDownAt != nil {
		t.Errorf("view says deleted: %+v", v)
	}
	// Supervision stopped: the time limit passes and nothing is deleted.
	obs.clusters = []*cnpgv1.Cluster{drill}
	clock.now = t0.Add(3 * time.Hour)
	sup.Tick(context.Background())
	if len(reaper.deleted) != 0 {
		t.Errorf("deleted a drill left in place: %v", reaper.deleted)
	}
}

// A namespace the console cannot read says nothing: the drill is not
// concluded gone.
func TestSupervisorUnreadableNamespaceIsUnknown(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: t0.Add(time.Hour)}
	store := NewStore(nil, nil, t0)
	r := running()
	r.SeenAt = t0
	store.Put(r)
	obs := &fakeObserver{unreadable: true}
	sup := NewSupervisor(store, obs, &fakeReaper{}, clock, nil)
	sup.Tick(context.Background())
	if got, _ := store.Get(r.Drill); got.Finished() || !got.TornDownAt.IsZero() {
		t.Errorf("unreadable namespace concluded = %+v", got)
	}
}

type countingHandler struct {
	mu    sync.Mutex
	warns map[string]int
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.Level >= slog.LevelWarn {
		h.warns[r.Message]++
	}
	return nil
}
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

type countingReaper struct {
	fakeReaper
	attempts int
}

func (f *countingReaper) Teardown(ctx context.Context, c *cnpgv1.Cluster) error {
	f.attempts++
	return f.fakeReaper.Teardown(ctx, c)
}

// A refused delete is retried with a growing delay, and warned once.
func TestSupervisorBacksOffARefusedTeardown(t *testing.T) {
	t.Parallel()
	key := []byte(strings.Repeat("k", 32))
	clock := &fakeClock{now: t0.Add(2 * time.Hour)}
	drill := sealedDrill(t, key, t0)
	obs := &fakeObserver{clusters: []*cnpgv1.Cluster{drill}, obs: map[string]Observation{}}
	reaper := &countingReaper{fakeReaper: fakeReaper{keys: [][]byte{key}, fail: &ops.WriteError{Status: 403, Text: "forbidden"}}}
	h := &countingHandler{warns: map[string]int{}}
	sup := NewSupervisor(NewStore(nil, nil, t0), obs, reaper, clock, slog.New(h))
	// One hour of passes every 15 s: 240 passes.
	for range 240 {
		sup.Tick(context.Background())
		clock.now = clock.now.Add(Interval)
	}
	if reaper.attempts > 15 || reaper.attempts < 5 {
		t.Errorf("teardown attempts in an hour = %d, want a backed-off handful", reaper.attempts)
	}
	if n := h.warns["restore drill not deleted"]; n != 1 {
		t.Errorf("warned %d times for the same refusal", n)
	}
}

// Labelled lookalikes are remembered for one pass only, and at most
// maxUnsealed of them.
func TestSupervisorBoundsTheUnsealedSet(t *testing.T) {
	t.Parallel()
	many := make([]*cnpgv1.Cluster, 0, maxUnsealed+50)
	for i := range maxUnsealed + 50 {
		many = append(many, &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: "fake-" + strconv.Itoa(i), Labels: map[string]string{ops.LabelDrillOwner: "team-a"},
		}})
	}
	obs := &fakeObserver{clusters: many}
	sup := NewSupervisor(NewStore(nil, nil, t0), obs, &fakeReaper{}, &fakeClock{now: t0}, nil)
	sup.Tick(context.Background())
	if n := len(sup.unsealed); n > maxUnsealed {
		t.Errorf("unsealed set = %d", n)
	}
	obs.clusters = nil
	sup.Tick(context.Background())
	if n := len(sup.unsealed); n != 0 {
		t.Errorf("unsealed set after they left = %d", n)
	}
}

func TestStoreRecordedSince(t *testing.T) {
	t.Parallel()
	start := t0.Add(-100 * 24 * time.Hour)
	if got := NewStore(nil, nil, t0).RecordedSince(); !got.Equal(t0) {
		t.Errorf("in memory = %v, want the console's start", got)
	}
	p := &memPersist{data: map[string][]byte{}, clean: true}
	if got := NewStore(p, nil, start).RecordedSince(); !got.Equal(start) {
		t.Errorf("first persistent start = %v", got)
	}
	if got := NewStore(p, nil, t0).RecordedSince(); !got.Equal(start) {
		t.Errorf("after a restart = %v, want the journal's start %v", got, start)
	}
	// A record that could not be read makes the record incomplete: it is
	// complete only from this start on.
	p.clean = false
	if got := NewStore(p, nil, t0).RecordedSince(); !got.Equal(t0) {
		t.Errorf("with a skipped record = %v, want %v", got, t0)
	}
}

func TestStoreBoundsKeepTheNewestPass(t *testing.T) {
	t.Parallel()
	s := NewStore(nil, nil, t0)
	pass := running()
	pass.Drill.Name, pass.Result, pass.FinishedAt = "the-pass", viewmodel.DrillPassed, t0.Add(time.Minute)
	s.Put(pass)
	for i := range MaxPerSource + 3 {
		r := running()
		r.Drill.Name = "fail-" + time.Duration(i).String()
		r.StartedAt = t0.Add(time.Duration(i+1) * time.Hour)
		r.Result, r.FinishedAt = viewmodel.DrillFailed, r.StartedAt.Add(time.Minute)
		s.Put(r)
	}
	at, ok := s.LastPassed(SourceKey{"team-a", "orders"})
	if !ok || !at.Equal(pass.FinishedAt) {
		t.Errorf("last pass = %v, %v: the bound dropped the only pass", at, ok)
	}
}

func checkText(r Record, name viewmodel.DrillCheckName) string {
	for _, c := range r.Checks {
		if c.Name == name {
			return c.Text
		}
	}
	return ""
}

// memKeeper is a Keeper whose contents outlive a Store, as the
// shared-state ConfigMap outlives a console process.
type memKeeper struct {
	mu    sync.Mutex
	kept  map[Key]Kept
	since time.Time
	keeps bool
}

func (k *memKeeper) KeepDrill(r Kept) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.kept[r.Drill] = r
}

func (k *memKeeper) KeptDrills() []Kept {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]Kept, 0, len(k.kept))
	for _, r := range k.kept {
		out = append(out, r)
	}
	return out
}
func (k *memKeeper) KeepsDrills() bool    { return k.keeps }
func (k *memKeeper) KeptSince() time.Time { return k.since }

// TestKeeperKeepsFinishedDrillsAcrossARestart: without a journal, the
// outcome of a finished drill survives a console restart in the keeper,
// marked as having lost its checks; a running drill is not kept (its
// sealed Cluster carries it), and without a keeper nothing survives.
func TestKeeperKeepsFinishedDrillsAcrossARestart(t *testing.T) {
	keeper := &memKeeper{kept: map[Key]Kept{}, since: t0.Add(-30 * 24 * time.Hour), keeps: true}
	before := NewStore(nil, nil, t0)
	before.SetKeeper(keeper)
	r := running()
	before.Put(r)
	if len(keeper.KeptDrills()) != 0 {
		t.Fatal("a running drill was kept")
	}
	r.Result, r.HealthyAt, r.FinishedAt = viewmodel.DrillPassed, t0.Add(20*time.Minute), t0.Add(25*time.Minute)
	r.Checks = []viewmodel.DrillCheck{{Name: viewmodel.DrillCheckHealthy, Outcome: viewmodel.DrillCheckPassed}}
	before.Put(r)

	after := NewStore(nil, nil, t0.Add(time.Hour))
	after.SetKeeper(keeper)
	got := after.Source(r.Source)
	if len(got) != 1 || got[0].Result != viewmodel.DrillPassed || !got[0].Restored || len(got[0].Checks) != 0 {
		t.Fatalf("after a restart = %+v", got)
	}
	if v := View(got[0]); !v.ChecksNotKept || v.DurationSeconds == nil || *v.DurationSeconds != 20*60 {
		t.Errorf("view = %+v", v)
	}
	if at, ok := after.LastPassed(r.Source); !ok || !at.Equal(r.FinishedAt) {
		t.Errorf("last passed = %v %v", at, ok)
	}
	if !after.Persistent() || !after.Kept() || !after.RecordedSince().Equal(keeper.since) {
		t.Errorf("persistent %v kept %v since %v", after.Persistent(), after.Kept(), after.RecordedSince())
	}

	// A record held in memory hides the keeper's copy (no duplicate).
	after.Put(r)
	if n := len(after.Source(r.Source)); n != 1 {
		t.Errorf("%d records of one drill", n)
	}

	// A keeper that cannot keep (ConfigMap missing) is no persistence.
	keeper.keeps = false
	if lost := NewStore(nil, nil, t0); lost.Persistent() {
		t.Error("a store without journal or keeper reports persistent")
	}
	unkept := NewStore(nil, nil, t0.Add(time.Hour))
	unkept.SetKeeper(keeper)
	if unkept.Persistent() || !unkept.RecordedSince().Equal(t0.Add(time.Hour)) {
		t.Errorf("a keeper that keeps nothing: persistent %v since %v", unkept.Persistent(), unkept.RecordedSince())
	}
}

// TestRecordOfTheLiveCluster: a Cluster deleted and recreated under the
// same name does not inherit the drills of the one before it, whether
// the record carries the source UID or (an older record) only its times;
// the source UID survives a restart through the keeper and the seal.
func TestRecordOfTheLiveCluster(t *testing.T) {
	created := t0.Add(-time.Hour)
	cases := []struct {
		name string
		rec  func(Record) Record
		live Live
		want bool
	}{
		{"same UID", func(r Record) Record { r.SourceUID = "u1"; return r }, Live{UID: "u1", CreatedAt: created}, true},
		{"recreated: another UID", func(r Record) Record { r.SourceUID = "u0"; return r }, Live{UID: "u1", CreatedAt: t0.Add(-48 * time.Hour)}, false},
		{"older record started after creation", func(r Record) Record { return r }, Live{UID: "u1", CreatedAt: created}, true},
		{"older record started before creation", func(r Record) Record { return r }, Live{UID: "u1", CreatedAt: t0.Add(time.Hour)}, false},
		{"older record, creation within the same second", func(r Record) Record { return r }, Live{UID: "u1", CreatedAt: t0.Add(500 * time.Millisecond)}, true},
		{"older record without a start judged by its finish", func(r Record) Record {
			r.StartedAt, r.FinishedAt = time.Time{}, t0.Add(-2*time.Hour)
			return r
		}, Live{CreatedAt: created}, false},
		{"nothing known of the live Cluster", func(r Record) Record { r.SourceUID = "u0"; return r }, Live{}, true},
	}
	for _, c := range cases {
		if got := c.rec(running()).Of(c.live); got != c.want {
			t.Errorf("%s: Of = %v, want %v", c.name, got, c.want)
		}
	}

	keeper := &memKeeper{kept: map[Key]Kept{}, keeps: true}
	before := NewStore(nil, nil, t0)
	before.SetKeeper(keeper)
	r := running()
	r.SourceUID, r.Result, r.FinishedAt = "u0", viewmodel.DrillPassed, t0.Add(25*time.Minute)
	before.Put(r)
	after := NewStore(nil, nil, t0.Add(time.Hour))
	after.SetKeeper(keeper)
	if _, ok := after.LastPassedOf(r.Source, Live{UID: "u1"}); ok {
		t.Error("a recreated Cluster inherits a pass kept across a restart")
	}
	if at, ok := after.LastPassedOf(r.Source, Live{UID: "u0"}); !ok || !at.Equal(r.FinishedAt) {
		t.Errorf("the same Cluster lost its pass: %v %v", at, ok)
	}
	if got := FromMeta(r.Drill, ops.DrillMeta{SourceNamespace: "team-a", Source: "orders", SourceUID: "u0"}); got.SourceUID != "u0" {
		t.Errorf("FromMeta dropped the source UID: %+v", got)
	}
}
