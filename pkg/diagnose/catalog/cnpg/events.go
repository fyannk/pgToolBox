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

// eventRules cover the refusals the operator records as events on the
// Cluster object. Only Cluster and member-pod events are in the
// observed window; what the operator writes on Backup objects surfaces
// through the backup rules instead.
func eventRules() []diagnose.Rule {
	return []diagnose.Rule{
		{
			ID:        "cnpg-primary-status-check",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "a Ready primary whose status check the operator cannot pass",
			Summary:   "The primary looks Ready but the operator's own status check on it fails, and failover is deliberately deferred.",
			Detail: "The operator short-circuits reconciliation in this state: it will not " +
				"fail over until Kubernetes itself marks the primary not Ready. A " +
				"cluster can sit here indefinitely looking almost healthy.",
			When:      diagnose.EventMatch{Reasons: []string{"PrimaryStatusCheckFailed"}},
			Link:      "/cluster/pods",
			LinkLabel: "Pods",
		},
		{
			// The primary lease is 1.30 machinery; older operators
			// arbitrate the primary through status alone.
			ID:        "cnpg-primary-lease-conflict",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "a primary lease owned by something else",
			Summary:   "The primary lease is controlled by another owner, and the operator refuses to adopt it.",
			Detail: "Usually a leftover lease from a deleted cluster with the same name. " +
				"Reconciliation errors out on every loop until the lease is removed.",
			When:      diagnose.EventMatch{Reasons: []string{"PrimaryLeaseConflict"}},
			Link:      "/cluster/overview",
			LinkLabel: "Cluster overview",
		},
		{
			ID:        "cnpg-scale-down-refused",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "a scale-down the operator reverted",
			Summary:   "The requested instance count conflicts with maxSyncReplicas, and the operator reverted it.",
			Detail: "The operator wrote the old count back into the spec, so the scale-down " +
				"silently never happens until maxSyncReplicas is lowered first.",
			When:      diagnose.EventMatch{Reasons: []string{"NoScaleDown"}},
			Link:      "/cluster/overview",
			LinkLabel: "Cluster overview",
		},
		{
			ID:        "cnpg-ca-secret-unusable",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityHigh,
			Describes: "a referenced CA secret that is missing or unparseable",
			Summary:   "A CA secret this cluster references is missing or malformed, and PKI reconciliation is stopped.",
			When:      diagnose.EventMatch{Reasons: []string{"SecretNotFound", "InvalidCASecret"}},
			Link:      "/cluster/overview",
			LinkLabel: "Cluster overview",
		},
		{
			ID:        "cnpg-ca-expiring",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "a user-supplied CA approaching expiry",
			Summary:   "A user-supplied CA certificate is close to expiring.",
			Detail: "The operator does not rotate user-supplied CAs. When this one lapses, " +
				"TLS between the operator, the instances, and clients breaks with it.",
			When:      diagnose.EventMatch{Reasons: []string{"SecretIsExpiring"}},
			Link:      "/cluster/overview",
			LinkLabel: "Cluster overview",
		},
		{
			// The event exists in every supported minor (1.30 and later).
			ID:        "cnpg-service-account-missing",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerKubernetes,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "a specified ServiceAccount that does not exist",
			Summary:   "The ServiceAccount named in the spec does not exist, so pods cannot be created.",
			When:      diagnose.EventMatch{Reasons: []string{"ServiceAccountNotFound"}, Kinds: []string{"Cluster", "Pooler"}},
			Link:      "/cluster/pods",
			LinkLabel: "Pods",
		},
		{
			ID:        "cnpg-bootstrap-backup-missing",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityCritical,
			Describes: "a bootstrap recovery pointing at a Backup that does not exist",
			Summary:   "Bootstrap-from-recovery references a Backup object that is missing, so the primary is never created.",
			When:      diagnose.EventMatch{Reasons: []string{"ErrorNoBackup"}},
			Link:      "/backups",
			LinkLabel: "Backups",
		},
		{
			ID:        "cnpg-manager-upgrade-failed",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "an in-place instance-manager upgrade that failed",
			Summary:   "The in-place instance-manager upgrade failed, so pods are still running the old binary.",
			When:      diagnose.EventMatch{Reasons: []string{"InstanceManagerUpgradeFailed"}},
			Link:      "/cluster/pods",
			LinkLabel: "Pods",
		},
		{
			ID:        "cnpg-retention-failed",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerBackups,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "a backup retention policy that failed to prune",
			Summary:   "The backup retention policy failed, so the object store keeps growing.",
			When:      diagnose.EventMatch{Reasons: []string{"RetentionPolicyFailed"}},
			Link:      "/backups",
			LinkLabel: "Backups",
		},
		{
			// The Backup, ScheduledBackup and Pooler events below are the
			// operator's own account of why a backup or a pooler is not
			// happening, recorded on the object rather than on the
			// Cluster. Each is admitted only for an object the respective
			// catalog lists as this cluster's.
			ID:        "cnpg-backup-target-unhealthy",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerBackups,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "the operator refusing to run a backup on an unhealthy target instance",
			Summary:   "A backup cannot run because the instance it targets is not healthy, and the operator says how.",
			Detail: "A backup runs on one instance — the primary, or the preferred " +
				"replica — and the operator checks that instance before starting. " +
				"The quoted message is the check's own failure. The backup stays " +
				"pending until the target recovers or another target is chosen.",
			When:      diagnose.EventMatch{Reasons: []string{"TargetPodNotHealthy"}, Kinds: []string{"Backup"}},
			Link:      "/backups",
			LinkLabel: "Backups",
		},
		{
			ID:        "cnpg-backup-waiting-for-target",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerBackups,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "a backup waiting because its target instance is not ready or not found",
			Summary:   "A backup is waiting for its target instance to become ready.",
			Detail: "The operator keeps the backup pending while the instance it would " +
				"run on is not ready or does not exist yet, and retries. A target " +
				"that never becomes ready is the finding; the pod-level checks say " +
				"why it is not.",
			When:      diagnose.EventMatch{Reasons: []string{"BackupPending", "FindingPod"}, Kinds: []string{"Backup"}},
			Link:      "/backups",
			LinkLabel: "Backups",
		},
		{
			ID:        "cnpg-backup-error",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerBackups,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "a backup the operator recorded as exiting with an error",
			Summary:   "A backup exited with an error, and the operator quotes it.",
			Detail: "The event carries the error the backup command or the snapshot " +
				"returned, which the Backup object's own status also records. It is " +
				"the reason behind a failed backup phase.",
			When:      diagnose.EventMatch{Reasons: []string{"Error"}, Kinds: []string{"Backup"}},
			Link:      "/backups",
			LinkLabel: "Backups",
		},
		{
			ID:        "cnpg-backup-manager-restarted",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerBackups,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "a backup the operator failed because the instance manager restarted under it",
			Summary:   "A backup failed because the instance manager restarted on its target while it ran.",
			Detail: "The instance manager runs the backup, so its restart ends the backup " +
				"mid-way and the operator marks it failed rather than resuming. " +
				"The restart itself is the thing to explain: a pod restart, a " +
				"liveness failure, or an in-place manager upgrade.",
			When:      diagnose.EventMatch{Reasons: []string{"InstanceManagerRestarted"}, Kinds: []string{"Backup"}},
			Link:      "/backups",
			LinkLabel: "Backups",
		},
		{
			ID:        "cnpg-schedule-cluster-unhealthy",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerBackups,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "a schedule the operator holds back because the cluster is not healthy",
			Summary:   "A backup schedule is not firing because the operator is waiting for the cluster to be healthy.",
			Detail: "The schedule creates a Backup only while the cluster reports " +
				"healthy, and the quoted message names the phase it found instead. " +
				"While this holds, no scheduled backup is taken.",
			When:      diagnose.EventMatch{Reasons: []string{"ClusterNotHealthy"}, Kinds: []string{"ScheduledBackup"}},
			Link:      "/backups",
			LinkLabel: "Backups",
		},
		{
			ID:        "cnpg-schedule-invalid",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerBackups,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityHigh,
			Describes: "a schedule expression no time satisfies",
			Summary:   "A backup schedule's expression matches no time at all, so it never fires.",
			Detail: "The operator evaluates the schedule and found no next run. The " +
				"expression is the six-field cron form with seconds first; a " +
				"five-field expression or an impossible day is the usual cause.",
			When:      diagnose.EventMatch{Reasons: []string{"NoSchedule"}, Kinds: []string{"ScheduledBackup"}},
			NextSteps: "Correct the schedule expression: six fields, seconds first.",
			Link:      "/backups",
			LinkLabel: "Backups",
		},
		{
			ID:        "cnpg-schedule-adoption-refused",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerBackups,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "a schedule skipping a run because a Backup of that name is not its own",
			Summary:   "A backup schedule skipped a run: a Backup with the name it would use exists and is not owned by it.",
			Detail: "The schedule names each Backup after its run time, and refuses to " +
				"adopt one it did not create. A Backup created by hand or by a " +
				"previous schedule of the same name blocks that iteration.",
			When:      diagnose.EventMatch{Reasons: []string{"BackupAdoptionRefused"}, Kinds: []string{"ScheduledBackup"}},
			Link:      "/backups",
			LinkLabel: "Backups",
		},
		{
			ID:        "cnpg-schedule-creation-failed",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerBackups,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "a schedule the operator could not create a Backup object for",
			Summary:   "A backup schedule could not create its Backup object.",
			Detail: "The API server refused the Backup the schedule tried to create — " +
				"a quota, an admission policy, or operator RBAC. No backup is taken " +
				"for that run.",
			When:      diagnose.EventMatch{Reasons: []string{"BackupCreation"}, Kinds: []string{"ScheduledBackup"}},
			Link:      "/backups",
			LinkLabel: "Backups",
		},
		{
			ID:        "cnpg-pooler-image-catalog-error",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerPoolers,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "a Pooler whose image the operator cannot resolve from its catalog",
			Summary:   "The operator cannot resolve a Pooler's image from the catalog it names.",
			When:      diagnose.EventMatch{Reasons: []string{"ImageCatalogError"}, Kinds: []string{"Pooler"}},
			Link:      "/poolers",
			LinkLabel: "Poolers",
		},
		{
			ID:        "cnpg-pooler-ownership-invalid",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerPoolers,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "a Pooler whose managed resources the operator found owned by something else",
			Summary:   "A Pooler's managed resources are owned by something other than the Pooler, and the operator will not touch them.",
			Detail: "The operator creates a Deployment, a Service and a ServiceAccount " +
				"for each Pooler and refuses to manage ones it did not create. A " +
				"pre-existing object of the same name — from a previous Pooler, or " +
				"made by hand — leaves the pooler unreconciled until it is removed.",
			When:      diagnose.EventMatch{Reasons: []string{"InvalidOwnership"}, Kinds: []string{"Pooler"}},
			Link:      "/poolers",
			LinkLabel: "Poolers",
		},
	}
}
