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

package cnpg

import (
	"time"

	"github.com/fyannk/pgToolBox/pkg/diagnose"
)

// The metric thresholds, console-pinned knowledge stated in one place.
const (
	// archiveBacklogSegments is how many segments may wait to be
	// archived. Each is sixteen megabytes by default; thirty-two is half
	// a gigabyte of WAL the archiver has not taken.
	archiveBacklogSegments = 32
	// archiveBacklogHeld is how long the backlog must hold. A burst of
	// writes outruns the archiver for a moment; a quarter of an hour is
	// an archiver that is not catching up.
	archiveBacklogHeld = 15 * time.Minute
	// archiverFailureRate is the failure rate, per second, that counts
	// as continuous: one failure a minute, which is the archiver
	// retrying and failing every time.
	archiverFailureRate = 1.0 / 60
	// archiverFailingHeld is how long that rate must hold.
	archiverFailingHeld = 15 * time.Minute
)

// metricRules cover the exporter flags whose non-zero value is a
// standing operator state — states that are otherwise only visible in
// annotations the console does not read.
func metricRules() []diagnose.Rule {
	return []diagnose.Rule{
		{
			ID:        "cnpg-instance-fenced",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerPostgreSQL,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "an instance the operator has fenced",
			Summary:   "An instance is fenced: PostgreSQL is deliberately stopped there and the operator will not restart it.",
			Detail: "Fencing is intentional and meant to be temporary. A fenced replica is " +
				"one fewer member than the cluster's count suggests; a fenced primary " +
				"is an outage.",
			When:      diagnose.InstantNonZero{Key: "fencing-on"},
			Pinned:    []string{"fencing_on"},
			Link:      "/cluster/metrics",
			LinkLabel: "Metrics",
		},
		{
			ID:        "cnpg-switchover-required",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "an instance reporting that a manual switchover is required",
			Summary:   "An instance reports that a manual switchover is required before pending work resumes.",
			Detail: "The exporter's own word for the supervised-strategy wait, from inside " +
				"the instance — the operator-side view of the same state is the " +
				"waiting-for-user phase check.",
			When:      diagnose.InstantNonZero{Key: "switchover-required"},
			Pinned:    []string{"switchover_required"},
			Link:      "/cluster/metrics",
			LinkLabel: "Metrics",
		},
		{
			// The one rule that compares two sources rather than reading
			// one: the operator's currentPrimary against each instance's
			// own pg_is_in_recovery(), which the exporter publishes under
			// the pinned metric name. Neither side is presumed right — the
			// finding is the contradiction itself. A primary move in flight
			// is excluded, because then the operator is the one saying
			// roles are changing.
			ID:        "cnpg-primary-disagreement",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerReplication,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "an instance whose own recovery state contradicts the operator's current primary",
			Summary:   "An instance and the operator disagree about which instance is the primary.",
			Detail: "PostgreSQL's own answer to pg_is_in_recovery() on the instance does not " +
				"match the role the Cluster status assigns it, with no primary move in " +
				"flight to explain the lag. Two instances accepting writes is a split " +
				"brain; a named primary that is in recovery is a cluster with no " +
				"primary at all. Both quoted claims are current readings; the " +
				"disagreement, not either side, is the finding.",
			When:   diagnose.PrimaryDisagreement{},
			Pinned: []string{"in_recovery"},
			NextSteps: "Read the instance's log and the operator's log before touching " +
				"anything: a stale exporter reading clears on the next scrape, while a " +
				"real split brain needs the extra writer fenced. Do not promote or " +
				"demote by hand while the two sources still disagree.",
			Link:      "/cluster/metrics",
			LinkLabel: "Metrics",
		},
		{
			// The earliest signal of archiving trouble: segments the
			// archiver has not taken pile up as .ready files long before
			// the condition flips or the volume fills. The instance
			// manager publishes the count under the pinned collector.
			ID:        "cnpg-wal-archive-backlog",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerBackups,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityHigh,
			Describes: "WAL segments waiting to be archived, at least 32 of them, held for a quarter of an hour",
			Summary:   "WAL segments are piling up unarchived on an instance: at least 32 have waited a quarter of an hour.",
			Detail: "PostgreSQL marks each finished segment ready and the archiver " +
				"takes it; a backlog that holds is an archiver that is slower than " +
				"the WAL rate or not taking segments at all. The WAL volume fills at " +
				"the backlog's pace. The archiving condition and the archive-command " +
				"log check usually say why.",
			When:          diagnose.SeriesAbove{Key: "wal-archive-ready", Threshold: archiveBacklogSegments, For: archiveBacklogHeld},
			Pinned:        []string{"pg_wal_archive_status"},
			ConsequenceOf: []diagnose.Relation{{Cause: "cnpg-wal-archiving-failing"}, {Cause: "cnpg-wal-archive-command-failed"}},
			Link:          "/cluster/metrics",
			LinkLabel:     "Metrics",
		},
		{
			// PostgreSQL's own count of archive_command failures, read as
			// a rate: a rate above zero held for a quarter of an hour is an
			// archiver failing continuously, in the server's words rather
			// than the instance manager's.
			ID:        "cnpg-archiver-failing",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerBackups,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityHigh,
			Describes: "PostgreSQL counting archive failures continuously for a quarter of an hour",
			Summary:   "PostgreSQL has been counting archive-command failures continuously for a quarter of an hour.",
			Detail: "pg_stat_archiver counts every failed archive attempt. A failure rate " +
				"that never returns to zero across the window is an archiver failing " +
				"on every attempt, which the archiving condition reports from the " +
				"instance manager's side.",
			When:          diagnose.SeriesAbove{Key: "wal-archive-failed", Threshold: archiverFailureRate, For: archiverFailingHeld},
			Pinned:        []string{"pg_stat_archiver", "failed_count"},
			ConsequenceOf: []diagnose.Relation{{Cause: "cnpg-wal-archiving-failing"}},
			Link:          "/cluster/metrics",
			LinkLabel:     "Metrics",
		},
	}
}
