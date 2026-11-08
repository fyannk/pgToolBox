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

// backupRules read the Backup objects' own reported phases. The
// operator records its backup refusals as events on the Backup objects,
// which are outside the observed event window — but the phase those
// refusals leave behind is on the object, and the object is observed.
func backupRules() []diagnose.Rule {
	return []diagnose.Rule{
		{
			ID:        "cnpg-backup-failed",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerBackups,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "a Backup object reporting the failed phase",
			Summary:   "A backup failed and will not be retried.",
			Detail: "A failed Backup object stays failed: only a new backup — scheduled or " +
				"manual — produces the next recovery point.",
			When: diagnose.BackupPhase{AnyOf: []string{"failed"}},
			// The Backup's own events carry the error the phase summarises.
			ConsequenceOf: []diagnose.Relation{
				{Cause: "cnpg-backup-error"}, {Cause: "cnpg-backup-manager-restarted"},
			},
		},
		{
			ID:        "cnpg-backup-stuck-pending",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerBackups,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "a Backup pending for over half an hour",
			Summary:   "A backup has been pending for over half an hour, which means the operator cannot start it.",
			Detail: "The operator parks a backup in pending and retries every 30 seconds " +
				"while its target pod is missing or not ready, or while the cluster is " +
				"not healthy enough to back up — including being hibernated. It stays " +
				"pending until the blocker clears.",
			When: diagnose.BackupPhase{AnyOf: []string{"pending"}, MinAge: 30 * time.Minute},
			ConsequenceOf: []diagnose.Relation{
				{Cause: "cnpg-backup-waiting-for-target"}, {Cause: "cnpg-backup-target-unhealthy"},
			},
		},
		{
			ID:        "cnpg-backup-wal-archiving",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerBackups,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityHigh,
			Describes: "a Backup blocked by failing WAL archiving",
			Summary:   "A backup is blocked because WAL archiving is not working.",
			Detail: "A base backup is only restorable together with its WAL, so the " +
				"operator refuses to take one while archiving fails. Fixing archiving " +
				"unblocks it.",
			When: diagnose.BackupPhase{AnyOf: []string{"walArchivingFailing"}},
		},
		{
			ID:        "cnpg-schedule-suspended",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerBackups,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "a suspended backup schedule",
			Summary:   "A backup schedule is suspended, so it produces no recovery points.",
			Detail: "Suspension is deliberate and easy to forget, like fencing. While " +
				"it holds, the recovery window this schedule was providing stops " +
				"growing.",
			When:   diagnose.ScheduledBackupSuspended{},
			Pinned: []string{`json:"suspend,omitempty"`},
		},
		{
			// The cadence detector catches a schedule firing too often;
			// this is the opposite failure. The operator refuses to
			// schedule while the cluster is not healthy, and it records
			// that refusal only on the ScheduledBackup object — whose
			// events are outside the observed window — so the stale
			// next-run field is what the console can honestly read.
			ID:        "cnpg-schedule-not-firing",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerBackups,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityHigh,
			Describes: "a backup schedule that has stopped firing",
			Summary:   "A backup schedule's next run is long past, so scheduling has stopped.",
			Detail: "The operator advances the next-run time whenever it schedules, and " +
				"it declines to schedule at all while the cluster is not healthy. A " +
				"next run more than half an hour in the past means backups from this " +
				"schedule are not being taken, whatever else looks fine.",
			When:   diagnose.ScheduledBackupOverdue{Grace: 30 * time.Minute},
			Pinned: []string{`json:"nextScheduleTime,omitempty"`},
		},
	}
}
