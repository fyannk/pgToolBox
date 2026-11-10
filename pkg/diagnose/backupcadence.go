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
	stdbits "math/bits"
	"time"

	"github.com/robfig/cron"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

// cadenceAlarm is the runs-per-day above which a base-backup schedule is
// reported. Six a day is already aggressive for a full backup and still
// plausibly deliberate; the mistake this detector exists for produces at
// least twenty-four.
const cadenceAlarm = 6

// observedWindow is how far back the catalog is counted. A day, because
// that is the period the implied rate is expressed in and the shortest
// window that averages out a single missed or retried run.
const observedWindow = 24 * time.Hour

// backupCadenceDetector reports a ScheduledBackup whose schedule runs far
// more often than its author can plausibly have meant.
//
// The mistake it catches is the five-field cron habit. CloudNativePG
// validates and runs a schedule with github.com/robfig/cron v1
// cron.Parse (Second | Minute | Hour | Dom | Month | DowOptional; the
// ScheduledBackup webhook and controller, verified on 1.29 and 1.30), so
// seconds come first and the day of week is optional. Someone intending
// "daily at 02:00" and writing the crontab they know — 0 2 * * * — gets
// second 0, minute 2, every hour, every day: valid, accepted by the
// operator's validation, and hourly. Writing 0 2 * * * * (adding the
// seconds field at the wrong end) lands on the same schedule.
//
// Nothing then looks wrong. The cluster is healthy, backups are
// succeeding, and a terabyte-scale base backup is running twenty-four
// times a day, burning storage, egress and IO continuously until someone
// reads the bill. That is why this finding is the one exception to the
// screen's neutral register: everything else here describes something
// already visibly broken, and this describes something working exactly as
// written while doing damage.
//
// It implements Detector.
type backupCadenceDetector struct{}

func (backupCadenceDetector) Name() string { return "backup-cadence" }

func (backupCadenceDetector) Layer() Layer { return LayerBackups }

func (backupCadenceDetector) Describes() string {
	return "a backup schedule that runs far more often than its author is likely to have intended"
}

func (d backupCadenceDetector) Detect(in Input) ([]Finding, string) {
	if !in.HasBackups {
		return nil, "the backup catalog has not been observed yet"
	}
	if in.Backups.Stale {
		// A stale catalog still carries the schedules, but the observed
		// count it would be compared against is not current, and half of
		// this finding is that comparison.
		return nil, "the backup catalog is stale, so the observed rate cannot be compared"
	}

	var findings []Finding
	for _, schedule := range in.Backups.ScheduledBackups {
		if schedule.Suspended != nil && *schedule.Suspended {
			continue
		}
		perDay, everyDay, ok := runsPerDay(schedule.Schedule)
		if !ok || perDay < cadenceAlarm {
			continue
		}
		findings = append(findings, d.finding(in, schedule, perDay, everyDay))
	}
	return findings, ""
}

// finding states the implied rate, the observed rate, and the schedule
// verbatim. It says what both numbers are and lets the reader conclude —
// the escalation is in the severity and in naming the cost, not in
// inventing a claim about intent.
func (d backupCadenceDetector) finding(in Input, schedule observe.ScheduledBackupFacts, perDay int, everyDay bool) Finding {
	observed := countRecentBackups(in.Backups, schedule.Name, in.Now)
	rate := fmt.Sprintf("%d times a day", perDay)
	if !everyDay {
		rate += " on the days it runs"
	}
	finding := Finding{
		ID:       "backup-cadence/" + schedule.Name,
		Subject:  EntityRef{Kind: "ScheduledBackup", Name: schedule.Name},
		Severity: SeverityHigh,
		Summary: fmt.Sprintf(
			"ScheduledBackup %s is set to run %s.", schedule.Name, rate),
		Detail: "A CloudNativePG schedule starts with seconds and makes the day of " +
			"week optional, so a five-field crontab written from habit (0 2 * * * " +
			"meant as 02:00 daily) is read as second, minute, hour, day of month, " +
			"month and usually lands on an hourly or faster schedule. Nothing " +
			"reports this as unhealthy: the " +
			"backups succeed, and a full base backup simply runs at that rate, " +
			"consuming storage, egress and IO until someone notices the cost.",
		Evidence: []Evidence{{
			Origin: "operator-reported",
			Object: "ScheduledBackup/" + schedule.Name,
			Detail: fmt.Sprintf("schedule: %q", schedule.Schedule),
		}},
		Link:      "/backups",
		LinkLabel: "Backups",
	}
	if observed >= 0 {
		finding.Evidence = append(finding.Evidence, Evidence{
			Origin: "Kubernetes-observed",
			Object: "Backup catalog",
			Detail: fmt.Sprintf("%d backups created by this schedule in the last 24 hours", observed),
		})
	}
	return finding
}

