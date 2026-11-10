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
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/evidence"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// TestTargetTimelineIgnoresOffTargetPaths: an off-target path (Barman or
// pgBackRest) follows the backup's own timeline because the agent proved the
// backup cannot reach the newest one. A default restore follows 'latest', so
// that timeline is not the target, and an off-target path with no frontier
// is not an orphan timeline either.
func TestTargetTimelineIgnoresOffTargetPaths(t *testing.T) {
	t.Parallel()
	frontier := func(timeline uint64, offTarget bool) evidence.RecoveryPath {
		return evidence.RecoveryPath{BackupID: "b", TargetTimeline: timeline, State: evidencev1alpha1.Warning, Stop: evidencev1alpha1.CoverageFrontier, FrontierTimeline: pu64(timeline), FrontierPosition: pu64(5), OffTarget: offTarget}
	}
	unevaluated := func(timeline uint64, offTarget bool) evidence.RecoveryPath {
		return evidence.RecoveryPath{BackupID: "b", TargetTimeline: timeline, State: evidencev1alpha1.Unknown, Stop: evidencev1alpha1.CoverageUnknownLimited, OffTarget: offTarget}
	}
	for _, test := range []struct {
		name           string
		paths          []evidence.RecoveryPath
		target, orphan uint64
	}{
		{name: "only an off-target path: no target", paths: []evidence.RecoveryPath{frontier(1, true)}},
		{name: "an off-target path beside one on the newest timeline", paths: []evidence.RecoveryPath{frontier(3, true), frontier(2, false)}, target: 2},
		{name: "an off-target path without a frontier is not an orphan", paths: []evidence.RecoveryPath{unevaluated(4, true), frontier(2, false)}, target: 2},
		{name: "control: an on-target path names the target", paths: []evidence.RecoveryPath{frontier(1, false)}, target: 1},
		{name: "control: an unconnected timeline is an orphan", paths: []evidence.RecoveryPath{unevaluated(4, false), frontier(2, false)}, target: 2, orphan: 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			target, orphan := targetTimeline(test.paths)
			if target != test.target || orphan != test.orphan {
				t.Fatalf("targetTimeline() = %d, %d; want %d, %d", target, orphan, test.target, test.orphan)
			}
		})
	}
}

// TestRecoveryTimelineDoesNotDrawAnOffTargetBackup: a backup that recovers
// only on an abandoned timeline places no restore point on the bar.
func TestRecoveryTimelineDoesNotDrawAnOffTargetBackup(t *testing.T) {
	t.Parallel()
	path := tlPath("b1", 48*time.Hour, 20*time.Hour, evidencev1alpha1.CoverageFrontier)
	path.State, path.OffTarget = evidencev1alpha1.Warning, true
	ev := timelineFixture{paths: []evidence.RecoveryPath{path}}.evidence()
	svc := &Service{}
	timeline := svc.recoveryTimeline(ev, viewmodel.BackupSummary{}, nil, recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout})
	if timeline.State != string(evidencev1alpha1.Unknown) || timeline.Latest != nil || timeline.TargetTimeline != 0 {
		t.Fatalf("off-target backup drawn: state %s target %d latest %v", timeline.State, timeline.TargetTimeline, timeline.Latest)
	}
}

