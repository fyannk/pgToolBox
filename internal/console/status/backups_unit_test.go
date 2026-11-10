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

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

func completedBackup(name string, created, stopped time.Time, owner *metav1.OwnerReference) *cnpgv1.Backup {
	b := &cnpgv1.Backup{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: name, CreationTimestamp: metav1.NewTime(created)},
		Spec:       cnpgv1.BackupSpec{Cluster: cnpgv1.LocalObjectReference{Name: "reborn"}, Method: "plugin"},
		Status:     cnpgv1.BackupStatus{Phase: cnpgv1.BackupPhaseCompleted, StoppedAt: new(metav1.NewTime(stopped))},
	}
	if owner != nil {
		b.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	return b
}

// TestBackupsPredatingClusterAreNotItsRecency: a Cluster deleted and
// recreated under the same name must not inherit the old Cluster's
// "last backup 3h ago". Backups created before it, or owned by a Cluster
// with another UID, are listed and labelled, never counted.
func TestBackupsPredatingClusterAreNotItsRecency(t *testing.T) {
	const rebornUID = types.UID("5e1f0c2a-0000-4000-8000-000000000001")
	created := now.Add(-time.Hour)
	cluster := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "reborn", UID: rebornUID, CreationTimestamp: metav1.NewTime(created)},
		Spec: cnpgv1.ClusterSpec{Instances: 1, Plugins: []cnpgv1.PluginConfiguration{{
			Name: "barman-cloud.cloudnative-pg.io", IsWALArchiver: new(true), Parameters: map[string]string{"barmanObjectName": "store"},
		}}},
		Status: cnpgv1.ClusterStatus{Phase: "Cluster in healthy state", Instances: 1, ReadyInstances: 1, CurrentPrimary: "reborn-1"},
	}
	otherOwner := &metav1.OwnerReference{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster", Name: "reborn", UID: "an-earlier-cluster"}
	ownOwner := &metav1.OwnerReference{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster", Name: "reborn", UID: rebornUID}

	tests := []struct {
		name          string
		backups       []client.Object
		wantCompleted *time.Time
		wantPrior     map[string]bool
	}{
		{
			name:      "a backup created before the Cluster is not its last backup",
			backups:   []client.Object{completedBackup("old", now.Add(-4*time.Hour), now.Add(-3*time.Hour), nil)},
			wantPrior: map[string]bool{"old": true},
		},
		{
			name:      "a backup owned by a Cluster with another UID is not its last backup",
			backups:   []client.Object{completedBackup("foreign", now.Add(-30*time.Minute), now.Add(-20*time.Minute), otherOwner)},
			wantPrior: map[string]bool{"foreign": true},
		},
		{
			name: "a backup of this Cluster still counts beside an older one",
			backups: []client.Object{
				completedBackup("old", now.Add(-4*time.Hour), now.Add(-3*time.Hour), nil),
				completedBackup("own", now.Add(-30*time.Minute), now.Add(-10*time.Minute), ownOwner),
			},
			wantCompleted: new(now.Add(-10 * time.Minute)),
			wantPrior:     map[string]bool{"old": true, "own": false},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme, err := cache.NewScheme()
			if err != nil {
				t.Fatal(err)
			}
			builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append([]client.Object{cluster.DeepCopy()}, tt.backups...)...)
			for _, index := range cache.Indexes() {
				builder = builder.WithIndex(index.Object, index.Field, index.Func)
			}
			fc := &fakeCache{reader: builder.Build(), namespaces: []string{"team-a"}}
			svc := New(Options{Cache: fc, Sweep: fakeSweep{snaps: sweepSnaps()}, Now: func() time.Time { return now }})

			got, err := svc.Backups(context.Background(), "team-a", "reborn")
			if err != nil {
				t.Fatal(err)
			}
			last := got.Summary.LastCompletedAt
			switch {
			case tt.wantCompleted == nil && last != nil:
				t.Errorf("last completed = %v, want none: a predating Backup leaked into recency", *last)
			case tt.wantCompleted != nil && (last == nil || !last.Equal(*tt.wantCompleted)):
				t.Errorf("last completed = %v, want %v", last, *tt.wantCompleted)
			}
			if tt.wantCompleted == nil && got.Summary.Health == viewmodel.HealthOK {
				t.Errorf("health = ok with no backup of this Cluster")
			}
			if len(got.Items) != len(tt.wantPrior) {
				t.Fatalf("items = %d, want %d: predating Backups are listed, not hidden", len(got.Items), len(tt.wantPrior))
			}
			for _, row := range got.Items {
				if row.PredatesCluster != tt.wantPrior[row.Name] {
					t.Errorf("%s: predatesCluster = %v, want %v", row.Name, row.PredatesCluster, tt.wantPrior[row.Name])
				}
			}
			for _, m := range got.RecoveryTimeline.Markers {
				if tt.wantPrior[m.Name] {
					t.Errorf("marker %s comes from a predating Backup", m.Name)
				}
			}
		})
	}
}

func TestBackupCadence(t *testing.T) {
	scheduled := func(last, next time.Time, suspend bool) *cnpgv1.ScheduledBackup {
		return &cnpgv1.ScheduledBackup{
			Spec:   cnpgv1.ScheduledBackupSpec{Suspend: new(suspend)},
			Status: cnpgv1.ScheduledBackupStatus{LastScheduleTime: new(metav1.NewTime(last)), NextScheduleTime: new(metav1.NewTime(next))},
		}
	}
	daily := scheduled(now.Add(-2*time.Hour), now.Add(22*time.Hour), false)
	cluster := &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(now.Add(-30 * 24 * time.Hour))}}
	young := &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(now.Add(-time.Hour))}}
	tests := []struct {
		name        string
		cluster     *cnpgv1.Cluster
		scheduled   []*cnpgv1.ScheduledBackup
		last        *time.Time
		wantBasis   string
		wantOverdue bool
	}{
		{"daily schedule, backup 20h ago", cluster, []*cnpgv1.ScheduledBackup{daily}, new(now.Add(-20 * time.Hour)), "schedule", false},
		{"daily schedule, backup 3d ago is overdue", cluster, []*cnpgv1.ScheduledBackup{daily}, new(now.Add(-72 * time.Hour)), "schedule", true},
		{"a suspended schedule gives no cadence", cluster, []*cnpgv1.ScheduledBackup{scheduled(now.Add(-2*time.Hour), now.Add(22*time.Hour), true)}, new(now.Add(-72 * time.Hour)), "default", false},
		{"no schedule: one backup 8d ago is not ok forever", cluster, nil, new(now.Add(-8 * 24 * time.Hour)), "default", true},
		{"no schedule: one backup 2d ago", cluster, nil, new(now.Add(-48 * time.Hour)), "default", false},
		{"no backup on an old Cluster is overdue", cluster, nil, nil, "default", true},
		{"no backup on a young Cluster is not yet overdue", young, nil, nil, "default", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := backupCadence(&gathered{cluster: tt.cluster, scheduled: tt.scheduled}, now, tt.last)
			if got.Basis != tt.wantBasis || got.Overdue != tt.wantOverdue {
				t.Errorf("cadence = %+v, want basis %s overdue %v", *got, tt.wantBasis, tt.wantOverdue)
			}
		})
	}
}
