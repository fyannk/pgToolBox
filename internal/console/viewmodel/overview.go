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

// ClusterOverview is the /c/<ns>/<cluster> screen: the roll-up verdict
// with every contributor, the primary election, instance cards, "why the
// operator waits", conditions, recent warnings, and the sources and
// blind spots that bound what the page can claim.
type ClusterOverview struct {
	Ref         ClusterRef        `json:"ref"`
	Labels      map[string]string `json:"labels,omitempty" untrusted:"true"`
	GeneratedAt time.Time         `json:"generatedAt" doc:"When the read model assembled this view."`
	// Present is false when the Cluster was deleted while the view was
	// open; every other field is then the last observation.
	Present   bool            `json:"present" doc:"The Cluster exists."`
	Verdict   Verdict         `json:"verdict"`
	Phase     Phase           `json:"phase"`
	Primary   PrimaryElection `json:"primary"`
	Instances InstanceCounts  `json:"instances"`
	// Cards are one per member pod or reported instance, primary first.
	Cards      []InstanceCard `json:"cards"`
	Conditions []Condition    `json:"conditions"`
	// Waiting explains why the operator is not progressing, when it is
	// not: the phaseReason and the Warning events that name a cause.
	Waiting        *OperatorWaiting `json:"waiting,omitempty"`
	RecentWarnings []EventRow       `json:"recentWarnings" doc:"Warning events of the last hour, newest first."`
	EscapeHatches  []EscapeHatch    `json:"escapeHatches" doc:"Annotations that switch operator behaviour off; each explains why nothing happens."`
	Fencing        Fencing          `json:"fencing"`
	// Failover is the failover rehearsal, also on the replication screen.
	Failover    FailoverRehearsal `json:"failover"`
	Hibernation Hibernation       `json:"hibernation"`
	Backups     BackupSummary     `json:"backups"`
	Version     VersionInfo       `json:"version"`
	Findings    FindingCounts     `json:"findings"`
	Sources     []SourceState     `json:"sources" doc:"Every source this view rests on and its state."`
	BlindSpots  []BlindSpot       `json:"blindSpots"`
}

// PrimaryElection is "who is primary, right now": the
// operator's claim, each instance's own claim and the Lease, side by
// side, because they disagree exactly when it matters.
type PrimaryElection struct {
	PrimarySummary
	// FailingSince is currentPrimaryFailingSinceTimestamp.
	FailingSince *time.Time `json:"failingSince,omitempty" doc:"When the operator saw the current primary fail."`
	// LivePrimaries are the instances whose /pg/status says isPrimary.
	// Two entries means an old primary not yet demoted.
	LivePrimaries []string `json:"livePrimaries,omitempty" doc:"Instances that report isPrimary themselves."`
	// LabelPrimaries are the pods labelled cnpg.io/instanceRole=primary.
	LabelPrimaries []string `json:"labelPrimaries,omitempty" doc:"Pods labelled as primary."`
	// Lease is the primary-election Lease (CNPG 1.30+); absent before.
	Lease      *Lease     `json:"lease,omitempty"`
	Provenance Provenance `json:"provenance"`
}

// Lease is the primary-election Lease of CNPG 1.30+.
type Lease struct {
	Holder     string     `json:"holder,omitempty"`
	RenewedAt  *time.Time `json:"renewedAt,omitempty"`
	Provenance Provenance `json:"provenance"`
}

// InstanceCard is the compact per-instance card of the overview.
type InstanceCard struct {
	Name   string       `json:"name"`
	Role   InstanceRole `json:"role" enum:"primary,replica,unknown"`
	Health Health       `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	// Ready is the Pod's Ready condition; absent when the pod is not
	// observed (an instance the operator reports without a pod).
	Ready *bool `json:"ready,omitempty"`
	// StatusRead is the outcome of the last /pg/status read.
	StatusRead StatusReadState `json:"statusRead" enum:"ok,pending,unreachable,notVerifiable,noTLS,tlsError,badResponse,notRunning,disabled"`
	Fenced     bool            `json:"fenced,omitempty"`
	// Detail is one line, such as "lag 12 MB" or "CrashLoopBackOff".
	Detail string `json:"detail,omitempty"`
}

// Condition is one Cluster condition. Unknown types are rendered
// generically, never hidden.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status" doc:"True, False or Unknown, verbatim."`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty" doc:"Bounded operator text, rendered escaped."`
	// LastTransition is the operator's clock.
	LastTransition *time.Time `json:"lastTransition,omitempty"`
	// Health is the console's reading of this condition for the observed
	// minor; info for a type it does not know.
	Health Health `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	// Known is false for a condition type the table of the observed
	// minor does not list.
	Known bool `json:"known"`
}

// OperatorWaiting is "why is the operator waiting?".
type OperatorWaiting struct {
	PhaseReason string     `json:"phaseReason,omitempty" untrusted:"true"`
	Causes      []EventRow `json:"causes" doc:"Warning events whose reasons name a cause (QuotaExceeded, AdmissionDenied, ...)."`
	// FilteredCacheTrap is set when the Cluster is Unrecoverable for want
	// of PVCs while some of its Pods carry the fork's cnpg.io/reconcile
	// label and others do not: a filtered-cache operator cannot see the
	// objects it has not labelled yet.
	FilteredCacheTrap bool `json:"filteredCacheTrap,omitempty"`
	// Reasons explain the wait, in the order the operator acts on them:
	// causes the operator reported (heuristic false) and causes the
	// console derived (heuristic true), each with its evidence.
	Reasons []WaitReason `json:"reasons,omitempty"`
}

// WaitReason is one explanation of why the operator waits.
type WaitReason struct {
	ID        string   `json:"id" doc:"Stable code, such as pod-unschedulable or supervised-switchover."`
	Text      string   `json:"text"`
	Heuristic bool     `json:"heuristic" doc:"True when the console derived the cause; false when the operator reported it."`
	Evidence  []string `json:"evidence" doc:"The facts the reason rests on, each naming its source; bounded operator and Kubernetes text, rendered escaped."`
}

// EscapeHatch is an annotation that switches operator behaviour off.
type EscapeHatch struct {
	Annotation string `json:"annotation" doc:"Annotation key, such as cnpg.io/reconciliationLoop."`
	Value      string `json:"value" doc:"Annotation value, bounded."`
	// Effect is the English consequence, such as "the operator does not
	// reconcile this Cluster".
	Effect string `json:"effect"`
	Health Health `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
}

