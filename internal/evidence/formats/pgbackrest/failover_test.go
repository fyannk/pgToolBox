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

package pgbackrest

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
)

// The real B1 timeline switch (testdata/pgbackrest-2.60.0/plain): timeline 2
// branched from timeline 1 at 0/E000050, so S = 0xE; timeline 1 has segments
// 1..0x12 (it went on after the branch), timeline 2 has 0xE..0x14. One backup
// ran on timeline 1 from segment 0xD to 0xE and stopped exactly at the
// switchpoint; one ran on timeline 2 from 0xF to 0x10.
const (
	switchSegmentTL1 = "00000001000000000000000E"
	beforeSwitchTL1  = "00000001000000000000000D"
	switchSegmentTL2 = "00000002000000000000000E"
	switchHistoryKey = "archive/demo/17-1/00000002.history"
)

// backupStartingAt returns the backup whose backup-archive-start is name.
func backupStartingAt(t testing.TB, report Report, name string) BackupReport {
	t.Helper()
	for _, backup := range report.Backups {
		if backup.Backup.ArchiveStart == name {
			return backup
		}
	}
	t.Fatalf("no backup starts at %s", name)
	return BackupReport{}
}

// segmentKeysOn lists the keys of the timeline's segments from position
// first on (backup label files included, as expire removes them too).
func segmentKeysOn(t testing.TB, repository *fixtureRepository, timeline uint32, first uint64) []string {
	t.Helper()
	var keys []string
	for _, object := range repository.objects {
		classified, ok := ClassifyWALObject(object)
		if !ok || (classified.Class != WALSegment && classified.Class != WALBackupHistory) || classified.Timeline != timeline {
			continue
		}
		_, position, err := ParseWALName(classified.Name, 16<<20)
		if err != nil {
			t.Fatal(err)
		}
		if position >= first {
			keys = append(keys, object.Key)
		}
	}
	return keys
}

// withListed adds names to the listing only (no content), as WAL is.
func (r *fixtureRepository) withListed(keys ...string) *fixtureRepository {
	clone := r.without()
	for _, key := range keys {
		clone.objects = append(clone.objects, storeObject{Key: key, Size: 16 << 20})
	}
	sort.Slice(clone.objects, func(i, j int) bool { return clone.objects[i].Key < clone.objects[j].Key })
	return clone
}

func TestHistorySwitchPositionIsTheSegmentHoldingTheSwitchpoint(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		switchpoint string
		want        uint64
	}{
		{switchpoint: "0/E000050", want: 0xE}, // mid-segment, as in the real fixture
		{switchpoint: "0/E000000", want: 0xE}, // on a boundary: a fresh child segment S
		{switchpoint: "0/DFFFFFF", want: 0xD}, // the last byte of the previous segment
		{switchpoint: "1/0", want: 0x100},     // across a log boundary
	} {
		edges, err := ParseHistory([]byte("1\t"+test.switchpoint+"\tno recovery target specified\n"), 2, 16<<20)
		if err != nil || len(edges) != 1 || edges[0].SwitchPosition != test.want {
			t.Fatalf("%s: edges %+v, err %v", test.switchpoint, edges, err)
		}
	}
}

// TestFailoverRuleOnTheRealTimelineSwitch checks the parent < S, child >= S
// rule against the history and WAL that PostgreSQL and pgBackRest wrote.
func TestFailoverRuleOnTheRealTimelineSwitch(t *testing.T) {
	t.Parallel()
	plain := loadFixture(t, fixtureRoot, "plain")
	baseline := analyzeFixture(t, plain, "plain")
	if len(baseline.Timelines) != 1 || baseline.Timelines[0].Edges[0].SwitchPosition != 0xE {
		t.Fatalf("the fixture's switch moved: %+v", baseline.Timelines)
	}
	crossing := backupStartingAt(t, baseline, beforeSwitchTL1)
	if !crossing.Usable || crossing.Path.State != evidence.Healthy || !slices.Equal(crossing.Path.Timelines, []uint32{1, 2}) || crossing.Path.FrontierTimeline != 2 {
		t.Fatalf("the backup that ends at the switchpoint: %+v", crossing.Path)
	}

	t.Run("the parent's segments from S on are not needed", func(t *testing.T) {
		t.Parallel()
		report := analyzeFixture(t, plain.without(segmentKeysOn(t, plain, 1, 0xE)...), "plain")
		if report.State != evidence.Healthy || len(report.Findings) != 0 {
			t.Fatalf("state %s %q, findings %+v", report.State, report.Reason, report.Findings)
		}
		for _, backup := range report.Backups {
			if !backup.Usable {
				t.Fatalf("%s: path %+v", backup.Backup.Label, backup.Path)
			}
		}
		if report.Window.EndTimeline != baseline.Window.EndTimeline || report.Window.EndWAL != baseline.Window.EndWAL || report.Window.OldestBackup != baseline.Window.OldestBackup {
			t.Fatalf("window %+v, was %+v", report.Window, baseline.Window)
		}
	})
	t.Run("the parent's segment S-1 is needed", func(t *testing.T) {
		t.Parallel()
		report := analyzeFixture(t, plain.without(segmentKey(t, plain, beforeSwitchTL1)), "plain")
		backup := backupStartingAt(t, report, beforeSwitchTL1)
		if backup.Usable || backup.Path.State != evidence.Unhealthy || backup.Path.Consistent {
			t.Fatalf("path %+v", backup.Path)
		}
	})
	t.Run("the child's segment S is needed", func(t *testing.T) {
		t.Parallel()
		report := analyzeFixture(t, plain.without(segmentKey(t, plain, switchSegmentTL2)), "plain")
		backup := backupStartingAt(t, report, beforeSwitchTL1)
		if backup.Usable || backup.Path.State != evidence.Unhealthy || backup.Path.FrontierTimeline != 1 || backup.Path.FrontierWAL != beforeSwitchTL1 {
			t.Fatalf("path %+v", backup.Path)
		}
		// A child that starts after S is pruning, not a contradiction.
		if report.Timelines[0].State != evidence.Healthy {
			t.Fatalf("history %+v", report.Timelines[0])
		}
		if onTwo := backupStartingAt(t, report, "00000002000000000000000F"); !onTwo.Usable {
			t.Fatalf("the backup on timeline 2: %+v", onTwo.Path)
		}
	})
}

