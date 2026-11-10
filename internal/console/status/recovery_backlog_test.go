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

	"github.com/fyannk/pgToolBox/internal/console/evidence"
	"github.com/fyannk/pgToolBox/internal/console/sweep"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

func pint(v int) *int { return &v }

// TestRecoveryFactsReadTheBacklog: the primary's readyWalFiles enters the
// facts at cnpgstatus.ArchiveBacklogThreshold and not below; the newest
// archiving failure is taken over every readable instance (a former
// primary's survives a failover), clamped to when it was read.
func TestRecoveryFactsReadTheBacklog(t *testing.T) {
	observed := now.Add(-time.Minute)
	future := now.Add(time.Hour)
	older := now.Add(-3 * time.Hour)
	g := &gathered{
		cluster: &cnpgv1.Cluster{Status: cnpgv1.ClusterStatus{CurrentPrimary: "db-2"}},
		swept: sweep.Snapshot{Instances: map[string]sweep.Instance{
			// The former primary: a replica now, its failure kept.
			"db-1": {State: viewmodel.StatusReadOK, Reading: &instancestatus.Reading{ObservedAt: observed, LastFailedWALTime: &future}},
			"db-2": {State: viewmodel.StatusReadOK, Reading: &instancestatus.Reading{IsPrimary: true, ObservedAt: observed, LastFailedWALTime: &older}},
		}},
	}
	for _, tc := range []struct {
		ready *int
		want  int
	}{
		{nil, 0},
		{pint(cnpgstatus.ArchiveBacklogSegments - 1), 0},
		{pint(cnpgstatus.ArchiveBacklogSegments), cnpgstatus.ArchiveBacklogSegments},
		{pint(40), 40},
	} {
		f := recoveryFactsOf(g, viewmodel.Archiving{Health: viewmodel.HealthOK, ReadyWALFiles: tc.ready}, now)
		if f.backlog != tc.want {
			t.Errorf("ready %v: backlog %d, want %d", tc.ready, f.backlog, tc.want)
		}
		if f.lastFailedWAL == nil || !f.lastFailedWAL.Equal(observed) {
			t.Errorf("lastFailedWAL %v, want the former primary's, clamped to %s", f.lastFailedWAL, observed)
		}
	}
	if f := recoveryFactsOf(g, viewmodel.Archiving{Health: viewmodel.HealthCritical}, now); !f.failing {
		t.Error("critical archiving is not failing in the facts")
	}
}

// TestArchiveBacklogNeverReadsHealthy: segments waiting on the primary
// without any failure. On the evidence basis the newest edge is unknown
// with the count, and the window is never healthy; on the reported
// basis no answer says "may well be possible", and each names the count.
func TestArchiveBacklogNeverReadsHealthy(t *testing.T) {
	svc := &Service{}
	frontier := evidencev1alpha1.CoverageFrontier
	fx := timelineFixture{paths: []evidence.RecoveryPath{tlPath("b1", 48*time.Hour, 10*time.Minute, frontier)}}

	keepingUp := svc.recoveryTimeline(fx.evidence(), viewmodel.BackupSummary{}, nil, recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout})
	if keepingUp.State != string(evidencev1alpha1.Healthy) {
		t.Fatalf("baseline without a backlog: %s (%s)", keepingUp.State, keepingUp.Reason)
	}
	facts := recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout, backlog: 12}
	tl := svc.recoveryTimeline(fx.evidence(), viewmodel.BackupSummary{}, nil, facts)
	if tl.State == string(evidencev1alpha1.Healthy) || len(tl.Reasons) == 0 || tl.Reasons[0].Code != "recovery.reason.archiveBacklog" || tl.Reasons[0].Params["segments"] != "12" {
		t.Errorf("backlog timeline: %s %+v", tl.State, tl.Reasons)
	}
	b := viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}
	got := restoreCheck(b, now.Add(-2*time.Hour))
	if got.Answer != "unknown" || got.Code != "recovery.segment.archiveBacklog" || got.Params["segments"] != "12" {
		t.Errorf("two hours before the frontier with a backlog: %+v", got)
	}
	if got := restoreCheck(b, now.Add(-5*time.Minute)); got.Code != "recovery.segment.afterLastReadBacklog" || strings.Contains(got.Text, "may cover") {
		t.Errorf("after the frontier with a backlog: %+v", got)
	}
	if got := restoreCheck(b, now.Add(-30*time.Hour)); got.Answer != "covered" {
		t.Errorf("a day before the backlog's lookback stays covered: %+v", got)
	}

	// Reported basis: cnpgstatus.Archiving says ArchiveBacklog (warning).
	read := now.Add(-2 * time.Minute)
	arch := viewmodel.Archiving{
		Health: viewmodel.HealthWarning, Reason: cnpgstatus.ArchivingReasonBacklog, ReadyWALFiles: pint(12), LastArchivedAt: ago(5 * time.Minute),
		Provenance: viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: "/pg/status", ObservedAt: &read},
	}
	first, last := now.Add(-48*time.Hour), now.Add(-6*time.Hour)
	summary := viewmodel.BackupSummary{Archiving: arch, RecoveryWindow: &viewmodel.RecoveryWindow{First: &first, Last: &last, Provenance: viewmodel.Provenance{Origin: viewmodel.OriginOperator, Source: "Cluster.status"}}}
	rt := svc.recoveryTimeline(clusterEvidence{}, summary, nil, facts)
	rb := viewmodel.Backups{RecoveryTimeline: rt, Summary: summary, GeneratedAt: now}
	for at, code := range map[time.Duration]string{
		2 * time.Hour: "recovery.reportedTail.beforeArchivedBacklog",
		time.Minute:   "recovery.reportedTail.afterArchivedBacklog",
	} {
		got := restoreCheck(rb, now.Add(-at))
		if got.Code != code || strings.Contains(got.Text, "may well be possible") || strings.Contains(got.Text, "healthy") || got.Params["segments"] != "12" {
			t.Errorf("-%s reported with a backlog: %+v", at, got)
		}
	}
}

