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
	"maps"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/sweep"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
)

// TestPromoteReadinessGolden: the offers' state carries, for every
// replica and never for the primary, the five readiness facts in order.
func TestPromoteReadinessGolden(t *testing.T) {
	svc := newScreenService(t)
	st, err := svc.OperationState(context.Background(), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]viewmodel.ReadinessFact{}
	for _, inst := range st.Instances {
		if inst.Name == st.Cluster.Status.CurrentPrimary {
			if inst.Readiness != nil {
				t.Errorf("the primary %s has readiness facts: %+v", inst.Name, inst.Readiness)
			}
			continue
		}
		keys := []viewmodel.ReadinessKey{}
		for _, f := range inst.Readiness {
			keys = append(keys, f.Key)
		}
		want := []viewmodel.ReadinessKey{viewmodel.ReadinessPod, viewmodel.ReadinessStreaming, viewmodel.ReadinessReplayLag, viewmodel.ReadinessSyncState, viewmodel.ReadinessFencing}
		if len(keys) != len(want) {
			t.Fatalf("%s: keys %v, want %v", inst.Name, keys, want)
		}
		for i := range want {
			if keys[i] != want[i] {
				t.Errorf("%s: keys %v, want %v", inst.Name, keys, want)
			}
		}
		got[inst.Name] = inst.Readiness
	}
	if len(got) == 0 {
		t.Fatal("the fixture has no replica")
	}
	golden(t, "promote-readiness-orders", got)
}

