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
	"errors"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/drills"
	"github.com/fyannk/pgToolBox/internal/console/sweep"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

func TestDrillReads(t *testing.T) {
	ctx := context.Background()
	svc := newService(ctx, t, nil)
	// The fixture's store has no spec.configuration: it exists, with
	// empty facts that match nothing (ops tests the parsing).
	if f, exists, readable := svc.ObjectStore(ctx, "team-a", "store"); !exists || !readable || f.DestinationPath != "" {
		t.Errorf("store: %+v %v %v", f, exists, readable)
	}
	if _, exists, readable := svc.ObjectStore(ctx, "team-a", "other"); exists || !readable {
		t.Errorf("other: %v %v", exists, readable)
	}
	notWatched := newService(ctx, t, map[string]viewmodel.SourceState{"objectStores": {Source: "objectStores", State: viewmodel.SourceForbidden}})
	if _, _, readable := notWatched.ObjectStore(ctx, "team-a", "store"); readable {
		t.Error("forbidden ObjectStores reported readable")
	}
	// By name: a missing Cluster is nil, an unreadable namespace is an
	// error (unknown, never gone).
	if c, err := svc.DrillCluster(ctx, "team-a", "orders"); err != nil || c == nil {
		t.Errorf("drill cluster by name = %v, %v", c, err)
	}
	if c, err := svc.DrillCluster(ctx, "team-a", "gone"); err != nil || c != nil {
		t.Errorf("missing cluster = %v, %v", c, err)
	}
	noClusters := newService(ctx, t, map[string]viewmodel.SourceState{"clusters": {Source: "clusters", State: viewmodel.SourceForbidden}})
	if _, err := noClusters.DrillCluster(ctx, "team-a", "orders"); !errors.Is(err, ErrDrillNamespaceUnreadable) {
		t.Errorf("unreadable namespace = %v", err)
	}
	if cs, err := svc.DrillClusters(ctx, "team-a"); err != nil || len(cs) != 0 {
		t.Errorf("drill clusters of the fixtures = %d, %v", len(cs), err)
	}
	o, err := svc.DrillObservation(ctx, "team-a", "orders")
	if err != nil || o.Reading == nil || !o.Reading.IsPrimary || o.DatabaseBytes != nil {
		t.Errorf("observation = %+v, %v", o, err)
	}
	if svc.DatabaseBytes("team-a", "orders", "orders-1") != nil {
		t.Error("database bytes without an exporter store")
	}

	if in, ok := svc.Input(ctx, cache.ClusterKey{Namespace: "team-a", Name: "orders"}); !ok || in.HasDrills {
		t.Errorf("drills off: diagnostics input has drills = %v (%v)", in.HasDrills, ok)
	}
	b, err := svc.Backups(ctx, "team-a", "orders")
	if err != nil || b.Drills.Enabled || len(b.Drills.Items) != 0 || b.Drills.Scope == "" {
		t.Errorf("disabled block = %+v, %v", b.Drills, err)
	}
	store := drills.NewStore(nil, nil, now)
	store.Put(drills.Record{
		Drill: drills.Key{Namespace: "team-a", Name: "drill-orders-1"}, Source: drills.SourceKey{Namespace: "team-a", Name: "orders"},
		StartedAt: now.Add(-time.Hour), Deadline: now.Add(time.Hour), HealthyAt: now.Add(-50 * time.Minute), FinishedAt: now.Add(-49 * time.Minute),
		Result: viewmodel.DrillPassed,
	})
	store.Put(drills.Record{
		Drill: drills.Key{Namespace: "team-a", Name: "drill-billing-1"}, Source: drills.SourceKey{Namespace: "team-a", Name: "billing"},
		StartedAt: now, Deadline: now.Add(time.Hour),
	})
	svc.SetDrills(&DrillsView{Records: store, TimeLimit: 2 * time.Hour})
	b, _ = svc.Backups(ctx, "team-a", "orders")
	d := b.Drills
	if !d.Enabled || d.Namespace != "team-a" || d.TimeLimitSeconds != 7200 || len(d.Items) != 1 || d.Items[0].Result != viewmodel.DrillPassed ||
		d.LastPassedAt == nil || !d.LastPassedAt.Equal(now.Add(-49*time.Minute)) || *d.Items[0].DurationSeconds != 600 || len(d.NotChecked) == 0 {
		t.Errorf("enabled block = %+v", d)
	}
	if !d.RestoreTested || d.WindowDays != 90 || !b.RecoveryTimeline.RestoreTested {
		t.Errorf("a drill passed 49 minutes ago: restoreTested %v (window %d), timeline %v", d.RestoreTested, d.WindowDays, b.RecoveryTimeline.RestoreTested)
	}
	old := drills.NewStore(nil, nil, now)
	old.Put(drills.Record{
		Drill: drills.Key{Namespace: "team-a", Name: "drill-orders-0"}, Source: drills.SourceKey{Namespace: "team-a", Name: "orders"},
		StartedAt: now.Add(-91 * 24 * time.Hour), Deadline: now.Add(-91*24*time.Hour + time.Hour), FinishedAt: now.Add(-91*24*time.Hour + 30*time.Minute),
		Result: viewmodel.DrillPassed,
	})
	svc.SetDrills(&DrillsView{Records: old, TimeLimit: 2 * time.Hour})
	if b, _ := svc.Backups(ctx, "team-a", "orders"); b.Drills.RestoreTested || b.RecoveryTimeline.RestoreTested || b.Drills.LastPassedAt == nil {
		t.Errorf("a pass 91 days ago is outside the window: %+v", b.Drills)
	}
	svc.SetDrills(&DrillsView{Records: store, TimeLimit: 2 * time.Hour})
	if !d.LostAtRestart || !strings.Contains(d.Scope, "drill history lost at restart") {
		t.Errorf("an in-memory record must say it is lost at restart: %v %q", d.LostAtRestart, d.Scope)
	}
	in, ok := svc.Input(ctx, cache.ClusterKey{Namespace: "team-a", Name: "orders"})
	if dr := in.Drills; !ok || !in.HasDrills || dr.Persistent || !dr.RecordedSince.Equal(now) || dr.Recorded != 1 ||
		!dr.LastPassedAt.Equal(now.Add(-49*time.Minute)) {
		t.Errorf("diagnostics drills = %+v (%v, %v)", in.Drills, in.HasDrills, ok)
	}
}