// TestTailAfterLastReadWhileFailing: after the frontier, with archiving
// failing, the tail does not suggest that WAL archived since may cover
// the time.
func TestTailAfterLastReadWhileFailing(t *testing.T) {
	svc := &Service{}
	scanned := now.Add(-10 * time.Minute)
	fx := timelineFixture{paths: []evidence.RecoveryPath{tlPath("b1", 48*time.Hour, 30*time.Minute, evidencev1alpha1.CoverageFrontier)}, scannedAt: scanned}
	tl := svc.recoveryTimeline(fx.evidence(), viewmodel.BackupSummary{}, nil, recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout, failing: true})
	got := restoreCheck(viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}, now.Add(-20*time.Minute))
	if got.Code != "recovery.segment.afterLastReadFailing" || strings.Contains(got.Text, "may cover") || !strings.Contains(got.Text, "failing") {
		t.Errorf("after the last read while failing: %+v", got)
	}
}

// TestReportedTailClampsTheInstanceClock: a last_archived_time later than
// the reading (a primary whose clock runs ahead) is read as the reading.
func TestReportedTailClampsTheInstanceClock(t *testing.T) {
	read := now.Add(-10 * time.Minute)
	arch := viewmodel.Archiving{
		Health: viewmodel.HealthOK, LastArchivedAt: ago(-time.Hour),
		Provenance: viewmodel.Provenance{Origin: viewmodel.OriginInstance, ObservedAt: &read},
	}
	last := now.Add(-6 * time.Hour)
	segs := reportedTail(viewmodel.Provenance{Origin: viewmodel.OriginOperator}, arch, recoveryFacts{now: now}, last)
	if len(segs) != 2 {
		t.Fatalf("segments %+v", segs)
	}
	if segs[0].Params["archivedAt"] != rfc3339(read) || !segs[0].To.Equal(read) {
		t.Errorf("before-archived segment not clamped to the reading: %+v", segs[0])
	}
	if segs[1].Code != "recovery.reportedTail.afterArchived" || !segs[1].From.Equal(read) {
		t.Errorf("after-archived segment does not start at the reading: %+v", segs[1])
	}
}

