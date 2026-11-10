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
