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
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/diag"
	"github.com/fyannk/pgToolBox/internal/console/sweep"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
	"github.com/fyannk/pgToolBox/pkg/diagnose"
)

type runningDiagnostics struct{}

func (runningDiagnostics) Latest(cache.ClusterKey) (diag.Run, bool) { return diag.Run{}, false }
func (runningDiagnostics) State() viewmodel.SourceState {
	return viewmodel.SourceState{State: viewmodel.SourceLive}
}

// A high finding is counted in the overview's Diagnostics line and turns it
// red: the line must agree with the findings counter beside it.
func TestDiagnosticsContributorCountsHigh(t *testing.T) {
	t.Parallel()
	run := &diag.Run{At: time.Now(), Result: diagnose.Result{Findings: []diagnose.Finding{
		{ID: "a", Severity: diagnose.SeverityHigh},
		{ID: "b", Severity: diagnose.SeverityWarning},
	}}}
	c := diagnosticsContributor(run, runningDiagnostics{})
	if c.Health != viewmodel.HealthCritical {
		t.Errorf("health = %s, want critical", c.Health)
	}
	if !strings.HasPrefix(c.Detail, "0 critical, 1 high, 1 warning, 0 note") {
		t.Errorf("detail = %q", c.Detail)
	}
}

// TestPrimaryArchiverFollowsCurrentPrimary: the archiver is
// status.currentPrimary, so a replica cluster's designated primary (a
// standby, isPrimary false) is read rather than ignored; an unread or
// unset currentPrimary leaves the archiver unknown: no instance that
// reports itself primary stands in for it.
func TestPrimaryArchiverFollowsCurrentPrimary(t *testing.T) {
	read := func(primary, archiving bool) sweep.Instance {
		return sweep.Instance{State: viewmodel.StatusReadOK, Reading: &instancestatus.Reading{IsPrimary: primary, IsArchivingWAL: archiving}}
	}
	tests := []struct {
		name      string
		current   string
		instances map[string]sweep.Instance
		want      *bool
	}{
		{
			"replica cluster: the designated primary is a standby", "dr-1",
			map[string]sweep.Instance{"dr-1": read(false, true), "dr-2": read(false, false)},
			new(true),
		},
		{
			"primary cluster", "app-1",
			map[string]sweep.Instance{"app-1": read(true, true), "app-2": read(false, false)},
			new(true),
		},
		{
			"current primary unread: a self-reported primary does not stand in", "app-1",
			map[string]sweep.Instance{"app-1": {State: viewmodel.StatusReadUnreachable}, "app-2": read(true, false)},
			nil,
		},
		{
			"no current primary: a self-reported primary does not stand in", "",
			map[string]sweep.Instance{"app-2": read(true, true)},
			nil,
		},
		{
			"replica cluster with the designated primary unread is unknown", "dr-1",
			map[string]sweep.Instance{"dr-1": {State: viewmodel.StatusReadUnreachable}, "dr-2": read(false, true)},
			nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &gathered{
				cluster: &cnpgv1.Cluster{Status: cnpgv1.ClusterStatus{CurrentPrimary: tt.current}},
				swept:   sweep.Snapshot{Instances: tt.instances},
			}
			got := primaryArchiver(g)
			switch {
			case tt.want == nil && got != nil:
				t.Errorf("archiver = %+v, want none (unknown)", *got)
			case tt.want != nil && (got == nil || got.IsArchivingWAL != *tt.want):
				t.Errorf("archiver = %+v, want isArchivingWAL %v", got, *tt.want)
			}
		})
	}
}

// TestPhaseDetailNamesTheTableUsed: an unknown phase names the table it
// was classified against (1.30 for an undetected or unsupported minor,
// never the planned 1.31 it was not read with); a capped one says why,
// and the cap reaches the view as a translatable phrase.
func TestPhaseDetailNamesTheTableUsed(t *testing.T) {
	for _, tc := range []struct {
		minor          cnpgstatus.Minor
		phase, want    string
		capCode, param string
	}{
		{"", "Cluster is dancing", "Cluster is dancing (not in the CNPG 1.30 table)", "", ""},
		{"1.32", "Cluster is dancing", "Cluster is dancing (not in the CNPG 1.30 table)", "", ""},
		{"1.31", "Cluster is dancing", "Cluster is dancing (not in the CNPG 1.31 table)", "", ""},
		{"1.30", cnpgv1.PhaseHealthy, cnpgv1.PhaseHealthy, "", ""},
		{"1.31", cnpgv1.PhaseHealthy, cnpgv1.PhaseHealthy + " (phase table not verified for CNPG 1.31)", "phase.cap.unverifiedMinor", "1.31"},
		{"", cnpgv1.PhaseHealthy, cnpgv1.PhaseHealthy + " (CNPG version not detected: phase read against the CNPG 1.30 table, not verified for this operator)", "phase.cap.undetectedMinor", "1.30"},
	} {
		reading, got := phaseDetail(tc.minor, tc.phase)
		if got != tc.want {
			t.Errorf("phaseDetail(%q, %q) = %q, want %q", tc.minor, tc.phase, got, tc.want)
		}
		capped := tableCap("phase", tc.minor, reading.Reason)
		switch {
		case tc.capCode == "" && capped != nil:
			t.Errorf("%q %q: cap %+v", tc.minor, tc.phase, capped)
		case tc.capCode != "" && (capped == nil || capped.Code != tc.capCode || (capped.Params["minor"] != tc.param && capped.Params["table"] != tc.param) || capped.Text != reading.Reason):
			t.Errorf("%q %q: cap %+v, want %s %s", tc.minor, tc.phase, capped, tc.capCode, tc.param)
		}
	}
}