// TestWALEndBand: just before the newest restore point the bar says the
// archived WAL ends there and how to restore to its end; the band is one
// archive_timeout wide (a floor of walEndFloor), inside the near-end
// margin.
func TestWALEndBand(t *testing.T) {
	svc := &Service{}
	fx := timelineFixture{paths: []evidence.RecoveryPath{tlPath("b1", 48*time.Hour, 30*time.Minute, evidencev1alpha1.CoverageFrontier)}}
	for _, tc := range []struct {
		timeout, want time.Duration
	}{
		{defaultArchiveTimeout, defaultArchiveTimeout},
		{10 * time.Second, walEndFloor},
		{0, unboundedFrontierMargin},
	} {
		tl := svc.recoveryTimeline(fx.evidence(), viewmodel.BackupSummary{}, nil, recoveryFacts{now: now, archiveTimeout: tc.timeout})
		var band *viewmodel.RecoverySegment
		for i := range tl.Segments {
			if tl.Segments[i].Kind == segmentWALEnd {
				band = &tl.Segments[i]
			}
		}
		if band == nil {
			t.Fatalf("timeout %s: no walEnd band: %+v", tc.timeout, tl.Segments)
		}
		if !band.To.Equal(*tl.Latest) || band.To.Sub(band.From) != tc.want {
			t.Errorf("timeout %s: band %s..%s, want %s wide ending at %s", tc.timeout, band.From, band.To, tc.want, tl.Latest)
		}
		got := restoreCheck(viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}, tl.Latest.Add(-walEndFloor/2))
		if got.Answer != "unknown" || got.Code != "recovery.segment.walEnd" || !strings.Contains(got.Text, "recovery ended before configured recovery target was reached") {
			t.Errorf("timeout %s: inside the band: %+v", tc.timeout, got)
		}
	}
	// A path that ends at a gap has no WAL end band.
	gapped := timelineFixture{paths: []evidence.RecoveryPath{tlPath("b1", 48*time.Hour, 20*time.Hour, evidencev1alpha1.CoverageGapLimited)}}
	for _, seg := range svc.recoveryTimeline(gapped.evidence(), viewmodel.BackupSummary{}, nil, recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout}).Segments {
		if seg.Kind == segmentWALEnd {
			t.Errorf("walEnd band before a gap: %+v", seg)
		}
	}
}

