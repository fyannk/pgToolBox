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

// InstanceList is the /instances screen: one row per member pod, and one
// per instance the operator reports without a pod. An instance whose
// status could not be read is an error row, never omitted.
type InstanceList struct {
	Ref         ClusterRef `json:"ref"`
	GeneratedAt time.Time  `json:"generatedAt"`
	// Sweep is the state of the /pg/status sweep for this Cluster.
	Sweep      SourceState   `json:"sweep"`
	Items      []InstanceRow `json:"items"`
	BlindSpots []BlindSpot   `json:"blindSpots"`
}

// InstanceRole is the role of an instance.
type InstanceRole string

// The roles.
const (
	RolePrimary InstanceRole = "primary"
	RoleReplica InstanceRole = "replica"
	RoleUnknown InstanceRole = "unknown"
)

// StatusReadState is the outcome of reading one instance's /pg/status.
type StatusReadState string

// The read outcomes. Every one but ok is an error row with a reason.
const (
	// StatusReadOK is a verified HTTPS read that parsed.
	StatusReadOK StatusReadState = "ok"
	// StatusReadPending is not swept yet.
	StatusReadPending StatusReadState = "pending"
	// StatusReadUnreachable is a connection failure or timeout: "not
	// reachable from this console" (NetworkPolicy, pod down).
	StatusReadUnreachable StatusReadState = "unreachable"
	// StatusReadNotVerifiable means the Cluster's CA is not in the
	// console's admin-filled bundle: never read unverified.
	StatusReadNotVerifiable StatusReadState = "notVerifiable"
	// StatusReadNoTLS means the pod's command lacks --status-port-tls
	// (created before CNPG 1.24): never read over HTTP.
	StatusReadNoTLS StatusReadState = "noTLS"
	// StatusReadTLSError is another handshake failure (name mismatch,
	// expired certificate).
	StatusReadTLSError StatusReadState = "tlsError"
	// StatusReadBadResponse is a non-200 or an unparseable body.
	StatusReadBadResponse StatusReadState = "badResponse"
	// StatusReadNotRunning is a pod without an IP or being deleted.
	StatusReadNotRunning StatusReadState = "notRunning"
	// StatusReadDisabled means the sweep is switched off.
	StatusReadDisabled StatusReadState = "disabled"
)

