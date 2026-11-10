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

package fixtures

import (
	"time"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// RecoveryTimeline is orders' recovery window from the evidence agent:
// a week of coverage with one confirmed WAL hole, daily backups, one of
// them failed.
func RecoveryTimeline() viewmodel.RecoveryTimeline {
	day := 24 * time.Hour
	daily := func(daysAgo int) time.Time { return Now.Add(-time.Duration(daysAgo)*day - 5*time.Hour + 8*time.Minute) }
	markers := []viewmodel.RecoveryMarker{}
	for d := 7; d >= 1; d-- {
		id := daily(d).Format("20060102T150405")
		m := viewmodel.RecoveryMarker{At: daily(d), BackupID: id, Kind: "completed", Source: "repository"}
		if d == 1 {
			m = viewmodel.RecoveryMarker{
				At: Now.Add(-29 * time.Hour), Name: "orders-daily-20261028070000", Kind: "failed", Source: "backupObject",
				Text: "rpc error: code = Unknown desc = missing WAL file 000000030000000000000007",
			}
		}
		markers = append(markers, m)
	}
	markers = append(markers, viewmodel.RecoveryMarker{At: *ago(4*time.Hour + 52*time.Minute), BackupID: "20261029T070001", Kind: "completed", Source: "repository"})
	earliest, latest := daily(7), *ago(45 * time.Second)
	holeFrom, holeTo := daily(5).Add(9*time.Hour), daily(4)
	// The newest edge as the read model draws it: within three
	// archive_timeouts of the newest receipt the target is unknown, and
	// the last archive_timeout is where the archived WAL ends.
	safe, walEnd := latest.Add(-15*time.Minute), latest.Add(-5*time.Minute)
	return viewmodel.RecoveryTimeline{
		Basis: "evidence", State: "warning", Reason: "the window has holes where WAL is missing",
		Earliest: &earliest, Latest: &latest, LastSafeTarget: &safe, TargetTimeline: 3,
		Segments: []viewmodel.RecoverySegment{
			{
				From: earliest, To: holeFrom, Kind: "covered", BackupID: daily(6).Format("20060102T150405"),
				Text: "Structurally covered: base backup " + daily(6).Format("20060102T150405") + " and contiguous archived WAL.",
				Code: "recovery.segment.covered", Params: map[string]string{"backupId": daily(6).Format("20060102T150405")},
			},
			{From: holeFrom, To: holeTo, Kind: "missingWAL", Text: "WAL is missing from the repository: no restore target in this span.", Code: "recovery.segment.missingWAL"},
			{
				From: holeTo, To: safe, Kind: "covered", BackupID: "20261029T070001",
				Text: "Structurally covered: base backup 20261029T070001 and contiguous archived WAL.",
				Code: "recovery.segment.covered", Params: map[string]string{"backupId": "20261029T070001"},
			},
			{
				From: safe, To: walEnd, Kind: "unknown",
				Text: "Within 15 min of the end of the archived WAL: segment names cannot tell whether this target's WAL is archived yet.",
				Code: "recovery.segment.nearEnd", Params: map[string]string{"marginSeconds": "900"},
			},
			{
				From: walEnd, To: latest, Kind: "walEnd",
				Text: "WAL ends near here: the newest archived segment reached the repository at " + latest.UTC().Format(time.RFC3339) +
					", and its last commit may be up to 5 min older. A recovery_target_time after the last commit fails with \"recovery ended before configured recovery target was reached\"; to restore everything archived, restore to the end of WAL (no recovery target).",
				Code: "recovery.segment.walEnd", Params: map[string]string{"endAt": latest.UTC().Format(time.RFC3339), "marginSeconds": "300"},
			},
		},
		Markers:     markers,
		Assumptions: []string{"segment-name-presence-only", "wal-bytes-and-restore-not-verified"},
		Complete:    true,
		Provenance:  evidenceProv("EvidenceAgent orders-evidence /api/v1alpha1/recovery-paths"),
	}
}

// RestoreCheck is orders' answer for a time inside the newer span.
func RestoreCheck() viewmodel.RestoreCheck {
	return viewmodel.RestoreCheck{
		Ref: ordersRef, GeneratedAt: Now, At: Now.Add(-2 * time.Hour), Answer: "covered", BackupID: "20261029T070001",
		Text:   "Structurally covered: restore from base backup 20261029T070001 and replay the archived WAL to this time.",
		Code:   "recovery.check.covered",
		Params: map[string]string{"backupId": "20261029T070001"},
		Basis:  "evidence",
		Caveats: []viewmodel.Phrase{
			{Code: "recovery.caveat.segmentNames", Text: "Judged from WAL segment names in the repository: the WAL bytes were not read and no restore was run."},
			{Code: "recovery.caveat.latestBounded", Text: "The latest restore point is bounded by when the last contiguous segment reached the repository; the newest transactions may still be only on the instance."},
		},
		Provenance: evidenceProv("EvidenceAgent orders-evidence /api/v1alpha1/recovery-paths"),
	}
}
