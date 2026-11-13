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
	"strings"
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

// TestBackupRecencyFailureAfterAQueuedSuccess (round 20, CNPG P3-3): B2
// is created at 02:00 and waits behind B1, which completes at 02:10; B2
// then fails at 02:30. CloudNativePG never sets stoppedAt on a failure,
// so timing B2 at its creation hid it behind B1's success; its
// completion stamp (SetAsFailed) shows the failure after it.
func TestBackupRecencyFailureAfterAQueuedSuccess(t *testing.T) {
	const uid = types.UID("5e1f0c2a-0000-4000-8000-000000000003")
	t0 := now.Add(-3 * time.Hour)
	cluster := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "reborn", UID: uid, CreationTimestamp: metav1.NewTime(t0.Add(-24 * time.Hour))},
		Spec: cnpgv1.ClusterSpec{Instances: 1, Plugins: []cnpgv1.PluginConfiguration{{
			Name: "barman-cloud.cloudnative-pg.io", IsWALArchiver: new(true), Parameters: map[string]string{"barmanObjectName": "store"},
		}}},
		Status: cnpgv1.ClusterStatus{Phase: "Cluster in healthy state", Instances: 1, ReadyInstances: 1, CurrentPrimary: "reborn-1"},
	}
	owner := &metav1.OwnerReference{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster", Name: "reborn", UID: uid}
	for _, tt := range []struct {
		name       string
		failedAt   *time.Time
		opStarted  *time.Time
		wantFailed bool
		wantAt     *time.Time // nil: failedAt
	}{
		{"failed after the success", new(t0.Add(30 * time.Minute)), nil, true, nil},
		{"failed before the success", new(t0.Add(5 * time.Minute)), nil, false, nil},
		{"no stamp: its creation, before the success", nil, nil, false, nil},
		// Round 22, CNPG P3-2: B2 waits behind B1, the operator starts it
		// at 02:15 and its instance, 20 min behind, stamps the failure at
		// 01:56 on its own clock. The skewed stamp is dropped
		// (ClockBehind); timed at its creation (02:00) the failure hid
		// behind B1's 02:10 success, timed at the operator's start it
		// is after it.
		{"a skewed failure stamp: the operator's start, after the success", new(t0.Add(-4 * time.Minute)), new(t0.Add(15 * time.Minute)), true, new(t0.Add(15 * time.Minute))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b1 := completedBackup("b1", t0.Add(-time.Minute), t0.Add(10*time.Minute), owner)
			b1.UID = "b1-uid"
			b2 := &cnpgv1.Backup{
				ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "b2", UID: "b2-uid", CreationTimestamp: metav1.NewTime(t0), OwnerReferences: []metav1.OwnerReference{*owner}},
				Spec:       cnpgv1.BackupSpec{Cluster: cnpgv1.LocalObjectReference{Name: "reborn"}, Method: "plugin"},
				Status:     cnpgv1.BackupStatus{Phase: cnpgv1.BackupPhaseFailed, Error: "boom"},
			}
			if tt.failedAt != nil {
				b2.Status.ReconciliationTerminatedAt = new(metav1.NewTime(*tt.failedAt))
			}
			if tt.opStarted != nil {
				b2.Status.ReconciliationStartedAt = new(metav1.NewTime(*tt.opStarted))
			}
			scheme, err := cache.NewScheme()
			if err != nil {
				t.Fatal(err)
			}
			builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster.DeepCopy(), b1, b2)
			for _, index := range cache.Indexes() {
				builder = builder.WithIndex(index.Object, index.Field, index.Func)
			}
			fc := &fakeCache{reader: builder.Build(), namespaces: []string{"team-a"}}
			svc := New(Options{Cache: fc, Sweep: fakeSweep{snaps: sweepSnaps()}, Now: func() time.Time { return now }})
			got, err := svc.Backups(context.Background(), "team-a", "reborn")
			if err != nil {
				t.Fatal(err)
			}
			r := got.Summary.BackupRecency
			if failed := r.LastFailedAt != nil; failed != tt.wantFailed {
				t.Fatalf("last failed %v, want a failure shown: %v", r.LastFailedAt, tt.wantFailed)
			}
			want := tt.wantAt
			if want == nil {
				want = tt.failedAt
			}
			if tt.wantFailed && (!r.LastFailedAt.Equal(*want) || r.Health != viewmodel.HealthWarning) {
				t.Errorf("failure at %v health %s, want %v warning", r.LastFailedAt, r.Health, *want)
			}
		})
	}
}