// InstanceRow is one instance with its three views: the operator's, the
// Pod's and the instance's own.
type InstanceRow struct {
	Name   string   `json:"name" doc:"Instance (pod) name."`
	Health Health   `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	Role   Roles    `json:"role"`
	Pod    *PodView `json:"pod,omitempty" doc:"Absent for an instance the operator reports without a pod."`
	// StatusRead is the last /pg/status outcome with its reason.
	StatusRead StatusRead `json:"statusRead"`
	// Live is the last good /pg/status reading; it may be older than
	// StatusRead when the latest read failed (then Provenance.Stale).
	Live   *LiveStatus `json:"live,omitempty"`
	Fenced bool        `json:"fenced,omitempty" doc:"Named in cnpg.io/fencedInstances (or all fenced)."`
	// Series are the short in-memory rings (default one hour) for the
	// "since when" sparklines; absent when the ring is empty.
	Series []Series `json:"series,omitempty"`
}

// Roles is what each source says the instance is.
type Roles struct {
	// Operator is primary when status.currentPrimary names the instance.
	Operator InstanceRole `json:"operator" enum:"primary,replica,unknown"`
	// Label is the pod's cnpg.io/instanceRole label.
	Label InstanceRole `json:"label" enum:"primary,replica,unknown"`
	// Live is the instance's own isPrimary.
	Live InstanceRole `json:"live" enum:"primary,replica,unknown"`
}

// PodView is the Kubernetes side of an instance.
type PodView struct {
	Phase    string `json:"phase,omitempty" doc:"Pod phase, verbatim."`
	Ready    bool   `json:"ready"`
	Node     string `json:"node,omitempty"`
	Restarts int32  `json:"restarts" doc:"Sum of container restarts."`
	// Waiting is the worst container waiting reason, such as
	// CrashLoopBackOff or ImagePullBackOff.
	Waiting    string     `json:"waiting,omitempty"`
	Image      string     `json:"image,omitempty"`
	Deleting   bool       `json:"deleting,omitempty"`
	CreatedAt  *time.Time `json:"createdAt,omitempty"`
	Provenance Provenance `json:"provenance"`
}

// StatusRead is the outcome of the last /pg/status read of an instance.
type StatusRead struct {
	State StatusReadState `json:"state" enum:"ok,pending,unreachable,notVerifiable,noTLS,tlsError,badResponse,notRunning,disabled"`
	// Reason is the secret-free explanation shown on the error row.
	Reason string `json:"reason,omitempty"`
	// At is when the read was attempted.
	At *time.Time `json:"at,omitempty"`
	// Since is when the instance entered State (the "since when" clock).
	Since *time.Time `json:"since,omitempty"`
}

// LiveStatus is one instance's /pg/status self-report.
type LiveStatus struct {
	IsPrimary   bool   `json:"isPrimary"`
	TimelineID  *int   `json:"timelineId,omitempty"`
	SystemID    string `json:"systemId,omitempty"`
	CurrentLSN  string `json:"currentLsn,omitempty"`
	ReceivedLSN string `json:"receivedLsn,omitempty"`
	ReplayLSN   string `json:"replayLsn,omitempty"`
	// LagBytes is this replica's replay distance behind the primary's
	// current LSN as both were last read; absent on the primary or when
	// either side was not read.
	LagBytes           *int64     `json:"lagBytes,omitempty"`
	ReplayPaused       bool       `json:"replayPaused,omitempty"`
	WALReceiverActive  bool       `json:"walReceiverActive,omitempty"`
	PendingRestart     bool       `json:"pendingRestart,omitempty"`
	PendingRestartDecr bool       `json:"pendingRestartForDecrease,omitempty"`
	ArchivingWAL       bool       `json:"archivingWal,omitempty"`
	LastFailedWAL      string     `json:"lastFailedWal,omitempty"`
	ReadyWALFiles      int        `json:"readyWalFiles,omitempty"`
	MightBeUnavailable bool       `json:"mightBeUnavailable,omitempty"`
	PgRewindRunning    bool       `json:"pgRewindRunning,omitempty"`
	ManagerVersion     string     `json:"instanceManagerVersion,omitempty"`
	ManagerUpgrading   bool       `json:"instanceManagerUpgrading,omitempty"`
	Standbys           []Standby  `json:"standbys,omitempty" doc:"The primary's streaming_replica walsenders only."`
	Provenance         Provenance `json:"provenance"`
}

// Standby is one walsender row the primary reports. SentLSN is
// pg_stat_replication.sent_lsn, which CNPG serialises as "receivedLsn".
type Standby struct {
	Name      string `json:"name"`
	State     string `json:"state,omitempty"`
	SyncState string `json:"syncState,omitempty"`
	SentLSN   string `json:"sentLsn,omitempty"`
	ReplayLSN string `json:"replayLsn,omitempty"`
	ReplayLag string `json:"replayLag,omitempty"`
}

// Series is one short ring of samples for a sparkline.
type Series struct {
	// Key is a stable metric key, such as "lagBytes" or "readyWalFiles".
	Key  string `json:"key"`
	Unit string `json:"unit,omitempty" doc:"bytes, files, seconds, count."`
	// Points are oldest first; a nil value is a gap (the read failed).
	Points []Point `json:"points"`
}

// Point is one sample. A nil value is a gap, drawn as a gap.
type Point struct {
	At    time.Time `json:"at"`
	Value *float64  `json:"value"`
	// ReadBy is when the read behind the point returned (At is the
	// sweep's start, before it was sent); zero when unknown. Not served:
	// the recovery facts read it (a "by then" claim takes ReadBy, a
	// "not yet by then" one At).
	ReadBy time.Time `json:"-"`
}
