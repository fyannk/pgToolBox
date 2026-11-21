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
	"strconv"
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/drills"
	"github.com/fyannk/pgToolBox/internal/console/ops"
	"github.com/fyannk/pgToolBox/internal/console/opstate"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// backupCR is a Backup created at created.
func backupCR(name string, created time.Time, annotations map[string]string) *cnpgv1.Backup {
	return &cnpgv1.Backup{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: name, Annotations: annotations, CreationTimestamp: metav1.NewTime(created)},
		Spec:       cnpgv1.BackupSpec{Cluster: cnpgv1.LocalObjectReference{Name: "orders"}},
	}
}

// watching is a persistent timeline that saw no primary change.
var watching = opstate.Observed{Enabled: true, Persistent: true, Since: now.Add(-30 * 24 * time.Hour)}

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
	b.CreationTimestamp = metav1.NewTime(now.Add(time.Second))
	held := c.DeepCopy()
	key, value, _ := strings.Cut(w.patches[0], "=")
	held.Annotations = map[string]string{key: value}

	v := recentOf(t, ClusterState{Cluster: held, Pods: pods(), Backups: []*cnpgv1.Backup{b}, BackupsListed: true, Observed: watching})
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
			backups: []*cnpgv1.Backup{backupCR("ondemand-1", now.Add(-time.Second), map[string]string{ops.AnnotationRequestedAt: at(time.Second)})},
			listed:  true,
			want:    []viewmodel.OperationKind{viewmodel.OperationBackup, viewmodel.OperationReload, viewmodel.OperationRestart},
		},
		{
			name: "markers in another shape are skipped; the Backup is still listed, by its creation",
			annotations: map[string]string{
				ops.RestartAnnotation: "yesterday",
				ops.ReloadAnnotation:  now.Format(time.RFC3339Nano) + strings.Repeat("0", 64),
			},
			backups: []*cnpgv1.Backup{backupCR("ondemand-2", now, map[string]string{ops.AnnotationRequestedAt: "now"})},
			listed:  true,
			want:    []viewmodel.OperationKind{viewmodel.OperationBackup},
		},
		{
			name: "a Backup without a creation time is not listed",
			backups: []*cnpgv1.Backup{
				backupCR("no-creation", time.Time{}, map[string]string{ops.AnnotationRequestedAt: at(0)}),
			},
			listed: true,
		},
		{
			name:        "a restart or reload marker dated in the future is skipped",
			annotations: map[string]string{ops.RestartAnnotation: "2099-01-01T00:00:00Z", ops.ReloadAnnotation: now.Add(2 * time.Minute).Format(ops.CNPGTimestamp)},
			listed:      true,
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
			v := recentOf(t, ClusterState{Cluster: c, Pods: pods(), Backups: tc.backups, BackupsListed: tc.listed, Observed: watching})
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
	st := ClusterState{Cluster: c, Pods: pods(), BackupsListed: true, Observed: watching}
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
	if len(v.RecentGaps) != 1 || !strings.HasPrefix(v.RecentGaps[0], "1 older entries are not listed") {
		t.Errorf("gaps = %v, want the one entry the cap cut counted", v.RecentGaps)
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
	entries, gaps := svc.recentOf(ClusterState{Cluster: c, BackupsListed: true, Observed: watching}, nil)
	if len(entries) != 1 || entries[0].id.Created != "orders-drill-1" || entries[0].id.CreatedNamespace != "drills" || entries[0].in != viewmodel.RecordCluster {
		t.Fatalf("entries = %+v, want the drill of this Cluster", entries)
	}
	if len(gaps) != 1 || !strings.Contains(gaps[0], "restore drills") {
		t.Errorf("gaps = %v, want the in-memory drill record named", gaps)
	}
}

// A restart found on the Cluster past its deadline is closed, not judged
// on the live state: a months-old restartedAt during a later switchover
// is not an unfinished restart. Its request is the marker's, not the
// console's, to attribute.
func TestRecentFromTheClusterIsClosedPastItsDeadline(t *testing.T) {
	tests := []struct {
		name  string
		age   time.Duration
		phase string
		want  viewmodel.OperationPhase
	}{
		{"an old marker during a switchover", 90 * 24 * time.Hour, "Switchover in progress", viewmodel.OpPhaseNotObservable},
		{"an old marker on a healthy Cluster", 90 * 24 * time.Hour, "Cluster in healthy state", viewmodel.OpPhaseNotObservable},
		{"a marker within its deadline is still followed", time.Minute, "Switchover in progress", viewmodel.OpPhaseActing},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := cluster()
			c.Annotations = map[string]string{ops.RestartAnnotation: now.Add(-tc.age).Format(time.RFC3339)}
			c.Status.Phase = tc.phase
			v := recentOf(t, ClusterState{Cluster: c, Pods: pods(), BackupsListed: true, Observed: watching})
			if len(v.Recent) != 1 {
				t.Fatalf("Recent = %+v", v.Recent)
			}
			op := v.Recent[0]
			if op.Phase != tc.want {
				t.Errorf("phase = %s, want %s (steps %+v)", op.Phase, tc.want, op.Steps)
			}
			if src := op.Steps[0].Source; src != op.RecordedFrom || src == "console" {
				t.Errorf("request step attributed to %q, want the marker %q", src, op.RecordedFrom)
			}
		})
	}
}