// TestCappedPhaseIsNotWaiting: a healthy phase capped for an unverified
// minor is not the operator waiting.
func TestCappedPhaseIsNotWaiting(t *testing.T) {
	g := &gathered{cluster: &cnpgv1.Cluster{Status: cnpgv1.ClusterStatus{Phase: cnpgv1.PhaseHealthy}}, phaseMinor: "1.31"}
	if w := waitingFor(g, nil); w != nil {
		t.Errorf("waiting %+v on a capped healthy phase", w)
	}
}

// TestHibernatedClusterHeadlineIsInactive (round 8, DBA): a deliberately
// hibernated Cluster runs no instance, so its archiver cannot be read and
// no pod answers the sweep. Both contributors are inactive, as the
// hibernation is, and the headline reads inactive, not unknown; its
// recovery window's newest edge may stay unknown.
func TestHibernatedClusterHeadlineIsInactive(t *testing.T) {
	scheme, err := cache.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	sleepy := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "sleepy", UID: "5e1e9c1a-0000-4000-8000-000000000001", Annotations: map[string]string{cnpgstatus.AnnotationHibernation: "on"}},
		Spec: cnpgv1.ClusterSpec{Instances: 2, Backup: &cnpgv1.BackupConfiguration{BarmanObjectStore: &cnpgv1.BarmanObjectStoreConfiguration{}}},
		Status: cnpgv1.ClusterStatus{
			Phase: "Cluster in healthy state", Instances: 2, CurrentPrimary: "sleepy-1", TargetPrimary: "sleepy-1",
			Conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue, Reason: "ClusterIsReady", LastTransitionTime: metav1.NewTime(now.Add(-48 * time.Hour))},
				{Type: cnpgstatus.ConditionContinuousArchiving, Status: metav1.ConditionTrue, Reason: "ContinuousArchivingSuccess", LastTransitionTime: metav1.NewTime(now.Add(-48 * time.Hour))},
				{Type: cnpgstatus.ConditionHibernation, Status: metav1.ConditionTrue, Reason: "Hibernated", LastTransitionTime: metav1.NewTime(now.Add(-24 * time.Hour))},
			},
		},
	}
	stopped := metav1.NewTime(now.Add(-26 * time.Hour))
	backup := &cnpgv1.Backup{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "sleepy-last", CreationTimestamp: metav1.NewTime(now.Add(-27 * time.Hour))},
		Spec:       cnpgv1.BackupSpec{Cluster: cnpgv1.LocalObjectReference{Name: "sleepy"}},
		Status:     cnpgv1.BackupStatus{Phase: cnpgv1.BackupPhaseCompleted, StartedAt: &stopped, StoppedAt: &stopped},
	}
	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sleepy, backup)
	for _, index := range cache.Indexes() {
		builder = builder.WithIndex(index.Object, index.Field, index.Func)
	}
	snaps := map[sweep.Key]sweep.Snapshot{{Namespace: "team-a", Name: "sleepy"}: {SweptAt: now, Interval: 30 * time.Second, Instances: map[string]sweep.Instance{}}}
	svc := New(Options{Cache: &fakeCache{reader: builder.Build(), namespaces: []string{"team-a"}}, Sweep: fakeSweep{snaps: snaps}, Now: func() time.Time { return now }})
	ov, err := svc.Overview(t.Context(), "team-a", "sleepy")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]viewmodel.Contributor{}
	for _, c := range ov.Verdict.Contributors {
		byID[c.ID] = c
	}
	for _, id := range []string{"hibernation", "archiving", "instanceStatus"} {
		if byID[id].Health != viewmodel.HealthInactive {
			t.Errorf("%s contributor %s (%s), want inactive", id, byID[id].Health, byID[id].Detail)
		}
	}
	if ov.Verdict.Health != viewmodel.HealthInactive {
		t.Errorf("headline %s, want inactive; contributors %+v", ov.Verdict.Health, ov.Verdict.Contributors)
	}
}
