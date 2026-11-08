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

package diag

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
	"github.com/fyannk/pgToolBox/pkg/diagnose"
)

type fakeSource struct {
	mu     sync.Mutex
	keys   []cache.ClusterKey
	inputs map[cache.ClusterKey]diagnose.Input
	calls  map[cache.ClusterKey]int
}

func (f *fakeSource) Keys(context.Context) []cache.ClusterKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]cache.ClusterKey(nil), f.keys...)
}

func (f *fakeSource) Input(_ context.Context, key cache.ClusterKey) (diagnose.Input, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[key]++
	in, ok := f.inputs[key]
	return in, ok
}

type noChanges struct{}

func (noChanges) Subscribe() *cache.Subscription  { return nil }
func (noChanges) Unsubscribe(*cache.Subscription) {}

var (
	orders  = cache.ClusterKey{Namespace: "a", Name: "orders"}
	billing = cache.ClusterKey{Namespace: "a", Name: "billing"}
)

// crashing is an input on which the hand-written image-pull detector
// reports a finding: one pod waiting in ImagePullBackOff.
func crashing(now time.Time) diagnose.Input {
	return diagnose.Input{
		Now:     now,
		HasPods: true,
		Pods: observe.PodsSnapshot{ObservedAt: now, Pods: []observe.PodFacts{{
			Name: "orders-1", UID: "u1",
			Containers: []observe.ContainerFacts{{Name: "postgres", Image: "x:1", State: "Waiting", Reason: "ImagePullBackOff"}},
		}}},
	}
}

func TestRunnerSinceClock(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	src := &fakeSource{keys: []cache.ClusterKey{orders}, inputs: map[cache.ClusterKey]diagnose.Input{orders: crashing(now)}, calls: map[cache.ClusterKey]int{}}
	r := New(src, noChanges{}, Options{Interval: time.Minute, Now: func() time.Time { return now }, Rules: []diagnose.Rule{}})
	if st := r.State(); st.State != viewmodel.SourcePending {
		t.Errorf("before a run: %+v", st)
	}
	r.RunAll(context.Background())
	first, ok := r.Latest(orders)
	if !ok || len(first.Result.Findings) == 0 {
		t.Fatalf("no finding: %+v", first.Result)
	}
	id := first.Result.Findings[0].ID
	if !first.Since[id].Equal(now) {
		t.Errorf("since = %v", first.Since[id])
	}

	now = now.Add(time.Minute)
	r.RunAll(context.Background())
	second, _ := r.Latest(orders)
	if !second.Since[id].Equal(first.At) || second.Generation <= first.Generation {
		t.Errorf("since moved or generation did not: %v %d", second.Since[id], second.Generation)
	}

	now = now.Add(time.Minute)
	src.inputs[orders] = diagnose.Input{Now: now, HasPods: true}
	r.RunAll(context.Background())
	now = now.Add(time.Minute)
	src.inputs[orders] = crashing(now)
	r.RunAll(context.Background())
	again, _ := r.Latest(orders)
	if !again.Since[id].Equal(now) {
		t.Errorf("a finding that cleared and came back kept its old clock: %v", again.Since[id])
	}
	if st := r.State(); st.State != viewmodel.SourceLive {
		t.Errorf("after runs: %+v", st)
	}
}

func TestRunnerForgetsGoneClusters(t *testing.T) {
	src := &fakeSource{keys: []cache.ClusterKey{orders, billing}, inputs: map[cache.ClusterKey]diagnose.Input{orders: {}, billing: {}}, calls: map[cache.ClusterKey]int{}}
	r := New(src, noChanges{}, Options{Interval: time.Minute, Rules: []diagnose.Rule{}})
	r.RunAll(context.Background())
	src.keys = []cache.ClusterKey{orders}
	r.RunAll(context.Background())
	if _, ok := r.Latest(billing); ok {
		t.Error("a deleted Cluster keeps its run")
	}
	delete(src.inputs, orders)
	r.RunKeys(context.Background(), []cache.ClusterKey{orders})
	if _, ok := r.Latest(orders); ok {
		t.Error("a Cluster whose input vanished keeps its run")
	}
}

