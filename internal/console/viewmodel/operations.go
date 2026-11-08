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

// OperationKind is one of the enumerated cluster operations. The set is
// closed.
type OperationKind string

// The operations.
const (
	OperationBackup  OperationKind = "backup"
	OperationReload  OperationKind = "reload"
	OperationRestart OperationKind = "restart"
	OperationPromote OperationKind = "promote"
	// OperationRestoreDrill restores the latest backup into a throwaway
	// Cluster, checks it and deletes it.
	OperationRestoreDrill OperationKind = "restoreDrill"
)

// OperationPhase is a tracker step. notObservable is a
// settled-without-proof state: the operator reports no outcome for the
// operation at all (reload), which is not a failure.
type OperationPhase string

// The phases.
const (
	OpPhaseRequested     OperationPhase = "requested"
	OpPhaseAccepted      OperationPhase = "accepted"
	OpPhaseActing        OperationPhase = "acting"
	OpPhaseSettled       OperationPhase = "settled"
	OpPhaseFailed        OperationPhase = "failed"
	OpPhaseTimedOut      OperationPhase = "timedOut"
	OpPhaseNotObservable OperationPhase = "notObservable"
)

// Operations is the /operations screen of one Cluster: what the caller
// may do here, why something cannot be done now, and the recent
// operations this console replica issued.
type Operations struct {
	Ref         ClusterRef       `json:"ref"`
	GeneratedAt time.Time        `json:"generatedAt"`
	Offers      []OperationOffer `json:"offers"`
	// Recent are the operations issued through this console replica,
	// newest first. It is not the cluster's audit trail (the API
	// server's audit log is): another replica or kubectl is not listed.
	Recent      []Operation `json:"recent"`
	RecentScope string      `json:"recentScope" doc:"What Recent covers, in words."`
	// Revision is the Cluster as these offers read it. It is not served:
	// an MCP confirmation seals it, and the operation is refused when the
	// Cluster it writes to is no longer the one the person confirmed on.
	Revision ClusterRevision `json:"-"`
}

// ClusterRevision is what a confirmed operation expects of its Cluster.
type ClusterRevision struct {
	UID            string `json:"u,omitempty"`
	Generation     int64  `json:"g,omitempty"`
	Phase          string `json:"p,omitempty"`
	CurrentPrimary string `json:"c,omitempty"`
}

// OperationOffer is one operation as offered to the caller.
type OperationOffer struct {
	Kind    OperationKind `json:"kind" enum:"backup,reload,restart,promote,restoreDrill"`
	Title   string        `json:"title"`
	Summary string        `json:"summary"`
	// Consequence and ExpectedDuration fill the confirmation dialog.
	Consequence      string `json:"consequence"`
	ExpectedDuration string `json:"expectedDuration"`
	// Allowed is the API server's answer (SelfSubjectAccessReview) for the
	// verbs in Requires; Undecided when it could not be asked.
	Allowed   bool     `json:"allowed"`
	Undecided bool     `json:"undecided,omitempty"`
	Requires  []string `json:"requires" doc:"The verbs the operation needs, such as patch clusters/status."`
	// Blockers are preconditions not met now, from the read model; the
	// server re-checks them against a live read before writing.
	Blockers []OperationBlocker `json:"blockers"`
	// Confirmation is what the caller must type: the Cluster name, or
	// for promote the chosen instance name.
	Confirmation string `json:"confirmation"`
	// Observable is false when the operator reports no outcome for the
	// operation (reload): the tracker ends at notObservable.
	Observable bool `json:"observable"`
	// Targets is the replica roster for promote, with eligibility.
	Targets []PromoteTarget `json:"targets,omitempty"`
	// Drill is what a restore drill would do (restoreDrill only).
	Drill *DrillOffer `json:"drill,omitempty"`
}

// DrillOffer details a restore-drill offer for the confirmation dialog.
type DrillOffer struct {
	// Namespace is where the drill Cluster would be created.
	Namespace string `json:"namespace"`
	// Backup is the newest completed Backup, when there is one.
	Backup string `json:"backup,omitempty"`
	// BackupLabel is the pgBackRest backup the drill restores: the
	// newest of the evidence agent's recovery window.
	BackupLabel     string     `json:"backupLabel,omitempty" untrusted:"true"`
	BackupStoppedAt *time.Time `json:"backupStoppedAt,omitempty"`
	// RecoverySource says what the drill restores through, such as
	// "ObjectStore orders-store (barman-cloud plugin)".
	RecoverySource string `json:"recoverySource,omitempty"`
	// TimeLimitSeconds is when the console deletes it at the latest.
	TimeLimitSeconds int64 `json:"timeLimitSeconds"`
}

// OperationBlocker is one unmet precondition.
type OperationBlocker struct {
	Reason string `json:"reason" doc:"Stable code, such as ClusterIsHibernated or SwitchoverInProgress."`
	Text   string `json:"text"`
	Source string `json:"source" doc:"The field the precondition was read from."`
}