// FencingState is the fencing intent of the Cluster annotation.
type FencingState string

// The fencing states.
const (
	FencingNone       FencingState = "none"
	FencingSome       FencingState = "some"
	FencingAll        FencingState = "all"
	FencingUnreadable FencingState = "unreadable"
)

// Fencing is cnpg.io/fencedInstances: the source of truth for fencing.
// /pg/status never carries isFenced on a direct read.
type Fencing struct {
	State     FencingState `json:"state" enum:"none,some,all,unreadable"`
	Instances []string     `json:"instances,omitempty"`
}

// HibernationState is the hibernation reading.
type HibernationState string

// The hibernation states.
const (
	HibernationOff        HibernationState = "off"
	HibernationInProgress HibernationState = "inProgress"
	HibernationOn         HibernationState = "hibernated"
	HibernationUnreadable HibernationState = "unreadable"
)

// Hibernation combines the cnpg.io/hibernation annotation (intent) and
// the cnpg.io/hibernation condition (outcome).
type Hibernation struct {
	State HibernationState `json:"state" enum:"off,inProgress,hibernated,unreadable"`
	// Reason is the condition reason, such as WaitingPodsDeletion.
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// BackupSummary is the backup panel of the overview.
type BackupSummary struct {
	BackupRecency
	// Archiving is the WAL archiving fact: the ContinuousArchiving
	// condition plus the primary's live isArchivingWAL.
	Archiving Archiving `json:"archiving"`
	// RecoveryWindow is ObjectStore.status.serverRecoveryWindow for
	// plugin backups, or the in-tree first/last recoverability point.
	RecoveryWindow *RecoveryWindow `json:"recoveryWindow,omitempty"`
	// NextScheduledAt is the soonest ScheduledBackup.status.nextScheduleTime.
	NextScheduledAt *time.Time `json:"nextScheduledAt,omitempty"`
	// NotReported lists facts the backup method does not report, each a
	// stable key such as "firstRecoverabilityPoint", so the UI says "not
	// reported by plugin-based backups" rather than "no backup".
	NotReported []string `json:"notReported,omitempty"`
}

// Archiving is the WAL archiving reading.
type Archiving struct {
	Health Health `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	// ConditionStatus is ContinuousArchiving's status, verbatim.
	ConditionStatus string `json:"conditionStatus,omitempty"`
	Reason          string `json:"reason,omitempty"`
	Message         string `json:"message,omitempty"`
	// LiveArchiving is the primary's isArchivingWAL; absent when the
	// primary's /pg/status was not read. An idle primary with one old
	// failure reads false.
	LiveArchiving *bool  `json:"liveArchiving,omitempty"`
	LastFailedWAL string `json:"lastFailedWal,omitempty"`
	// LastArchivedWAL and LastArchivedAt are the archiving instance's
	// pg_stat_archiver last_archived_wal and last_archived_time, its own
	// report as of Provenance.ObservedAt; absent when not read or when it
	// never archived. The time is when the segment was archived, so WAL
	// written after it may still be only on the instance.
	LastArchivedWAL string     `json:"lastArchivedWal,omitempty"`
	LastArchivedAt  *time.Time `json:"lastArchivedAt,omitempty" doc:"pg_stat_archiver.last_archived_time on the archiving instance, its own report."`
	// LastFailedAt is pg_stat_archiver.last_failed_time, the same way.
	LastFailedAt *time.Time `json:"lastFailedAt,omitempty" doc:"pg_stat_archiver.last_failed_time on the archiving instance, its own report."`
	// ReadyWALFiles is the archive backlog on the primary.
	ReadyWALFiles *int       `json:"readyWalFiles,omitempty"`
	Provenance    Provenance `json:"provenance"`
}

// RecoveryWindow is the span a restore can target.
type RecoveryWindow struct {
	First      *time.Time `json:"first,omitempty"`
	Last       *time.Time `json:"last,omitempty"`
	Provenance Provenance `json:"provenance"`
}
