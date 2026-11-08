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

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

var now = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

func input(schedules []observe.ScheduledBackupFacts, backups []observe.BackupFacts) Input {
	return Input{
		Now:        now,
		HasBackups: true,
		Backups: observe.BackupsSnapshot{
			ScheduledBackups: schedules,
			Backups:          backups,
		},
	}
}

func schedule(name, expr string) observe.ScheduledBackupFacts {
	return observe.ScheduledBackupFacts{Name: name, Schedule: expr, Method: "barmanObjectStore"}
}

// TestRunsPerDayReadsTheSixFieldCron pins the arithmetic the whole
// finding rests on, including the case it exists for: a five-field
// expression written from habit gains a field and becomes hourly.
func TestRunsPerDayReadsTheSixFieldCron(t *testing.T) {
	t.Parallel()
	cases := []struct {
		expr string
		want int
		ok   bool
	}{
		{"0 0 2 * * *", 1, true},              // daily at 02:00, written correctly
		{"0 2 * * * *", 24, true},             // the mistake: every hour at 02 past
		{"0 0 */6 * * *", 4, true},            // every six hours
		{"*/30 * * * * *", 60 * 2 * 24, true}, // every thirty seconds
		{"0 2 * * *", 0, false},               // five fields: the operator refuses it
		{"0 0 2 * * 0", 0, false},             // restricted weekday: declines
		{"0 0 1,13 * * *", 0, false},          // list: declines rather than guesses
		{"", 0, false},
		{"garbage", 0, false},
	}
	for _, tc := range cases {
		got, ok := runsPerDay(tc.expr)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("runsPerDay(%q) = %d, %v; want %d, %v", tc.expr, got, ok, tc.want, tc.ok)
		}
	}
}

// TestCadenceReportsTheShiftedSchedule proves the finding fires on the
// mistake, carries the schedule verbatim, and states both rates so the
// reader can see the comparison rather than being told a conclusion.
func TestCadenceReportsTheShiftedSchedule(t *testing.T) {
	t.Parallel()
	backups := make([]observe.BackupFacts, 0, 23)
	for i := range 23 {
		backups = append(backups, observe.BackupFacts{
			Name:            "orders-backup",
			Method:          "barmanObjectStore",
			ScheduledBackup: "orders-nightly",
			CreatedAt:       now.Add(-time.Duration(i) * time.Hour),
		})
	}
	result := Run(input([]observe.ScheduledBackupFacts{schedule("orders-nightly", "0 2 * * * *")}, backups))

	if len(result.Findings) != 1 {
		t.Fatalf("findings = %d, want 1: %+v", len(result.Findings), result.Findings)
	}
	finding := result.Findings[0]
	if finding.Severity != SeverityHigh {
		t.Errorf("severity = %v, want high", finding.Severity)
	}
	if !strings.Contains(finding.Summary, "24 times a day") {
		t.Errorf("summary does not state the implied rate: %q", finding.Summary)
	}
	// The schedule must appear exactly as written: the reader has to be
	// able to spot the missing field themselves.
	if len(finding.Evidence) != 2 {
		t.Fatalf("evidence = %+v, want the schedule and the observed count", finding.Evidence)
	}
	if !strings.Contains(finding.Evidence[0].Detail, `"0 2 * * * *"`) {
		t.Errorf("schedule not quoted verbatim: %q", finding.Evidence[0].Detail)
	}
	if finding.Evidence[0].Origin != "operator-reported" {
		t.Errorf("schedule evidence origin = %q", finding.Evidence[0].Origin)
	}
	if !strings.Contains(finding.Evidence[1].Detail, "23 backups") {
		t.Errorf("observed count missing: %q", finding.Evidence[1].Detail)
	}
	if finding.Evidence[1].Origin != "Kubernetes-observed" {
		t.Errorf("count evidence origin = %q", finding.Evidence[1].Origin)
	}
	// It proposes; it never remediates.
	if finding.Link != "/backups" {
		t.Errorf("link = %q, want a screen", finding.Link)
	}
}

// TestCadenceCountsEachScheduleOwnBackups is the regression test of the
// pooled count: two hourly schedules of one method (and a manual backup)
// each report the backups they created, not the method's total.
func TestCadenceCountsEachScheduleOwnBackups(t *testing.T) {
	t.Parallel()
	backups := make([]observe.BackupFacts, 0, 50)
	for i := range 24 {
		at := now.Add(-time.Duration(i) * time.Hour)
		backups = append(backups,
			observe.BackupFacts{Name: "a", Method: "barmanObjectStore", ScheduledBackup: "orders-a", CreatedAt: at},
			observe.BackupFacts{Name: "b", Method: "barmanObjectStore", ScheduledBackup: "orders-b", CreatedAt: at.Add(-time.Minute)})
	}
	backups = append(backups,
		observe.BackupFacts{Name: "manual", Method: "barmanObjectStore", CreatedAt: now.Add(-time.Hour)},
		observe.BackupFacts{Name: "old", Method: "barmanObjectStore", ScheduledBackup: "orders-a", CreatedAt: now.Add(-25 * time.Hour)})
	result := Run(input([]observe.ScheduledBackupFacts{schedule("orders-a", "0 2 * * * *"), schedule("orders-b", "0 3 * * * *")}, backups))
	if len(result.Findings) != 2 {
		t.Fatalf("findings = %d, want 2: %+v", len(result.Findings), result.Findings)
	}
	for _, finding := range result.Findings {
		if len(finding.Evidence) != 2 || !strings.HasPrefix(finding.Evidence[1].Detail, "24 backups") {
			t.Errorf("%s: evidence %+v, want 24 backups of its own", finding.ID, finding.Evidence)
		}
	}
}

