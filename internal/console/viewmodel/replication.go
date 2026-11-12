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

package viewmodel

import "time"

// Replication is the /replication screen: the primary
// election, the synchronous configuration and what PostgreSQL really
// applied (FailoverQuorum), the primary's walsenders, each replica's own
// receiver, and the replication slots. Every row is an instance
// self-report or an operator claim with its provenance; the walsender
// list is structurally partial (BlindSpots says how).
type Replication struct {
	Ref         ClusterRef      `json:"ref"`
	GeneratedAt time.Time       `json:"generatedAt"`
	Primary     PrimaryElection `json:"primary"`
	Synchronous SyncConfig      `json:"synchronous"`
	// Quorum is FailoverQuorum.status, the synchronous set the primary's
	// instance manager says PostgreSQL applied; absent when the kind is
	// not served or the object does not exist.
	Quorum *QuorumView `json:"quorum,omitempty" doc:"FailoverQuorum.status: the synchronous set PostgreSQL really applied; absent when not reported."`
	// Standbys are the primary's streaming_replica walsenders, in the
	// order /pg/status reports them.
	Standbys []StandbyRow `json:"standbys" doc:"The primary's walsenders for this Cluster's own instances only."`
	// Receivers are the replicas' own view of their WAL receiver.
	Receivers []ReceiverRow `json:"receivers" doc:"One row per replica instance, from its own /pg/status."`
	// Failover is the failover rehearsal: "if the primary died now".
	Failover FailoverRehearsal `json:"failover"`
	// Slots are the primary's replication slots, unfiltered.
	Slots []SlotRow `json:"slots"`
	// PrimaryUnread: the primary's manager answered without reaching
	// PostgreSQL, so Standbys and Slots are empty because they were not
	// read, never because there are none.
	PrimaryUnread *PostgresUnread `json:"primaryUnread,omitempty" doc:"Set when the primary's instance manager answered without reaching PostgreSQL: standbys and slots are then not read, never empty."`

	Sources    []SourceState `json:"sources"`
	BlindSpots []BlindSpot   `json:"blindSpots"`
}

// SyncConfig is the synchronous-replication configuration the Cluster
// declares (operator claim of intent, not proof of what PostgreSQL
// applied).
type SyncConfig struct {
	// Mode is none, legacy (spec.minSyncReplicas/maxSyncReplicas) or
	// explicit (spec.postgresql.synchronous).
	Mode             string   `json:"mode" enum:"none,legacy,explicit" doc:"none, legacy (min/maxSyncReplicas) or explicit (spec.postgresql.synchronous)."`
	Method           string   `json:"method,omitempty" doc:"any (quorum) or first (priority), verbatim."`
	Number           *int     `json:"number,omitempty"`
	MinSync          *int     `json:"minSync,omitempty"`
	MaxSync          *int     `json:"maxSync,omitempty"`
	DataDurability   string   `json:"dataDurability,omitempty" doc:"required or preferred, verbatim."`
	StandbyNamesPre  []string `json:"standbyNamesPre,omitempty"`
	StandbyNamesPost []string `json:"standbyNamesPost,omitempty"`
	FailoverQuorum   bool     `json:"failoverQuorum,omitempty" doc:"The quorum-based failover protection is requested."`
	// Unreadable lists fields this console's API types cannot read on
	// the observed CNPG minor (shown as unknown, never as unset).
	Unreadable []string   `json:"unreadable,omitempty"`
	Provenance Provenance `json:"provenance"`
}

// QuorumView is FailoverQuorum.status.
type QuorumView struct {
	Health       Health     `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	Method       string     `json:"method,omitempty"`
	Number       *int       `json:"number,omitempty"`
	StandbyNames []string   `json:"standbyNames,omitempty"`
	Primary      string     `json:"primary,omitempty"`
	Text         string     `json:"text,omitempty" doc:"What the reading means, in words."`
	Provenance   Provenance `json:"provenance"`
}

// StandbyRow is one walsender of the primary (pg_stat_replication as
// /pg/status reports it). Values are verbatim; absent means not reported.
type StandbyRow struct {
	Name         string `json:"name" doc:"application_name." untrusted:"true"`
	State        string `json:"state,omitempty" doc:"pg_stat_replication.state, verbatim."`
	SyncState    string `json:"syncState,omitempty" doc:"async, potential, sync or quorum, verbatim."`
	SyncPriority string `json:"syncPriority,omitempty"`
	Synchronous  bool   `json:"synchronous,omitempty" doc:"The sync state makes commits wait for this standby."`
	// SentLSN is what /pg/status calls receivedLsn: it is sent_lsn.
	SentLSN   string `json:"sentLsn,omitempty"`
	WriteLSN  string `json:"writeLsn,omitempty"`
	FlushLSN  string `json:"flushLsn,omitempty"`
	ReplayLSN string `json:"replayLsn,omitempty"`
	WriteLag  string `json:"writeLag,omitempty" doc:"Interval as PostgreSQL renders it."`
	FlushLag  string `json:"flushLag,omitempty"`
	ReplayLag string `json:"replayLag,omitempty"`
	// LagBytes is the primary's current LSN minus the replay LSN.
	LagBytes   *int64     `json:"lagBytes,omitempty"`
	Health     Health     `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info" doc:"From the walsender state only; lag is shown, not judged."`
	Provenance Provenance `json:"provenance"`
}

// ReceiverRow is one replica's own account of its WAL receiver.
type ReceiverRow struct {
	Name        string     `json:"name"`
	Health      Health     `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	StatusRead  StatusRead `json:"statusRead"`
	ReceivedLSN string     `json:"receivedLsn,omitempty"`
	ReplayLSN   string     `json:"replayLsn,omitempty"`
	// LagBytes is the primary's current LSN minus this replica's replay
	// LSN, when both were read in the same sweep.
	LagBytes          *int64 `json:"lagBytes,omitempty"`
	ReplayPaused      bool   `json:"replayPaused,omitempty"`
	WALReceiverActive *bool  `json:"walReceiverActive,omitempty"`
	TimelineID        *int   `json:"timelineId,omitempty"`
	// PostgresUnread: the replica's manager answered without reaching
	// PostgreSQL; its receiver, positions and timeline are not read.
	PostgresUnread *PostgresUnread `json:"postgresUnread,omitempty" doc:"Set when the replica's instance manager answered without reaching PostgreSQL: its receiver, positions and timeline are not read."`
	Provenance     Provenance      `json:"provenance"`
}

// SlotRow is one replication slot of the primary.
type SlotRow struct {
	Instance   string     `json:"instance" doc:"The instance that reported the slot."`
	Name       string     `json:"name" untrusted:"true"`
	Type       string     `json:"type,omitempty" doc:"physical or logical."`
	Database   string     `json:"database,omitempty" untrusted:"true"`
	Plugin     string     `json:"plugin,omitempty" untrusted:"true"`
	WALStatus  string     `json:"walStatus,omitempty" doc:"reserved, extended, unreserved or lost, verbatim."`
	Active     bool       `json:"active"`
	Health     Health     `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	Provenance Provenance `json:"provenance"`
}
