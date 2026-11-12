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
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fyannk/pgToolBox/internal/console/evidence"
	"github.com/fyannk/pgToolBox/internal/console/sweep"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// maskedReading is one of the instancestatus fixtures: a 200 answer
// CloudNativePG's instance manager gives for a fenced or restarting
// primary, in its wire shape, read at observed.
func maskedReading(t *testing.T, name string, observed time.Time) *instancestatus.Reading {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "pkg", "cnpgstatus", "instancestatus", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	r, err := instancestatus.Parse(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Instance, r.ObservedAt = "db-2", observed
	return &r
}

// edgeCase is a gathered Cluster with ContinuousArchiving at condition
// (since 2 h ago), hibernated or not, and db-2, its current primary, read
// as primary (nil: not read).
func edgeCase(condition metav1.ConditionStatus, hibernated bool, primary *sweep.Instance) *gathered {
	g := failoverGathered("db-2", now.Add(-time.Hour), nil)
	g.cluster.Status.Conditions = []metav1.Condition{{Type: cnpgstatus.ConditionContinuousArchiving, Status: condition, Reason: "ContinuousArchivingFailing", LastTransitionTime: metav1.NewTime(now.Add(-2 * time.Hour))}}
	if hibernated {
		g.cluster.Annotations = map[string]string{cnpgstatus.AnnotationHibernation: "on"}
		g.cluster.Status.Conditions = append(g.cluster.Status.Conditions, metav1.Condition{Type: cnpgstatus.ConditionHibernation, Status: metav1.ConditionTrue, Reason: "Hibernated"})
	}
	g.swept = sweep.Snapshot{SweptAt: now, Interval: 30 * time.Second, Instances: map[string]sweep.Instance{}}
	if primary != nil {
		g.swept.Instances["db-2"] = *primary
	}
	return g
}

// edgeTimelines are g's archiving fact, its recovery facts and the
// timelines drawn from them on the evidence and the reported basis.
func edgeTimelines(g *gathered) (cnpgstatus.ArchivingReading, recoveryFacts, viewmodel.RecoveryTimeline, viewmodel.RecoveryTimeline) {
	reading := cnpgstatus.Archiving(g.cluster, primaryArchiver(g))
	arch := viewmodel.Archiving{Health: health(reading.Class), Reason: reading.Reason, ConditionStatus: reading.ConditionStatus}
	facts := recoveryFactsOf(g, arch, now)
	p := tlPath("b1", 48*time.Hour, 0, evidencev1alpha1.CoverageFrontier)
	withEvidence := timelineFixture{paths: []evidence.RecoveryPath{p}}.evidence()
	first, last := now.Add(-48*time.Hour), now.Add(-6*time.Hour)
	window := &viewmodel.RecoveryWindow{First: &first, Last: &last, Provenance: viewmodel.Provenance{Origin: viewmodel.OriginOperator, Source: "Cluster.status"}}
	onEvidence := (&Service{}).recoveryTimeline(withEvidence, viewmodel.BackupSummary{Archiving: arch}, nil, facts)
	onReport := (&Service{}).recoveryTimeline(clusterEvidence{}, viewmodel.BackupSummary{Archiving: arch, RecoveryWindow: window}, nil, facts)
	return reading, facts, onEvidence, onReport
}

func reasonCodes(tl viewmodel.RecoveryTimeline) []string {
	out := make([]string, 0, len(tl.Reasons))
	for _, r := range tl.Reasons {
		out = append(out, r.Code)
	}
	return out
}

// TestMaskedPrimaryIsNoArchiverReading (round 16, CNPG P2-1): a fenced or
// restarting primary answers /pg/status 200 with isPrimary true, the
// error it masked and every archiver field at zero. That is no reading of
// the archiver: archiving is not ok (the primary's archiver unread), the
// facts say the primary was not read, and neither basis offers a check
// to run on a PostgreSQL that is not running.
func TestMaskedPrimaryIsNoArchiverReading(t *testing.T) {
	for _, fixture := range []string{"fenced-primary.json", "restarting-primary.json"} {
		for _, condition := range []metav1.ConditionStatus{metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown} {
			r := maskedReading(t, fixture, now.Add(-10*time.Second))
			g := edgeCase(condition, false, &sweep.Instance{State: viewmodel.StatusReadOK, Reading: r})
			if got := primaryArchiverReading(g); got != nil {
				t.Errorf("%s, %s: primaryArchiverReading returned the masked report", fixture, condition)
			}
			reading, facts, onEvidence, onReport := edgeTimelines(g)
			if reading.Class == cnpgstatus.ClassOK {
				t.Errorf("%s, %s: archiving ok (%s) from a masked report", fixture, condition, reading.Reason)
			}
			if condition == metav1.ConditionTrue && reading.Reason != cnpgstatus.ArchivingReasonPrimaryUnread {
				t.Errorf("%s, True: archiving reason %q, want %q", fixture, reading.Reason, cnpgstatus.ArchivingReasonPrimaryUnread)
			}
			if !facts.primaryUnread || facts.backlog != 0 {
				t.Errorf("%s, %s: primaryUnread %v backlog %d, want unread and no backlog", fixture, condition, facts.primaryUnread, facts.backlog)
			}
			if onEvidence.State == string(evidencev1alpha1.Healthy) {
				t.Errorf("%s, %s: evidence basis healthy", fixture, condition)
			}
			for basis, tl := range map[string]viewmodel.RecoveryTimeline{"evidence": onEvidence, "reported": onReport} {
				if tl.EdgeAdvice != viewmodel.EdgeAdviceNone {
					t.Errorf("%s, %s, %s basis: edgeAdvice %q, want none (reasons %v)", fixture, condition, basis, tl.EdgeAdvice, reasonCodes(tl))
				}
			}
		}
	}
}

// TestMaskedReadingsFeedNoArchiverFact (round 16, CNPG P2-1): a masked
// report whose fields were partly filled before the error (the instance
// manager returns what it read so far) speaks for none of the facts read
// from pg_stat_archiver or the WAL file: the newest failure, the catch-up
// and the live timeline.
func TestMaskedReadingsFeedNoArchiverFact(t *testing.T) {
	failed, archived := now.Add(-20*time.Minute), now.Add(-10*time.Minute)
	partial := func() *instancestatus.Reading {
		r := maskedReading(t, "fenced-primary.json", now.Add(-10*time.Second))
		r.CurrentWAL, r.LastFailedWALTime, r.LastFailedWAL, r.LastArchivedWALTime = "000000050000000000000003", &failed, "000000050000000000000002", &archived
		return r
	}
	g := edgeCase(metav1.ConditionTrue, false, &sweep.Instance{State: viewmodel.StatusReadOK, Reading: partial()})
	standby := partial()
	standby.Instance, standby.IsPrimary = "db-1", false
	g.swept.Instances["db-1"] = sweep.Instance{State: viewmodel.StatusReadOK, Reading: standby}
	if at, on := newestArchiveFailure(g); at != nil {
		t.Errorf("newestArchiveFailure = %s on %s from masked reports, want none", at, on)
	}
	if got := caughtUpBy(g, "db-2"); got != nil {
		t.Errorf("caughtUpBy = %s from a masked report, want nil", got)
	}
	if tl := liveTimelineOf(g, promotedAt(g)); tl != 0 {
		t.Errorf("liveTimelineOf = %d from a masked report's WAL file, want 0", tl)
	}
	// The same partial reading, not masked, is read: the test discriminates.
	g.swept.Instances["db-2"].Reading.MightBeUnavailableMaskedError = ""
	if tl := liveTimelineOf(g, promotedAt(g)); tl != 5 {
		t.Errorf("unmasked control: liveTimelineOf = %d, want 5", tl)
	}
}

// TestNotReportedFromTheLiveFact (round 16, CNPG P3-b): a designated
// primary (isPrimary false) with ContinuousArchiving False after a
// recovery, or Unknown, keeps CNPG's reason in the archiving fact, not
// BacklogNotReported; pg_switch_wal() still fails on it, so both bases
// offer the archiver check only.
func TestNotReportedFromTheLiveFact(t *testing.T) {
	for _, condition := range []metav1.ConditionStatus{metav1.ConditionFalse, metav1.ConditionUnknown} {
		r := &instancestatus.Reading{Instance: "db-2", IsArchivingWAL: true, LastArchivedWALTime: ago(time.Minute), ObservedAt: now.Add(-10 * time.Second)}
		g := edgeCase(condition, false, &sweep.Instance{State: viewmodel.StatusReadOK, Reading: r})
		reading, facts, onEvidence, onReport := edgeTimelines(g)
		if reading.Reason == cnpgstatus.ArchivingReasonBacklogNotReported {
			t.Fatalf("%s: the archiving fact already says BacklogNotReported: the case does not test the live fact", condition)
		}
		if !facts.notReported {
			t.Errorf("%s: notReported false for a designated primary", condition)
		}
		for basis, tl := range map[string]viewmodel.RecoveryTimeline{"evidence": onEvidence, "reported": onReport} {
			if tl.EdgeAdvice != viewmodel.EdgeAdviceArchiver {
				t.Errorf("%s, %s basis: edgeAdvice %q, want archiver (reasons %v)", condition, basis, tl.EdgeAdvice, reasonCodes(tl))
			}
		}
	}
}

// TestNoClusterNoCheck (round 16, CNPG P3-c): without the Cluster no
// primary is named, so none was read: no check is offered.
func TestNoClusterNoCheck(t *testing.T) {
	f := recoveryFactsOf(&gathered{}, viewmodel.Archiving{Health: viewmodel.HealthOK}, now)
	if !f.primaryUnread {
		t.Error("no Cluster: primaryUnread false")
	}
	if got := edgeAdvice(nil, f); got != viewmodel.EdgeAdviceNone {
		t.Errorf("no Cluster: edgeAdvice %q, want none", got)
	}
}

// TestHibernatedAfterAFailureSaysSo (round 16, PG P3-2 and CNPG P3-a; DBA
// idea a): a Cluster hibernated while ContinuousArchiving was False keeps
// the hibernated reason and adds that its last archive attempt failed,
// with the condition's time, at warning; hibernated with the condition
// True stays unknown with the one reason. On the reported basis the
// hibernated reason says why no check is offered.
func TestHibernatedAfterAFailureSaysSo(t *testing.T) {
	since := now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	_, _, onEvidence, onReport := edgeTimelines(edgeCase(metav1.ConditionFalse, true, nil))
	codes := reasonCodes(onEvidence)
	if onEvidence.State != string(evidencev1alpha1.Warning) || len(codes) < 2 || codes[0] != "recovery.reason.archiverHibernated" || codes[1] != "recovery.reason.archivingFailedHibernated" {
		t.Errorf("hibernated, False: state %s reasons %v, want warning with archiverHibernated then archivingFailedHibernated", onEvidence.State, codes)
	} else if got := onEvidence.Reasons[1].Params["sinceAt"]; got != since {
		t.Errorf("hibernated, False: sinceAt %q, want %q", got, since)
	}
	if onEvidence.EdgeAdvice != viewmodel.EdgeAdviceNone {
		t.Errorf("hibernated, False: edgeAdvice %q, want none", onEvidence.EdgeAdvice)
	}
	if codes := reasonCodes(onReport); !containsCode(codes, "recovery.reason.archiverHibernated") || !containsCode(codes, "recovery.reason.archivingFailedHibernated") {
		t.Errorf("reported, hibernated, False: reasons %v, want the hibernated and the failed-attempt reasons", codes)
	}
	_, _, onEvidence, onReport = edgeTimelines(edgeCase(metav1.ConditionTrue, true, nil))
	if codes := reasonCodes(onEvidence); onEvidence.State != string(evidencev1alpha1.Unknown) || len(codes) != 1 || codes[0] != "recovery.reason.archiverHibernated" {
		t.Errorf("hibernated, True: state %s reasons %v, want unknown with archiverHibernated alone", onEvidence.State, codes)
	}
	if codes := reasonCodes(onReport); !containsCode(codes, "recovery.reason.archiverHibernated") || containsCode(codes, "recovery.reason.archivingFailedHibernated") {
		t.Errorf("reported, hibernated, True: reasons %v, want the hibernated reason only", codes)
	}
}

func containsCode(codes []string, code string) bool {
	for _, c := range codes {
		if c == code {
			return true
		}
	}
	return false
}
