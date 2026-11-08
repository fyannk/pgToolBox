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

// logRules are the instance manager's own log messages: the process
// that runs inside every instance pod, whose stream the console
// follows. Each substring is copied verbatim from the operator source,
// and the pin attests the span of releases the copy was checked
// against. The operator's controller pod logs are not followed —
// anything only that process says has to reach the console as a phase,
// condition, or event instead.
//
// These rules earn their place the usual way: each names a failure that
// either appears in no object status at all, or appears there stripped
// of the error text the log line carries.
func logRules() []diagnose.Rule {
	return []diagnose.Rule{
		// ------------------------------------------------ WAL archiving
		{
			ID:        "cnpg-wal-archive-command-failed",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerBackups,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityHigh,
			Describes: "the archive_command wrapper failing",
			Summary:   "The WAL archive command is failing, so WAL is accumulating on the instance.",
			Detail:    streamCaveat,
			When: diagnose.LogFields{Fields: []diagnose.LogField{
				{Path: "msg", Equals: "failed to run wal-archive command"},
			}},
			ConsequenceOf: []diagnose.Relation{{Cause: "cnpg-wal-archiving-failing"}},
		},
		{
			ID:        "cnpg-wal-archive-plugin-missing",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerBackups,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityHigh,
			Describes: "a WAL-archive plugin whose socket is absent from the pod",
			Summary:   "The configured WAL-archive plugin is not available in this pod, so nothing is being archived.",
			Detail: "The plugin's sidecar socket is missing — the sidecar failed to start, " +
				"or the pod predates the plugin and needs a rollout. " + streamCaveat,
			When:      diagnose.LogContains{Substrings: []string{"wal archive plugin is not available:"}},
			NextSteps: "Roll the instance pods so they gain the plugin sidecar, and check the sidecar's own container state if it fails to start.",
		},
		// -------------------------------------------------- WAL restore
		{
			ID:        "cnpg-wal-restore-failed",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerReplication,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityHigh,
			Describes: "the restore_command wrapper failing for reasons other than a missing WAL",
			Summary:   "WAL restore from the archive is failing, so recovery on this instance cannot advance.",
			Detail: "A replica or a point-in-time recovery that cannot fetch WAL stays " +
				"where it is; asking for a WAL that simply does not exist yet is normal " +
				"and is not what this line reports. " + streamCaveat,
			When: diagnose.LogContains{Substrings: []string{"wal-restore command failed"}},
		},
		// "Refusing to restore future timeline history file" is
		// deliberately absent. It looks like the split-brain guard
		// speaking, but the same line fires on every replica's routine
		// probe for the next timeline's history file at recovery start —
		// verified live: a healthy cluster on timeline 1 logs it for
		// 00000002.history on each standby. A substring cannot tell the
		// routine probe from a divergent history, so the rule would be
		// wrong far more often than right.
		// ---------------------------------------------------- pg_rewind
		{
			ID:        "cnpg-pg-rewind-failed",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerPostgreSQL,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityHigh,
			Describes: "pg_rewind failing on a demoted primary",
			Summary:   "A former primary cannot rewind to rejoin the cluster.",
			Detail: "After a failover the old primary must rewind before following the new " +
				"one. While this fails, the cluster runs one instance short; the usual " +
				"way out is deleting the pod's data volume so it re-clones. " + streamCaveat,
			When: diagnose.LogContains{Substrings: []string{"Failed to execute pg_rewind"}},
		},
		{
			ID:        "cnpg-pg-control-lost",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerPostgreSQL,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "a zero-length pg_control with no surviving backup copy",
			Summary:   "An instance's pg_control file is empty and no backup copy of it survives: that data directory is unusable.",
			Detail: "An interrupted pg_rewind truncates pg_control and normally leaves " +
				"pg_control.old behind; this instance has neither. The data directory " +
				"cannot start PostgreSQL again. " + streamCaveat,
			When: diagnose.LogContains{Substrings: []string{"pg_control file is zero and we don't have a pg_control.old"}},
		},
		// ------------------------------------------------- replica join
		{
			ID:        "cnpg-join-failed",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityHigh,
			Describes: "a new replica failing to join the cluster",
			Summary:   "A new replica cannot join: its clone from the primary is failing.",
			Detail: "The quoted error carries the cause — connectivity to the primary, " +
				"authentication, exhausted max_wal_senders, or disk. The cluster stays " +
				"below its declared instance count until this succeeds. " + streamCaveat,
			When: diagnose.LogFields{Fields: []diagnose.LogField{
				{Path: "msg", Equals: "Error joining node"},
			}},
		},
		// ---------------------------------------------------- bootstrap
		{
			ID:        "cnpg-initdb-failed",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "initdb bootstrap failing",
			Summary:   "The initdb bootstrap failed, so the cluster never gets its primary.",
			Detail:    streamCaveat,
			When:      diagnose.LogContains{Substrings: []string{"Error while bootstrapping data directory"}},
		},
		{
			ID:        "cnpg-restore-failed",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "a recovery bootstrap failing",
			Summary:   "The restore this cluster is bootstrapping from failed.",
			Detail: "Emitted by both the object-store and the volume-snapshot recovery " +
				"paths; the quoted error names the cause. " + streamCaveat,
			When: diagnose.LogContains{Substrings: []string{"Error while restoring a backup"}},
		},
		{
			ID:        "cnpg-recovery-target-missing",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "a recovery target matching no backup in the catalog",
			Summary:   "No backup in the catalog matches the requested recovery target.",
			Detail: "The recovery job dies immediately and will keep dying: the target " +
				"time, label, or backup ID has to change, or the right object store " +
				"has to be pointed at. " + streamCaveat,
			When: diagnose.LogContains{Substrings: []string{"no target backup found"}},
		},
		// -------------------------------------------- lifecycle and disk
		{
			ID:        "cnpg-wal-disk-full",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerPostgreSQL,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "the instance manager refusing to start PostgreSQL for lack of WAL space",
			Summary:   "An instance has no free disk space for WAL, and PostgreSQL is being kept down there.",
			Detail: "The instance manager exits with a dedicated code so the operator can " +
				"react; the pod-level view of the same state is the operator's " +
				"not-enough-disk-space phase. " + streamCaveat,
			When: diagnose.LogContains{Substrings: []string{"no free disk space for WALs"}},
			NextSteps: "Grow the WAL volume (the storage class must allow expansion), or fix " +
				"the archiving failure that filled it — archived WAL is recycled on its own.",
			// Two ways the volume fills: WAL that cannot leave because
			// archiving is broken, and WAL that cannot be removed
			// because a slot still pins it. Both are facts about the
			// cluster's storage, whichever instance logged the refusal.
			ConsequenceOf: []diagnose.Relation{
				{Cause: "cnpg-wal-archiving-failing"}, {Cause: "cnpg-slot-retaining-wal"},
			},
		},
		{
			ID:        "cnpg-postgres-start-failed",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerPostgreSQL,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "the postmaster failing to launch",
			Summary:   "PostgreSQL failed to start on an instance.",
			Detail:    streamCaveat,
			When: diagnose.LogFields{Fields: []diagnose.LogField{
				{Path: "msg", Equals: "Unable to start PostgreSQL up"},
			}},
		},
		{
			ID:        "cnpg-postgres-exited",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerPostgreSQL,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "the postmaster exiting with errors",
			Summary:   "PostgreSQL exited with errors — this is what a crash-looping instance looks like from inside.",
			Detail: "The instance manager terminates with it, so the kubelet restarts the " +
				"pod and the restart count climbs. " + streamCaveat,
			When: diagnose.LogFields{Fields: []diagnose.LogField{
				{Path: "msg", Equals: "PostgreSQL process exited with errors"},
			}},
			// Both causes are read from the same instance's log, so the
			// relation is pinned to the pod: a panic on one instance does not
			// explain an exit on another. The panic is placed within the hour
			// because both lines carry a time; the disk-full line is renewed
			// on every retry and needs no window.
			ConsequenceOf: []diagnose.Relation{
				{Cause: "postgres-panic", Scope: diagnose.ScopePod, Within: time.Hour},
				{Cause: "cnpg-wal-disk-full", Scope: diagnose.ScopePod},
			},
		},
		// ----------------------------------------------- primary lease
		{
			// The primary lease is 1.30 machinery; these lines do not
			// exist in earlier releases.
			ID:        "cnpg-lease-preempted",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerReplication,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "a primary shut down because another instance took its lease",
			Summary:   "A primary shut itself down because another instance now holds the primary lease.",
			Detail: "This is why a primary stops without warning during lease-based " +
				"failover: the preempted side steps down on its own. " + streamCaveat,
			When: diagnose.LogContains{Substrings: []string{"Primary lease preempted, shutting down"}},
		},
		{
			ID:        "cnpg-lease-not-acquired",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerReplication,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityHigh,
			Describes: "a promotion stalled waiting for the primary lease",
			Summary:   "The instance being promoted cannot acquire the primary lease, so the cluster has no writable primary yet.",
			Detail:    streamCaveat,
			When:      diagnose.LogContains{Substrings: []string{"Primary lease not yet acquired, retrying"}},
		},
		// ------------------------------------------------- replication
		{
			ID:        "cnpg-replica-not-streaming",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerReplication,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityHigh,
			Describes: "the streaming readiness probe finding no replication connection",
			Summary:   "A replica is not connected via streaming replication, and the readiness probe is holding it out of service.",
			Detail: "Reported only when the readiness probe is configured for the " +
				"streaming strategy. The replica serves nothing until it reconnects, " +
				"and its data ages from here. " + streamCaveat,
			When: diagnose.LogContains{Substrings: []string{"replica not connected via streaming replication"}},
		},
		{
			ID:        "cnpg-replica-lagging",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerReplication,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "a replica exceeding the configured maximum lag",
			Summary:   "A replica exceeds the configured maximum lag and has been taken out of read traffic.",
			Detail:    streamCaveat,
			When:      diagnose.LogContains{Substrings: []string{"streaming replica lagging; detectedLag="}},
		},
		{
			ID:        "cnpg-slot-sync-failing",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerReplication,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "replication slot synchronization failing on a replica",
			Summary:   "Replication slot synchronization is failing, so a failover may not be able to resume replication cleanly.",
			Detail: "Slot drift is silent until the failover that needs the slots. The " +
				"quoted error says which side — primary or local — is failing. " + streamCaveat,
			When: diagnose.LogContains{Substrings: []string{"synchronizing replication slots"}},
		},
		// -------------------------------------------------- isolation
		{
			ID:        "cnpg-liveness-isolation",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "an instance concluding it is network-isolated",
			Summary:   "An instance cannot reach the API server or its peers and is letting the liveness probe kill it.",
			Detail: "The isolation check failed on every front, so the instance manager " +
				"fails its own liveness probe on purpose: better a restart than a " +
				"split brain. " + streamCaveat,
			When: diagnose.LogContains{Substrings: []string{"liveness probe failing"}},
		},
		// ------------------------------------------------------ backup
		{
			ID:        "cnpg-backup-plugin-missing",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerBackups,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityHigh,
			Describes: "a backup targeting a plugin absent from the pod",
			Summary:   "A backup targets a plugin that is not available in the instance pod, so backups quietly never succeed.",
			Detail: "As with the WAL-archive twin of this rule, the pod needs the plugin " +
				"sidecar — usually a rollout after enabling the plugin. " + streamCaveat,
			When: diagnose.LogContains{Substrings: []string{"requested plugin is not available:"}},
		},
		{
			ID:        "cnpg-backup-stop-blocked",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerBackups,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "pg_backup_stop failing at the end of a physical backup",
			Summary:   "A backup cannot finish: stopping the physical backup failed, most often because it is waiting on WAL archiving.",
			Detail: "pg_backup_stop waits for the backup's WAL to be archived, so a hung " +
				"backup here usually points back at the archiving checks above. " + streamCaveat,
			When: diagnose.LogContains{Substrings: []string{"while stopping PostgreSQL physical backup"}},
		},
	}
}
