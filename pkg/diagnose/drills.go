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

package diagnose

import (
	"fmt"
	"time"
)

// DrillWindow is how recent a passed restore drill must be:
// a quarter, the cadence a recovery time objective is usually re-proved at.
const DrillWindow = 90 * 24 * time.Hour

// DrillsSnapshot is what the console's restore-drill record says about one
// Cluster. Input.HasDrills is false when restore drills are not enabled on
// the console: then nothing is recorded and nothing may be concluded.
type DrillsSnapshot struct {
	// Drillable reports that the console could drill the Cluster: it backs
	// up through the barman-cloud plugin or barmanObjectStore, and is not
	// itself a drill.
	Drillable bool
	// RecordedSince is when the record became complete: every drill the
	// console ran from then on is in it (or, when it was dropped by the
	// record's bound, a newer passed one is). Before it, nothing is known.
	RecordedSince time.Time
	// Persistent reports that the record survives a console restart.
	Persistent bool
	// LastPassedAt is when the newest passed drill of the Cluster was
	// judged; zero when the record holds none.
	LastPassedAt time.Time
	// Recorded is how many drills of the Cluster the record holds, any
	// result.
	Recorded int
}

// The two ways the drill record cannot answer. Both are conditions of the
// deployment, not faults: they read as switched-off sources (sourceOff).
const (
	reasonDrillsOff = "restore drills are not enabled on this console (RESTORE_DRILL_ENABLED)"
	// reasonDrillsInMemory: the record restarts empty with the console, so
	// "no drill in 90 days" would be a claim without evidence.
	reasonDrillsInMemory = "the drill record is kept in memory since the console started, less than 90 days ago, so a drill before then is not known (enable persistent history to keep it)"
	// reasonDrillsRecordShort: persistent, but it began less than 90 days
	// ago.
	reasonDrillsRecordShort = "the console's persistent drill record began less than 90 days ago, so a drill before then is not known"
)

// restoreDrillDetector notes a drillable Cluster whose console recorded no
// passed restore drill in the last 90 days. It concludes only from a
// record that reaches back the whole window: a console that restarted, or
// started keeping drills recently, cannot know, and says so instead.
//
// It implements Detector.
type restoreDrillDetector struct{}

func (restoreDrillDetector) Name() string { return "restore-drill-overdue" }

func (restoreDrillDetector) Layer() Layer { return LayerBackups }

func (restoreDrillDetector) Describes() string {
	return "a Cluster this console can drill with no passed restore drill in its record for 90 days"
}

func (d restoreDrillDetector) Detect(in Input) ([]Finding, string) {
	if !in.HasDrills {
		return nil, reasonDrillsOff
	}
	dr := in.Drills
	if !dr.Drillable {
		// Nothing to drill: the drill button says why, and missing
		// backups are other checks' findings.
		return nil, ""
	}
	cutoff := in.Now.Add(-DrillWindow)
	if !dr.LastPassedAt.IsZero() && dr.LastPassedAt.After(cutoff) {
		return nil, ""
	}
	if dr.RecordedSince.IsZero() || dr.RecordedSince.After(cutoff) {
		if dr.Persistent {
			return nil, reasonDrillsRecordShort
		}
		return nil, reasonDrillsInMemory
	}
	summary := "No restore drill run from this console has passed for this Cluster in the last 90 days."
	if !dr.LastPassedAt.IsZero() {
		summary = fmt.Sprintf("The last restore drill of this Cluster that passed was judged on %s, more than 90 days ago.",
			dr.LastPassedAt.UTC().Format(time.DateOnly))
	}
	return []Finding{{
		ID:       d.Name(),
		Subject:  EntityRef{Kind: "Cluster"},
		Severity: SeverityNote,
		Summary:  summary,
		Detail: "A backup is proven restorable only by restoring it. A drill restores the latest backup " +
			"into a throwaway Cluster, checks it and deletes it; its duration is the measured recovery time. " +
			"Drills run by hand or by another console are not in this console's record.",
		Evidence: []Evidence{{
			Origin: "console-recorded",
			Object: "restore drill record",
			Detail: recordDetail(dr),
		}},
		NextSteps: "Run a restore drill from the Backups screen.",
		Link:      "/backups",
		LinkLabel: "Backups",
	}}, ""
}

// recordDetail states what the record holds and since when.
func recordDetail(dr DrillsSnapshot) string {
	since := dr.RecordedSince.UTC().Format(time.DateOnly)
	switch dr.Recorded {
	case 0:
		return "no drill of this Cluster recorded since " + since
	case 1:
		return "1 drill of this Cluster recorded since " + since + ", none passed in the last 90 days"
	default:
		return fmt.Sprintf("%d drills of this Cluster recorded since %s, none passed in the last 90 days", dr.Recorded, since)
	}
}
