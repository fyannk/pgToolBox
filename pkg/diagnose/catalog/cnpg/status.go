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

// The holding windows, console-pinned knowledge stated in one place.
// Each is how long a state the operator passes through in ordinary
// operation must last before it is a finding. Every rule's check row
// renders the number it applies.
const (
	// bootstrapHeld is how long "Setting up primary" or "Creating a new
	// replica" may last. A clone of a large primary takes longer than a
	// few minutes; half an hour is past what a healthy join needs on any
	// cluster small enough for the operator to have called it a phase.
	bootstrapHeld = 30 * time.Minute
	// rolloutHeld is how long a rollout or configuration phase may last.
	// The operator restarts one instance at a time and waits for each,
	// so a rollout's length scales with the instance count; half an hour
	// is past three instances' worth of restarts.
	rolloutHeld = 30 * time.Minute
	// majorUpgradeHeld is how long a PostgreSQL major upgrade may last:
	// pg_upgrade on a large cluster can take an hour, so two.
	majorUpgradeHeld = 2 * time.Hour
	// promotionHeld is how long a replica cluster's promotion may last.
	promotionHeld = 15 * time.Minute
	// notReadyHeld is how long Ready may stay False before it is a
	// finding of its own rather than a moment in a rollout.
	notReadyHeld = 10 * time.Minute
	// noSystemIDHeld is how long the operator may go without any
	// instance reporting a system identifier.
	noSystemIDHeld = 15 * time.Minute
	// hibernationHeld is how long hibernation may sit deleting pods.
	hibernationHeld = 15 * time.Minute
	// danglingHeld is how long a claim may sit without its pod. The
	// operator recreates a deleted pod in seconds; a quarter of an hour
	// is a pod that is not coming back on its own.
	danglingHeld = 15 * time.Minute
	// resizingHeld is how long a claim may carry the resize condition.
	// Online expansion completes in minutes; a resize the storage class
	// cannot do online waits for the pod to be deleted, indefinitely.
	resizingHeld = 30 * time.Minute
	// initializingHeld is how long a claim may exist with its creating
	// job and no pod.
	initializingHeld = 30 * time.Minute
	// primaryFailingHeld is how long the operator may report the
	// primary as failing. Its own failover delay defaults to zero, so a
	// minute of failing without a failover is a failover that is not
	// happening.
	primaryFailingHeld = time.Minute
	// timelineHeld is how long an instance may report a timeline other
	// than the cluster's. A switchover moves every instance to the new
	// timeline within its restart; ten minutes is a replica left behind.
	timelineHeld = 10 * time.Minute
	// certificateExpiringWithin is how close to expiry a certificate
	// may be. The operator renews the certificates it manages seven days
	// ahead, so three days out is a renewal that did not happen.
	certificateExpiringWithin = 3 * 24 * time.Hour
	// replicaSwitchHeld is how long a switch to replica cluster may be
	// in progress.
	replicaSwitchHeld = 15 * time.Minute
	// instancesShortHeld is how long fewer instances than declared may
	// be ready. A rollout is short one instance for the length of a
	// restart; ten minutes is a member that has not returned.
	instancesShortHeld = 10 * time.Minute
	// leaseGrace is how far past its own duration the primary lease
	// may go unrenewed. The default duration is fifteen seconds and the
	// holder renews every two; a minute past expiry is a holder that
	// has stopped, not a slow API server.
	leaseGrace = time.Minute
)