// TestBackupRecencyReadsPlausibleTimes (round 19, CNPG P3-1): the
// Overview's last backup and the timeline read a Backup's stop through
// cnpgstatus.BackupTimesOf. A plugin's unset stop (1970) is not "last
// completed 1970, overdue", a stop in the future is not a backup that
// never ages (a false ok), and a Completed Backup with no plausible stop
// is timed by CloudNativePG's completion stamp, attributed to that field.
func TestBackupRecencyReadsPlausibleTimes(t *testing.T) {
	const uid = types.UID("5e1f0c2a-0000-4000-8000-000000000002")
	created := now.Add(-time.Hour)
	cluster := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "reborn", UID: uid, CreationTimestamp: metav1.NewTime(created)},
		Spec: cnpgv1.ClusterSpec{Instances: 1, Plugins: []cnpgv1.PluginConfiguration{{
			Name: "barman-cloud.cloudnative-pg.io", IsWALArchiver: new(true), Parameters: map[string]string{"barmanObjectName": "store"},
		}}},
		Status: cnpgv1.ClusterStatus{Phase: "Cluster in healthy state", Instances: 1, ReadyInstances: 1, CurrentPrimary: "reborn-1"},
	}
	owner := &metav1.OwnerReference{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster", Name: "reborn", UID: uid}
	stamp := now.Add(-10 * time.Minute)
	// An instance clock 20 min behind: the operator started the Backup a
	// second after its creation, the instance stopped it and stamped it
	// Completed on its own clock, more than BackupClockSkew before the
	// creation.
	opStarted := now.Add(-30*time.Minute + time.Second)
	skewedStop, skewedStamp := now.Add(-20*time.Minute-20*time.Minute), now.Add(-20*time.Minute-20*time.Minute+3*time.Second)
	created30 := now.Add(-30 * time.Minute)
	for _, tt := range []struct {
		name          string
		created       time.Time // zero: 30 min ago
		stopped       time.Time
		stamp         *time.Time
		opStarted     *time.Time
		wantCompleted *time.Time
		wantSource    string
	}{
		{"a plugin's unset stop is not 1970", time.Time{}, time.Unix(0, 0), nil, nil, nil, "Backup.status"},
		{"a stop in the future is not a fresh backup", time.Time{}, now.Add(1000 * 24 * time.Hour), nil, nil, nil, "Backup.status"},
		{"no plausible stop: CloudNativePG's completion stamp", time.Time{}, time.Unix(0, 0), &stamp, nil, &stamp, backupStampSource},
		{"a plausible stop", time.Time{}, now.Add(-20 * time.Minute), &stamp, &opStarted, new(now.Add(-20 * time.Minute)), "Backup.status"},
		{"an instance clock behind: neither its stop nor its stamp, its creation", time.Time{}, skewedStop, &skewedStamp, &opStarted, &created30, backupCreatedSource},
		// An honest instance stops at -1m while the operator, 4.5 min
		// ahead, stamped its start in the console's future: the times are
		// checked against the creation only, and the stop is true.
		{"an operator clock ahead: the instance's stop", now.Add(-4 * time.Minute), now.Add(-time.Minute), new(now.Add(-time.Minute + 3*time.Second)), new(now.Add(4*time.Minute + 30*time.Second)), new(now.Add(-time.Minute)), "Backup.status"},
		// An operator 2 min behind the API server started it before its
		// creation, and the instance runs 20 min behind: its creation.
		{"an operator start before the creation, an instance behind: its creation", time.Time{}, now.Add(-50 * time.Minute), new(now.Add(-50*time.Minute + 3*time.Second)), new(now.Add(-32 * time.Minute)), &created30, backupCreatedSource},
		// A status restored with the object (Velero): every time is older
		// than the creation, so none is plausible and it is not counted,
		// never timed at its restore.
		{"a restored status: not counted", time.Time{}, now.Add(-72 * time.Hour), new(now.Add(-72*time.Hour + 3*time.Second)), new(now.Add(-73 * time.Hour)), nil, "Backup.status"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			created := tt.created
			if created.IsZero() {
				created = created30
			}
			b := completedBackup("b", created, tt.stopped, owner)
			b.UID = "b-uid"
			if tt.stamp != nil {
				b.Status.ReconciliationTerminatedAt = new(metav1.NewTime(*tt.stamp))
			}
			if tt.opStarted != nil {
				b.Status.ReconciliationStartedAt = new(metav1.NewTime(*tt.opStarted))
			}
			scheme, err := cache.NewScheme()
			if err != nil {
				t.Fatal(err)
			}
			builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster.DeepCopy(), b)
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
				t.Errorf("last completed = %v, want none", *last)
			case tt.wantCompleted != nil && (last == nil || !last.Equal(*tt.wantCompleted)):
				t.Errorf("last completed = %v, want %v", last, *tt.wantCompleted)
			}
			if tt.wantCompleted == nil && got.Summary.Health == viewmodel.HealthOK {
				t.Errorf("health ok with no plausible completed backup")
			}
			if src := got.Summary.BackupRecency.Provenance.Source; src != tt.wantSource {
				t.Errorf("provenance %q, want %q", src, tt.wantSource)
			}
			if detail := backupDetail(got.Summary.BackupRecency, now); (tt.wantSource == backupStampSource) != strings.Contains(detail, "CloudNativePG's completion stamp") {
				t.Errorf("detail %q does not attribute the time to its source", detail)
			} else if skewed := tt.wantSource == backupCreatedSource; skewed != strings.Contains(detail, "not plausible") ||
				skewed != strings.Contains(detail, "the instance's clock and the API server's disagree") {
				t.Errorf("detail %q does not say the skewed times are not plausible", detail)
			}
			facts, ok := svc.ClusterFacts(context.Background(), cache.ClusterKey{Namespace: "team-a", Name: "reborn"})
			if !ok || len(facts.Backups) != 1 {
				t.Fatalf("timeline facts: %v %+v", ok, facts.Backups)
			}
			at := facts.Backups[0].StoppedAt
			// The timeline places a Backup at its end: one whose times are
			// skewed has none and is observed by the console.
			wantStop := tt.wantCompleted
			if tt.wantSource == backupCreatedSource {
				wantStop = nil
			}
			switch {
			case wantStop == nil && !at.IsZero():
				t.Errorf("timeline stop %s, want none (observed by the console)", at)
			case wantStop != nil && !at.Equal(*wantStop):
				t.Errorf("timeline stop %s, want %s", at, *wantStop)
			}
		})
	}
}