// TestFailoverWithAPartialParentSegment is a promotion as a failover leaves
// it: the parent's switch segment exists only as .partial, which recovery
// never reads and which fills nothing.
func TestFailoverWithAPartialParentSegment(t *testing.T) {
	t.Parallel()
	plain := loadFixture(t, fixtureRoot, "plain")
	switchKey := segmentKey(t, plain, switchSegmentTL1)
	partialKey := strings.Replace(switchKey, switchSegmentTL1+"-", switchSegmentTL1+".partial-", 1)
	repository := plain.without(segmentKeysOn(t, plain, 1, 0xE)...).withListed(partialKey)
	report := analyzeFixture(t, repository, "plain")
	if report.State != evidence.Healthy || len(report.Findings) != 0 {
		t.Fatalf("state %s %q, findings %+v", report.State, report.Reason, report.Findings)
	}
	if report.WAL[0].Counts.Partials != 1 || report.WAL[0].State != evidence.Healthy {
		t.Fatalf("WAL %+v", report.WAL[0])
	}
	for _, value := range report.WAL[0].Ranges {
		if value.Timeline == 1 && value.End >= 0xE {
			t.Fatalf("the .partial filled continuity: %+v", value)
		}
	}
	backup := backupStartingAt(t, report, beforeSwitchTL1)
	if !backup.Usable || !slices.Equal(backup.Path.Timelines, []uint32{1, 2}) || backup.Path.FrontierTimeline != 2 {
		t.Fatalf("path %+v", backup.Path)
	}
	// A boundary switchpoint (a fresh child segment S) reads the same way.
	boundary := repository.with(switchHistoryKey, []byte("1\t0/E000000\tno recovery target specified\n"))
	if backup := backupStartingAt(t, analyzeFixture(t, boundary, "plain"), "000000010000000000000008"); !backup.Usable {
		t.Fatalf("boundary switchpoint: %+v", backup.Path)
	}
}

// TestPrunedAncestryKeepsTheHistory is the repository after retention
// expired everything before the failover: the parent has no WAL left and
// the child starts after its switch segment.
func TestPrunedAncestryKeepsTheHistory(t *testing.T) {
	t.Parallel()
	plain := loadFixture(t, fixtureRoot, "plain")
	pruned := plain.without(append(segmentKeysOn(t, plain, 1, 0), segmentKey(t, plain, switchSegmentTL2))...)
	report := analyzeFixture(t, pruned, "plain")
	if len(report.Timelines) != 1 || report.Timelines[0].State != evidence.Healthy {
		t.Fatalf("history %+v", report.Timelines)
	}
	for _, finding := range report.Findings {
		if finding.Subject == switchHistoryKey {
			t.Fatalf("finding on the history: %+v", finding)
		}
	}
	onTwo := backupStartingAt(t, report, "00000002000000000000000F")
	if !onTwo.Usable || onTwo.Path.State != evidence.Healthy || report.Window == nil || report.Window.OldestBackup != onTwo.Backup.Label {
		t.Fatalf("path %+v, window %+v", onTwo.Path, report.Window)
	}
	if crossing := backupStartingAt(t, report, beforeSwitchTL1); crossing.Usable || crossing.Path.State == evidence.Unknown {
		t.Fatalf("a pre-failover backup whose WAL is gone: %+v", crossing.Path)
	}
}

// TestChildWALBeforeTheSwitchContradictsTheHistory: PostgreSQL never writes
// a child segment before S, so such a listing makes the ancestry unknown.
func TestChildWALBeforeTheSwitchContradictsTheHistory(t *testing.T) {
	t.Parallel()
	plain := loadFixture(t, fixtureRoot, "plain")
	early := strings.Replace(segmentKey(t, plain, switchSegmentTL2), switchSegmentTL2, "00000002000000000000000D", 1)
	report := analyzeFixture(t, plain.withListed(early), "plain")
	if report.Timelines[0].State != evidence.Unknown || report.Timelines[0].Reason != "child WAL range disagrees with timeline switchpoint" {
		t.Fatalf("history %+v", report.Timelines[0])
	}
	if !slices.Contains(findingCodes(report), FindingWALUnknown) {
		t.Fatalf("findings %+v", report.Findings)
	}
	if crossing := backupStartingAt(t, report, beforeSwitchTL1); crossing.Usable || crossing.Path.State != evidence.Unknown {
		t.Fatalf("path %+v", crossing.Path)
	}
}
