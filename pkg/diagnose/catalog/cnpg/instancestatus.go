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

import "github.com/fyannk/pgToolBox/pkg/diagnose"

// readyWALFiles is how many segments may wait to be archived by the
// instance's own count before it is a finding — the same number the
// metrics rule applies to the exporter's reading of the same files.
const readyWALFiles = 32

// instanceStatusRules read each instance manager's own status report,
// the one the operator reads before deciding and kubectl cnpg status
// prints. The strings pinned are the JSON tags the instance manager
// writes the fields under.
func instanceStatusRules() []diagnose.Rule {
	return []diagnose.Rule{
		{
			ID:        "cnpg-pending-restart",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerPostgreSQL,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "an instance reporting a configuration change waiting for a restart",
			Summary:   "An instance has a configuration change that takes effect only after a restart, and has not been restarted.",
			Detail: "PostgreSQL reports a parameter it loaded but cannot apply without a " +
				"restart, and the instance manager passes that on. The operator " +
				"restarts instances for it on its own unless the update strategy is " +
				"supervised, or the change is a decrease of a hot-standby parameter " +
				"the replicas must take first.",
			When:      diagnose.InstanceFlagSet{Flag: diagnose.FlagPendingRestart},
			Pinned:    []string{`json:"pendingRestart"`, `json:"pendingRestartForDecrease"`},
			Link:      "/cluster/pods",
			LinkLabel: "Pods",
		},
		{
			ID:        "cnpg-replay-paused",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerReplication,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "a replica reporting its WAL replay paused",
			Summary:   "A replica's WAL replay is paused: it receives WAL and applies none of it.",
			Detail: "Replay is paused by pg_wal_replay_pause() or a recovery target with " +
				"a pause action. The replica serves reads from the point it paused, " +
				"and every commit since then waits on its disk. Nothing resumes it " +
				"but pg_wal_replay_resume().",
			When:      diagnose.InstanceFlagSet{Flag: diagnose.FlagReplayPaused, ReplicaOnly: true},
			Pinned:    []string{`json:"replayPaused"`},
			Link:      "/cluster/pods",
			LinkLabel: "Pods",
		},
		{
			ID:        "cnpg-instance-might-be-unavailable",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerPostgreSQL,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "an instance manager doubting its own PostgreSQL is reachable",
			Summary:   "An instance manager reports that its PostgreSQL may be unavailable, and quotes the error it saw.",
			Detail: "The instance manager could not complete its own status query and " +
				"says so rather than reporting stale numbers as current. The masked " +
				"error is its own account; the pod may still read ready.",
			When:      diagnose.InstanceFlagSet{Flag: diagnose.FlagMightBeUnavailable},
			Pinned:    []string{`json:"mightBeUnavailable"`, `json:"mightBeUnavailableMaskedError,omitempty"`},
			Link:      "/cluster/pods",
			LinkLabel: "Pods",
		},
		{
			ID:        "cnpg-instance-archive-failing",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerBackups,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityHigh,
			Describes: "an instance whose last archive failure is more recent than its last success",
			Summary:   "An instance's last WAL archive attempt failed, more recently than its last success.",
			Detail: "pg_stat_archiver's own last failed and last archived instants, as the " +
				"instance manager reports them — the line kubectl cnpg status prints " +
				"as Last Failed WAL. The archiving condition is the operator's " +
				"summary of the same failure.",
			When:          diagnose.InstanceArchiveFailing{},
			Pinned:        []string{`json:"lastFailedWALTime,omitempty"`, `json:"lastArchivedWALTime,omitempty"`},
			ConsequenceOf: []diagnose.Relation{{Cause: "cnpg-wal-archiving-failing"}},
			Link:          "/backups",
			LinkLabel:     "Backups",
		},
		{
			ID:        "cnpg-wal-files-waiting",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerBackups,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityHigh,
			Describes: "an instance counting at least 32 WAL segments waiting to be archived",
			Summary:   "WAL segments are piling up unarchived on an instance, by its own count.",
			Detail: "The instance manager counts the .ready files in the archive status " +
				"directory on every report. It is the same backlog the metrics rule " +
				"reads from the exporter, from the other source, so one of the two " +
				"is enough for the finding.",
			When:          diagnose.InstanceReadyWAL{Threshold: readyWALFiles},
			Pinned:        []string{`json:"readyWalFiles,omitempty"`},
			ConsequenceOf: []diagnose.Relation{{Cause: "cnpg-wal-archiving-failing"}, {Cause: "cnpg-instance-archive-failing"}},
			Link:          "/backups",
			LinkLabel:     "Backups",
		},
		{
			ID:        "cnpg-replication-slot-inactive",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerReplication,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "an inactive replication slot the operator does not manage",
			Summary:   "A replication slot the operator does not manage is inactive: its consumer is gone, and the slot keeps WAL for it.",
			Detail: "A slot holds WAL until its consumer reads it. One left inactive — a " +
				"logical decoding client that stopped, a standby that was removed by " +
				"hand — holds WAL indefinitely, which is how a volume fills with " +
				"nothing wrong on the primary. The operator's own slots carry the " +
				"_cnpg_ prefix and are left out.",
			When:      diagnose.InstanceSlotInactive{},
			Pinned:    []string{`json:"replicationSlotsInfo,omitempty"`, "_cnpg_"},
			NextSteps: "Reconnect the consumer, or drop the slot with pg_drop_replication_slot() if it is not coming back.",
			Link:      "/cluster/pods",
			LinkLabel: "Pods",
		},
		{
			ID:        "cnpg-manager-version-drift",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityNote,
			Describes: "instances reporting more than one instance-manager version",
			Summary:   "The instances do not all run the same instance manager: a rollout or in-place upgrade did not reach every one.",
			Detail: "Each instance manager reports its own version. After an operator " +
				"upgrade every instance is updated, in place or by restart; a mix " +
				"that lasts is an update that stopped part-way.",
			When:      diagnose.InstanceManagerDrift{},
			Pinned:    []string{`json:"instanceManagerVersion"`},
			Link:      "/cluster/pods",
			LinkLabel: "Pods",
		},
	}
}