// Every Backup of the Cluster is listed, whoever created it, with its
// origin named; the console's own keeps the handle its request returned.
func TestRecentListsEveryBackup(t *testing.T) {
	created := now.Add(-10 * time.Minute)
	scheduled := backupCR("orders-daily-1", created, map[string]string{})
	scheduled.Labels = map[string]string{scheduledBackupLabel: "daily"}
	scheduled.Status.Phase = cnpgv1.BackupPhaseCompleted
	owned := backupCR("orders-nightly-1", created, nil)
	owned.OwnerReferences = []metav1.OwnerReference{{APIVersion: "postgresql.cnpg.io/v1", Kind: "ScheduledBackup", Name: "nightly"}}
	other := backupCR("billing-1", created, nil)
	other.Spec.Cluster.Name = "billing"
	tests := []struct {
		name     string
		backup   *cnpgv1.Backup
		listed   bool
		from     string
		accepted time.Time
		phase    viewmodel.OperationPhase
	}{
		{
			name:   "the console's requested-at keeps the issuing replica's handle",
			backup: backupCR("ondemand-1", created, map[string]string{ops.AnnotationRequestedAt: created.Add(-2 * time.Second).Format(time.RFC3339)}),
			listed: true, from: "metadata.annotations[" + ops.AnnotationRequestedAt + "]", accepted: created.Add(-2 * time.Second),
			phase: viewmodel.OpPhaseAccepted,
		},
		{
			name:   "a requested-at far from the creation is not its request: listed as any client's, by its creation",
			backup: backupCR("forged", created, map[string]string{ops.AnnotationRequestedAt: "2099-01-01T00:00:00Z"}),
			listed: true, from: "(on-demand, any client", accepted: created, phase: viewmodel.OpPhaseAccepted,
		},
		{
			name:   "an on-demand Backup of any client",
			backup: backupCR("by-hand", created, nil),
			listed: true, from: "Backup by-hand (on-demand, any client", accepted: created, phase: viewmodel.OpPhaseAccepted,
		},
		{
			name:   "a scheduled Backup, by CloudNativePG's label, followed through the judge",
			backup: scheduled,
			listed: true, from: "(created by ScheduledBackup daily: metadata.labels[cnpg.io/scheduled-backup])", accepted: created,
			phase: viewmodel.OpPhaseSettled,
		},
		{
			name:   "a scheduled Backup, by its owner reference",
			backup: owned,
			listed: true, from: "(created by ScheduledBackup nightly: metadata.ownerReferences)", accepted: created,
			phase: viewmodel.OpPhaseAccepted,
		},
		{name: "a Backup of another Cluster is not listed", backup: other},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := ClusterState{Cluster: cluster(), Pods: pods(), Backups: []*cnpgv1.Backup{tc.backup}, BackupsListed: true, Observed: watching}
			v := recentOf(t, st)
			if !tc.listed {
				if len(v.Recent) != 0 {
					t.Fatalf("Recent = %+v, want nothing", v.Recent)
				}
				return
			}
			if len(v.Recent) != 1 {
				t.Fatalf("Recent = %+v, want the Backup", v.Recent)
			}
			op := v.Recent[0]
			if op.Kind != viewmodel.OperationBackup || op.Created != tc.backup.Name || op.RecordedIn != viewmodel.RecordCluster {
				t.Errorf("listed %s %s in %q", op.Kind, op.Created, op.RecordedIn)
			}
			if !strings.Contains(op.RecordedFrom, tc.from) {
				t.Errorf("recorded from %q, want %q", op.RecordedFrom, tc.from)
			}
			if !op.AcceptedAt.Equal(tc.accepted.Truncate(time.Second)) {
				t.Errorf("accepted at %s, want %s", op.AcceptedAt, tc.accepted)
			}
			if op.Phase != tc.phase {
				t.Errorf("phase = %s, want %s", op.Phase, tc.phase)
			}
			// The tracker follows it under the same handle.
			id, err := ParseID(op.OperationID)
			if err != nil {
				t.Fatalf("handle refused: %v", err)
			}
			var log bytes.Buffer
			got, err := service(st, &fakeWriter{c: st.Cluster, pods: pods()}, &log).Lookup(context.Background(), id)
			if err != nil || got.Phase != tc.phase || got.Created != tc.backup.Name {
				t.Errorf("Lookup = %s %s, %v; want %s", got.Phase, got.Created, err, tc.phase)
			}
		})
	}
}