// TestTimelineReasonsAreTranslatable: the agent's timeline templates are
// recognised, with the history file named; other text is kept as the
// detail of a generic code.
func TestTimelineReasonsAreTranslatable(t *testing.T) {
	for _, tc := range []struct {
		msg, code, file string
		params          map[string]string
	}{
		{"history 3 is missing: no restore can follow timeline 3", "timeline.reason.historyMissingNoFollow", "00000003.history", map[string]string{"timeline": "3"}},
		{"timeline 12 needs an explicit targetTLI; history 11 is unreadable or invalid", "timeline.reason.needsTarget", "0000000B.history", map[string]string{"timeline": "12", "gap": "invalid", "gapTimeline": "11"}},
		{"the timeline a default restore follows is unknown: history 4 is unreadable", "timeline.reason.latestUnknown", "00000004.history", map[string]string{"gap": "unreadable", "gapTimeline": "4"}},
		{"a default restore follows timeline 3, whose history does not lead through timeline 2: needs targetTLI 2", "timeline.reason.needsTargetSibling", "", map[string]string{"latest": "3", "timeline": "2"}},
		{"timeline ancestry contains missing or invalid evidence", "timeline.reason.ancestryInvalid", "", nil},
		{"timeline ancestry contains missing or invalid evidence; 00000003.history: timeline history is malformed", "timeline.reason.ancestryInvalid", "00000003.history", map[string]string{"problem": "malformed"}},
		{"00000002.history: timeline history is malformed; no default restore reads it", "timeline.reason.unneededInvalid", "00000002.history", map[string]string{"problem": "malformed"}},
		{
			"the agent's timeline evidence is unknown (timeline ancestry contains missing or invalid evidence; 00000005.history: timeline history could not be read): no recovery path is shown as coverage until the timeline a default restore follows is established",
			"recovery.reason.timelineNotEstablished", "00000005.history",
			map[string]string{"state": "unknown", "inner": "timeline.reason.ancestryInvalid", "problem": "unreadable"},
		},
		{agentPredatesReason, "recovery.reason.agentPredates", "", nil},
		{"the timeline a default restore follows is unknown: timeline history files were not all read", "timeline.reason.latestUnknown", "", map[string]string{"gap": "notAllRead"}},
		{"a default restore (recovery_target_timeline 'latest') stops on timeline 2: history 3 is missing; timeline 4 needs an explicit targetTLI", "timeline.reason.latestStops", "00000003.history", map[string]string{"latest": "2", "newest": "4", "gap": "missing", "gapTimeline": "3"}},
	} {
		p, ok := timelinePhrase(tc.msg)
		if !ok || p.Code != tc.code || p.Text != tc.msg || p.Params["historyFile"] != tc.file {
			t.Errorf("%q: %+v %v", tc.msg, p, ok)
			continue
		}
		for k, v := range tc.params {
			if p.Params[k] != v {
				t.Errorf("%q: param %s = %q, want %q", tc.msg, k, p.Params[k], v)
			}
		}
	}
	for _, msg := range []string{
		"", "history 3 is missing: no restore can follow timeline 4", "history 3 is missing; extra",
		"a default restore follows timeline 3, whose history does not lead through timeline 2: needs targetTLI 4",
		"00000002.history: something else; no default restore reads it",
	} {
		if p, ok := timelinePhrase(msg); ok {
			t.Errorf("%q recognised as %+v", msg, p)
		}
	}
	if p := evidenceReason("recovery.reason.agentCoverage", "something else"); p.Code != "recovery.reason.agentCoverage" || p.Params["detail"] != "something else" {
		t.Errorf("fallback %+v", p)
	}
	for in, want := range map[string]string{
		"timeline history 0000000a.history could not be read":                           "0000000A.history",
		"the agent's timeline evidence is unknown (history 7 is unreadable or invalid)": "00000007.history",
		"history 0 is missing": "",
		"no history named":     "",
	} {
		if got := historyFileIn(in); got != want {
			t.Errorf("historyFileIn(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRestoreCheckNamesTheHistoryFile: when an unreadable history file
// withholds every restore point, the check says which file, and carries
// the timeline's reasons as phrases.
func TestRestoreCheckNamesTheHistoryFile(t *testing.T) {
	reason := "the agent's timeline evidence is unknown (timeline history 00000005.history could not be read): no recovery path is shown as coverage until the timeline a default restore follows is established"
	snap := evidencev1alpha1.RepositoryEvidenceSnapshot{}
	ev := clusterEvidence{result: evidence.Result{Snapshot: &snap, Recovery: evidence.Recovery{State: evidence.RecoveryUncertain, Reason: reason}}}
	tl := (&Service{}).recoveryTimeline(ev, viewmodel.BackupSummary{}, nil, recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout})
	got := restoreCheck(viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}, now.Add(-time.Hour))
	if got.Answer != "unknown" || got.Code != "recovery.check.noWindow" || got.HistoryFile != "00000005.history" {
		t.Errorf("check %+v", got)
	}
	if len(got.Reasons) != 1 || got.Reasons[0].Code != "recovery.reason.timelineNotEstablished" || got.Reasons[0].Params["detail"] != "timeline history 00000005.history could not be read" {
		t.Errorf("reasons %+v", got.Reasons)
	}
	// Nothing named: no file.
	clean := (&Service{}).recoveryTimeline(clusterEvidence{}, viewmodel.BackupSummary{}, nil, recoveryFacts{now: now})
	if got := restoreCheck(viewmodel.Backups{RecoveryTimeline: clean, GeneratedAt: now}, now); got.HistoryFile != "" {
		t.Errorf("history file %q without one named", got.HistoryFile)
	}
}

// TestEveryTimelineReasonHasACode: the SPA translates reasons by code.
func TestEveryTimelineReasonHasACode(t *testing.T) {
	svc := &Service{}
	frontier, gap := evidencev1alpha1.CoverageFrontier, evidencev1alpha1.CoverageGapLimited
	for i, f := range []timelineFixture{
		{paths: []evidence.RecoveryPath{tlPath("b1", 48*time.Hour, 30*time.Hour, gap), tlPath("b2", 24*time.Hour, time.Minute, frontier)}},
		{paths: []evidence.RecoveryPath{tlPath("b1", 48*time.Hour, 20*time.Hour, evidencev1alpha1.CoverageCandidateLimited)}, stale: true},
		{paths: []evidence.RecoveryPath{tlPath("b1", 48*time.Hour, 20*time.Hour, frontier)}, cut: true},
		{},
	} {
		for _, facts := range []recoveryFacts{{now: now, backlog: 9}, {now: now, lastFailedWAL: ago(time.Hour)}} {
			tl := svc.recoveryTimeline(f.evidence(), viewmodel.BackupSummary{}, nil, facts)
			joined := make([]string, 0, len(tl.Reasons))
			for _, r := range tl.Reasons {
				if r.Code == "" || r.Text == "" {
					t.Errorf("fixture %d: reason without a code: %+v", i, r)
				}
				joined = append(joined, r.Text)
			}
			if strings.Join(joined, "; ") != tl.Reason {
				t.Errorf("fixture %d: reason %q is not its parts %q", i, tl.Reason, joined)
			}
		}
	}
}
