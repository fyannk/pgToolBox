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

package triage

// upstreamTroubleshooting is the guide whose order the playbooks
// follow. The commands quoted in each step are its.
const upstreamTroubleshooting = "https://cloudnative-pg.io/docs/1.30/troubleshooting/"

// Playbooks are the symptoms an operator arrives with, each walked in
// the order the upstream guide asks its questions: the platform first,
// then the operator, then the database, then the paths out of it.
// Every check named here is one the catalog or the detectors declare;
// the tests refuse a name that is not, and refuse a playbook with no
// golden scenario.
func Playbooks() []Playbook {
	return []Playbook{
		{
			ID:        "cannot-connect",
			Symptom:   "Clients cannot connect",
			Describes: "connections to the cluster's services fail or time out",
			Upstream:  upstreamTroubleshooting,
			Steps: []Step{
				{
					Question: "Is the cluster deliberately down?",
					Checks:   []string{"cnpg-hibernated", "cnpg-demotion-fencing", "cnpg-instance-fenced"},
					Explain: "A hibernated cluster has no pods; a demoted or fenced one has pods " +
						"with PostgreSQL stopped. Nothing is broken, and nothing answers.",
				},
				{
					Question: "Does the Cluster exist, and does the operator name a primary?",
					Fact:     NoPrimaryNamed{},
					Checks:   []string{"cnpg-unrecoverable", "cnpg-invalid-definition", "cnpg-bootstrap-stuck", "cnpg-no-system-id"},
					Explain: "With no primary named there is nothing for the read-write service " +
						"to route to. The phase says whether the operator is still creating " +
						"one or has given up.",
					Yourself: "kubectl -n <namespace> get cluster <name> -o yaml",
				},
				{
					Question: "Is the primary failing, or moving?",
					Checks: []string{
						"cnpg-primary-failing", "cnpg-primary-move-stuck", "cnpg-primary-lease-expired",
						"cnpg-primary-lease-holder-mismatch", "cnpg-primary-disagreement", "cnpg-lease-not-acquired",
					},
					Explain: "Between primaries, the read-write service points at nothing that " +
						"accepts writes; a primary the operator reports as failing is one it " +
						"has not yet replaced.",
				},
				{
					Question: "Is any instance pod ready at all?",
					Fact:     NoReadyInstance{},
					Checks: []string{
						"cnpg-instances-short", "k8s-container-crashloop", "k8s-container-oom", "pod-scheduling",
						"image-pull", "k8s-container-config-error", "k8s-volume-mount-failed", "k8s-pod-evicted", "cnpg-instance-failed",
					},
					Explain: "A pod that is not ready is not behind any Service. The pod-level " +
						"checks name what keeps it out: scheduling, an image, a crash.",
					Yourself: "kubectl -n <namespace> get pods -l cnpg.io/cluster=<name> -L role -o wide",
				},
				{
					Question: "Is PostgreSQL itself down on the instances?",
					Checks: []string{
						"cnpg-postgres-start-failed", "cnpg-postgres-exited", "cnpg-wal-disk-space-phase",
						"cnpg-wal-disk-full", "postgres-panic", "cnpg-pg-control-lost",
					},
					Explain: "The pod runs but the postmaster does not: a failed start, a crash, " +
						"or the operator keeping it down for lack of disk.",
					Yourself: "kubectl -n <namespace> logs <pod> -c postgres --previous",
				},
				{
					Question: "Has a certificate expired?",
					Checks:   []string{"cnpg-certificate-expired", "cnpg-certificate-expiring", "cnpg-ca-secret-unusable"},
					Explain: "An expired server certificate ends every TLS connection; clients " +
						"see a handshake failure rather than a refusal.",
				},
				{
					Question: "Does the read-write Service exist?",
					Fact:     NoWriteService{},
					Checks:   []string{"cnpg-cannot-create-objects"},
					Explain:  "Without the -rw Service there is no stable name for clients to reach.",
					Yourself: "kubectl -n <namespace> get svc -l cnpg.io/cluster=<name>",
				},
				{
					Question: "If clients go through a pooler, is the pooler up?",
					Checks:   []string{"cnpg-pooler-failed", "cnpg-pooler-inactive", "cnpg-pooler-short", "cnpg-pooler-clients-waiting"},
					Explain: "A pooler with no ready instance answers nothing; one that is " +
						"queueing answers slowly enough to time out.",
				},
				{
					Question: "Is PostgreSQL refusing the connections it receives?",
					Checks:   []string{"postgres-fatal"},
					Explain: "A FATAL record on every attempt is authentication or a full " +
						"connection slot, in the server's own words.",
					Yourself: `kubectl -n <namespace> logs <pod> -c postgres | jq 'select(.record.error_severity=="FATAL")'`,
				},
				{
					Question: "Is a NetworkPolicy dropping the traffic?",
					Checks:   []string{"cnpg-status-unreachable", "cnpg-liveness-isolation"},
					Explain: "A policy that drops traffic produces no event and no condition. " +
						"The console sees only its consequences: the operator unable to read " +
						"instance status, or an instance unable to reach its peers.",
					Yourself: "kubectl -n <namespace> get networkpolicies",
				},
			},
		},
		{
			ID:        "writes-refused",
			Symptom:   "Writes are refused or hang",
			Describes: "reads work but writes fail, or commits wait",
			Upstream:  upstreamTroubleshooting,
			Steps: []Step{
				{
					Question: "Is this cluster a replica, or being demoted to one?",
					Checks:   []string{"cnpg-demotion-fencing", "cnpg-replica-switch-stuck", "cnpg-promotion-stuck"},
					Explain: "A replica cluster's designated primary is read-only until promoted. " +
						"Every write fails until then.",
				},
				{
					Question: "Do the operator and the instances agree on who is primary?",
					Checks:   []string{"cnpg-primary-disagreement", "cnpg-primary-lease-holder-mismatch", "cnpg-primary-move-stuck", "cnpg-timeline-divergence"},
					Explain: "A named primary that is in recovery refuses every write; a move " +
						"in flight has no writer at all.",
				},
				{
					Question: "Is PostgreSQL protecting itself from wraparound?",
					Checks:   []string{"postgres-xid-wraparound", "postgres-mxid-wraparound"},
					Explain: "Near wraparound PostgreSQL refuses new transactions until a vacuum " +
						"of the oldest database finishes.",
				},
				{
					Question: "Are commits waiting on synchronous standbys that are not there?",
					Checks:   []string{"cnpg-sync-replicas-short", "cnpg-quorum-standbys-short", "cnpg-replica-not-streaming", "cnpg-replica-not-receiving"},
					Explain: "A commit waits for as many standbys as the configuration names; " +
						"fewer streaming means every commit hangs.",
				},
				{
					Question: "Has the WAL volume filled?",
					Checks:   []string{"cnpg-wal-disk-full", "cnpg-wal-disk-space-phase", "cnpg-wal-archive-backlog", "cnpg-wal-archiving-failing"},
					Explain: "With no room for the next segment PostgreSQL stops writing, and " +
						"the operator keeps it down until there is.",
				},
				{
					Question: "Is a lock, a long transaction or a deadlock pattern holding writes?",
					Checks:   []string{"postgres-backends-waiting", "postgres-long-transaction", "postgres-deadlocks-ongoing"},
					Explain: "Writes that hang rather than fail are usually waiting on a lock " +
						"another session holds.",
					Yourself: "kubectl cnpg psql <name> -- -c 'select pid, state, wait_event_type, query from pg_stat_activity where wait_event is not null'",
				},
			},
		},
		{
			ID:        "backups-not-happening",
			Symptom:   "Backups are not happening",
			Describes: "no recent backup, or backups that fail",
			Upstream:  upstreamTroubleshooting,
			Steps: []Step{
				{
					Question: "Is there a schedule, and is it firing?",
					Fact:     NoBackupSchedule{},
					Checks: []string{
						"cnpg-schedule-suspended", "cnpg-schedule-not-firing", "cnpg-schedule-invalid",
						"cnpg-schedule-cluster-unhealthy", "cnpg-schedule-adoption-refused", "cnpg-schedule-creation-failed", "backup-cadence",
					},
					Explain: "No Backup object is created unless a schedule fires, and the " +
						"schedule's own events say why it did not.",
					Yourself: "kubectl -n <namespace> get scheduledbackup,backup -l cnpg.io/cluster=<name>",
				},
				{
					Question: "Is a backup stuck before it starts?",
					Checks:   []string{"cnpg-backup-stuck-pending", "cnpg-backup-target-unhealthy", "cnpg-backup-waiting-for-target"},
					Explain: "A backup runs on one instance and waits while that instance is " +
						"not healthy; a target that never becomes healthy is the finding.",
				},
				{
					Question: "Did the backup fail?",
					Checks: []string{
						"cnpg-backup-failed", "cnpg-backup-error", "cnpg-backup-manager-restarted",
						"cnpg-backup-plugin-missing", "cnpg-backup-stop-blocked", "cnpg-last-backup-failed",
					},
					Explain:  "The Backup object and its events carry the error the command returned.",
					Yourself: "kubectl -n <namespace> describe backup <backup>",
				},
				{
					Question: "Is WAL archiving working?",
					Checks: []string{
						"cnpg-wal-archiving-failing", "cnpg-wal-archive-command-failed", "cnpg-wal-archive-plugin-missing",
						"cnpg-archiver-failing", "cnpg-wal-archive-backlog", "cnpg-backup-wal-archiving",
						"object-store-denied", "object-store-forbidden", "object-store-unreachable",
						"backup-destination-conflict", "wal-archive-not-empty",
					},
					Explain: "A backup is not a recovery point without the WAL that follows it, " +
						"and the operator refuses to finish one while archiving fails.",
					Yourself: "kubectl -n <namespace> logs <primary-pod> -c plugin-barman-cloud",
				},
				{
					Question: "What does the object store itself hold?",
					Checks: []string{
						"barman-no-successful-backup", "barman-last-backup-failed",
						"repository-wal-unhealthy", "repository-coverage-unhealthy", "repository-retention-unhealthy", "cnpg-retention-failed",
					},
					Explain: "The plugin's and the evidence agent's own accounts of the store, " +
						"independent of what the operator claims.",
				},
			},
		},
		{
			ID:        "will-not-come-up",
			Symptom:   "The cluster will not come up",
			Describes: "a new cluster, or a recreated instance, never becomes healthy",
			Upstream:  upstreamTroubleshooting,
			Steps: []Step{
				{
					Question: "Has the operator refused the definition, or stopped before starting?",
					Fact:     ClusterAbsent{},
					Checks: []string{
						"cnpg-invalid-definition", "cnpg-unrecoverable", "cnpg-cannot-create-objects",
						"cnpg-unknown-plugin", "cnpg-plugin-failure", "cnpg-image-catalog-unusable",
						"cnpg-image-catalog-missing", "cnpg-image-catalog-lacks-major", "cnpg-service-account-missing",
					},
					Explain: "These phases stop reconciliation entirely; nothing else runs " +
						"until the definition or the platform is corrected.",
					Yourself: "kubectl -n <namespace> get cluster <name> -o jsonpath='{.status.phase}: {.status.phaseReason}'",
				},
				{
					Question: "Is the bootstrap or the clone failing?",
					Checks: []string{
						"cnpg-bootstrap-stuck", "cnpg-initdb-failed", "cnpg-restore-failed", "cnpg-recovery-target-missing",
						"cnpg-bootstrap-backup-missing", "wal-archive-not-empty", "cnpg-join-failed", "cnpg-pvc-initializing-stuck",
					},
					Explain: "The bootstrap job's log carries the step that failed: initdb, a " +
						"restore, an archive that was not empty, a clone the primary refused.",
					Yourself: "kubectl -n <namespace> logs job/<name>-1-initdb",
				},
				{
					Question: "Can the pods be scheduled and started at all?",
					Checks: []string{
						"pod-scheduling", "quota-exhausted", "resource-quota", "image-pull", "volume-binding",
						"k8s-volume-mount-failed", "k8s-container-config-error", "k8s-container-crashloop", "k8s-container-oom",
					},
					Explain:  "Before the operator's job can run, Kubernetes has to place it and pull its image.",
					Yourself: "kubectl -n <namespace> get events --sort-by=.metadata.creationTimestamp",
				},
				{
					Question: "Can the operator reach the instances it created?",
					Checks:   []string{"cnpg-status-unreachable", "cnpg-liveness-isolation", "cnpg-no-system-id", "cnpg-not-ready"},
					Explain: "A cluster stuck creating a replica with an instance status " +
						"extraction error is networking: the operator cannot reach port " +
						"8000 on the pod.",
					Yourself: "kubectl -n cnpg-system logs deployment/cnpg-controller-manager | grep 'Cannot extract Pod status'",
				},
				{
					Question: "Is the data directory itself usable?",
					Checks:   []string{"cnpg-pvc-unusable", "cnpg-pvc-dangling", "cnpg-pg-control-lost", "cnpg-pg-rewind-failed"},
					Explain: "A claim missing its WAL sibling, or a pg_control that is empty, " +
						"is an instance that has to be recreated from the primary.",
				},
			},
		},
		{
			ID:        "replica-behind",
			Symptom:   "A replica is behind or not replicating",
			Describes: "a standby lags, is out of the read service, or is on the wrong timeline",
			Upstream:  upstreamTroubleshooting,
			Steps: []Step{
				{
					Question: "Is the replica streaming at all?",
					Checks:   []string{"cnpg-replica-not-streaming", "cnpg-replica-not-receiving", "cnpg-replica-lagging", "cnpg-replication-lag-high"},
					Explain: "A replica with no WAL receiver is not replicating; one whose lag " +
						"holds past the threshold is not keeping up.",
					Yourself: "kubectl cnpg status <name> --verbose",
				},
				{
					Question: "Is a replication slot or its synchronisation the problem?",
					Checks:   []string{"cnpg-slot-retaining-wal", "cnpg-slot-sync-failing"},
					Explain: "A slot that holds WAL back is a consumer that stopped; slot " +
						"synchronisation failing means a failover cannot resume replication cleanly.",
				},
				{
					Question: "Did the replica fail to rejoin after a promotion?",
					Checks:   []string{"cnpg-pg-rewind-failed", "cnpg-timeline-divergence", "cnpg-pg-control-lost", "cnpg-wal-restore-failed"},
					Explain: "A former primary that cannot rewind, or an instance still on the " +
						"old timeline, is serving reads from before the promotion.",
				},
				{
					Question: "Is the instance deliberately stopped, or crashing?",
					Checks:   []string{"cnpg-instance-fenced", "k8s-container-crashloop", "k8s-container-oom", "cnpg-postgres-exited", "cnpg-instance-failed"},
					Explain:  "A fenced or crash-looping replica replicates nothing while it is down.",
				},
			},
		},
		{
			ID:        "operation-stuck",
			Symptom:   "A switchover, failover or upgrade is stuck",
			Describes: "the cluster has been between states for too long",
			Upstream:  upstreamTroubleshooting,
			Steps: []Step{
				{
					Question: "Is a primary move in flight and not finishing?",
					Checks: []string{
						"cnpg-primary-move-stuck", "cnpg-primary-failing", "cnpg-primary-lease-expired",
						"cnpg-lease-not-acquired", "cnpg-lease-preempted", "cnpg-primary-lease-conflict", "cnpg-primary-status-check",
					},
					Explain: "The operator's current and target primaries disagree, and the " +
						"promotion-side checks say which side is wedged.",
					Yourself: "kubectl -n <namespace> get lease <name> -o yaml",
				},
				{
					Question: "Is the operator waiting for a person?",
					Checks:   []string{"cnpg-waiting-for-user", "cnpg-switchover-required", "cnpg-upgrade-delayed"},
					Explain: "A supervised update strategy parks the cluster until someone " +
						"issues the switchover; nothing is broken and nothing resumes on its own.",
					Yourself: "kubectl cnpg promote <name> <instance>",
				},
				{
					Question: "Is a rollout or upgrade not finishing?",
					Checks: []string{
						"cnpg-rollout-stuck", "cnpg-major-upgrade-stuck", "cnpg-arch-binary-missing",
						"cnpg-manager-upgrade-failed", "cnpg-not-ready", "cnpg-instances-short",
					},
					Explain: "A rollout waits on one instance at a time; the phase reason names " +
						"the one it is waiting for, and the pod-level checks say why.",
				},
				{
					Question: "Does the quorum forbid the failover?",
					Checks:   []string{"cnpg-quorum-standbys-short", "cnpg-sync-replicas-short"},
					Explain: "With quorum failover, the operator refuses to promote a standby " +
						"the quorum never saw. The missing standbys are the problem.",
				},
				{
					Question: "Is a replica-cluster promotion or demotion in progress?",
					Checks:   []string{"cnpg-promotion-stuck", "cnpg-replica-switch-stuck", "cnpg-demotion-fencing"},
					Explain: "A promotion with a token waits for the replica to replay to the " +
						"token's point; a demotion fences everything first.",
				},
			},
		},
		{
			ID:        "operator-silent",
			Symptom:   "The operator seems to be doing nothing",
			Describes: "a change to the Cluster has no effect, or the status stops moving",
			Upstream:  upstreamTroubleshooting,
			Steps: []Step{
				{
					Question: "Has the operator been told to wait, or to stop?",
					Checks:   []string{"cnpg-hibernated", "cnpg-waiting-for-user", "cnpg-upgrade-delayed", "cnpg-hibernation-blocked"},
					Explain: "Hibernation, a supervised strategy and a rollout delay are the " +
						"operator waiting on purpose.",
					Yourself: "kubectl -n <namespace> get cluster <name> -o jsonpath='{.metadata.annotations}'",
				},
				{
					Question: "Has reconciliation stopped on a plugin or the definition?",
					Checks:   []string{"cnpg-unknown-plugin", "cnpg-plugin-failure", "cnpg-invalid-definition", "cnpg-image-catalog-unusable"},
					Explain: "These are checked before anything else on every reconcile; while " +
						"one holds, no failover, rollout or backup happens.",
				},
				{
					Question: "Can the operator reach the instances?",
					Checks:   []string{"cnpg-status-unreachable", "cnpg-liveness-isolation", "cnpg-no-system-id"},
					Explain:  "An operator that cannot read instance status makes no decision at all.",
					Yourself: "kubectl -n cnpg-system logs deployment/cnpg-controller-manager --all-containers=true",
				},
				{
					Question: "Is something rewriting the definition under the operator?",
					Checks:   []string{"k8s-definition-rewritten-repeatedly", "cnpg-scale-down-refused", "cnpg-pvc-resizing-stuck"},
					Explain: "Two controllers alternating on one object is each undoing the " +
						"other; the field managers the API server attributes the writes to say who.",
				},
				{
					Question: "Has the operator stopped renewing what it manages?",
					Checks:   []string{"cnpg-certificate-expired", "cnpg-certificate-expiring", "cnpg-not-ready", "cnpg-rollout-stuck"},
					Explain: "A certificate the operator issued and let expire is an operator " +
						"that has not reconciled this cluster in days.",
				},
				{
					Question: "Is the operator itself running?",
					Explain: "The operator lives in its own namespace, outside this console's " +
						"authority. Its pod, its leader election and its webhook are read there.",
					Yourself: "kubectl -n cnpg-system get pods && kubectl -n cnpg-system describe deployment cnpg-controller-manager",
				},
			},
		},
	}
}