// countRecentBackups counts the catalog entries the schedule created
// inside the window — attributed by the operator's own label, so two
// schedules of one method, or on-demand backups, do not inflate each
// other's rate. It returns -1 when the catalog is truncated, because a
// bounded list cannot support a count: reporting a floor as though it
// were the rate would be exactly the kind of quiet inaccuracy this screen
// exists to avoid.
func countRecentBackups(catalog observe.BackupsSnapshot, schedule string, now time.Time) int {
	if catalog.BackupsTruncated {
		return -1
	}
	count := 0
	cutoff := now.Add(-observedWindow)
	for _, backup := range catalog.Backups {
		if backup.ScheduledBackup != schedule {
			continue
		}
		if backup.CreatedAt.After(cutoff) {
			count++
		}
	}
	return count
}

// cronModule is the cron library, at the exact version, that
// CloudNativePG validates and runs ScheduledBackup schedules with: its
// go.mod requires it on every verified release (1.30.0, 1.30.1) and
// internal/controller/scheduledbackup_controller.go and
// internal/webhook/v1/scheduledbackup_webhook.go call its cron.Parse.
// The console imports the same version, so a schedule reads here exactly
// as the operator reads it; make verify-pins fails when a CloudNativePG
// release moves to another one.
const cronModule = "github.com/robfig/cron v1.2.0"

// CronModule is the cron library requirement the cadence detector shares
// with the operator, exported for the catalog's pin verification.
func CronModule() string { return cronModule }

// day is the period a rate is expressed in.
const day = 24 * time.Hour

// cronFirstSecond is the starting point used to ask the schedule whether
// it fires at all. Any instant works; a fixed one keeps the answer
// deterministic.
var cronFirstSecond = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// runsPerDay reports how many times a CloudNativePG schedule fires on a
// day it runs, whether it runs every day, and whether it could be read at
// all.
//
// It parses with the operator's own parser (robfig/cron v1 cron.Parse:
// seconds first, day of week optional, descriptors such as @hourly and
// @every 30m), so a five-field crontab is read exactly as the operator
// reads it. The count is exact rather than estimated:
//
//   - a field schedule fires at every combination of its selected
//     seconds, minutes and hours on each day its day-of-month, month and
//     day-of-week fields select, so a run day carries the product of the
//     three counts. Restricting the days does not make the hours above
//     them less frequent: 0 2 * * 1-5, the weekday crontab, is read as
//     every hour in January to May, and those days run twenty-four
//     backups each;
//   - an @every schedule fires once per delay, so a day holds as many
//     whole delays as fit in it (@every 30m: 48).
//
// Anything it cannot read (a parse error, a schedule that never fires
// such as 0 0 0 30 2 *, an unknown schedule type) returns ok false: a
// wrong "not ok" costs a finding nobody sees; a wrong rate would put an
// alarm on a correct schedule, which would teach operators to ignore the
// screen.
func runsPerDay(expr string) (perDay int, everyDay, ok bool) {
	// The string is parsed verbatim, as the operator parses it.
	parsed, err := cron.Parse(expr)
	if err != nil {
		return 0, false, false
	}
	switch schedule := parsed.(type) {
	case *cron.SpecSchedule:
		if schedule.Next(cronFirstSecond).IsZero() {
			// No date satisfies it (the 30th of February): it never runs.
			return 0, false, false
		}
		perDay = bitCount(schedule.Second, 0, 59) *
			bitCount(schedule.Minute, 0, 59) *
			bitCount(schedule.Hour, 0, 23)
		return perDay, firesEveryDay(schedule), true
	case cron.ConstantDelaySchedule:
		if schedule.Delay <= 0 {
			return 0, false, false
		}
		return int(day / schedule.Delay), true, true
	default:
		return 0, false, false
	}
}

// firesEveryDay reports whether the day fields select every calendar
// day, using robfig/cron v1's own day rule: when either the day of month
// or the day of week was written as a wildcard both must match, otherwise
// either one is enough.
func firesEveryDay(schedule *cron.SpecSchedule) bool {
	if !allSet(schedule.Month, 1, 12) {
		return false
	}
	domAll := allSet(schedule.Dom, 1, 31)
	dowAll := allSet(schedule.Dow, 0, 6)
	if isWildcard(schedule.Dom) || isWildcard(schedule.Dow) {
		return domAll && dowAll
	}
	return domAll || dowAll
}

// wildcardBit is the bit robfig/cron v1 sets on a field written as * or
// ?; it is not a value and is masked out of every count.
const wildcardBit = uint64(1) << 63

func isWildcard(bits uint64) bool { return bits&wildcardBit != 0 }

// rangeMask returns the bits of the values low..high.
func rangeMask(low, high uint) uint64 {
	return (^uint64(0) >> (63 - high)) &^ ((uint64(1) << low) - 1)
}

// bitCount counts the values a field selects within its bounds.
func bitCount(bits uint64, low, high uint) int {
	return stdbits.OnesCount64(bits & rangeMask(low, high))
}

// allSet reports whether a field selects every value within its bounds.
func allSet(bits uint64, low, high uint) bool {
	mask := rangeMask(low, high)
	return bits&mask == mask
}
