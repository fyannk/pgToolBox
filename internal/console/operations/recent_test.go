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

package operations

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/drills"
	"github.com/fyannk/pgToolBox/internal/console/ops"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

func backupCR(name string, annotations map[string]string) *cnpgv1.Backup {
	return &cnpgv1.Backup{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: name, Annotations: annotations},
		Spec:       cnpgv1.BackupSpec{Cluster: cnpgv1.LocalObjectReference{Name: "orders"}},
	}
}

// recentOf lists the Recent of a fresh service (a restarted replica, or
// another one) on st.
func recentOf(t *testing.T, st ClusterState) viewmodel.Operations {
	t.Helper()
	var log bytes.Buffer
	v, err := service(st, &fakeWriter{c: st.Cluster, pods: pods()}, &log).Offers(context.Background(), principal(), authz.Capabilities{View: true}, "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestRecentSurvivesARestart: what one replica issued, another (or the
// same one restarted) lists under the same handle, from the markers the
// writes left on the objects.
func TestRecentSurvivesARestart(t *testing.T) {
	c := cluster()
	w := &fakeWriter{c: c, pods: pods()}
	var log bytes.Buffer
	issuer := service(ClusterState{Cluster: c, Pods: pods()}, w, &log)
	accepted := make([]string, 0, 2)
	for _, req := range []viewmodel.OperationRequest{
		{Kind: viewmodel.OperationBackup, Confirmation: "orders"},
		{Kind: viewmodel.OperationRestart, Confirmation: "orders"},
	} {
		a, err := issuer.Request(context.Background(), principal(), "team-a", "orders", req)
		if err != nil {
			t.Fatal(err)
		}
		accepted = append(accepted, a.OperationID)
	}
	// What the API server now holds: the Backup the request created, and
	// the Cluster with the annotation the restart set.
	b := w.backups[0].DeepCopy()
	b.Name = b.GenerateName + "abcde"
	held := c.DeepCopy()
	key, value, _ := strings.Cut(w.patches[0], "=")
	held.Annotations = map[string]string{key: value}

	v := recentOf(t, ClusterState{Cluster: held, Pods: pods(), Backups: []*cnpgv1.Backup{b}, BackupsListed: true})
	if len(v.Recent) != 2 || len(v.RecentGaps) != 0 {
		t.Fatalf("Recent = %+v, gaps %v; want the backup and the restart", v.Recent, v.RecentGaps)
	}
	got := map[string]viewmodel.Operation{}
	for _, op := range v.Recent {
		got[op.OperationID] = op
	}
	for _, id := range accepted {
		op, ok := got[id]
		if !ok {
			t.Errorf("operation %s is not listed under the handle the request returned", id)
			continue
		}
		if op.RecordedIn != viewmodel.RecordCluster || op.RecordedFrom == "" {
			t.Errorf("%s recorded in %q from %q, want the cluster and its marker", op.Kind, op.RecordedIn, op.RecordedFrom)
		}
	}
}

func TestRecentFromMarkers(t *testing.T) {
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	tests := []struct {
		name        string
		annotations map[string]string
		backups     []*cnpgv1.Backup
		listed      bool
		want        []viewmodel.OperationKind
		gap         bool
	}{
		{
			name: "every marker, newest first",
			annotations: map[string]string{
				ops.RestartAnnotation: at(time.Hour),
				ops.ReloadAnnotation:  now.Add(-time.Minute).Format(ops.CNPGTimestamp),
			},
			backups: []*cnpgv1.Backup{backupCR("ondemand-1", map[string]string{ops.AnnotationRequestedAt: at(time.Second)})},
			listed:  true,
			want:    []viewmodel.OperationKind{viewmodel.OperationBackup, viewmodel.OperationReload, viewmodel.OperationRestart},
		},
		{
			name:    "a Backup the console did not mark is not an operation it lists",
			backups: []*cnpgv1.Backup{backupCR("orders-daily-1", map[string]string{"cnpg.io/scheduled-backup": "daily"}), backupCR("by-hand", nil)},
			listed:  true,
		},
		{
			name: "markers in another shape are skipped",
			annotations: map[string]string{
				ops.RestartAnnotation: "yesterday",
				ops.ReloadAnnotation:  now.Format(time.RFC3339Nano) + strings.Repeat("0", 64),
			},
			backups: []*cnpgv1.Backup{backupCR("ondemand-2", map[string]string{ops.AnnotationRequestedAt: "now"})},
			listed:  true,
		},
		{
			name:    "Backups the console cannot read are a gap, never silence",
			backups: nil, listed: false,
			gap: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := cluster()
			c.Annotations = tc.annotations
			v := recentOf(t, ClusterState{Cluster: c, Pods: pods(), Backups: tc.backups, BackupsListed: tc.listed})
			var kinds []viewmodel.OperationKind
			for _, op := range v.Recent {
				kinds = append(kinds, op.Kind)
				if op.RecordedIn != viewmodel.RecordCluster {
					t.Errorf("%s recorded in %q", op.Kind, op.RecordedIn)
				}
				if _, err := ParseID(op.OperationID); err != nil {
					t.Errorf("%s listed under a handle the tracker refuses", op.Kind)
				}
			}
			if strings.Join(kindStrings(kinds), ",") != strings.Join(kindStrings(tc.want), ",") {
				t.Errorf("Recent kinds = %v, want %v", kinds, tc.want)
			}
			if gap := len(v.RecentGaps) > 0; gap != tc.gap {
				t.Errorf("gaps = %v, want a gap: %v", v.RecentGaps, tc.gap)
			}
		})
	}
}

func kindStrings(ks []viewmodel.OperationKind) []string {
	out := make([]string, 0, len(ks))
	for _, k := range ks {
		out = append(out, string(k))
	}
	return out
}

// A promote leaves no marker the console can tell from a failover: only
// the replica that issued it lists it, and says so.
func TestRecentPromoteIsThisReplicasOnly(t *testing.T) {
	c := cluster()
	var log bytes.Buffer
	st := ClusterState{Cluster: c, Pods: pods(), BackupsListed: true}
	svc := service(st, &fakeWriter{c: c, pods: pods()}, &log)
	if _, err := svc.Request(context.Background(), principal(), "team-a", "orders", viewmodel.OperationRequest{Kind: viewmodel.OperationPromote, Target: "orders-2", Confirmation: "orders-2"}); err != nil {
		t.Fatal(err)
	}
	v, err := svc.Offers(context.Background(), principal(), authz.Capabilities{View: true}, "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Recent) != 1 || v.Recent[0].RecordedIn != viewmodel.RecordReplica || v.Recent[0].RecordedFrom == "" {
		t.Fatalf("Recent = %+v, want the promote, from this replica's memory", v.Recent)
	}
	held := c.DeepCopy()
	held.Status.TargetPrimary, held.Status.TargetPrimaryTimestamp = "orders-2", now.Format(ops.CNPGTimestamp)
	if v := recentOf(t, ClusterState{Cluster: held, Pods: pods(), BackupsListed: true}); len(v.Recent) != 0 {
		t.Errorf("a fresh replica lists %+v from status.targetPrimary, which a failover also sets", v.Recent)
	}
}

// An operation both remembered and on the cluster is listed once, as the
// cluster's; the list is capped at RecentPerCluster.
func TestRecentMergesAndCaps(t *testing.T) {
	c := cluster()
	c.Annotations = map[string]string{ops.RestartAnnotation: now.Format(time.RFC3339)}
	var log bytes.Buffer
	st := ClusterState{Cluster: c, Pods: pods(), BackupsListed: true}
	svc := service(st, &fakeWriter{c: c, pods: pods()}, &log)
	svc.opts.RecentPerCluster = 2
	svc.remember(ID{Kind: viewmodel.OperationRestart, Namespace: "team-a", Cluster: "orders", Marker: now.Format(time.RFC3339), AcceptedAt: now.Unix()})
	svc.remember(ID{Kind: viewmodel.OperationReload, Namespace: "team-a", Cluster: "orders", Marker: "m1", AcceptedAt: now.Unix() - 10})
	svc.remember(ID{Kind: viewmodel.OperationReload, Namespace: "team-a", Cluster: "orders", Marker: "m2", AcceptedAt: now.Unix() - 20})
	v, err := svc.Offers(context.Background(), principal(), authz.Capabilities{View: true}, "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Recent) != 2 {
		t.Fatalf("Recent = %+v, want 2 (the cap)", v.Recent)
	}
	if v.Recent[0].Kind != viewmodel.OperationRestart || v.Recent[0].RecordedIn != viewmodel.RecordCluster {
		t.Errorf("first = %s in %q, want the restart, once, as the cluster's", v.Recent[0].Kind, v.Recent[0].RecordedIn)
	}
	if v.Recent[1].Kind != viewmodel.OperationReload || v.Recent[1].RecordedIn != viewmodel.RecordReplica {
		t.Errorf("second = %s in %q, want the newer remembered reload", v.Recent[1].Kind, v.Recent[1].RecordedIn)
	}
}

func TestRecentDrillsFromTheRecord(t *testing.T) {
	c := cluster()
	c.CreationTimestamp = metav1.NewTime(now.Add(-24 * time.Hour))
	store := drills.NewStore(nil, nil, now.Add(-time.Hour))
	src := drills.SourceKey{Namespace: "team-a", Name: "orders"}
	store.Put(drills.Record{Drill: drills.Key{Namespace: "drills", Name: "orders-drill-1"}, Source: src, SourceUID: "u1", StartedAt: now.Add(-time.Minute), Result: viewmodel.DrillRunning})
	// A drill of an earlier Cluster of the same name is not this one's.
	store.Put(drills.Record{Drill: drills.Key{Namespace: "drills", Name: "orders-drill-0"}, Source: src, SourceUID: "old", StartedAt: now.Add(-48 * time.Hour), Result: viewmodel.DrillPassed})
	svc := New(Options{Now: func() time.Time { return now }, Drills: &DrillOptions{Records: store}})
	entries, gaps := svc.recentOf(ClusterState{Cluster: c, BackupsListed: true}, nil)
	if len(entries) != 1 || entries[0].id.Created != "orders-drill-1" || entries[0].id.CreatedNamespace != "drills" || entries[0].in != viewmodel.RecordCluster {
		t.Fatalf("entries = %+v, want the drill of this Cluster", entries)
	}
	if len(gaps) != 1 || !strings.Contains(gaps[0], "restore drills") {
		t.Errorf("gaps = %v, want the in-memory drill record named", gaps)
	}
}