// statusRules cover the parts of the Cluster status that are neither a
// phase nor a condition: the operator's own lists of what is failed,
// unusable, dangling or resizing, the certificates it manages, and what
// each instance reported back. The strings pinned are the JSON tags the
// operator declares the fields under.
func statusRules() []diagnose.Rule {
	return []diagnose.Rule{
		{
			ID:        "cnpg-instance-failed",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityHigh,
			Describes: "an instance the operator lists as failed",
			Summary:   "The operator lists an instance as failed: its pod will not be scheduled again.",
			Detail: "The operator sorts its instances into healthy, replicating and failed, " +
				"and failed is the pod that was deleted or evicted and is not coming " +
				"back as it was. The pod-level checks usually say why; the operator " +
				"recreates the pod on its own once whatever removed it stops.",
			When:      diagnose.StatusListed{List: diagnose.ListFailedInstances},
			Pinned:    []string{"PodFailed", `json:"instancesStatus,omitempty"`},
			Link:      "/pods",
			LinkLabel: "Pods",
		},
		{
			ID:        "cnpg-pvc-unusable",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerKubernetes,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityHigh,
			Describes: "a volume claim the operator lists as unusable",
			Summary:   "The operator lists a volume claim as unusable: it belongs to an incomplete set.",
			Detail: "An instance's claims come as a group — data, and WAL or tablespaces " +
				"when the cluster declares them — and a claim whose group is incomplete " +
				"cannot start an instance. The usual cause is a claim deleted by hand. " +
				"The operator will not create an instance on the remaining claim; the " +
				"instance has to be recreated from the primary.",
			When:   diagnose.StatusListed{List: diagnose.ListUnusablePVC},
			Pinned: []string{`json:"unusablePVC,omitempty"`},
			NextSteps: "Delete the claims of the affected instance and let the operator " +
				"clone a fresh one from the primary. Nothing is lost that the " +
				"primary still has.",
			Link:      "/objects",
			LinkLabel: "Objects",
		},
		{
			ID:        "cnpg-pvc-dangling",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerKubernetes,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "a volume claim with no pod, listed by the operator for a quarter of an hour",
			Summary:   "A volume claim has had no pod for a quarter of an hour: the instance it belongs to is not being recreated.",
			Detail: "The operator lists a claim as dangling while no pod uses it. Every " +
				"pod deletion passes through this for the seconds the recreation " +
				"takes; a claim that stays dangling is an instance the operator is not " +
				"recreating — a fenced or hibernated cluster, a scale-down in " +
				"progress, or a pod it cannot create.",
			When:      diagnose.StatusListed{List: diagnose.ListDanglingPVC, MinAge: danglingHeld},
			Pinned:    []string{`json:"danglingPVC,omitempty"`},
			Link:      "/objects",
			LinkLabel: "Objects",
		},
		{
			ID:        "cnpg-pvc-resizing-stuck",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerKubernetes,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "a volume claim carrying the resize condition for half an hour",
			Summary:   "A volume claim has been resizing for half an hour: the expansion is not completing.",
			Detail: "A storage class that expands volumes online finishes in minutes. One " +
				"that does not waits for the pod to be deleted and the volume " +
				"reattached, which the operator does not do on its own; and a class " +
				"that does not allow expansion at all leaves the claim in this state " +
				"with the API server's refusal in the claim's events.",
			When:   diagnose.StatusListed{List: diagnose.ListResizingPVC, MinAge: resizingHeld},
			Pinned: []string{`json:"resizingPVC,omitempty"`},
			NextSteps: "Read the claim's events for the storage class's answer. If the " +
				"class expands only offline, delete the instance's pod and let the " +
				"operator recreate it on the resized volume — one replica at a time, " +
				"the primary last.",
			Link:      "/objects",
			LinkLabel: "Objects",
		},
		{
			ID:        "cnpg-pvc-initializing-stuck",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerKubernetes,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "a volume claim whose creating job has produced no pod in half an hour",
			Summary:   "A volume claim has been initializing for half an hour: the job creating its instance has not finished.",
			Detail: "The operator lists a claim as initializing while the job that " +
				"bootstraps or clones the instance is still running. The job's own " +
				"logs carry the reason it has not finished; the bootstrap log checks " +
				"quote the common ones.",
			When:      diagnose.StatusListed{List: diagnose.ListInitializingPVC, MinAge: initializingHeld},
			Pinned:    []string{`json:"initializingPVC,omitempty"`},
			Link:      "/objects",
			LinkLabel: "Objects",
		},
		{
			ID:        "cnpg-primary-failing",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerReplication,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "the operator reporting the current primary as failing for over a minute",
			Summary:   "The operator has reported the current primary as failing for over a minute, and has not failed over.",
			Detail: "The operator stamps the instant its status check on the primary " +
				"started failing, and clears it when the primary recovers or a " +
				"failover replaces it. A stamp that stays is a primary neither " +
				"recovering nor being replaced: a configured failover delay still " +
				"running, a quorum that forbids the failover, or no candidate to " +
				"promote.",
			When:   diagnose.PrimaryFailing{MinAge: primaryFailingHeld},
			Pinned: []string{`json:"currentPrimaryFailingSinceTimestamp,omitempty"`},
			NextSteps: "Check the failover delay and the quorum before forcing anything: " +
				"if the delay is deliberate, wait it out; if a quorum forbids the " +
				"failover, the standbys it needs are the problem, not the primary.",
			Link:      "/cluster/overview",
			LinkLabel: "Cluster overview",
		},
		{
			ID:        "cnpg-timeline-divergence",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerReplication,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "an instance reporting a PostgreSQL timeline other than the cluster's for ten minutes",
			Summary:   "An instance has reported a PostgreSQL timeline other than the cluster's for ten minutes: it is not following the current primary.",
			Detail: "Every promotion opens a new timeline, and every instance that " +
				"follows the new primary moves to it. One still reporting the old " +
				"timeline after the move settled is replaying nothing from the " +
				"primary: it serves reads from before the promotion, and it cannot be " +
				"promoted without losing what happened since.",
			When:   diagnose.TimelineDivergence{MinAge: timelineHeld},
			Pinned: []string{`json:"timeLineID,omitempty"`, `json:"instancesReportedState,omitempty"`},
			NextSteps: "Read the instance's log: it is usually a rewind that failed or a " +
				"replica that was fenced through the promotion. Recreating the " +
				"instance from the primary is the reliable repair.",
			Link:      "/cluster/overview",
			LinkLabel: "Cluster overview",
		},
		{
			ID:        "cnpg-certificate-expired",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "a certificate the operator reports as already expired",
			Summary:   "A certificate this cluster uses has expired.",
			Detail: "The operator reports an expiry for each certificate the cluster " +
				"uses, its own and any supplied by the user. An expired server " +
				"certificate ends every TLS connection to the cluster; an expired " +
				"replication certificate ends streaming replication. The operator " +
				"renews only the certificates it issued.",
			When:   diagnose.CertificateExpiring{},
			Pinned: []string{`json:"expirations,omitempty"`},
			NextSteps: "A certificate the operator issued that still expired means the " +
				"operator has not been reconciling this cluster — check its own " +
				"health. A user-supplied certificate has to be replaced in its Secret.",
			Link:      "/cluster/overview",
			LinkLabel: "Cluster overview",
		},
		{
			ID:        "cnpg-certificate-expiring",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityHigh,
			Describes: "a certificate the operator reports as expiring within three days",
			Summary:   "A certificate this cluster uses expires within three days and has not been renewed.",
			Detail: "The operator renews the certificates it issues seven days before " +
				"they expire, so one inside three days is either user-supplied, " +
				"which the operator never renews, or a renewal that did not happen.",
			When:      diagnose.CertificateExpiring{Within: certificateExpiringWithin},
			Pinned:    []string{`json:"expirations,omitempty"`},
			Link:      "/cluster/overview",
			LinkLabel: "Cluster overview",
		},
		{
			ID:        "cnpg-managed-role-unreconcilable",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "a managed role the operator reports it cannot reconcile",
			Summary:   "The operator cannot apply a managed role, and quotes PostgreSQL's refusal.",
			Detail: "The roles under spec.managed.roles are applied by the operator on " +
				"the primary, and the quoted error is PostgreSQL's own — most often a " +
				"role that owns objects and cannot be dropped, or a password Secret " +
				"that does not exist.",
			When:      diagnose.RoleUnreconcilable{},
			Pinned:    []string{`json:"cannotReconcile,omitempty"`},
			Link:      "/databases",
			LinkLabel: "Databases",
		},
		{
			ID:        "cnpg-tablespace-error",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "a declared tablespace the operator reports an error for",
			Summary:   "The operator cannot reconcile a declared tablespace, and quotes the error.",
			When:      diagnose.TablespaceError{},
			Pinned:    []string{`json:"tablespacesStatus,omitempty"`},
			Link:      "/cluster/overview",
			LinkLabel: "Cluster overview",
		},
		{
			ID:        "cnpg-replica-switch-stuck",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerReplication,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "a switch to replica cluster in progress for a quarter of an hour",
			Summary:   "The cluster has been switching to a replica cluster for a quarter of an hour: the demotion is not completing.",
			Detail: "Demoting a cluster to a replica fences every instance, records a " +
				"demotion token and restarts the instances as replicas of the new " +
				"source. A switch that stays in progress is usually an instance that " +
				"cannot reach the source it was told to follow.",
			When:      diagnose.ReplicaSwitchStuck{MinAge: replicaSwitchHeld},
			Pinned:    []string{"SwitchReplicaClusterStatus", `json:"inProgress,omitempty"`},
			Link:      "/cluster/overview",
			LinkLabel: "Cluster overview",
		},
		{
			ID:        "cnpg-instances-short",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityHigh,
			Describes: "fewer ready instances than declared, by the operator's own count, for ten minutes",
			Summary:   "Fewer instances are ready than the cluster declares, and have been for ten minutes.",
			Detail: "A rollout is short one instance for the length of a restart. A " +
				"shortfall that holds is an instance that is not coming back on its " +
				"own, and the pod-level checks usually say why: a pod that cannot " +
				"be scheduled, an image that cannot be pulled, a container that " +
				"keeps crashing, or an instance deliberately fenced.",
			When:   diagnose.InstancesShort{MinAge: instancesShortHeld},
			Pinned: []string{`json:"readyInstances,omitempty"`},
			ConsequenceOf: []diagnose.Relation{
				{Cause: "quota-exhausted", Strength: diagnose.StrengthPlausible},
				{Cause: "pod-scheduling", Strength: diagnose.StrengthPlausible},
				{Cause: "image-pull", Strength: diagnose.StrengthPlausible},
				{Cause: "k8s-container-crashloop", Strength: diagnose.StrengthPlausible},
				{Cause: "cnpg-instance-fenced", Strength: diagnose.StrengthPlausible},
				{Cause: "cnpg-instance-failed", Strength: diagnose.StrengthPlausible},
			},
			Link:      "/cluster/overview",
			LinkLabel: "Cluster overview",
		},
		{
			// The lease is 1.30 machinery: the instance promoted to primary
			// acquires the Lease named after the cluster and renews it as
			// long as it runs. The strings pinned are the operator's
			// reconciler that creates it and the field the holder writes.
			ID:        "cnpg-primary-lease-expired",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerReplication,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "the primary lease unrenewed for a minute past its duration, or released with a primary named",
			Summary:   "The primary lease is not being renewed: the instance holding the primary role has stopped keeping it.",
			Detail: "The primary's instance manager renews the lease every few " +
				"seconds for as long as it runs and releases it on shutdown. A " +
				"lease past its duration is a primary whose manager has stopped or " +
				"cannot reach the API server; a released lease while the operator " +
				"still names a primary is a primary that shut down without the " +
				"operator noticing yet. Either way no failover can complete until " +
				"the lease is acquired again.",
			When:   diagnose.PrimaryLeaseExpired{Grace: leaseGrace},
			Pinned: []string{"reconcilePrimaryLease", "HolderIdentity"},
			NextSteps: "Read the primary pod's state and log: a stopped instance " +
				"manager, a liveness failure or an API-server timeout is written " +
				"there. Do not delete the lease by hand; the next promotion acquires it.",
			Link:      "/cluster/overview",
			LinkLabel: "Cluster overview",
		},
		{
			ID:        "cnpg-primary-lease-holder-mismatch",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerReplication,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "the primary lease held by an instance other than the operator's current primary",
			Summary:   "The instance holding the primary lease is not the one the operator names as primary.",
			Detail: "The lease is what stops two instances from both being promoted, " +
				"and the holder is the instance that last won it. The operator's " +
				"currentPrimary naming another instance, with no primary move in " +
				"flight, is two writers disagreeing about who the primary is. The " +
				"lease holder is the one PostgreSQL will let accept writes; the " +
				"services follow the operator's label.",
			When:   diagnose.PrimaryLeaseHolderMismatch{},
			Pinned: []string{"reconcilePrimaryLease", "HolderIdentity"},
			NextSteps: "Read both instances' logs before touching anything. Fence " +
				"nothing until the operator's next reconcile has either moved " +
				"the primary or corrected the status.",
			ConsequenceOf: []diagnose.Relation{{Cause: "cnpg-primary-disagreement", Strength: diagnose.StrengthPlausible}},
			Link:          "/cluster/overview",
			LinkLabel:     "Cluster overview",
		},
	}
}