// PromoteTarget is one instance offered as a promote target.
type PromoteTarget struct {
	Name     string             `json:"name"`
	Eligible bool               `json:"eligible"`
	Blockers []OperationBlocker `json:"blockers,omitempty"`
	Health   Health             `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	// LagBytes is the instance's replay lag behind the primary, when read.
	LagBytes *int64 `json:"lagBytes,omitempty"`
}

// OperationRequest is the body of POST .../operations.
type OperationRequest struct {
	Kind OperationKind `json:"kind" enum:"backup,reload,restart,promote,restoreDrill"`
	// Target is the instance to promote; absent for the other kinds.
	Target string `json:"target,omitempty" maxLength:"253" doc:"The instance to promote (promote only)."`
	// Confirmation must equal the offer's confirmation text: the Cluster
	// name, or the target instance for promote.
	Confirmation string `json:"confirmation" maxLength:"253" doc:"The typed confirmation: the Cluster name, or the target instance for promote."`
}

// OperationAccepted is the 202 body.
type OperationAccepted struct {
	OperationID string        `json:"operationId" doc:"Opaque handle for GET /api/v1/operations/{id}; any console replica resolves it."`
	Kind        OperationKind `json:"kind" enum:"backup,reload,restart,promote,restoreDrill"`
	Target      ClusterRef    `json:"target"`
	Instance    string        `json:"instance,omitempty" doc:"The promoted instance."`
	AcceptedAt  time.Time     `json:"acceptedAt"`
	// ObservedGeneration is metadata.generation of the object the
	// write returned (an annotation patch does not bump it).
	ObservedGeneration int64 `json:"observedGeneration"`
	// Created is the Backup or drill Cluster the request created.
	Created string `json:"created,omitempty"`
	// CreatedNamespace is the drill Cluster's namespace (restoreDrill).
	CreatedNamespace string `json:"createdNamespace,omitempty"`
}

// Operation is one tracked operation.
type Operation struct {
	OperationID string        `json:"operationId"`
	Kind        OperationKind `json:"kind" enum:"backup,reload,restart,promote,restoreDrill"`
	Target      ClusterRef    `json:"target"`
	Instance    string        `json:"instance,omitempty"`
	Created     string        `json:"created,omitempty"`
	// CreatedNamespace is the drill Cluster's namespace (restoreDrill).
	CreatedNamespace string         `json:"createdNamespace,omitempty"`
	Phase            OperationPhase `json:"phase" enum:"requested,accepted,acting,settled,failed,timedOut,notObservable"`
	// Health is the console's reading of Phase and Blocked
	// (OperationHealth), so no client maps phases to colours itself. The
	// console always fills it; the schema leaves it optional so clients
	// built before it existed keep compiling.
	Health Health `json:"health,omitempty" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	// Steps are the phases reached, each attributed to the field that
	// showed it.
	Steps      []OperationStep `json:"steps"`
	AcceptedAt time.Time       `json:"acceptedAt"`
	// Deadline is when the console stops waiting; past it with no
	// outcome the phase is timedOut, which does not mean it failed.
	Deadline time.Time `json:"deadline"`
	// Blocked reports the operator waits for something (a supervised
	// primary update waits for user action).
	Blocked bool `json:"blocked,omitempty"`
	// Actor is reserved and not filled: who asked is the API server's
	// audit log's to say, and user names are not shown to every viewer
	// of a Cluster.
	Actor string `json:"actor,omitempty" doc:"Reserved; not filled (the API server's audit log names who asked)."`
}

// OperationHealth reads a tracker phase as a health: settled is ok, failed
// is critical, timedOut is unknown (no outcome was reported, which is not
// a failure), notObservable is info (the operator reports no outcome for
// the kind at all), and a phase still moving is progressing, or warning
// when the operator reports it waits for someone (blocked: a supervised
// primary update waiting for user action). An unrecognised phase is
// unknown, never ok.
func OperationHealth(phase OperationPhase, blocked bool) Health {
	switch phase {
	case OpPhaseSettled:
		return HealthOK
	case OpPhaseFailed:
		return HealthCritical
	case OpPhaseTimedOut:
		return HealthUnknown
	case OpPhaseNotObservable:
		return HealthInfo
	case OpPhaseRequested, OpPhaseAccepted, OpPhaseActing:
		if blocked {
			return HealthWarning
		}
		return HealthProgressing
	default:
		return HealthUnknown
	}
}

// OperationStep is one tracker step.
type OperationStep struct {
	Phase  OperationPhase `json:"phase" enum:"requested,accepted,acting,settled,failed,timedOut,notObservable"`
	At     time.Time      `json:"at" doc:"When the console observed the step."`
	Source string         `json:"source" doc:"The field that showed it, such as Backup.status.phase."`
	Text   string         `json:"text"`
}
