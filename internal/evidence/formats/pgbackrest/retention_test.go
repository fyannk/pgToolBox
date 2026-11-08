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
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
)

// newest is the reference of the synthetic labels and stop times; the
// evaluation time is the stop of a stanza's newest backup (its last
// expire), so the time-based cases end with a backup at day 0.
var newest = time.Date(2026, 11, 7, 8, 0, 0, 0, time.UTC)

// synthetic is a stanza built in memory: backups on timeline 1 of archive
// 17-1, with 16 MiB segments, and the WAL ranges given.
type synthetic struct {
	backups []BackupReport
	ranges  [][2]uint64
}

// stamp is a label timestamp `days` days before newest (labels sort by it).
func stamp(days int) string {
	return newest.Add(-time.Duration(days) * 24 * time.Hour).Format("20060102-150405")
}

func walName(position uint64) string {
	name, err := WALName(1, position, DefaultWALSegmentSize)
	if err != nil {
		panic(err)
	}
	return name
}

// full adds a full backup that stopped `days` days before newest, its WAL
// from start to stop; it returns its label.
func (s *synthetic) full(days int, start, stop uint64) string {
	label := stamp(days) + "F"
	s.add(Backup{Label: label, Type: BackupFull}, days, start, stop)
	return label
}

// child adds a differential or incremental backup of the full backup.
func (s *synthetic) child(full string, kind BackupType, days int, start, stop uint64) string {
	suffix := "D"
	if kind == BackupIncremental {
		suffix = "I"
	}
	label := full + "_" + stamp(days) + suffix
	s.add(Backup{Label: label, Type: kind, Prior: full, Reference: []string{full}}, days, start, stop)
	return label
}

func (s *synthetic) add(backup Backup, days int, start, stop uint64) {
	backup.DBID = 1
	backup.TimestampStop = newest.Add(-time.Duration(days) * 24 * time.Hour)
	backup.TimestampStart = backup.TimestampStop.Add(-time.Minute)
	backup.ArchiveStart, backup.ArchiveStop = walName(start), walName(stop)
	s.backups = append(s.backups, BackupReport{Backup: backup, ArchiveID: "17-1", Usable: true})
}

func (s *synthetic) report() Report {
	wal := ArchiveWAL{Stanza: "demo", ArchiveID: "17-1", State: evidence.Healthy, SegmentSize: DefaultWALSegmentSize}
	for _, value := range s.ranges {
		wal.Ranges = append(wal.Ranges, WALRange{Timeline: 1, Start: value[0], End: value[1], Count: value[1] - value[0] + 1, First: walName(value[0]), Last: walName(value[1])})
	}
	return Report{
		Stanza: "demo", ArchiveStanza: "demo", Database: &PGHistory{ID: 1, Version: "17"}, ArchiveIDs: []string{"17-1"},
		SegmentSize: DefaultWALSegmentSize, Backups: slices.Clone(s.backups), WAL: []ArchiveWAL{wal},
	}
}

// without is the report minus the backups with these labels.
func without(report Report, labels ...string) Report {
	report.Backups = slices.DeleteFunc(slices.Clone(report.Backups), func(backup BackupReport) bool { return slices.Contains(labels, backup.Backup.Label) })
	return report
}

type wantFinding struct {
	code     FindingCode
	severity Severity
	subject  string
}

func findingsOf(result RetentionReport) []wantFinding {
	got := make([]wantFinding, 0, len(result.Findings))
	for _, finding := range result.Findings {
		got = append(got, wantFinding{finding.Code, finding.Severity, finding.Subject})
	}
	return got
}

