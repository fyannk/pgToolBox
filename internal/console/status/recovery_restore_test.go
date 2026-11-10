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
	"slices"
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"

	"github.com/fyannk/pgToolBox/internal/console/evidence"
	"github.com/fyannk/pgToolBox/internal/console/sweep"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// TestReportedRestoreCheckReadsTheArchiver: without an evidence agent the
// window's end is the operator's last base backup. The time after it is
// answered from the primary's own pg_stat_archiver report: "may well be
// possible" only with healthy, fresh archiving and a last archived WAL
// time later than the target; failing archiving says the WAL after the
// last archived segment is likely not in the repository. The answer is
// unknown either way, never covered nor notCovered.
func TestReportedRestoreCheckReadsTheArchiver(t *testing.T) {
	svc := &Service{}
	facts := recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout}
	first, last := now.Add(-48*time.Hour), now.Add(-6*time.Hour)
	read := now.Add(-2 * time.Minute)
	instance := viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: "/pg/status", ObservedAt: &read}
	healthy := viewmodel.Archiving{Health: viewmodel.HealthOK, Reason: "ContinuousArchivingSuccess", LastArchivedAt: ago(5 * time.Minute), Provenance: instance}
	failing := viewmodel.Archiving{Health: viewmodel.HealthCritical, Reason: "ContinuousArchivingFailing", LastArchivedAt: ago(4 * time.Hour), Provenance: instance}
	backlog := failing
	backlog.Health, backlog.Reason = viewmodel.HealthWarning, cnpgstatus.ArchivingReasonFailureWithBacklog
	failingUnread := failing
	failingUnread.LastArchivedAt = nil
	staleHealthy := healthy
	staleHealthy.Provenance.Stale = true
	unknown := healthy
	unknown.Health = viewmodel.HealthUnknown
	healthyUnread := healthy
	healthyUnread.LastArchivedAt = nil

	type probe struct {
		at       time.Duration // before now; negative is in the future
		code     string
		mayWell  bool
		contains string
	}
	cases := []struct {
		name      string
		archiving viewmodel.Archiving
		probes    []probe
		caveat    string // a caveat code the answer must carry; empty: no archiving caveat
	}{
		{
			"healthy archiving: may well be possible only before the last archived WAL",
			healthy,
			[]probe{
				{2 * time.Hour, "recovery.reportedTail.beforeArchived", true, rfc3339(*healthy.LastArchivedAt)},
				{time.Minute, "recovery.reportedTail.afterArchived", false, "only on the primary"},
				{-time.Hour, "recovery.reportedTail.afterArchived", false, ""},
			},
			"",
		},
		{
			// The scenario of the review: archiving failing since 01:10,
			// the DBA asks at 03:00 for 02:55.
			"failing archiving: WAL after the last archived segment is likely not in the repository",
			failing,
			[]probe{
				{5 * time.Hour, "recovery.reportedTail.beforeArchivedFailing", false, "failing"},
				{5 * time.Minute, "recovery.reportedTail.afterArchivedFailing", false, "likely not in the repository"},
				{0, "recovery.reportedTail.afterArchivedFailing", false, rfc3339(*failing.LastArchivedAt)},
			},
			"recovery.caveat.archivingFailingSince",
		},
		{
			"a failure with backlog is failing",
			backlog,
			[]probe{{5 * time.Minute, "recovery.reportedTail.afterArchivedFailing", false, "likely not in the repository"}},
			"recovery.caveat.archivingFailingSince",
		},
		{
			"failing archiving with no last archived time",
			failingUnread,
			[]probe{{time.Hour, "recovery.reportedTail.archivedUnknownFailing", false, "likely not in the repository"}},
			"recovery.caveat.archivingFailing",
		},
		{
			"a stale archiver report is not healthy",
			staleHealthy,
			[]probe{{2 * time.Hour, "recovery.reportedTail.beforeArchivedUnconfirmed", false, "not confirmed"}},
			"",
		},
		{
			"unknown archiving is not healthy",
			unknown,
			[]probe{{2 * time.Hour, "recovery.reportedTail.beforeArchivedUnconfirmed", false, "not confirmed"}},
			"",
		},
		{
			"healthy archiving with no last archived time cannot say may well",
			healthyUnread,
			[]probe{{2 * time.Hour, "recovery.reportedTail.archivedUnknown", false, "not known"}},
			"",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			summary := viewmodel.BackupSummary{
				Archiving: c.archiving,
				RecoveryWindow: &viewmodel.RecoveryWindow{First: &first, Last: &last, Provenance: viewmodel.Provenance{
					Origin: viewmodel.OriginOperator, Source: "ObjectStore.status.serverRecoveryWindow[orders]",
				}},
			}
			tl := svc.recoveryTimeline(clusterEvidence{}, summary, nil, facts)
			if tl.Basis != "reported" {
				t.Fatalf("basis %s", tl.Basis)
			}
			for _, seg := range tl.Segments {
				if seg.Kind == segmentCovered || seg.Code == "" {
					t.Errorf("segment %+v: covered or without a code", seg)
				}
			}
			b := viewmodel.Backups{RecoveryTimeline: tl, Summary: summary, GeneratedAt: now}
			for _, p := range c.probes {
				got := restoreCheck(b, now.Add(-p.at))
				if got.Answer != "unknown" || got.Code != p.code {
					t.Errorf("-%s: %s %s (%s)", p.at, got.Answer, got.Code, got.Text)
				}
				if may := strings.Contains(got.Text, "may well be possible"); may != p.mayWell {
					t.Errorf("-%s: 'may well be possible' %v in %q", p.at, may, got.Text)
				}
				if !strings.Contains(got.Text, p.contains) {
					t.Errorf("-%s: %q does not say %q", p.at, got.Text, p.contains)
				}
				if c.archiving.LastArchivedAt != nil && (got.Params["archivedAt"] == "" || got.Params["ageSeconds"] != "120" || !strings.Contains(got.Text, "read 2 min ago")) {
					t.Errorf("-%s: the last archived time or its age is not cited: %v %q", p.at, got.Params, got.Text)
				}
				hasCaveat := slices.ContainsFunc(got.Caveats, func(v viewmodel.Phrase) bool { return strings.HasPrefix(v.Code, "recovery.caveat.archivingFailing") })
				switch {
				case c.caveat == "" && hasCaveat:
					t.Errorf("-%s: archiving caveat on healthy archiving: %+v", p.at, got.Caveats)
				case c.caveat != "" && !slices.ContainsFunc(got.Caveats, func(v viewmodel.Phrase) bool { return v.Code == c.caveat }):
					t.Errorf("-%s: no %s caveat: %+v", p.at, c.caveat, got.Caveats)
				}
			}
		})
	}
}