// TestCadenceStatesNoCountFromATruncatedCatalog: a bounded catalog only
// gives a floor, so the observed count is left out rather than understated.
func TestCadenceStatesNoCountFromATruncatedCatalog(t *testing.T) {
	t.Parallel()
	backups := []observe.BackupFacts{{Name: "a", Method: "barmanObjectStore", ScheduledBackup: "orders-nightly", CreatedAt: now.Add(-time.Hour)}}
	in := input([]observe.ScheduledBackupFacts{schedule("orders-nightly", "0 2 * * * *")}, backups)
	in.Backups.BackupsTruncated = true
	result := Run(in)
	if len(result.Findings) != 1 {
		t.Fatalf("findings = %d, want 1: the schedule alone is the finding", len(result.Findings))
	}
	if ev := result.Findings[0].Evidence; len(ev) != 1 {
		t.Fatalf("evidence = %+v, want only the schedule: a truncated catalog supports no count", ev)
	}
}

// TestCadenceIgnoresACorrectSchedule proves the detector is quiet on the
// thing it must never flag. A daily backup written correctly produces no
// finding, and the check reports clear rather than nothing at all.
func TestCadenceIgnoresACorrectSchedule(t *testing.T) {
	t.Parallel()
	result := Run(input([]observe.ScheduledBackupFacts{schedule("orders-nightly", "0 0 2 * * *")}, nil))
	if len(result.Findings) != 0 {
		t.Fatalf("a correct daily schedule was flagged: %+v", result.Findings)
	}
	if got := outcomeOf(t, result, "backup-cadence"); got != CheckClear {
		t.Fatalf("backup-cadence outcome = %v, want clear", got)
	}
}

// TestCadenceIgnoresASuspendedSchedule proves a suspended schedule is not
// reported: it is not running, so it is not costing anything.
func TestCadenceIgnoresASuspendedSchedule(t *testing.T) {
	t.Parallel()
	suspended := true
	s := schedule("orders-nightly", "0 2 * * * *")
	s.Suspended = &suspended
	result := Run(input([]observe.ScheduledBackupFacts{s}, nil))
	if len(result.Findings) != 0 {
		t.Fatalf("a suspended schedule was flagged: %+v", result.Findings)
	}
}

// TestUnobservedInputIsNotAClearResult is the honesty property of the
// whole screen: with nothing observed, the detector reports that it could
// not run. Reporting "clear" would turn missing data into a statement
// that the cluster is fine.
func TestUnobservedInputIsNotAClearResult(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]Input{
		"never observed": {Now: now, HasBackups: false},
		"stale": {Now: now, HasBackups: true, Backups: observe.BackupsSnapshot{
			Stale:            true,
			ScheduledBackups: []observe.ScheduledBackupFacts{schedule("orders-nightly", "0 2 * * * *")},
		}},
	} {
		result := Run(in)
		if len(result.Findings) != 0 {
			t.Errorf("%s: reported findings from unusable input: %+v", name, result.Findings)
		}
		// Every detector here reads an input the case withholds, so all
		// of them must report that they could not run — with a reason.
		for _, check := range result.Checks {
			if check.Outcome != CheckUnavailable {
				t.Errorf("%s: %s = %v, want could-not-run", name, check.Name, check.Outcome)
			}
			if check.Because == "" {
				t.Errorf("%s: %s gave no reason", name, check.Name)
			}
		}
	}
}

// TestRunAlwaysAccountsForEveryDetector proves the screen can never show
// an empty result without also showing what was looked at. Every
// registered detector appears in Checks on every run, whatever it found.
func TestRunAlwaysAccountsForEveryDetector(t *testing.T) {
	t.Parallel()
	for _, in := range []Input{
		{Now: now},
		input(nil, nil),
		input([]observe.ScheduledBackupFacts{schedule("s", "0 2 * * * *")}, nil),
	} {
		// One synthetic rule rides along, proving every passed rule is
		// accounted for beside the detectors.
		rule := Rule{
			ID: "synthetic", Summary: "Synthetic.", Describes: "a synthetic condition",
			When: EventMatch{Reasons: []string{"Never"}},
		}
		result := Run(in, rule)
		if want := len(Detectors()) + 1; len(result.Checks) != want {
			t.Errorf("checks = %d, want one per detector and rule (%d)", len(result.Checks), want)
		}
		for _, check := range result.Checks {
			if check.Name == "" || check.Describes == "" {
				t.Errorf("check is unnamed or undescribed: %+v", check)
			}
		}
	}
}

// TestFindingsOrderMostSevereFirst proves the ordering is stable, so a
// refresh that changes nothing does not reshuffle the screen.
func TestFindingsOrderMostSevereFirst(t *testing.T) {
	t.Parallel()
	result := Run(input([]observe.ScheduledBackupFacts{
		schedule("zulu", "0 2 * * * *"),
		schedule("alpha", "0 2 * * * *"),
	}, nil))
	if len(result.Findings) != 2 {
		t.Fatalf("findings = %d, want 2", len(result.Findings))
	}
	if result.Findings[0].ID >= result.Findings[1].ID {
		t.Errorf("equal severities not ordered by ID: %q then %q",
			result.Findings[0].ID, result.Findings[1].ID)
	}
}

// outcomeOf finds one named check in a result, failing if it is absent —
// every registered detector must account for itself on every run.
func outcomeOf(t *testing.T, result Result, name string) CheckOutcome {
	t.Helper()
	for _, check := range result.Checks {
		if check.Name == name {
			return check.Outcome
		}
	}
	t.Fatalf("check %q did not account for itself: %+v", name, result.Checks)
	return CheckClear
}