// A drill instance the sweep cannot verify (its CA is not in the bundle)
// says so in the observation, so the promoted check names the cause
// instead of waiting silently until the time limit.
func TestDrillObservationSaysWhyThereIsNoReading(t *testing.T) {
	scheme, err := cache.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects()...)
	for _, index := range cache.Indexes() {
		builder = builder.WithIndex(index.Object, index.Field, index.Func)
	}
	fc := &fakeCache{reader: builder.Build(), namespaces: []string{"team-a"}}
	for _, tt := range []struct {
		state   viewmodel.StatusReadState
		problem string
	}{
		{viewmodel.StatusReadTLSError, sweep.ReasonCANotInBundle},
		{viewmodel.StatusReadPending, ""},
	} {
		snaps := sweepSnaps()
		snaps[sweep.Key{Namespace: "team-a", Name: "orders"}] = sweep.Snapshot{SweptAt: now, Interval: 30 * time.Second, Instances: map[string]sweep.Instance{
			"orders-1": {State: tt.state, Reason: sweep.ReasonCANotInBundle, At: now, Since: now},
		}}
		svc := New(Options{Cache: fc, Sweep: fakeSweep{snaps: snaps}, Now: func() time.Time { return now }})
		o, err := svc.DrillObservation(context.Background(), "team-a", "orders")
		if err != nil || o.Reading != nil || o.ReadProblem != tt.problem {
			t.Errorf("%s: observation = %+v, %v; want problem %q", tt.state, o, err, tt.problem)
		}
	}
}