// TestSiblingPathNeedsItsTimeline: timeline 3 is what 'latest' resolves
// to; a sibling path toward timeline 2 (lower number, off the ancestry)
// is not drawn, but a time only it reaches says which targetTLI a restore
// along it needs. A path flagged only by its reason code counts too.
func TestSiblingPathNeedsItsTimeline(t *testing.T) {
	t.Parallel()
	latest := tlPath("b1", 48*time.Hour, 30*time.Hour, evidencev1alpha1.CoverageFrontier)
	latest.TargetTimeline, latest.StartTimeline, latest.FrontierTimeline = 3, pu64(3), pu64(3)
	sibling := tlPath("b2", 20*time.Hour, 2*time.Hour, evidencev1alpha1.CoverageFrontier)
	sibling.State, sibling.Reason = evidencev1alpha1.Warning, evidencev1alpha1.Reason{
		Code:    evidencev1alpha1.ReasonPathNeedsTargetTimeline,
		Message: "a default restore follows timeline 3, whose history does not lead through timeline 2: needs targetTLI 2",
	}
	ev := timelineFixture{paths: []evidence.RecoveryPath{latest, sibling}}.evidence()
	tl := (&Service{}).recoveryTimeline(ev, viewmodel.BackupSummary{}, nil, recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout})
	if tl.TargetTimeline != 3 || len(tl.OffTarget) != 1 || tl.OffTarget[0].TargetTimeline != 2 || tl.OffTarget[0].BackupID != "b2" {
		t.Fatalf("target %d off-target %+v", tl.TargetTimeline, tl.OffTarget)
	}
	got := restoreCheck(viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}, now.Add(-10*time.Hour))
	if got.Answer == "covered" {
		t.Fatalf("a sibling-only time answered covered: %+v", got)
	}
	found := false
	for _, c := range got.Caveats {
		found = found || (c.Code == "recovery.offTarget.needsTarget" && c.Params["timeline"] == "2" && c.Params["backupId"] == "b2")
	}
	if !found {
		t.Errorf("no needs-targetTLI caveat: %+v", got.Caveats)
	}
	if covered := restoreCheck(viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}, now.Add(-40*time.Hour)); covered.Answer != "covered" {
		t.Errorf("control on the latest line: %+v", covered)
	} else {
		for _, c := range covered.Caveats {
			if c.Code == "recovery.offTarget.needsTarget" {
				t.Errorf("a covered answer carries the off-target caveat: %+v", c)
			}
		}
	}
}

// TestTimelineWarningIsShownNotCapped: the agent's timeline warning (a
// history no default restore reads is malformed) is on the timeline and
// in every answer's caveats, names the file, and is not the history file
// coverage rests on.
func TestTimelineWarningIsShownNotCapped(t *testing.T) {
	t.Parallel()
	ev := timelineFixture{paths: []evidence.RecoveryPath{tlPath("b1", 48*time.Hour, 20*time.Hour, evidencev1alpha1.CoverageFrontier)}}.evidence()
	ev.result.Recovery.TimelineWarning = "00000002.history: timeline history is malformed; no default restore reads it"
	tl := (&Service{}).recoveryTimeline(ev, viewmodel.BackupSummary{}, nil, recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout})
	if tl.State != string(evidencev1alpha1.Healthy) {
		t.Errorf("the warning capped the window: %s", tl.State)
	}
	if w := tl.TimelineWarning; w == nil || w.Code != "timeline.reason.unneededInvalid" || w.Params["historyFile"] != "00000002.history" {
		t.Fatalf("timeline warning %+v", w)
	}
	b := viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}
	got := restoreCheck(b, now.Add(-30*time.Hour))
	if got.Answer != "covered" || got.HistoryFile != "" {
		t.Errorf("answer %s history file %q", got.Answer, got.HistoryFile)
	}
	found := false
	for _, c := range got.Caveats {
		found = found || (c.Code == "recovery.caveat.timelineWarning" && c.Params["historyFile"] == "00000002.history")
	}
	if !found {
		t.Errorf("no timeline warning caveat: %+v", got.Caveats)
	}
}

// TestRecoverySpansSkipOffTargetAndCappedPaths: defensively, a path on
// the target's number flagged off-target, or capped by the console with
// a receipt left on it, is never drawn as covered.
func TestRecoverySpansSkipOffTargetAndCappedPaths(t *testing.T) {
	t.Parallel()
	onTarget := tlPath("b1", 48*time.Hour, 30*time.Hour, evidencev1alpha1.CoverageFrontier)
	sibling := tlPath("b2", 20*time.Hour, time.Hour, evidencev1alpha1.CoverageFrontier)
	sibling.OffTarget = true
	capped := tlPath("b3", 20*time.Hour, time.Hour, evidencev1alpha1.CoverageUnknownLimited)
	capped.Reason.Code = cappedPathCode
	for _, extra := range []evidence.RecoveryPath{sibling, capped} {
		ev := timelineFixture{paths: []evidence.RecoveryPath{onTarget, extra}}.evidence()
		tl := (&Service{}).recoveryTimeline(ev, viewmodel.BackupSummary{}, nil, recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout})
		if tl.Latest == nil || !tl.Latest.Equal(*onTarget.FrontierReceiptAt) {
			t.Errorf("%s: latest %v, want only %s's frontier", extra.BackupID, tl.Latest, onTarget.BackupID)
		}
		for _, seg := range tl.Segments {
			if seg.Kind == segmentCovered && seg.BackupID == extra.BackupID {
				t.Errorf("%s drawn as covered: %+v", extra.BackupID, seg)
			}
		}
	}
}