// TestFrontierBeforeScanIsNotNotCovered: the agent's frontier is a fact
// as of its scan. WAL generated between the frontier and the scan may
// have been in a segment not yet archived when the agent read the
// repository, so every time after the frontier is unknown, never
// notCovered; only a confirmed gap and a time before the earliest point
// stay negative.
func TestFrontierBeforeScanIsNotNotCovered(t *testing.T) {
	svc := &Service{}
	facts := recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout}
	frontier := evidencev1alpha1.CoverageFrontier
	scanned := now.Add(-10 * time.Minute)
	fx := timelineFixture{paths: []evidence.RecoveryPath{tlPath("b1", 48*time.Hour, 30*time.Minute, frontier)}, scannedAt: scanned}
	tl := svc.recoveryTimeline(fx.evidence(), viewmodel.BackupSummary{}, nil, facts)
	b := viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}
	for at := now.Add(-30*time.Minute + time.Second); !at.After(now.Add(time.Hour)); at = at.Add(30 * time.Second) {
		got := restoreCheck(b, at)
		if got.Answer != "unknown" {
			t.Fatalf("%s: %s (%s)", at, got.Answer, got.Text)
		}
	}
	got := restoreCheck(b, now.Add(-20*time.Minute))
	if got.Code != "recovery.segment.afterLastRead" || got.Params["scannedAt"] != rfc3339(scanned) || !strings.Contains(got.Text, "WAL archived since may cover") {
		t.Errorf("between the frontier and the scan: %+v", got)
	}
	if got := restoreCheck(b, now.Add(-49*time.Hour)); got.Answer != "notCovered" {
		t.Errorf("before the earliest point: %+v", got)
	}

	// A confirmed gap after the latest point stays a gap, up to and past now.
	gapped := timelineFixture{paths: []evidence.RecoveryPath{tlPath("b1", 48*time.Hour, 20*time.Hour, evidencev1alpha1.CoverageGapLimited)}}
	gb := viewmodel.Backups{RecoveryTimeline: svc.recoveryTimeline(gapped.evidence(), viewmodel.BackupSummary{}, nil, facts), GeneratedAt: now}
	for _, at := range []time.Time{now.Add(-10 * time.Hour), now, now.Add(time.Hour)} {
		if got := restoreCheck(gb, at); got.Answer != "gap" || got.Code != "recovery.segment.missingAfterLatest" {
			t.Errorf("%s after a confirmed gap: %+v", at, got)
		}
	}
}

