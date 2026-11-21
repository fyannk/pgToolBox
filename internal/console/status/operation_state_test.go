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

package status

import (
	"context"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/opstate"
	"github.com/fyannk/pgToolBox/internal/console/timeline"
)

// TestOperationStateObservedPrimary: the offers' state carries the
// primary changes of this Cluster incarnation the timeline observed, and
// none of its first observation, an earlier incarnation's or another
// Cluster's; no timeline is said, never an empty history.
func TestOperationStateObservedPrimary(t *testing.T) {
	j := timeline.New(timeline.Limits{}, staticClock(now))
	for _, e := range []timeline.Event{
		{Kind: timeline.KindConsole, Action: timeline.ActionStarted, At: now.Add(-4 * time.Hour), Timing: timeline.TimingObserved},
		{Key: "p0", Kind: timeline.KindPrimary, Action: timeline.ActionFirstObserved, At: now.Add(-3 * time.Hour), Timing: timeline.TimingObserved, Namespace: "team-a", Cluster: "orders", ClusterUID: string(ordersUID), To: "orders-1"},
		{Key: "p1", Kind: timeline.KindPrimary, Action: timeline.ActionFailover, At: now.Add(-2 * time.Hour), Timing: timeline.TimingReported, Namespace: "team-a", Cluster: "orders", ClusterUID: "an-earlier-orders", From: "orders-1", To: "orders-2"},
		{Key: "p2", Kind: timeline.KindPrimary, Action: timeline.ActionSwitchover, At: now.Add(-time.Hour), Timing: timeline.TimingReported, Namespace: "team-a", Cluster: "orders", ClusterUID: string(ordersUID), From: "orders-1", To: "orders-2"},
		{Key: "p3", Kind: timeline.KindPrimary, Action: timeline.ActionChanged, Uncertain: true, AfterGap: true, Since: now.Add(-50 * time.Minute), At: now.Add(-30 * time.Minute), Timing: timeline.TimingObserved, Namespace: "team-a", Cluster: "orders", ClusterUID: string(ordersUID), From: "orders-2", To: "orders-3"},
		{Key: "p4", Kind: timeline.KindPrimary, Action: timeline.ActionFailover, At: now.Add(-10 * time.Minute), Timing: timeline.TimingReported, Namespace: "team-a", Cluster: "billing", From: "billing-1", To: "billing-2"},
	} {
		j.Record(e)
	}
	svc := newScreenService(t)
	svc.opts.Timeline = j
	st, err := svc.OperationState(context.Background(), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	o := st.Observed
	if !o.Enabled || o.Persistent || !o.Since.Equal(now.Add(-4*time.Hour)) {
		t.Errorf("observed = enabled %v persistent %v since %s, want an in-memory timeline since the console started", o.Enabled, o.Persistent, o.Since)
	}
	want := []opstate.PrimaryChange{
		{From: "orders-2", To: "orders-3", At: now.Add(-30 * time.Minute), Uncertain: true, Since: now.Add(-50 * time.Minute)},
		{From: "orders-1", To: "orders-2", At: now.Add(-time.Hour), Reported: true, Cause: timeline.ActionSwitchover},
	}
	if len(o.PrimaryChanges) != len(want) {
		t.Fatalf("changes = %+v, want %+v", o.PrimaryChanges, want)
	}
	for i := range want {
		g, w := o.PrimaryChanges[i], want[i]
		if g.From != w.From || g.To != w.To || !g.At.Equal(w.At) || g.Reported != w.Reported || g.Cause != w.Cause || g.Uncertain != w.Uncertain || !g.Since.Equal(w.Since) {
			t.Errorf("change %d = %+v, want %+v", i, g, w)
		}
	}

	svc.opts.Timeline = nil
	if st, err = svc.OperationState(context.Background(), "team-a", "orders"); err != nil || st.Observed.Enabled {
		t.Errorf("without a timeline: observed %+v, %v; want it said disabled", st.Observed, err)
	}
}
