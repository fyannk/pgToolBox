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

package cnpgstatus

import (
	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
)

// What /pg/status reports about replication (pkg/postgres/status.go
// PostgresqlStatus, PgStatReplication, PgReplicationSlot; same fields at
// v1.30.0 and main):
//
//   - replicationInfo[] (primary only): application_name, state, sent_lsn
//     under the JSON key "receivedLsn" (status.go v1.30.0:112), write/
//     flush/replay LSN and lag, sync_state, sync_priority, from
//     pg_stat_replication WHERE application_name ~ '<cluster>-[0-9]+$'
//     AND usename = 'streaming_replica' (pkg/management/postgres/probes.go
//     v1.30.0:486, main:481).
//   - replicationSlotsInfo[] (primary only, PostgreSQL >= 13): EVERY row
//     of pg_replication_slots, unfiltered: slot_name, plugin, slot_type,
//     datoid, database, active, xmin, catalog_xmin, restart_lsn,
//     wal_status, safe_wal_size (probes.go v1.30.0:389-417, main:384-412;
//     status.go v1.30.0:167-179). Slots therefore show consumers that
//     replicationInfo hides (logical slots, external standbys).
//   - pgStatBasebackupsInfo[]: pg_stat_progress_basebackup rows whose
//     application_name ends in "-join" only (probes.go v1.30.0:306,
//     main:301): a joining instance, never an external pg_basebackup.

// Walsender states (PostgreSQL documentation, "pg_stat_replication",
// column state: startup, catchup, streaming, backup, stopping;
// https://www.postgresql.org/docs/current/monitoring-stats.html#MONITORING-PG-STAT-REPLICATION-VIEW,
// unchanged from PostgreSQL 13 to 18).
const (
	WalSenderStartup   = "startup"
	WalSenderCatchup   = "catchup"
	WalSenderStreaming = "streaming"
	WalSenderBackup    = "backup"
	WalSenderStopping  = "stopping"
)

// ReplicationValueReading classifies one PostgreSQL enum value.
type ReplicationValueReading struct {
	Class Class  `json:"class"`
	Known bool   `json:"known"`
	Text  string `json:"text,omitempty"`
}

var walSenderStates = map[string]ReplicationValueReading{
	WalSenderStreaming: {Class: ClassOK, Known: true, Text: "streaming changes after catching up"},
	WalSenderCatchup:   {Class: ClassProgressing, Known: true, Text: "catching up with the primary"},
	WalSenderStartup:   {Class: ClassProgressing, Known: true, Text: "WAL sender starting up"},
	// A <cluster>-N walsender in backup state is a base backup over the
	// replication protocol; transitional.
	WalSenderBackup:   {Class: ClassProgressing, Known: true, Text: "sending a base backup"},
	WalSenderStopping: {Class: ClassWarning, Known: true, Text: "WAL sender stopping"},
}

// ClassifyWalSenderState classifies replicationInfo[].state. Empty or
// unknown values are unknown, shown verbatim.
func ClassifyWalSenderState(state string) ReplicationValueReading {
	if reading, ok := walSenderStates[state]; ok {
		return reading
	}
	return ReplicationValueReading{Class: ClassUnknown, Text: bound(state, maxAnnotationValue)}
}

// Sync states (PostgreSQL documentation, "pg_stat_replication", column
// sync_state: async, potential, sync, quorum; same URL).
const (
	SyncStateAsync     = "async"
	SyncStatePotential = "potential"
	SyncStateSync      = "sync"
	SyncStateQuorum    = "quorum"
)

// SyncStateReading is replicationInfo[].syncState.
type SyncStateReading struct {
	ReplicationValueReading
	// Synchronous is true for sync and quorum: commits wait for this
	// standby (for quorum, for any num_sync of the quorum set).
	Synchronous bool `json:"synchronous"`
}

