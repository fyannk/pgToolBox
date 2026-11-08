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
	"strings"
	"testing"
	"time"
)

func TestRestoreDrillDetector(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	long := now.Add(-200 * day)
	cases := []struct {
		name    string
		in      Input
		finding string // a substring of the summary; empty: none
		why     string // the unavailable reason; empty: it ran
	}{
		{name: "drills off", in: Input{Now: now}, why: reasonDrillsOff},
		{
			name: "not drillable", in: Input{Now: now, HasDrills: true, Drills: DrillsSnapshot{Persistent: true, RecordedSince: long}},
		},
		{
			name: "passed within 90 days, record short",
			in: Input{Now: now, HasDrills: true, Drills: DrillsSnapshot{
				Drillable: true, RecordedSince: now.Add(-time.Hour), LastPassedAt: now.Add(-30 * time.Minute), Recorded: 1,
			}},
		},
		{
			name: "passed within 90 days",
			in: Input{Now: now, HasDrills: true, Drills: DrillsSnapshot{
				Drillable: true, Persistent: true, RecordedSince: long, LastPassedAt: now.Add(-89 * day), Recorded: 4,
			}},
		},
		{
			name: "in memory since a restart: unknown, never 'no drill'",
			in:   Input{Now: now, HasDrills: true, Drills: DrillsSnapshot{Drillable: true, RecordedSince: now.Add(-2 * time.Hour)}},
			why:  reasonDrillsInMemory,
		},
		{
			name: "persistent record younger than 90 days: unknown",
			in:   Input{Now: now, HasDrills: true, Drills: DrillsSnapshot{Drillable: true, Persistent: true, RecordedSince: now.Add(-60 * day)}},
			why:  reasonDrillsRecordShort,
		},
		{
			name: "no coverage start: unknown",
			in:   Input{Now: now, HasDrills: true, Drills: DrillsSnapshot{Drillable: true, Persistent: true}},
			why:  reasonDrillsRecordShort,
		},
		{
			name:    "record covers 90 days, nothing passed",
			in:      Input{Now: now, HasDrills: true, Drills: DrillsSnapshot{Drillable: true, Persistent: true, RecordedSince: long, Recorded: 2}},
			finding: "No restore drill run from this console has passed",
		},
		{
			name: "the last pass is older than 90 days",
			in: Input{Now: now, HasDrills: true, Drills: DrillsSnapshot{
				Drillable: true, Persistent: true, RecordedSince: long, LastPassedAt: now.Add(-120 * day), Recorded: 5,
			}},
			finding: "judged on 2026-04-11, more than 90 days ago",
		},
		{
			name: "an in-memory console up for more than 90 days knows",
			in: Input{Now: now, HasDrills: true, Drills: DrillsSnapshot{
				Drillable: true, RecordedSince: now.Add(-91 * day),
			}},
			finding: "No restore drill run from this console has passed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			findings, why := (restoreDrillDetector{}).Detect(tc.in)
			if why != tc.why {
				t.Fatalf("why = %q, want %q", why, tc.why)
			}
			if why != "" && !sourceOff(why) {
				t.Errorf("%q reads as a fault; the drill record's limits are conditions of the deployment", why)
			}
			if tc.finding == "" {
				if len(findings) != 0 {
					t.Fatalf("findings = %+v, want none", findings)
				}
				return
			}
			if len(findings) != 1 {
				t.Fatalf("findings = %d, want 1", len(findings))
			}
			f := findings[0]
			if !strings.Contains(f.Summary, tc.finding) || f.Severity != SeverityNote || f.ID != "restore-drill-overdue" {
				t.Errorf("finding = %+v", f)
			}
			if len(f.Evidence) != 1 || f.Evidence[0].Origin != "console-recorded" {
				t.Errorf("evidence = %+v", f.Evidence)
			}
		})
	}
}
