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

// phaseRules cover the phases in which the operator has stopped
// reconciling and said so. The phase strings are the operator's own
// constants; the phase reason — quoted in the evidence — carries the
// specific cause, which for several of these phases is the only place
// that cause is written at all.
func phaseRules() []diagnose.Rule {
	return []diagnose.Rule{
		{
			ID:        "cnpg-unrecoverable",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "the operator declaring the cluster unrecoverable",
			Summary:   "The operator has declared the cluster unrecoverable: it will not repair this on its own.",
			Detail: "This phase has four distinct causes, and the quoted reason says which: " +
				"every instance's PVCs are gone (the data is lost and only a restore from " +
				"backup recreates the cluster); a bootstrap or replica-creation job " +
				"exhausted its retries (the job's own logs carry the error); no pod is " +
				"active at all; or a replica-promotion token does not match this cluster.",
			When: diagnose.ClusterPhase{AnyOf: []string{"Cluster is unrecoverable and needs manual intervention"}},
			NextSteps: "Match the quoted reason to its remedy: a failed job means reading " +
				"that job's own logs, where the underlying error is; missing PVCs " +
				"mean restoring the cluster from a backup; a wrong promotion token " +
				"means regenerating it from the source cluster's demotion token.",
			Link:      "/cluster/overview",
			LinkLabel: "Cluster overview",
		},
		{
			// The phase is new in 1.30; older operators reject an invalid
			// definition at admission instead of parking it in a phase.
			ID:        "cnpg-invalid-definition",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "a cluster definition the operator's validation rejected",
			Summary:   "The cluster definition is invalid, so the operator is not reconciling it.",
			Detail: "The quoted reason is the validation message. Nothing changes until the " +
				"spec is corrected.",
			When:      diagnose.ClusterPhase{AnyOf: []string{"Invalid cluster definition"}},
			Link:      "/cluster/overview",
			LinkLabel: "Cluster overview",
		},
		{
			ID:        "cnpg-cannot-create-objects",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerKubernetes,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "the operator failing to create the cluster's auxiliary objects",
			Summary:   "The operator cannot create objects this cluster needs.",
			Detail: "The quoted reason carries the API server's refusal — typically operator " +
				"RBAC, a namespace quota, or a conflicting pre-existing object.",
			When:      diagnose.ClusterPhase{AnyOf: []string{"Unable to create required cluster objects"}},
			Link:      "/objects",
			LinkLabel: "Objects",
		},
		{
			ID:        "cnpg-unknown-plugin",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "a required CNPG-I plugin the operator cannot find",
			Summary:   "A plugin this cluster requires is not loaded, and reconciliation is fully stopped.",
			Detail: "The operator checks plugins before anything else, so while this phase " +
				"holds nothing runs: no failover, no rollout, no backup. The plugin name " +
				"in the quoted reason comes from spec.plugins or an external cluster's " +
				"plugin stanza; a plugin absent from the operator's registration is " +
				"usually not installed, not exposed as a service, or misspelled.",
			When: diagnose.ClusterPhase{AnyOf: []string{
				"Cluster cannot proceed to reconciliation due to an unknown plugin being required",
			}},
			Link:      "/cluster/overview",
			LinkLabel: "Cluster overview",
		},
		{
			ID:        "cnpg-plugin-failure",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "a CNPG-I plugin interaction failing during reconciliation",
			Summary:   "The operator cannot talk to a plugin this cluster requires, and reconciliation is stopped.",
			When: diagnose.ClusterPhase{AnyOf: []string{
				"Cluster cannot proceed to reconciliation due to an error while interacting with plugins",
			}},
			Link:      "/cluster/overview",
			LinkLabel: "Cluster overview",
		},
		{
			ID:        "cnpg-image-catalog-unusable",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityHigh,
			Describes: "an image catalog the operator cannot resolve an image from",
			Summary:   "The operator cannot pick a container image from the referenced catalog.",
			Detail: "No image means nothing can roll: pods keep their current image and a " +
				"pending upgrade cannot start. The quoted reason names what is missing — " +
				"usually the catalog itself, or an entry for the requested major version.",
			When: diagnose.ClusterPhase{AnyOf: []string{"Cluster has incomplete or invalid image catalog"}},
			// The catalog checks read the same reference the operator
			// failed to resolve, and say which way it failed.
			ConsequenceOf: []diagnose.Relation{
				{Cause: "cnpg-image-catalog-missing"}, {Cause: "cnpg-image-catalog-lacks-major"},
			},
			Link:      "/cluster/overview",
			LinkLabel: "Cluster overview",
		},
		{
			ID:        "cnpg-arch-binary-missing",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "an online instance-manager upgrade blocked by a missing architecture binary",
			Summary:   "The operator image carries no instance-manager binary for this node architecture, so the online upgrade cannot run.",
			When: diagnose.ClusterPhase{AnyOf: []string{
				"Cluster cannot execute instance online upgrade due to missing architecture binary",
			}},
			Link:      "/cluster/overview",
			LinkLabel: "Cluster overview",
		},
		{
			ID:        "cnpg-waiting-for-user",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "the operator waiting for a supervised switchover",
			Summary:   "The operator is waiting for a manual switchover and will not proceed on its own.",
			Detail: "primaryUpdateStrategy is supervised and a change is pending on the " +
				"primary — a rollout, or a decrease of a hot-standby-sensitive parameter. " +
				"Nothing is broken and nothing resumes until the switchover is performed " +
				"or the strategy is set to unsupervised.",
			When:      diagnose.ClusterPhase{AnyOf: []string{"Waiting for user action"}},
			Link:      "/cluster/overview",
			LinkLabel: "Cluster overview",
		},
		{
			ID:        "cnpg-upgrade-delayed",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityNote,
			Describes: "an upgrade the operator is configured to delay",
			Summary:   "An upgrade is pending but the operator is configured to delay it.",
			When:      diagnose.ClusterPhase{AnyOf: []string{"Cluster upgrade delayed"}},
			Link:      "/cluster/overview",
			LinkLabel: "Cluster overview",
		},
		{
			ID:        "cnpg-wal-disk-space-phase",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerPostgreSQL,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "the operator refusing to run PostgreSQL for lack of WAL disk space",
			Summary:   "One or more instances have no disk space left for WAL, and PostgreSQL is being kept down.",
			Detail: "This is the end state of a filling WAL volume — most often WAL " +
				"archiving having failed for long enough to fill it. The volume must " +
				"grow, or the archiving failure that filled it must be fixed.",
			When: diagnose.ClusterPhase{AnyOf: []string{"Not enough disk space"}},
			NextSteps: "Grow the affected volume (the storage class must allow expansion), " +
				"or fix the archiving failure that filled it. The operator restarts " +
				"PostgreSQL on its own once space exists.",
			ConsequenceOf: []diagnose.Relation{{Cause: "cnpg-wal-disk-full"}, {Cause: "cnpg-wal-archiving-failing"}},
			Link:          "/cluster/overview",
			LinkLabel:     "Cluster overview",
		},
		{
			// Not a phase but the same kind of claim: operator status
			// fields, read together. The operator stamps the request time
			// whenever it sets a new target primary, which is what makes
			// "still in flight after ten minutes" an observed fact rather
			// than the console's guess.
			ID:        "cnpg-primary-move-stuck",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerReplication,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "a switchover or failover still unfinished after ten minutes",
			Summary:   "A primary move has been in flight for over ten minutes: the cluster is between primaries and stuck there.",
			Detail: "The operator's current and target primaries disagree, and the " +
				"move was requested long enough ago that ordinary switchovers and " +
				"failovers are ruled out. While this holds, an in-place primary " +
				"restart is also blocked. The promotion-stall log checks usually say " +
				"which side is wedged.",
			When:      diagnose.PrimaryMismatch{MinAge: 10 * time.Minute},
			Pinned:    []string{`json:"targetPrimaryTimestamp,omitempty"`},
			Link:      "/cluster/overview",
			LinkLabel: "Cluster overview",
		},
		{
			ID:        "cnpg-status-unreachable",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "the operator unable to reach any ready instance's status endpoint",
			Summary:   "The operator cannot read status from the ready instances, so it can make no decision at all.",
			Detail: "Failover, rollouts, and every other operator decision need the " +
				"instance managers' status endpoint. The operator's own message points " +
				"at network policy; a stale operator TLS certificate produces the same " +
				"state.",
			When: diagnose.ClusterPhase{AnyOf: []string{
				"Instance Status Extraction Error: HTTP communication issue",
			}},
			Link:      "/cluster/pods",
			LinkLabel: "Pods",
		},
		{
			// The phases below are the operator on its way somewhere,
			// and a finding only when the way is blocked. The operator
			// writes no clock beside a phase, so these read the
			// console's own record of how long the phase has held.
			ID:        "cnpg-bootstrap-stuck",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "the operator creating the primary or a replica for half an hour",
			Summary:   "The operator has been creating an instance for half an hour, and the instance has not come up.",
			Detail: "Setting up the primary runs initdb or a restore; creating a replica " +
				"clones the primary. Both run as a job whose log carries the reason " +
				"it has not finished, and the bootstrap log checks quote the common " +
				"ones: a failing restore, an archive that was not empty, a clone the " +
				"primary refused. A job that never started at all is usually a pod " +
				"that cannot be scheduled or an image that cannot be pulled.",
			When: diagnose.ClusterPhaseHeld{
				AnyOf:  []string{"Setting up primary", "Creating a new replica"},
				MinAge: bootstrapHeld,
			},
			NextSteps: "Read the bootstrap job's log first: it names the step that failed. " +
				"If the job exists but its pod does not, the pod-level checks say why.",
			ConsequenceOf: []diagnose.Relation{
				{Cause: "cnpg-initdb-failed"},
				{Cause: "cnpg-restore-failed"},
				{Cause: "cnpg-join-failed"},
				{Cause: "wal-archive-not-empty"},
				{Cause: "cnpg-recovery-target-missing"},
				{Cause: "cnpg-bootstrap-backup-missing"},
				{Cause: "cnpg-status-unreachable"},
				{Cause: "pod-scheduling", Strength: diagnose.StrengthPlausible},
				{Cause: "image-pull", Strength: diagnose.StrengthPlausible},
				{Cause: "quota-exhausted", Strength: diagnose.StrengthPlausible},
			},
			Link:      "/cluster/overview",
			LinkLabel: "Cluster overview",
		},
		{
			ID:        "cnpg-rollout-stuck",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "a rollout or configuration phase held for half an hour",
			Summary:   "The operator has been rolling the cluster out for half an hour, and the rollout has not finished.",
			Detail: "A rollout restarts one instance at a time and waits for each to " +
				"be ready before the next; the primary goes last, through a " +
				"switchover. The phase reason names the instance being waited on. A " +
				"rollout that stops is that instance not becoming ready, and the " +
				"pod-level and replication checks say why.",
			When: diagnose.ClusterPhaseHeld{AnyOf: []string{
				"Upgrading cluster", "Applying configuration", "Online upgrade in progress",
				"Primary instance is being restarted in-place",
				"Primary instance is being restarted without a switchover",
				"Waiting for the instances to become active",
			}, MinAge: rolloutHeld},
			ConsequenceOf: []diagnose.Relation{
				{Cause: "cnpg-postgres-start-failed"},
				{Cause: "cnpg-postgres-exited"},
				{Cause: "cnpg-replica-not-streaming"},
				{Cause: "cnpg-instance-fenced"},
				{Cause: "k8s-container-crashloop", Strength: diagnose.StrengthPlausible},
				{Cause: "pod-scheduling", Strength: diagnose.StrengthPlausible},
				{Cause: "image-pull", Strength: diagnose.StrengthPlausible},
			},
			Link:      "/cluster/overview",
			LinkLabel: "Cluster overview",
		},
		{
			ID:        "cnpg-major-upgrade-stuck",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "a PostgreSQL major upgrade running for two hours",
			Summary:   "A PostgreSQL major upgrade has been running for two hours.",
			Detail: "The operator runs pg_upgrade in a job with no retries, so a failed " +
				"upgrade is a job with a failed pod whose log carries pg_upgrade's " +
				"own report, and the cluster stays in this phase until the image is " +
				"reverted or the upgrade repeated. A large cluster legitimately takes " +
				"a while; two hours is past most of them.",
			When: diagnose.ClusterPhaseHeld{AnyOf: []string{"Upgrading Postgres major version"}, MinAge: majorUpgradeHeld},
			NextSteps: "Find the upgrade job and read its pod's log. Reverting the " +
				"cluster's image to the previous major makes the operator delete the " +
				"failed job and resume on the old version.",
			Link:      "/cluster/overview",
			LinkLabel: "Cluster overview",
		},
		{
			ID:        "cnpg-promotion-stuck",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerReplication,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "a replica cluster's promotion running for a quarter of an hour",
			Summary:   "The cluster has been promoting itself from replica to primary for a quarter of an hour.",
			Detail: "Promotion with a token waits for the designated primary to replay " +
				"up to the point the token records; one that stays in this phase is " +
				"a replica that cannot reach that point, because the source is " +
				"ahead of what was replicated or the token belongs to another " +
				"cluster.",
			When:      diagnose.ClusterPhaseHeld{AnyOf: []string{"Promoting to primary cluster"}, MinAge: promotionHeld},
			Link:      "/cluster/overview",
			LinkLabel: "Cluster overview",
		},
	}
}
