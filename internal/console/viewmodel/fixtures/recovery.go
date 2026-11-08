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
	return viewmodel.RecoveryTimeline{
		Basis: "evidence", State: "warning", Reason: "the window has holes where WAL is missing",
		Earliest: &earliest, Latest: &latest, TargetTimeline: 3,
		Segments: []viewmodel.RecoverySegment{
			{From: earliest, To: holeFrom, Kind: "covered", BackupID: daily(6).Format("20060102T150405"), Text: "Structurally covered: base backup " + daily(6).Format("20060102T150405") + " and contiguous archived WAL."},
			{From: holeFrom, To: holeTo, Kind: "missingWAL", Text: "WAL is missing from the repository: no restore target in this span."},
			{From: holeTo, To: latest, Kind: "covered", BackupID: "20261029T070001", Text: "Structurally covered: base backup 20261029T070001 and contiguous archived WAL."},
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
		Text:  "Structurally covered: restore from base backup 20261029T070001 and replay the archived WAL to this time.",
		Basis: "evidence",
		Caveats: []string{
			"Judged from WAL segment names in the repository: the WAL bytes were not read and no restore was run.",
			"The latest restore point is bounded by when the last contiguous segment reached the repository; the newest transactions may still be only on the instance.",
		},
		Provenance: evidenceProv("EvidenceAgent orders-evidence /api/v1alpha1/recovery-paths"),
	}
}