func TestRunnerSurvivesAPanickingRule(t *testing.T) {
	src := &fakeSource{keys: []cache.ClusterKey{orders}, inputs: map[cache.ClusterKey]diagnose.Input{orders: {}}, calls: map[cache.ClusterKey]int{}}
	r := New(src, noChanges{}, Options{Interval: time.Minute, engine: func(diagnose.Input) diagnose.Result { panic("rule bug: password=hunter2") }})
	r.RunAll(context.Background())
	run, ok := r.Latest(orders)
	if !ok || run.Failed != RunPanicked {
		t.Fatalf("a panic is said in a fixed sentence, its value in the log only: run = %+v", run)
	}
	if st := r.State(); st.State != viewmodel.SourceUnavailable || strings.Contains(st.Reason, "hunter2") {
		t.Errorf("state = %+v", st)
	}
	// The failure clears when the rule recovers, and when the Cluster goes.
	r.opts.engine = func(diagnose.Input) diagnose.Result { return diagnose.Result{} }
	r.RunAll(context.Background())
	if st := r.State(); st.State != viewmodel.SourceLive {
		t.Errorf("state after recovery = %+v", st)
	}
	r.opts.engine = func(diagnose.Input) diagnose.Result { panic("rule bug") }
	r.RunAll(context.Background())
	src.keys, src.inputs = nil, map[cache.ClusterKey]diagnose.Input{}
	r.RunAll(context.Background())
	if st := r.State(); st.State != viewmodel.SourceLive {
		t.Errorf("state after the failing Cluster went = %+v", st)
	}
}

func TestExpandNamespaceKeys(t *testing.T) {
	src := &fakeSource{keys: []cache.ClusterKey{orders, billing, {Namespace: "b", Name: "x"}}, calls: map[cache.ClusterKey]int{}}
	r := New(src, noChanges{}, Options{Interval: time.Minute, Rules: []diagnose.Rule{}})
	got := r.expand(context.Background(), []cache.ClusterKey{{Namespace: "a"}})
	if len(got) != 2 {
		t.Errorf("expand = %v", got)
	}
}

func TestDisabledRunner(t *testing.T) {
	r := New(&fakeSource{}, noChanges{}, Options{Disabled: "DIAGNOSTICS_ENABLED=false", Rules: []diagnose.Rule{}})
	if st := r.State(); st.State != viewmodel.SourceDisabled || st.Reason == "" {
		t.Errorf("state = %+v", st)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Run(ctx); err != nil {
		t.Error(err)
	}
}

// sharedFindings stands in for the shared state: an earlier first-seen
// time for every finding, and a record of the runs it was told about.
type sharedFindings struct {
	earlier time.Time
	calls   int
	uid     string
}

func (s *sharedFindings) FindingsSince(uid string, present map[string]time.Time, _ time.Time) map[string]time.Time {
	s.calls++
	s.uid = uid
	out := map[string]time.Time{}
	for id := range present {
		out[id] = s.earlier
	}
	return out
}

func TestRunnerSharesFindingClocks(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	in := crashing(now)
	in.Cluster.Cluster.UID = "cluster-uid"
	src := &fakeSource{keys: []cache.ClusterKey{orders}, inputs: map[cache.ClusterKey]diagnose.Input{orders: in}, calls: map[cache.ClusterKey]int{}}
	shared := &sharedFindings{earlier: now.Add(-time.Hour)}
	r := New(src, noChanges{}, Options{Interval: time.Minute, Now: func() time.Time { return now }, Rules: []diagnose.Rule{}, Shared: shared})
	r.RunAll(context.Background())
	run, _ := r.Latest(orders)
	if len(run.Result.Findings) == 0 || !run.Since[run.Result.Findings[0].ID].Equal(shared.earlier) || shared.uid != "cluster-uid" {
		t.Fatalf("since = %v, uid %q: the replica that saw the finding first sets its clock", run.Since, shared.uid)
	}
	// A failed run did not look: the shared clocks are not told the
	// findings ended.
	r.opts.engine = func(diagnose.Input) diagnose.Result { panic("rule bug") }
	r.RunAll(context.Background())
	if shared.calls != 1 {
		t.Fatalf("a failed run reached the shared state (%d calls)", shared.calls)
	}
}
