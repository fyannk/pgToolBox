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

// TestOldDegradedBackupLeavesNewerSpansCovered (round 7, P1, the pg and
// DBA overlay probes): an old backup stops at a confirmed WAL gap, so the
// agent's roll-up is unhealthy; the newer backups' own paths reach the
// frontier and stay healthy (the console caps a path only at a roll-up no
// path explains). Their spans are covered, a newest covered target is
// named, and the roll-up is said as a reason, never drawn as un-coverage.
func TestOldDegradedBackupLeavesNewerSpansCovered(t *testing.T) {
	svc := &Service{}
	facts := recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout}
	rollup := "one or more recovery paths cross a confirmed required gap"
	for _, tc := range []struct {
		name  string
		old   evidence.RecoveryPath
		paths []evidence.RecoveryPath
		at    []time.Duration
	}{
		{
			name:  "pg probe: 20-day-old gap-limited backup, two newer healthy ones",
			old:   tlPath("a-old", 20*24*time.Hour, 19*24*time.Hour, evidencev1alpha1.CoverageGapLimited),
			paths: []evidence.RecoveryPath{tlPath("b", 7*24*time.Hour, 10*time.Minute, evidencev1alpha1.CoverageFrontier), tlPath("c", 24*time.Hour, 10*time.Minute, evidencev1alpha1.CoverageFrontier)},
			at:    []time.Duration{3 * 24 * time.Hour, 2 * time.Hour},
		},
		{
			name:  "DBA probe: a week-old gap-limited backup, two newer healthy ones",
			old:   tlPath("b0", 200*time.Hour, 150*time.Hour, evidencev1alpha1.CoverageGapLimited),
			paths: []evidence.RecoveryPath{tlPath("b1", 48*time.Hour, time.Minute, evidencev1alpha1.CoverageFrontier), tlPath("b2", 24*time.Hour, time.Minute, evidencev1alpha1.CoverageFrontier)},
			at:    []time.Duration{30 * time.Hour, 2 * time.Hour},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := timelineFixture{paths: append([]evidence.RecoveryPath{tc.old}, tc.paths...)}.evidence()
			ev.result.Recovery.CoverageReason = rollup
			tl := svc.recoveryTimeline(ev, viewmodel.BackupSummary{}, nil, facts)
			if tl.NewestCoveredTarget == nil {
				t.Fatalf("no newest covered target: state %s reasons %+v", tl.State, tl.Reasons)
			}
			agentSaid := false
			for _, r := range tl.Reasons {
				agentSaid = agentSaid || (r.Code == "recovery.reason.agentCoverage" && r.Params["detail"] == rollup)
			}
			if !agentSaid {
				t.Errorf("the roll-up is not said as a reason: %+v", tl.Reasons)
			}
			b := viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}
			for _, at := range tc.at {
				if got := restoreCheck(b, now.Add(-at)); got.Answer != "covered" {
					t.Errorf("now-%s: %s %s, want covered", at, got.Answer, got.Code)
				}
			}
			// The old backup's gap is still drawn as a hole.
			if got := restoreCheck(b, tc.old.FrontierReceiptAt.Add(time.Hour)); got.Answer != "gap" {
				t.Errorf("inside the old backup's gap: %+v", got)
			}
		})
	}
}

// TestNewestCoveredTargetWithAHealthyAndADegradedSpanEndingTogether:
// a healthy span ending where a degraded one ends covers the newest end,
// so the newest covered target is named.
func TestNewestCoveredTargetWithAHealthyAndADegradedSpanEndingTogether(t *testing.T) {
	svc := &Service{}
	facts := recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout}
	warn := tlPath("b2", 24*time.Hour, 10*time.Minute, evidencev1alpha1.CoverageFrontier)
	warn.State, warn.Reason = evidencev1alpha1.Warning, evidencev1alpha1.Reason{Code: "coverage-path-warning", Message: "the WAL segment size is not proven across log id 0/40"}
	healthy := tlPath("b1", 48*time.Hour, 10*time.Minute, evidencev1alpha1.CoverageFrontier)
	tl := svc.recoveryTimeline(timelineFixture{paths: []evidence.RecoveryPath{healthy, warn}}.evidence(), viewmodel.BackupSummary{}, nil, facts)
	if tl.NewestCoveredTarget == nil {
		t.Fatalf("no newest covered target: %+v", tl.Segments)
	}
	if got := restoreCheck(viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}, now.Add(-2*time.Hour)); got.Answer != "covered" || got.BackupID != "b1" {
		t.Errorf("inside both spans the healthy one answers: %+v", got)
	}
}