func TestEvaluateRetention(t *testing.T) {
	t.Parallel()
	type scenario struct {
		report   Report
		previous *Report
		want     Retention
		state    evidence.State
		findings []wantFinding
		archive  int
		check    func(*testing.T, RetentionReport)
	}
	cases := map[string]func() scenario{
		"count: as many full backups as the count": func() scenario {
			var s synthetic
			s.full(14, 3, 4)
			s.full(7, 10, 11)
			s.ranges = [][2]uint64{{3, 20}}
			return scenario{report: s.report(), want: Retention{Full: 2}, state: evidence.Healthy, archive: 2}
		},
		"count: one full backup more than the count is not applied": func() scenario {
			var s synthetic
			oldest := s.full(21, 3, 4)
			s.full(14, 10, 11)
			s.full(7, 15, 16)
			s.ranges = [][2]uint64{{3, 20}}
			return scenario{
				report: s.report(), want: Retention{Full: 2}, state: evidence.Warning, archive: 2,
				findings: []wantFinding{{FindingRetentionNotApplied, SeverityWarning, oldest}},
			}
		},
		"count: fewer full backups than the count, none lost, is a young stanza": func() scenario {
			var s synthetic
			s.full(14, 3, 4)
			s.full(7, 10, 11)
			s.ranges = [][2]uint64{{1, 20}}
			return scenario{report: s.report(), want: Retention{Full: 7}, state: evidence.Healthy, archive: 7}
		},
		"count: a full backup the count keeps disappeared since the last scan": func() scenario {
			var s synthetic
			lost := s.full(21, 3, 4)
			s.full(14, 10, 11)
			s.full(7, 15, 16)
			s.ranges = [][2]uint64{{10, 20}}
			previous := s.report()
			return scenario{
				report: without(s.report(), lost), previous: &previous, want: Retention{Full: 3}, state: evidence.Unhealthy, archive: 3,
				findings: []wantFinding{{FindingRetentionShortfall, SeverityError, lost}},
				check: func(t *testing.T, result RetentionReport) {
					if len(result.Lost) != 1 || result.Lost[0].Label != lost {
						t.Fatalf("lost %+v", result.Lost)
					}
				},
			}
		},
		"count: a lost full backup is remembered on the next scan": func() scenario {
			var s synthetic
			lost := s.full(21, 3, 4)
			s.full(14, 10, 11)
			s.ranges = [][2]uint64{{10, 20}}
			previous := without(s.report(), lost)
			previous.Retention = &RetentionReport{Lost: []LostBackup{{Label: lost, Stop: newest.Add(-21 * 24 * time.Hour)}}}
			return scenario{
				report: without(s.report(), lost), previous: &previous, want: Retention{Full: 3}, state: evidence.Unhealthy, archive: 3,
				findings: []wantFinding{{FindingRetentionShortfall, SeverityError, lost}},
			}
		},
		"count: a full backup expired by a newer one is not lost": func() scenario {
			var s synthetic
			expired := s.full(21, 3, 4)
			s.full(14, 10, 11)
			s.full(7, 15, 16)
			s.ranges = [][2]uint64{{10, 20}}
			previous := without(s.report(), s.backups[2].Backup.Label)
			return scenario{report: without(s.report(), expired), previous: &previous, want: Retention{Full: 2}, state: evidence.Healthy, archive: 2}
		},
		"time: a full backup older than the newest one before the window is not applied": func() scenario {
			var s synthetic
			oldest := s.full(20, 3, 4)
			s.full(10, 10, 11)
			s.full(0, 15, 16)
			s.ranges = [][2]uint64{{3, 20}}
			return scenario{
				report: s.report(), want: Retention{Full: 7, FullType: RetentionTime}, state: evidence.Warning, archive: 2,
				findings: []wantFinding{{FindingRetentionNotApplied, SeverityWarning, oldest}},
				check: func(t *testing.T, result RetentionReport) {
					if !result.WindowStart.Equal(newest.Add(-7*24*time.Hour)) || !result.EvaluatedAt.Equal(newest) {
						t.Fatalf("window %s, evaluated %s", result.WindowStart, result.EvaluatedAt)
					}
				},
			}
		},
		"time: the newest full backup before the window is kept with its WAL": func() scenario {
			var s synthetic
			s.full(10, 10, 11)
			s.full(0, 15, 16)
			s.ranges = [][2]uint64{{10, 20}}
			return scenario{report: s.report(), want: Retention{Full: 7, FullType: RetentionTime}, state: evidence.Healthy, archive: 2}
		},
		"time: the WAL before the oldest full backup kept is not applied": func() scenario {
			var s synthetic
			s.full(10, 10, 11)
			s.full(0, 15, 16)
			s.ranges = [][2]uint64{{2, 20}}
			return scenario{
				report: s.report(), want: Retention{Full: 7, FullType: RetentionTime}, state: evidence.Warning, archive: 2,
				findings: []wantFinding{{FindingRetentionNotApplied, SeverityWarning, "archive/demo/17-1/" + walName(2)}},
			}
		},
		"time: a window not reached yet expires nothing, WAL included": func() scenario {
			var s synthetic
			s.full(5, 10, 11)
			s.full(3, 15, 16)
			s.ranges = [][2]uint64{{2, 20}}
			return scenario{report: s.report(), want: Retention{Full: 7, FullType: RetentionTime}, state: evidence.Healthy, archive: 0}
		},
		"time: the newest full backup before the window disappeared": func() scenario {
			var s synthetic
			lost := s.full(10, 10, 11)
			s.full(0, 15, 16)
			s.ranges = [][2]uint64{{15, 20}}
			previous := s.report()
			return scenario{
				report: without(s.report(), lost), previous: &previous, want: Retention{Full: 7, FullType: RetentionTime}, state: evidence.Unhealthy, archive: 0,
				findings: []wantFinding{{FindingRetentionShortfall, SeverityError, lost}},
			}
		},
		"diff: a differential beyond the count is not applied, a full counts as one": func() scenario {
			var s synthetic
			full := s.full(10, 3, 4)
			old := s.child(full, BackupDifferential, 8, 6, 7)
			s.child(full, BackupDifferential, 6, 9, 10)
			s.ranges = [][2]uint64{{3, 20}}
			// [F, D8, D6] with repo-retention-diff=1: D8 is expired, F is
			// skipped (src/command/expire/expire.c expireDiffBackup).
			return scenario{
				report: s.report(), want: Retention{Full: 2, Diff: 1}, state: evidence.Warning, archive: 2,
				findings: []wantFinding{{FindingRetentionNotApplied, SeverityWarning, old}},
			}
		},
		"diff: the differentials of an expired full are reported through it": func() scenario {
			var s synthetic
			oldest := s.full(20, 3, 4)
			s.child(oldest, BackupDifferential, 19, 5, 5)
			s.full(10, 10, 11)
			s.full(3, 15, 16)
			s.ranges = [][2]uint64{{3, 20}}
			return scenario{
				report: s.report(), want: Retention{Full: 2, Diff: 3}, state: evidence.Warning, archive: 2,
				findings: []wantFinding{{FindingRetentionNotApplied, SeverityWarning, oldest}},
			}
		},
		"archive: older backups keep only their WAL to consistency": func() scenario {
			var s synthetic
			s.full(14, 3, 4)
			s.full(7, 10, 11)
			s.ranges = [][2]uint64{{3, 4}, {10, 20}}
			return scenario{report: s.report(), want: Retention{Full: 2, Archive: 1}, state: evidence.Healthy, archive: 1}
		},
		"archive: WAL between an older backup's stop and the archive-retention backup is not applied": func() scenario {
			var s synthetic
			s.full(14, 3, 4)
			s.full(7, 10, 11)
			s.ranges = [][2]uint64{{3, 20}}
			return scenario{
				report: s.report(), want: Retention{Full: 2, Archive: 1}, state: evidence.Warning, archive: 1,
				findings: []wantFinding{{FindingRetentionNotApplied, SeverityWarning, "archive/demo/17-1/" + walName(5)}},
			}
		},
		"archive: a backup within archive retention without its WAL from its start is a shortfall": func() scenario {
			var s synthetic
			first := s.full(14, 3, 4)
			s.full(7, 10, 11)
			s.ranges = [][2]uint64{{10, 20}}
			return scenario{
				report: s.report(), want: Retention{Full: 2}, state: evidence.Unhealthy, archive: 2,
				findings: []wantFinding{{FindingRetentionShortfall, SeverityError, first}},
			}
		},
		"archive: type diff defaults to the differential count": func() scenario {
			var s synthetic
			full := s.full(10, 3, 4)
			s.child(full, BackupDifferential, 6, 9, 10)
			s.ranges = [][2]uint64{{9, 20}}
			return scenario{report: s.report(), want: Retention{Full: 2, Diff: 1, ArchiveType: BackupDifferential}, state: evidence.Healthy, archive: 1}
		},
		"archive: type incr without a count never expires WAL": func() scenario {
			var s synthetic
			full := s.full(10, 3, 4)
			s.child(full, BackupIncremental, 5, 6, 7)
			s.ranges = [][2]uint64{{1, 20}}
			return scenario{
				report: s.report(), want: Retention{Full: 1, ArchiveType: BackupIncremental}, state: evidence.Healthy, archive: 0,
				check: func(t *testing.T, result RetentionReport) {
					if result.FullBackups != 1 || result.IncrementalBackups != 1 {
						t.Fatalf("counted %d full, %d incremental", result.FullBackups, result.IncrementalBackups)
					}
				},
			}
		},
		"not configured: a warning, not unknown": func() scenario {
			var s synthetic
			s.full(10, 3, 4)
			s.ranges = [][2]uint64{{1, 20}}
			return scenario{
				report: s.report(), want: Retention{}, state: evidence.Warning, archive: 0,
				findings: []wantFinding{{FindingRetentionNotConfigured, SeverityWarning, "backup/demo/"}},
			}
		},
		"not configured, archive set: WAL still expires": func() scenario {
			var s synthetic
			s.full(10, 3, 4)
			s.ranges = [][2]uint64{{1, 20}}
			return scenario{
				report: s.report(), want: Retention{Archive: 1}, state: evidence.Warning, archive: 1,
				findings: []wantFinding{
					{FindingRetentionNotApplied, SeverityWarning, "archive/demo/17-1/" + walName(1)},
					{FindingRetentionNotConfigured, SeverityWarning, "backup/demo/"},
				},
			}
		},
		"unknown: the segment size is not known": func() scenario {
			var s synthetic
			s.full(10, 3, 4)
			report := s.report()
			report.SegmentSize = 0
			return scenario{report: report, want: Retention{Full: 1}, state: evidence.Unknown, archive: 1}
		},
		"unknown: the WAL listing was truncated": func() scenario {
			var s synthetic
			s.full(10, 3, 4)
			s.ranges = [][2]uint64{{3, 20}}
			report := s.report()
			report.WAL[0].RangesTruncated = true
			return scenario{report: report, want: Retention{Full: 1}, state: evidence.Unknown, archive: 1}
		},
		"unknown: no backup": func() scenario {
			var s synthetic
			return scenario{report: s.report(), want: Retention{Full: 1}, state: evidence.Unknown}
		},
		"unknown: backup.info unreadable keeps the not-configured warning": func() scenario {
			return scenario{
				report: Report{Stanza: "demo", ArchiveStanza: "demo"}, want: Retention{}, state: evidence.Unknown,
				findings: []wantFinding{{FindingRetentionNotConfigured, SeverityWarning, "backup/demo/"}},
			}
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			test := build()
			before := fmt.Sprintf("%+v", test.report)
			result := EvaluateRetention(test.report, test.want, test.previous)
			if after := fmt.Sprintf("%+v", test.report); after != before {
				t.Fatal("EvaluateRetention modified its report")
			}
			if result.State != test.state || result.Reason == "" {
				t.Fatalf("state %s (%s), want %s; findings %+v", result.State, result.Reason, test.state, result.Findings)
			}
			if got := findingsOf(result); !slices.Equal(got, test.findings) {
				t.Fatalf("findings %+v, want %+v", got, test.findings)
			}
			if result.ArchiveRetention != test.archive {
				t.Fatalf("archive retention %d, want %d", result.ArchiveRetention, test.archive)
			}
			if test.check != nil {
				test.check(t, result)
			}
			for _, finding := range result.Findings {
				if len(finding.Message) > 256 || strings.ContainsAny(finding.Message, "\n\r") {
					t.Fatalf("unbounded message %q", finding.Message)
				}
			}
		})
	}
}