// ClassifySyncState classifies a sync_state. The value alone is not a
// health verdict (async is right for an async cluster): sync and quorum
// are ok, async and potential info; anything else unknown. Compare the
// count of synchronous standbys with SyncConfig.
func ClassifySyncState(state string) SyncStateReading {
	switch state {
	case SyncStateSync:
		return SyncStateReading{ReplicationValueReading{ClassOK, true, "synchronous standby"}, true}
	case SyncStateQuorum:
		return SyncStateReading{ReplicationValueReading{ClassOK, true, "quorum-based synchronous candidate"}, true}
	case SyncStatePotential:
		return SyncStateReading{ReplicationValueReading{ClassInfo, true, "asynchronous now, synchronous if a current synchronous standby fails"}, false}
	case SyncStateAsync:
		return SyncStateReading{ReplicationValueReading{ClassInfo, true, "asynchronous standby"}, false}
	default:
		return SyncStateReading{ReplicationValueReading{Class: ClassUnknown, Text: bound(state, maxAnnotationValue)}, false}
	}
}

// Slot wal_status values (PostgreSQL documentation, "pg_replication_slots",
// column wal_status, PostgreSQL 13+:
// https://www.postgresql.org/docs/current/view-pg-replication-slots.html).
const (
	SlotWALReserved   = "reserved"
	SlotWALExtended   = "extended"
	SlotWALUnreserved = "unreserved"
	SlotWALLost       = "lost"
)

var slotWALStatus = map[string]ReplicationValueReading{
	SlotWALReserved: {Class: ClassOK, Known: true, Text: "the slot's WAL is within max_wal_size"},
	SlotWALExtended: {Class: ClassWarning, Known: true, Text: "max_wal_size is exceeded; the WAL is kept by the slot or wal_keep_size"},
	// "will be removed at the next checkpoint": the slot is about to be lost.
	SlotWALUnreserved: {Class: ClassCritical, Known: true, Text: "the slot no longer reserves its WAL; it is lost at the next checkpoint unless it catches up"},
	SlotWALLost:       {Class: ClassCritical, Known: true, Text: "WAL the slot needs was removed; the slot is no longer usable"},
}

// ClassifySlotWALStatus classifies replicationSlotsInfo[].walStatus. The
// instance coalesces NULL to "" (probes.go v1.30.0:415): a slot that has
// never reserved WAL, or PostgreSQL < 13; both unknown.
func ClassifySlotWALStatus(status string) ReplicationValueReading {
	if reading, ok := slotWALStatus[status]; ok {
		return reading
	}
	return ReplicationValueReading{Class: ClassUnknown, Text: bound(status, maxAnnotationValue)}
}

// Synchronous replication modes.
const (
	SyncModeNone     = "none"
	SyncModeLegacy   = "legacy"
	SyncModeExplicit = "explicit"
)

// SyncReading is the synchronous-replication configuration (operator
// claim of intent; what PostgreSQL applies is not in /pg/status, see
// BlindSpotSyncStandbyNames).
type SyncReading struct {
	// Mode: explicit when spec.postgresql.synchronous is set (it wins,
	// pkg/postgres/replication/replication.go v1.30.0:31,
	// main:31); legacy when spec.maxSyncReplicas > 0 (always ANY,
	// legacy.go :43 at all three); none otherwise.
	Mode string `json:"mode"`
	// Method is "any" or "first" (explicit), "any" (legacy).
	Method string `json:"method,omitempty"`
	// Number is synchronous.number (explicit); for legacy, MinSync and
	// MaxSync bound the number the operator computes from ready replicas.
	Number  int `json:"number,omitempty"`
	MinSync int `json:"minSync,omitempty"`
	MaxSync int `json:"maxSync,omitempty"`
	// DataDurability is required (default, explicit.go v1.30.0:36-43) or
	// preferred; preferred lowers the number to the healthy standbys.
	DataDurability             string   `json:"dataDurability,omitempty"`
	MaxStandbyNamesFromCluster *int     `json:"maxStandbyNamesFromCluster,omitempty"`
	StandbyNamesPre            []string `json:"standbyNamesPre,omitempty"`
	StandbyNamesPost           []string `json:"standbyNamesPost,omitempty"`
	// FailoverQuorum is synchronous.failoverQuorum (or the
	// alpha.cnpg.io/failoverQuorum annotation the defaulter converts,
	// api/v1/cluster_defaults.go v1.30.0:337-356): a FailoverQuorum
	// object named after the Cluster then exists.
	FailoverQuorum bool `json:"failoverQuorum"`
	// FailureDomainKeysUnreadable is set on 1.31 (main): synchronous
	// podFailureDomainKeys / nodeFailureDomainKeys exist
	// (cluster_types.go main:1610, :1627) but the v1.30.0 API types do
	// not carry them; read the unstructured object.
	FailureDomainKeysUnreadable bool `json:"failureDomainKeysUnreadable,omitempty"`
}

