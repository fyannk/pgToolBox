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
	"strings"
	"time"

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
// takes a six-field expression, seconds first, so someone intending
// "daily at 02:00" and writing the crontab they know — 0 2 * * * — is
// one field short. Add the field they did not know about and the
// expression becomes 0 2 * * * *, which is valid, passes the operator's
// validation, and means every hour at two minutes past.
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
		perDay, ok := runsPerDay(schedule.Schedule)
		if !ok || perDay < cadenceAlarm {
			continue
		}
		findings = append(findings, d.finding(in, schedule, perDay))
	}
	return findings, ""
}

// finding states the implied rate, the observed rate, and the schedule
// verbatim. It says what both numbers are and lets the reader conclude —
// the escalation is in the severity and in naming the cost, not in
// inventing a claim about intent.
func (d backupCadenceDetector) finding(in Input, schedule observe.ScheduledBackupFacts, perDay int) Finding {
	observed := countRecentBackups(in.Backups, schedule.Name, in.Now)
	finding := Finding{
		ID:       "backup-cadence/" + schedule.Name,
		Subject:  EntityRef{Kind: "ScheduledBackup", Name: schedule.Name},
		Severity: SeverityHigh,
		Summary: fmt.Sprintf(
			"ScheduledBackup %s is set to run %d times a day.", schedule.Name, perDay),
		Detail: "A six-field CloudNativePG cron starts with seconds, so a five-field " +
			"expression written from habit shifts every field and usually lands on " +
			"an hourly or faster schedule. Nothing reports this as unhealthy: the " +
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

// runsPerDay reports how many times a CloudNativePG cron expression fires
// in a day, and whether it could be read at all.
//
// This is deliberately not a cron library. It answers one question —
// roughly how often — for the shapes that actually appear in a schedule
// field, and returns false for anything it does not fully understand
// rather than guessing. A wrong "not ok" costs a finding nobody sees; a
// wrong rate would put a critical alarm on a correct schedule, which
// would teach operators to ignore the screen.
func runsPerDay(expr string) (int, bool) {
	fields := strings.Fields(expr)
	if len(fields) != 6 {
		// Five fields is the mistake this detector is named for, but an
		// expression that short is refused by the operator's validation
		// and so cannot be running. Anything else is unrecognised.
		return 0, false
	}
	seconds, secondsOK := fieldRate(fields[0], 60)
	minutes, minutesOK := fieldRate(fields[1], 60)
	hours, hoursOK := fieldRate(fields[2], 24)
	if !secondsOK || !minutesOK || !hoursOK {
		return 0, false
	}
	// Day-of-month, month and day-of-week only ever reduce the rate, so
	// treating them as "every" keeps this an upper bound. A schedule
	// restricted to Sundays is not reported as running daily, because a
	// restricted field makes the fields above it no more frequent.
	if !isEvery(fields[3]) || !isEvery(fields[4]) || !isEvery(fields[5]) {
		return 0, false
	}
	return seconds * minutes * hours, true
}

// fieldRate reports how many values one field selects out of its range.
// It understands the shapes CloudNativePG schedules actually use: a
// single value, a wildcard, and a wildcard step.
func fieldRate(field string, span int) (int, bool) {
	switch {
	case field == "*":
		return span, true
	case strings.HasPrefix(field, "*/"):
		step := 0
		if _, err := fmt.Sscanf(field, "*/%d", &step); err != nil || step <= 0 || step > span {
			return 0, false
		}
		return (span + step - 1) / step, true
	case !strings.ContainsAny(field, ",-/"):
		value := 0
		if _, err := fmt.Sscanf(field, "%d", &value); err != nil {
			return 0, false
		}
		return 1, true
	default:
		// Lists and ranges are legitimate and rare here; counting them
		// correctly is more surface than the question warrants, so the
		// detector declines rather than approximates.
		return 0, false
	}
}

// isEvery reports the unrestricted wildcard.
func isEvery(field string) bool { return field == "*" }