// TestRetentionWALScanIsBoundedBySpans: a range of 2^36 segments covered
// by one older backup, then a gap, is judged in a handful of steps, not one
// per segment (a per-segment walk would not finish). The steps are counted,
// not timed.
func TestRetentionWALScanIsBoundedBySpans(t *testing.T) {
	t.Parallel()
	const huge = uint64(1) << 36
	var s synthetic
	s.full(3, 1, huge)
	s.full(1, huge+10, huge+20)
	s.ranges = [][2]uint64{{1, huge + 20}}
	e := evaluateRetention(s.report(), Retention{Full: 2, Archive: 1}, nil)
	want := []wantFinding{{FindingRetentionNotApplied, SeverityWarning, "archive/demo/17-1/" + walName(huge+1)}}
	if got := findingsOf(e.result); !slices.Equal(got, want) {
		t.Fatalf("findings %+v, want %+v", got, want)
	}
	if e.walSteps > 2 {
		t.Fatalf("the WAL walk took %d steps, want at most 2 (one per kept span, then the gap)", e.walSteps)
	}
}

func TestRetentionValidate(t *testing.T) {
	t.Parallel()
	for _, value := range []Retention{{Full: -1}, {Diff: MaxRetention + 1}, {FullType: "days"}, {ArchiveType: "log"}} {
		if value.Validate() == nil {
			t.Fatalf("%+v accepted", value)
		}
	}
	for _, value := range []Retention{{}, {Full: MaxRetention, FullType: RetentionTime, Diff: 1, Archive: 1, ArchiveType: BackupIncremental}} {
		if err := value.Validate(); err != nil {
			t.Fatalf("%+v refused: %v", value, err)
		}
	}
}