// The primary changes this console's timeline observed are listed as
// observed, settled entries, merged with the promote this replica
// issued; what the timeline cannot cover is a gap.
func TestRecentPrimaryChanges(t *testing.T) {
	failover := opstate.PrimaryChange{From: "orders-1", To: "orders-2", At: now.Add(-time.Hour), Reported: true, Cause: "failover"}
	unseen := opstate.PrimaryChange{From: "orders-2", To: "orders-3", At: now.Add(-time.Minute), Since: now.Add(-2 * time.Hour)}
	tests := []struct {
		name     string
		observed opstate.Observed
		promote  string // a promote this replica issued, a minute ago
		kinds    []viewmodel.OperationKind
		in       []viewmodel.OperationRecord
		text     string
		gap      string
	}{
		{
			name:     "observed changes, newest first, with what was and was not seen",
			observed: opstate.Observed{Enabled: true, Persistent: true, PrimaryChanges: []opstate.PrimaryChange{unseen, failover}},
			kinds:    []viewmodel.OperationKind{viewmodel.OperationPrimaryChange, viewmodel.OperationPrimaryChange},
			in:       []viewmodel.OperationRecord{viewmodel.RecordTimeline, viewmodel.RecordTimeline},
			text:     "primary changed from orders-2 to orders-3 (a switchover or a failover: the console did not see which); it happened while the console was not looking",
		},
		{
			name:     "the promote this replica issued and the change it caused are one entry",
			observed: opstate.Observed{Enabled: true, Persistent: true, PrimaryChanges: []opstate.PrimaryChange{unseen}},
			promote:  "orders-3",
			kinds:    []viewmodel.OperationKind{viewmodel.OperationPromote},
			in:       []viewmodel.OperationRecord{viewmodel.RecordReplica},
		},
		{
			name:     "a promote to another instance is not the change",
			observed: opstate.Observed{Enabled: true, Persistent: true, PrimaryChanges: []opstate.PrimaryChange{unseen}},
			promote:  "orders-2",
			kinds:    []viewmodel.OperationKind{viewmodel.OperationPrimaryChange, viewmodel.OperationPromote},
			in:       []viewmodel.OperationRecord{viewmodel.RecordTimeline, viewmodel.RecordReplica},
		},
		{
			name:     "a timeline not kept across restarts is a gap",
			observed: opstate.Observed{Enabled: true, Since: now.Add(-time.Hour)},
			gap:      "primary changes before " + now.Add(-time.Hour).Format(time.RFC3339) + " are not listed: this console's timeline is not kept across restarts",
		},
		{
			name:     "a timeline whose bounds dropped events is a gap",
			observed: opstate.Observed{Enabled: true, Persistent: true, Evicted: true, Since: now.Add(-time.Hour)},
			gap:      "the timeline's bounds dropped older events",
		},
		{
			name: "no timeline is a gap, never silence",
			gap:  "HISTORY_ENABLED is false",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := cluster()
			var log bytes.Buffer
			svc := service(ClusterState{Cluster: c, Pods: pods(), BackupsListed: true, Observed: tc.observed}, &fakeWriter{c: c, pods: pods()}, &log)
			if tc.promote != "" {
				svc.remember(ID{Kind: viewmodel.OperationPromote, Namespace: "team-a", Cluster: "orders", Instance: tc.promote, Marker: "m", AcceptedAt: now.Add(-90 * time.Second).Unix()})
			}
			v, err := svc.Offers(context.Background(), principal(), authz.Capabilities{View: true}, "team-a", "orders")
			if err != nil {
				t.Fatal(err)
			}
			var kinds []viewmodel.OperationKind
			var in []viewmodel.OperationRecord
			for _, op := range v.Recent {
				kinds, in = append(kinds, op.Kind), append(in, op.RecordedIn)
				if op.Kind != viewmodel.OperationPrimaryChange {
					continue
				}
				if op.Phase != viewmodel.OpPhaseSettled || len(op.Steps) != 1 || op.RecordedFrom == "" {
					t.Errorf("primary change = %+v, want one settled, attributed step", op)
				}
				if _, err := ParseID(op.OperationID); err == nil {
					t.Errorf("a primary change is listed under a handle the tracker follows")
				}
			}
			if strings.Join(kindStrings(kinds), ",") != strings.Join(kindStrings(tc.kinds), ",") || len(in) != len(tc.in) {
				t.Fatalf("Recent = %v in %v, want %v in %v", kinds, in, tc.kinds, tc.in)
			}
			for i := range in {
				if in[i] != tc.in[i] {
					t.Errorf("entry %d recorded in %q, want %q", i, in[i], tc.in[i])
				}
			}
			if tc.text != "" && (len(v.Recent) == 0 || !strings.HasPrefix(v.Recent[0].Steps[0].Text, tc.text)) {
				t.Errorf("text = %q, want %q", v.Recent[0].Steps[0].Text, tc.text)
			}
			if len(v.Recent) > 1 && v.Recent[1].Kind == viewmodel.OperationPrimaryChange && v.Recent[1].Steps[0].Source != "Cluster.status.currentPrimaryTimestamp" {
				t.Errorf("reported change attributed to %q", v.Recent[1].Steps[0].Source)
			}
			if tc.promote == "orders-3" && !strings.Contains(v.Recent[0].RecordedFrom, "its timeline observed") {
				t.Errorf("merged promote recorded from %q, want the observed change named", v.Recent[0].RecordedFrom)
			}
			if got := strings.Join(v.RecentGaps, "|"); (tc.gap == "") != (got == "") || !strings.Contains(got, tc.gap) {
				t.Errorf("gaps = %q, want %q", got, tc.gap)
			}
		})
	}
}

// Scheduled Backups cannot push everything else out silently: the cap
// keeps the newest and counts what it cut.
func TestRecentCapCountsWhatItCut(t *testing.T) {
	c := cluster()
	c.Annotations = map[string]string{ops.RestartAnnotation: now.Add(-48 * time.Hour).Format(time.RFC3339)}
	backups := make([]*cnpgv1.Backup, 0, 25)
	for i := range 25 {
		b := backupCR("orders-hourly-"+strconv.Itoa(i), now.Add(-time.Duration(i)*time.Hour), nil)
		b.Labels = map[string]string{scheduledBackupLabel: "hourly"}
		backups = append(backups, b)
	}
	v := recentOf(t, ClusterState{Cluster: c, Pods: pods(), Backups: backups, BackupsListed: true, Observed: watching})
	if len(v.Recent) != 20 {
		t.Fatalf("Recent has %d entries, want the default cap of 20", len(v.Recent))
	}
	if len(v.RecentGaps) != 1 || v.RecentGaps[0] != "6 older entries are not listed: the list keeps the newest 20" {
		t.Errorf("gaps = %v, want the 5 Backups and the restart the cap cut counted", v.RecentGaps)
	}
}