// SyncConfig reads the synchronous-replication settings
// (api/v1/cluster_types.go minSyncReplicas/maxSyncReplicas :267/:275 at
// both tags; SynchronousReplicaConfiguration
// v1.30.0:1508-1555, main:1544-1628). A nil Cluster gives Mode none.
func SyncConfig(minor Minor, c *cnpgv1.Cluster) SyncReading {
	if c == nil {
		return SyncReading{Mode: SyncModeNone}
	}
	reading := SyncReading{
		MinSync: c.Spec.MinSyncReplicas,
		MaxSync: c.Spec.MaxSyncReplicas,
	}
	if s := c.Spec.PostgresConfiguration.Synchronous; s != nil {
		reading.Mode = SyncModeExplicit
		reading.Method = bound(string(s.Method), maxAnnotationValue)
		reading.Number = s.Number
		reading.DataDurability = bound(string(s.DataDurability), maxAnnotationValue)
		if reading.DataDurability == "" {
			reading.DataDurability = string(cnpgv1.DataDurabilityLevelRequired)
		}
		reading.MaxStandbyNamesFromCluster = s.MaxStandbyNamesFromCluster
		reading.StandbyNamesPre = boundAll(s.StandbyNamesPre)
		reading.StandbyNamesPost = boundAll(s.StandbyNamesPost)
		reading.FailoverQuorum = s.FailoverQuorum
		reading.FailureDomainKeysUnreadable = minor.atLeast(Minor131)
		return reading
	}
	if c.Spec.MaxSyncReplicas > 0 {
		reading.Mode = SyncModeLegacy
		reading.Method = "any"
		return reading
	}
	reading.Mode = SyncModeNone
	return reading
}

func boundAll(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	if len(values) > maxFencedInstances {
		values = values[:maxFencedInstances]
	}
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = bound(v, maxAnnotationValue)
	}
	return out
}

// FailoverQuorumReading is FailoverQuorum.status: the synchronous set
// PostgreSQL really applied, as the primary's instance manager last
// wrote it (internal/management/controller/instance_sync.go
// v1.30.0:54-77; the operator only creates the empty object,
// internal/controller/replicas_quorum.go v1.30.0:144-163).
type FailoverQuorumReading struct {
	Class        Class    `json:"class"`
	Method       string   `json:"method,omitempty"`
	Number       int      `json:"number,omitempty"`
	StandbyNames []string `json:"standbyNames,omitempty"`
	Primary      string   `json:"primary,omitempty"`
	Text         string   `json:"text,omitempty"`
}

// FailoverQuorumState reads a FailoverQuorum (api/v1/failoverquorum_types.go
// FailoverQuorumStatus :60-79, identical at v1.30.0 and main).
// An empty status is a warning: the instance resets it when the sync
// configuration changes, "de facto preventing any failover"
// (instance_controller.go v1.30.0:286-289). A status whose primary is
// not status.currentPrimary of the Cluster is a warning (a stale
// writer). A nil object is unknown (not created, or not readable).
func FailoverQuorumState(fq *cnpgv1.FailoverQuorum, currentPrimary string) FailoverQuorumReading {
	if fq == nil {
		return FailoverQuorumReading{Class: ClassUnknown, Text: "no FailoverQuorum object read"}
	}
	s := fq.Status
	reading := FailoverQuorumReading{
		Method:       bound(s.Method, maxAnnotationValue),
		Number:       s.StandbyNumber,
		StandbyNames: boundAll(s.StandbyNames),
		Primary:      bound(s.Primary, maxAnnotationValue),
	}
	switch {
	case s.Primary == "" && s.Method == "" && len(s.StandbyNames) == 0:
		reading.Class = ClassWarning
		reading.Text = "empty: failover is blocked until the primary reports the synchronous set it applied"
	case currentPrimary != "" && s.Primary != currentPrimary:
		reading.Class = ClassWarning
		reading.Text = "written by " + reading.Primary + ", not by the current primary"
	default:
		reading.Class = ClassOK
	}
	return reading
}