// TestReportedAnswerNamesItsSource: a reported window that comes from
// the evidence agent (pgBackRest without recovery paths) is not "the
// operator's".
func TestReportedAnswerNamesItsSource(t *testing.T) {
	svc := &Service{}
	facts := recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout}
	first, last := now.Add(-48*time.Hour), now.Add(-time.Hour)
	for _, c := range []struct {
		origin       viewmodel.Origin
		source, code string
	}{
		{viewmodel.OriginOperator, "ObjectStore.status.serverRecoveryWindow[orders]", "recovery.check.reported"},
		{viewmodel.OriginEvidence, "EvidenceAgent billing-evidence /api/v1alpha1/snapshot recovery_window", "recovery.check.reportedByAgent"},
	} {
		summary := viewmodel.BackupSummary{RecoveryWindow: &viewmodel.RecoveryWindow{First: &first, Last: &last, Provenance: viewmodel.Provenance{Origin: c.origin, Source: c.source}}}
		tl := svc.recoveryTimeline(clusterEvidence{}, summary, nil, facts)
		got := restoreCheck(viewmodel.Backups{RecoveryTimeline: tl, Summary: summary}, now.Add(-2*time.Hour))
		if got.Answer != "reported" || got.Code != c.code || got.Params["source"] != c.source || !strings.Contains(got.Text, c.source) {
			t.Errorf("%s: %+v", c.origin, got)
		}
		if c.origin == viewmodel.OriginEvidence && strings.Contains(got.Text, "operator") {
			t.Errorf("an agent's window is called the operator's: %q", got.Text)
		}
	}
}

// TestEveryRecoveryTextHasACode: the SPA translates by code; a segment,
// an answer or a caveat without one would show in English.
func TestEveryRecoveryTextHasACode(t *testing.T) {
	svc := &Service{}
	frontier, gap := evidencev1alpha1.CoverageFrontier, evidencev1alpha1.CoverageGapLimited
	fixtures := []timelineFixture{
		{paths: []evidence.RecoveryPath{tlPath("b1", 48*time.Hour, 30*time.Hour, gap), tlPath("b2", 24*time.Hour, time.Minute, frontier)}},
		{paths: []evidence.RecoveryPath{tlPath("b1", 48*time.Hour, 20*time.Hour, evidencev1alpha1.CoverageCandidateLimited)}, stale: true},
		{paths: []evidence.RecoveryPath{tlPath("b1", 48*time.Hour, 20*time.Hour, frontier)}, cut: true},
	}
	facts := recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout, lastFailedWAL: ago(2 * time.Hour)}
	for i, f := range fixtures {
		tl := svc.recoveryTimeline(f.evidence(), viewmodel.BackupSummary{}, nil, facts)
		for _, seg := range tl.Segments {
			if seg.Code == "" || seg.Text == "" {
				t.Errorf("fixture %d: segment without a code: %+v", i, seg)
			}
		}
		b := viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now, Summary: viewmodel.BackupSummary{Archiving: viewmodel.Archiving{Health: viewmodel.HealthCritical}}}
		for at := now.Add(-50 * time.Hour); at.Before(now.Add(time.Hour)); at = at.Add(17 * time.Minute) {
			got := restoreCheck(b, at)
			if got.Code == "" {
				t.Errorf("fixture %d %s: answer without a code: %+v", i, at, got)
			}
			for _, c := range got.Caveats {
				if c.Code == "" || c.Text == "" {
					t.Errorf("fixture %d: caveat without a code: %+v", i, c)
				}
			}
		}
	}
}

// TestBackupSummaryCitesLastArchivedWAL: the Overview's archiving line
// carries the archiving instance's own last_archived_time, with the
// sweep's age, and marks it stale under the sweep's staleness rule.
func TestBackupSummaryCitesLastArchivedWAL(t *testing.T) {
	archived, failed := now.Add(-3*time.Minute), now.Add(-time.Hour)
	g := &gathered{
		cluster:  &cnpgv1.Cluster{Status: cnpgv1.ClusterStatus{CurrentPrimary: "orders-1"}},
		hasSweep: true,
		swept: sweep.Snapshot{SweptAt: now.Add(-10 * time.Second), Interval: 30 * time.Second, Instances: map[string]sweep.Instance{
			"orders-1": {State: viewmodel.StatusReadOK, Reading: &instancestatus.Reading{
				IsPrimary: true, IsArchivingWAL: true, LastArchivedWAL: "000000010000000000000042", LastArchivedWALTime: &archived, LastFailedWALTime: &failed,
			}},
		}},
	}
	svc := newScreenService(t)
	a := svc.backupSummary(g, now).Archiving
	if a.LastArchivedAt == nil || !a.LastArchivedAt.Equal(archived) || a.LastArchivedWAL != "000000010000000000000042" || a.LastFailedAt == nil {
		t.Fatalf("last archived not surfaced: %+v", a)
	}
	if a.Provenance.Origin != viewmodel.OriginInstance || a.Provenance.ObservedAt == nil || a.Provenance.Stale {
		t.Errorf("provenance %+v", a.Provenance)
	}
	g.swept.SweptAt = now.Add(-2 * time.Minute) // more than three 30 s intervals
	if a := svc.backupSummary(g, now).Archiving; !a.Provenance.Stale {
		t.Errorf("an old sweep is not stale: %+v", a.Provenance)
	}
	g.swept.Instances["orders-1"] = sweep.Instance{State: viewmodel.StatusReadUnreachable}
	g.cluster.Status.CurrentPrimary = ""
	if a := svc.backupSummary(g, now).Archiving; a.LastArchivedAt != nil {
		t.Errorf("an unread primary reports a last archived time: %+v", a)
	}
}