// TestPromoteReadinessNeverOkUnread: each fact is unknown when its
// source was not read now (never swept, masked, failed, stale sweep,
// primary unread), and states what it read otherwise.
func TestPromoteReadinessNeverOkUnread(t *testing.T) {
	observed := now.Add(-5 * time.Second)
	lag := func(b int64) *int64 { return &b }
	okRead := viewmodel.StatusRead{State: viewmodel.StatusReadOK}
	replica := &instancestatus.Reading{IsWalReceiverActive: true, ObservedAt: observed}
	primaryOK := sweep.Instance{State: viewmodel.StatusReadOK, Reading: &instancestatus.Reading{
		IsPrimary: true, ObservedAt: observed,
		Standbys: []instancestatus.Standby{{ApplicationName: "db-2", State: "streaming", SyncState: "sync"}},
	}}
	cluster := func(ann map[string]string) *cnpgv1.Cluster {
		return &cnpgv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "db", Annotations: ann},
			Status:     cnpgv1.ClusterStatus{CurrentPrimary: "db-1", TargetPrimary: "db-1"},
		}
	}
	type want struct {
		key    viewmodel.ReadinessKey
		health viewmodel.Health
	}
	tests := []struct {
		name    string
		swept   map[string]sweep.Instance
		sweptAt time.Time
		ann     map[string]string
		states  map[string]viewmodel.SourceState
		row     viewmodel.InstanceRow
		want    []want
	}{
		{
			name:  "ready and synchronous",
			swept: map[string]sweep.Instance{"db-1": primaryOK, "db-2": {State: viewmodel.StatusReadOK, Reading: replica}},
			row: viewmodel.InstanceRow{
				Name: "db-2", Pod: &viewmodel.PodView{Ready: true}, StatusRead: okRead,
				Live: &viewmodel.LiveStatus{WALReceiverActive: true, LagBytes: lag(0)},
			},
			want: []want{
				{viewmodel.ReadinessPod, viewmodel.HealthOK},
				{viewmodel.ReadinessStreaming, viewmodel.HealthOK},
				{viewmodel.ReadinessReplayLag, viewmodel.HealthOK},
				{viewmodel.ReadinessSyncState, viewmodel.HealthOK},
				{viewmodel.ReadinessFencing, viewmodel.HealthOK},
			},
		},
		{
			name:  "lagging, not streaming, not a walsender of the primary",
			swept: map[string]sweep.Instance{"db-1": primaryOK, "db-3": {State: viewmodel.StatusReadOK, Reading: replica}},
			row: viewmodel.InstanceRow{
				Name: "db-3", Pod: &viewmodel.PodView{Ready: false, Waiting: "CrashLoopBackOff"}, StatusRead: okRead,
				Live: &viewmodel.LiveStatus{LagBytes: lag(3 << 20)},
			},
			want: []want{
				{viewmodel.ReadinessPod, viewmodel.HealthWarning},
				{viewmodel.ReadinessStreaming, viewmodel.HealthWarning},
				{viewmodel.ReadinessReplayLag, viewmodel.HealthInfo},
				{viewmodel.ReadinessSyncState, viewmodel.HealthWarning},
			},
		},
		{
			name:  "never swept, no primary report",
			swept: map[string]sweep.Instance{},
			row:   viewmodel.InstanceRow{Name: "db-2", Pod: &viewmodel.PodView{Ready: true}, StatusRead: viewmodel.StatusRead{State: viewmodel.StatusReadPending, Reason: "not swept yet"}},
			want: []want{
				{viewmodel.ReadinessStreaming, viewmodel.HealthUnknown},
				{viewmodel.ReadinessReplayLag, viewmodel.HealthUnknown},
				{viewmodel.ReadinessSyncState, viewmodel.HealthUnknown},
			},
		},
		{
			name: "last read failed: the kept reading is not current",
			swept: map[string]sweep.Instance{
				"db-1": {State: viewmodel.StatusReadUnreachable, Reason: "timed out", Reading: primaryOK.Reading},
				"db-2": {State: viewmodel.StatusReadUnreachable, Reason: "timed out", Reading: replica},
			},
			row: viewmodel.InstanceRow{
				Name: "db-2", Pod: &viewmodel.PodView{Ready: true}, StatusRead: viewmodel.StatusRead{State: viewmodel.StatusReadUnreachable, Reason: "timed out"},
				Live: &viewmodel.LiveStatus{WALReceiverActive: true, LagBytes: lag(0)},
			},
			want: []want{
				{viewmodel.ReadinessStreaming, viewmodel.HealthUnknown},
				{viewmodel.ReadinessReplayLag, viewmodel.HealthUnknown},
				{viewmodel.ReadinessSyncState, viewmodel.HealthUnknown},
			},
		},
		{
			name:  "masked: the manager did not reach PostgreSQL",
			swept: map[string]sweep.Instance{"db-1": primaryOK, "db-2": {State: viewmodel.StatusReadOK, Reading: &instancestatus.Reading{MightBeUnavailableMaskedError: "fenced", ObservedAt: observed}}},
			row: viewmodel.InstanceRow{
				Name: "db-2", Pod: &viewmodel.PodView{Ready: true}, StatusRead: okRead,
				Live: &viewmodel.LiveStatus{PostgresUnread: &viewmodel.PostgresUnread{Instance: "db-2", Error: "fenced"}},
			},
			want: []want{{viewmodel.ReadinessStreaming, viewmodel.HealthUnknown}, {viewmodel.ReadinessReplayLag, viewmodel.HealthUnknown}},
		},
		{
			name:    "stale sweep: an old good reading is not ok now",
			swept:   map[string]sweep.Instance{"db-1": primaryOK, "db-2": {State: viewmodel.StatusReadOK, Reading: replica}},
			sweptAt: now.Add(-10 * time.Minute),
			row: viewmodel.InstanceRow{
				Name: "db-2", Pod: &viewmodel.PodView{Ready: true}, StatusRead: okRead,
				Live: &viewmodel.LiveStatus{WALReceiverActive: true, LagBytes: lag(0)},
			},
			want: []want{
				{viewmodel.ReadinessStreaming, viewmodel.HealthUnknown},
				{viewmodel.ReadinessReplayLag, viewmodel.HealthUnknown},
				{viewmodel.ReadinessSyncState, viewmodel.HealthUnknown},
			},
		},
		{
			name:  "no Pod, fenced",
			swept: map[string]sweep.Instance{"db-1": primaryOK},
			ann:   map[string]string{"cnpg.io/fencedInstances": `["db-2"]`},
			row:   viewmodel.InstanceRow{Name: "db-2", Fenced: true, StatusRead: viewmodel.StatusRead{State: viewmodel.StatusReadNotRunning}},
			want:  []want{{viewmodel.ReadinessPod, viewmodel.HealthCritical}, {viewmodel.ReadinessFencing, viewmodel.HealthCritical}},
		},
		{
			// A broken watch keeps the last Pod and Cluster in the cache:
			// what they say is not a current reading.
			name:   "Pod and Cluster watches stale",
			swept:  map[string]sweep.Instance{"db-1": primaryOK},
			states: map[string]viewmodel.SourceState{cache.SourceClusters: {Source: cache.SourceClusters, State: viewmodel.SourceStale, Reason: "cluster watch lost"}},
			row: viewmodel.InstanceRow{
				Name: "db-2", StatusRead: okRead,
				Pod: &viewmodel.PodView{Ready: true, Provenance: viewmodel.Provenance{Stale: true, StaleReason: "pod watch lost"}},
			},
			want: []want{{viewmodel.ReadinessPod, viewmodel.HealthUnknown}, {viewmodel.ReadinessFencing, viewmodel.HealthUnknown}},
		},
		{
			name:  "fencing annotation unreadable",
			swept: map[string]sweep.Instance{"db-1": primaryOK},
			ann:   map[string]string{"cnpg.io/fencedInstances": `not json`},
			row:   viewmodel.InstanceRow{Name: "db-2", Pod: &viewmodel.PodView{Ready: true}, StatusRead: okRead},
			want:  []want{{viewmodel.ReadinessFencing, viewmodel.HealthUnknown}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at := tt.sweptAt
			if at.IsZero() {
				at = now.Add(-5 * time.Second)
			}
			fc := &fakeCache{namespaces: []string{"team-a"}, states: map[string]viewmodel.SourceState{}}
			maps.Copy(fc.states, tt.states)
			svc := New(Options{Cache: fc, Now: func() time.Time { return now }})
			g := &gathered{
				key: cache.ClusterKey{Namespace: "team-a", Name: "db"}, cluster: cluster(tt.ann), hasSweep: true,
				swept: sweep.Snapshot{SweptAt: at, Interval: 30 * time.Second, Instances: tt.swept},
			}
			facts := svc.promoteReadiness(g, tt.row, now)
			byKey := map[viewmodel.ReadinessKey]viewmodel.ReadinessFact{}
			for _, f := range facts {
				byKey[f.Key] = f
				if f.Text == "" || f.Provenance.Source == "" {
					t.Errorf("%s: no text or source: %+v", f.Key, f)
				}
			}
			for _, w := range tt.want {
				if got := byKey[w.key]; got.Health != w.health {
					t.Errorf("%s: health %s (%q), want %s", w.key, got.Health, got.Text, w.health)
				}
			}
		})
	}
}