// TestRetentionOnFixtures compares the real B1 repositories, written by
// pgBackRest 2.60.0 without a retention (so expire never removed anything),
// with expectations they meet and miss.
func TestRetentionOnFixtures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, variant string
		want          Retention
		state         evidence.State
		findings      []wantFinding
	}{
		{"plain: two full backups kept at most", "plain", Retention{Full: 2}, evidence.Healthy, nil},
		{
			// repo-retention-full=1 expires the WAL before the full backup's
			// archive start (segment 3) when that backup ends.
			"plain: one full backup leaves WAL expire removes", "plain",
			Retention{Full: 1},
			evidence.Warning,
			[]wantFinding{{FindingRetentionNotApplied, SeverityWarning, "archive/demo/17-1/000000010000000000000001"}},
		},
		{"plain: time-based, window not reached", "plain", Retention{Full: 1, FullType: RetentionTime}, evidence.Healthy, nil},
		{"plain: one differential kept besides the full", "plain", Retention{Full: 2, Diff: 2}, evidence.Healthy, nil},
		{
			"plain: not configured", "plain",
			Retention{},
			evidence.Warning,
			[]wantFinding{{FindingRetentionNotConfigured, SeverityWarning, "backup/demo/"}},
		},
		{"encrypted: two full backups kept at most", "encrypted", Retention{Full: 2}, evidence.Healthy, nil},
		{"format6: unreadable info files are unknown", "format6", Retention{Full: 2}, evidence.Unknown, nil},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			report := analyzeFixture(t, loadFixture(t, fixtureRoot, test.variant), test.variant)
			report.ApplyRetention(test.want, nil)
			if report.Retention == nil || report.Retention.State != test.state {
				t.Fatalf("retention %+v, want %s", report.Retention, test.state)
			}
			if got := findingsOf(*report.Retention); !slices.Equal(got, test.findings) {
				t.Fatalf("findings %+v, want %+v", got, test.findings)
			}
			for _, finding := range report.Retention.Findings {
				if !slices.Contains(report.Findings, finding) {
					t.Fatalf("%+v is not in the report's findings", finding)
				}
			}
			aspects := report.Aspects()
			if aspects.Retention.State != test.state || aspects.Overall.State != Conservative(aspects.Overall.State, test.state) {
				t.Fatalf("aspects %+v", aspects)
			}
		})
	}
}

// TestRetentionAspectWithoutExpectation: no expectation is unknown and
// stays out of the roll-up.
func TestRetentionAspectWithoutExpectation(t *testing.T) {
	t.Parallel()
	report := analyzeFixture(t, loadFixture(t, fixtureRoot, "plain"), "plain")
	aspects := report.Aspects()
	if aspects.Retention.State != evidence.Unknown || aspects.Overall.State != evidence.Healthy {
		t.Fatalf("aspects %+v", aspects)
	}
	report.ApplyRetention(Retention{}, nil)
	if aspects := report.Aspects(); aspects.Overall.State != evidence.Warning {
		t.Fatalf("an unset retention does not reach the roll-up: %+v", aspects)
	}
	clone := CloneReport(report)
	clone.Retention.Findings[0].Message = "changed"
	if report.Retention.Findings[0].Message == "changed" {
		t.Fatal("CloneReport shares the retention findings")
	}
}
