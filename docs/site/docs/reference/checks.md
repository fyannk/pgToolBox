---
sidebar_position: 4
title: Checks and playbooks
---

# Checks and playbooks

This page is generated from the diagnostics catalog by `make catalog-docs`
and kept equal to it by a test. It lists every check the console runs,
grouped by the layer of the stack it looks at, and every triage
playbook with the checks behind each of its steps. What a check
*means* — its four outcomes, how findings relate, what each kind of
evidence needs — is in the [diagnostics guide](../guides/diagnostics.md).

A check's **applies to** column is its version pin: on an observed
version outside it the check answers "does not apply". A check with
none applies everywhere. **Follows from** lists the checks the catalog
relates this one to as consequences of, with the relation's scope and
strength where they differ from *same cluster, established*.

162 checks: 149 catalog rules and 13 hand-written detectors.

## Kubernetes

| Check | Severity | Looks for | Finding | Applies to | Follows from |
|---|---|---|---|---|---|
| `cnpg-cannot-create-objects` | critical | the operator failing to create the cluster's auxiliary objects | The operator cannot create objects this cluster needs. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-pvc-dangling` | warning | a volume claim with no pod, listed by the operator for a quarter of an hour | A volume claim has had no pod for a quarter of an hour: the instance it belongs to is not being recreated. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-pvc-initializing-stuck` | warning | a volume claim whose creating job has produced no pod in half an hour | A volume claim has been initializing for half an hour: the job creating its instance has not finished. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-pvc-resizing-stuck` | warning | a volume claim carrying the resize condition for half an hour | A volume claim has been resizing for half an hour: the expansion is not completing. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-pvc-unusable` | high | a volume claim the operator lists as unusable | The operator lists a volume claim as unusable: it belongs to an incomplete set. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-service-account-missing` | critical | a specified ServiceAccount that does not exist | The ServiceAccount named in the spec does not exist, so pods cannot be created. | `CloudNativePG >=1.30 <1.31` |  |
| `image-pull` | by finding | a container whose image the kubelet cannot pull | *(hand-written detector; see the guide)* | every version | |
| `k8s-container-config-error` | critical | a container the kubelet cannot construct | A container cannot be created, so its pod is stuck before starting. | every version |  |
| `k8s-container-crashloop` | critical | a container in CrashLoopBackOff | A container is crash-looping: it keeps exiting and the kubelet is backing off restarting it. | every version | `cnpg-wal-disk-full` (same pod), `cnpg-postgres-exited` (same pod), `k8s-container-oom` (same pod) |
| `k8s-container-oom` | high | a container the kernel killed for exceeding its memory limit | A container was killed for exceeding its memory limit. | every version |  |
| `k8s-definition-rewritten-repeatedly` | warning | an object whose definition is rewritten again and again inside an hour | An object's definition is being rewritten again and again. | every version |  |
| `k8s-eol-soon` | note | a Kubernetes version leaving upstream support within 90 days | The Kubernetes server version leaves upstream support soon. | every version |  |
| `k8s-eol` | warning | a Kubernetes version past upstream end of life | The Kubernetes server version no longer receives upstream patches. | every version |  |
| `k8s-pod-evicted` | warning | a member pod evicted from its node | A member pod was evicted from its node. | every version |  |
| `k8s-pod-replaced-repeatedly` | warning | a pod replaced several times inside an hour | A pod has been replaced several times in the last hour. | every version |  |
| `k8s-volume-mount-failed` | critical | a pod that cannot mount or attach one of its volumes | A member pod cannot mount or attach a volume, so it cannot start. | every version |  |
| `pod-scheduling` | by finding | a pod the scheduler cannot place on any node | *(hand-written detector; see the guide)* | every version | |
| `quota-exhausted` | by finding | a namespace quota whose usage has reached its ceiling | *(hand-written detector; see the guide)* | every version | |
| `resource-quota` | by finding | an object the API server refused to create against a namespace quota | *(hand-written detector; see the guide)* | every version | |
| `volume-binding` | by finding | a persistent volume claim that has not bound | *(hand-written detector; see the guide)* | every version | |
| `volume-full-forecast` | by finding | a volume whose trend reaches full within two weeks, in bytes or inodes | *(hand-written detector; see the guide)* | every version | |

## Operator

| Check | Severity | Looks for | Finding | Applies to | Follows from |
|---|---|---|---|---|---|
| `cnpg-arch-binary-missing` | warning | an online instance-manager upgrade blocked by a missing architecture binary | The operator image carries no instance-manager binary for this node architecture, so the online upgrade cannot run. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-bootstrap-backup-missing` | critical | a bootstrap recovery pointing at a Backup that does not exist | Bootstrap-from-recovery references a Backup object that is missing, so the primary is never created. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-bootstrap-stuck` | critical | the operator creating the primary or a replica for half an hour | The operator has been creating an instance for half an hour, and the instance has not come up. | `CloudNativePG >=1.30 <1.31` | `cnpg-initdb-failed`, `cnpg-restore-failed`, `cnpg-join-failed`, `wal-archive-not-empty`, `cnpg-recovery-target-missing`, `cnpg-bootstrap-backup-missing`, `cnpg-status-unreachable`, `pod-scheduling` (plausible), `image-pull` (plausible), `quota-exhausted` (plausible) |
| `cnpg-ca-expiring` | warning | a user-supplied CA approaching expiry | A user-supplied CA certificate is close to expiring. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-ca-secret-unusable` | high | a referenced CA secret that is missing or unparseable | A CA secret this cluster references is missing or malformed, and PKI reconciliation is stopped. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-certificate-expired` | critical | a certificate the operator reports as already expired | A certificate this cluster uses has expired. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-certificate-expiring` | high | a certificate the operator reports as expiring within three days | A certificate this cluster uses expires within three days and has not been renewed. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-eol` | warning | a CloudNativePG release past its end of life | The CloudNativePG operator release is no longer supported upstream. | every version |  |
| `cnpg-hibernated` | note | the operator reporting the cluster as hibernated | The cluster is hibernated: every pod is deleted on purpose and nothing serves. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-hibernation-blocked` | warning | a requested hibernation waiting on cluster health | Hibernation was requested but will not start while the cluster is unhealthy. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-hibernation-stuck` | warning | hibernation waiting on pod deletion for a quarter of an hour | Hibernation has been deleting the cluster's pods for a quarter of an hour, and they are not gone. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-image-catalog-lacks-major` | high | a referenced image catalog with no image for the Cluster's PostgreSQL major | The referenced image catalog offers no image for the PostgreSQL major the Cluster runs. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-image-catalog-missing` | high | a Cluster whose imageCatalogRef names a catalog that does not exist | The Cluster references an image catalog that does not exist. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-image-catalog-unusable` | high | an image catalog the operator cannot resolve an image from | The operator cannot pick a container image from the referenced catalog. | `CloudNativePG >=1.30 <1.31` | `cnpg-image-catalog-missing`, `cnpg-image-catalog-lacks-major` |
| `cnpg-initdb-failed` | critical | initdb bootstrap failing | The initdb bootstrap failed, so the cluster never gets its primary. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-instance-failed` | high | an instance the operator lists as failed | The operator lists an instance as failed: its pod will not be scheduled again. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-instances-short` | high | fewer ready instances than declared, by the operator's own count, for ten minutes | Fewer instances are ready than the cluster declares, and have been for ten minutes. | `CloudNativePG >=1.30 <1.31` | `quota-exhausted` (plausible), `pod-scheduling` (plausible), `image-pull` (plausible), `k8s-container-crashloop` (plausible), `cnpg-instance-fenced` (plausible), `cnpg-instance-failed` (plausible) |
| `cnpg-invalid-definition` | critical | a cluster definition the operator's validation rejected | The cluster definition is invalid, so the operator is not reconciling it. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-join-failed` | high | a new replica failing to join the cluster | A new replica cannot join: its clone from the primary is failing. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-liveness-isolation` | critical | an instance concluding it is network-isolated | An instance cannot reach the API server or its peers and is letting the liveness probe kill it. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-major-upgrade-stuck` | warning | a PostgreSQL major upgrade running for two hours | A PostgreSQL major upgrade has been running for two hours. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-managed-role-unreconcilable` | warning | a managed role the operator reports it cannot reconcile | The operator cannot apply a managed role, and quotes PostgreSQL's refusal. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-manager-upgrade-failed` | warning | an in-place instance-manager upgrade that failed | The in-place instance-manager upgrade failed, so pods are still running the old binary. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-manager-version-drift` | note | instances reporting more than one instance-manager version | The instances do not all run the same instance manager: a rollout or in-place upgrade did not reach every one. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-no-system-id` | warning | no instance reporting a system identifier for a quarter of an hour | No instance has reported a PostgreSQL system identifier to the operator for a quarter of an hour. | `CloudNativePG >=1.30 <1.31` | `cnpg-status-unreachable`, `cnpg-bootstrap-stuck` |
| `cnpg-not-ready` | warning | the operator reporting the cluster not ready for ten minutes | The operator has reported the cluster as not ready for ten minutes. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-plugin-failure` | critical | a CNPG-I plugin interaction failing during reconciliation | The operator cannot talk to a plugin this cluster requires, and reconciliation is stopped. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-primary-lease-conflict` | critical | a primary lease owned by something else | The primary lease is controlled by another owner, and the operator refuses to adopt it. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-primary-status-check` | critical | a Ready primary whose status check the operator cannot pass | The primary looks Ready but the operator's own status check on it fails, and failover is deliberately deferred. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-recovery-target-missing` | critical | a recovery target matching no backup in the catalog | No backup in the catalog matches the requested recovery target. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-restore-failed` | critical | a recovery bootstrap failing | The restore this cluster is bootstrapping from failed. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-rollout-stuck` | warning | a rollout or configuration phase held for half an hour | The operator has been rolling the cluster out for half an hour, and the rollout has not finished. | `CloudNativePG >=1.30 <1.31` | `cnpg-postgres-start-failed`, `cnpg-postgres-exited`, `cnpg-replica-not-streaming`, `cnpg-instance-fenced`, `k8s-container-crashloop` (plausible), `pod-scheduling` (plausible), `image-pull` (plausible) |
| `cnpg-scale-down-refused` | warning | a scale-down the operator reverted | The requested instance count conflicts with maxSyncReplicas, and the operator reverted it. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-status-unreachable` | critical | the operator unable to reach any ready instance's status endpoint | The operator cannot read status from the ready instances, so it can make no decision at all. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-switchover-required` | warning | an instance reporting that a manual switchover is required | An instance reports that a manual switchover is required before pending work resumes. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-system-id-mismatch` | critical | instances reporting different PostgreSQL system identifiers | The instances do not belong to the same PostgreSQL cluster. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-tablespace-error` | warning | a declared tablespace the operator reports an error for | The operator cannot reconcile a declared tablespace, and quotes the error. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-unknown-plugin` | critical | a required CNPG-I plugin the operator cannot find | A plugin this cluster requires is not loaded, and reconciliation is fully stopped. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-unrecoverable` | critical | the operator declaring the cluster unrecoverable | The operator has declared the cluster unrecoverable: it will not repair this on its own. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-upgrade-delayed` | note | an upgrade the operator is configured to delay | An upgrade is pending but the operator is configured to delay it. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-waiting-for-user` | warning | the operator waiting for a supervised switchover | The operator is waiting for a manual switchover and will not proceed on its own. | `CloudNativePG >=1.30 <1.31` |  |
| `volume-autogrow-stopped` | by finding | the operator stopped growing a volume, or the storage refused an expansion | *(hand-written detector; see the guide)* | every version | |

## PostgreSQL

| Check | Severity | Looks for | Finding | Applies to | Follows from |
|---|---|---|---|---|---|
| `cnpg-instance-fenced` | warning | an instance the operator has fenced | An instance is fenced: PostgreSQL is deliberately stopped there and the operator will not restart it. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-instance-might-be-unavailable` | warning | an instance manager doubting its own PostgreSQL is reachable | An instance manager reports that its PostgreSQL may be unavailable, and quotes the error it saw. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-pending-restart` | warning | an instance reporting a configuration change waiting for a restart | An instance has a configuration change that takes effect only after a restart, and has not been restarted. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-pg-control-lost` | critical | a zero-length pg_control with no surviving backup copy | An instance's pg_control file is empty and no backup copy of it survives: that data directory is unusable. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-pg-rewind-failed` | high | pg_rewind failing on a demoted primary | A former primary cannot rewind to rejoin the cluster. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-postgres-exited` | critical | the postmaster exiting with errors | PostgreSQL exited with errors — this is what a crash-looping instance looks like from inside. | `CloudNativePG >=1.30 <1.31` | `postgres-panic` (same pod, within 1h0m0s), `cnpg-wal-disk-full` (same pod) |
| `cnpg-postgres-start-failed` | critical | the postmaster failing to launch | PostgreSQL failed to start on an instance. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-wal-disk-full` | critical | the instance manager refusing to start PostgreSQL for lack of WAL space | An instance has no free disk space for WAL, and PostgreSQL is being kept down there. | `CloudNativePG >=1.30 <1.31` | `cnpg-wal-archiving-failing`, `cnpg-slot-retaining-wal` |
| `cnpg-wal-disk-space-phase` | critical | the operator refusing to run PostgreSQL for lack of WAL disk space | One or more instances have no disk space left for WAL, and PostgreSQL is being kept down. | `CloudNativePG >=1.30 <1.31` | `cnpg-wal-disk-full`, `cnpg-wal-archiving-failing` |
| `insight-connection-saturation` | by finding | an instance with more than 80% of the connection slots ordinary roles may take in use by clients (insight pack) | *(hand-written detector; see the guide)* | every version | |
| `insight-idle-in-transaction` | by finding | a session idle inside an open transaction for longer than ten minutes (insight pack) | *(hand-written detector; see the guide)* | every version | |
| `insight-wraparound` | by finding | a database whose transaction id or multixact age is past its anti-wraparound threshold or near the stop limit (insight pack) | *(hand-written detector; see the guide)* | every version | |
| `pg-effective-cache-size-over-limit` | note | effective_cache_size above the postgres container's memory limit | effective_cache_size promises the planner more cache than the container can hold. | every version |  |
| `pg-huge-pages-missing` | high | huge_pages = on without a hugepages-* reservation large enough for shared_buffers + wal_buffers | huge_pages is on, but the container does not reserve enough huge pages: the next start of an instance fails. | every version |  |
| `pg-huge-pages-unused` | note | a hugepages-* reservation PostgreSQL will not use (huge_pages off, or try with too small a reservation) | The container reserves huge pages that PostgreSQL will not use. | every version |  |
| `pg-memory-over-limit` | warning | worst-case PostgreSQL memory (shared_buffers + wal_buffers + max_connections × work_mem + autovacuum_max_workers × autovacuum memory) above the postgres container's memory limit | PostgreSQL's worst-case memory exceeds the container's memory limit: the instance risks an OOM kill. | every version |  |
| `pg-memory-parameter-unparsable` | warning | a memory parameter in spec.postgresql.parameters that does not parse as PostgreSQL reads it | A declared memory parameter is not a value PostgreSQL accepts. | every version |  |
| `pg-shared-buffers-over-limit` | high | shared_buffers + wal_buffers at or above the postgres container's memory limit | shared_buffers alone does not fit the container's memory limit. | every version |  |
| `postgres-backends-waiting` | high | at least 300 backends waiting on a lock, held for five minutes | Hundreds of backends have been waiting on locks for five minutes: something holds a lock everything else needs. | `CloudNativePG >=1.30 <1.31` | `postgres-long-transaction` (plausible) |
| `postgres-deadlocks-ongoing` | warning | deadlocks at a rate of one a minute or more, held for a quarter of an hour | PostgreSQL has been breaking deadlocks continuously for a quarter of an hour. | `CloudNativePG >=1.30 <1.31` |  |
| `postgres-eol-soon` | note | a PostgreSQL major version leaving upstream support within 180 days | The PostgreSQL major version leaves upstream support soon. | every version |  |
| `postgres-eol` | warning | a PostgreSQL major version past upstream end of life | The PostgreSQL major version no longer receives upstream releases. | every version |  |
| `postgres-extension-update-available` | note | an installed extension older than the version its image ships | An installed extension is older than the version the image ships, and has not been updated. | `CloudNativePG >=1.30 <1.31` |  |
| `postgres-fatal` | warning | a server log record with FATAL severity | PostgreSQL logged a FATAL-severity record. | `CloudNativePG >=1.30 <1.31` |  |
| `postgres-long-transaction` | warning | an instance with a transaction open past the threshold | A transaction has been open past the threshold across the retained window. | `CloudNativePG >=1.30 <1.31` |  |
| `postgres-mxid-wraparound` | critical | a database's multixact-id age near wraparound | A database's multixact-id age is approaching wraparound, which ends in PostgreSQL refusing writes. | `CloudNativePG >=1.30 <1.31` | `postgres-long-transaction` |
| `postgres-panic` | critical | a server log record with PANIC severity | PostgreSQL panicked, which ends the whole server process. | `CloudNativePG >=1.30 <1.31` | `cnpg-wal-disk-full` (same pod, within 1h0m0s) |
| `postgres-xid-wraparound` | critical | a database's transaction-id age near wraparound | A database's transaction-id age is approaching wraparound, which ends in PostgreSQL refusing writes. | `CloudNativePG >=1.30 <1.31` | `postgres-long-transaction` |

## Replication

| Check | Severity | Looks for | Finding | Applies to | Follows from |
|---|---|---|---|---|---|
| `cnpg-demotion-fencing` | critical | every instance fenced for a replica-cluster transition | All instances are fenced for a demotion to replica cluster: PostgreSQL is not serving anywhere. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-lease-not-acquired` | high | a promotion stalled waiting for the primary lease | The instance being promoted cannot acquire the primary lease, so the cluster has no writable primary yet. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-lease-preempted` | critical | a primary shut down because another instance took its lease | A primary shut itself down because another instance now holds the primary lease. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-primary-disagreement` | critical | an instance whose own recovery state contradicts the operator's current primary | An instance and the operator disagree about which instance is the primary. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-primary-failing` | critical | the operator reporting the current primary as failing for over a minute | The operator has reported the current primary as failing for over a minute, and has not failed over. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-primary-lease-expired` | critical | the primary lease unrenewed for a minute past its duration, or released with a primary named | The primary lease is not being renewed: the instance holding the primary role has stopped keeping it. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-primary-lease-holder-mismatch` | critical | the primary lease held by an instance other than the operator's current primary | The instance holding the primary lease is not the one the operator names as primary. | `CloudNativePG >=1.30 <1.31` | `cnpg-primary-disagreement` (plausible) |
| `cnpg-primary-move-stuck` | critical | a switchover or failover still unfinished after ten minutes | A primary move has been in flight for over ten minutes: the cluster is between primaries and stuck there. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-promotion-stuck` | critical | a replica cluster's promotion running for a quarter of an hour | The cluster has been promoting itself from replica to primary for a quarter of an hour. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-quorum-standbys-short` | critical | a failover quorum with fewer potentially synchronous standbys than transactions wait for | Fewer standbys are potentially synchronous than transactions wait for. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-replay-paused` | warning | a replica reporting its WAL replay paused | A replica's WAL replay is paused: it receives WAL and applies none of it. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-replica-lagging` | warning | a replica exceeding the configured maximum lag | A replica exceeds the configured maximum lag and has been taken out of read traffic. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-replica-not-receiving` | high | a replica in recovery with no WAL receiver and a lag that is not closing | A replica has stopped streaming and is not catching up. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-replica-not-streaming` | high | the streaming readiness probe finding no replication connection | A replica is not connected via streaming replication, and the readiness probe is holding it out of service. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-replica-switch-stuck` | warning | a switch to replica cluster in progress for a quarter of an hour | The cluster has been switching to a replica cluster for a quarter of an hour: the demotion is not completing. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-replication-lag-high` | warning | a replica whose lag has stayed past the threshold | A replica's lag has stayed past the threshold across the retained window. | `CloudNativePG >=1.30 <1.31` | `cnpg-replica-not-receiving` (same pod) |
| `cnpg-replication-slot-inactive` | warning | an inactive replication slot the operator does not manage | A replication slot the operator does not manage is inactive: its consumer is gone, and the slot keeps WAL for it. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-slot-retaining-wal` | warning | a replication slot holding back WAL past the threshold | A replication slot is holding back more WAL than the threshold, and not releasing it. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-slot-sync-failing` | warning | replication slot synchronization failing on a replica | Replication slot synchronization is failing, so a failover may not be able to resume replication cleanly. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-sync-replicas-short` | critical | an instance reporting fewer synchronous replicas than it expects | An instance has fewer synchronous replicas than it expects. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-timeline-divergence` | critical | an instance reporting a PostgreSQL timeline other than the cluster's for ten minutes | An instance has reported a PostgreSQL timeline other than the cluster's for ten minutes: it is not following the current primary. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-wal-restore-failed` | high | the restore_command wrapper failing for reasons other than a missing WAL | WAL restore from the archive is failing, so recovery on this instance cannot advance. | `CloudNativePG >=1.30 <1.31` |  |

## Backups and archive

| Check | Severity | Looks for | Finding | Applies to | Follows from |
|---|---|---|---|---|---|
| `backup-cadence` | by finding | a backup schedule that runs far more often than its author is likely to have intended | *(hand-written detector; see the guide)* | every version | |
| `backup-destination-conflict` | high | a backup destination already holding another cluster's data | The backup destination already holds data for this server name, so the operator refused to write into it. | every version |  |
| `barman-last-backup-failed` | warning | the barman-cloud plugin reporting the last backup as failed | The barman-cloud plugin reports this cluster's most recent backup as failed. | every version |  |
| `barman-no-successful-backup` | high | the barman-cloud plugin reporting no successful backup for a cluster scheduled for a day | The object store holds no successful backup of this cluster, though a backup schedule has existed for a day. | every version |  |
| `barman-plugin-eol` | warning | a Barman Cloud plugin release past its end of life | The Barman Cloud plugin release is no longer supported upstream. | every version |  |
| `cnpg-archiver-failing` | high | PostgreSQL counting archive failures continuously for a quarter of an hour | PostgreSQL has been counting archive-command failures continuously for a quarter of an hour. | `CloudNativePG >=1.30 <1.31` | `cnpg-wal-archiving-failing` |
| `cnpg-backup-error` | warning | a backup the operator recorded as exiting with an error | A backup exited with an error, and the operator quotes it. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-backup-failed` | warning | a Backup object reporting the failed phase | A backup failed and will not be retried. | `CloudNativePG >=1.30 <1.31` | `cnpg-backup-error`, `cnpg-backup-manager-restarted` |
| `cnpg-backup-manager-restarted` | warning | a backup the operator failed because the instance manager restarted under it | A backup failed because the instance manager restarted on its target while it ran. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-backup-plugin-missing` | high | a backup targeting a plugin absent from the pod | A backup targets a plugin that is not available in the instance pod, so backups quietly never succeed. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-backup-stop-blocked` | warning | pg_backup_stop failing at the end of a physical backup | A backup cannot finish: stopping the physical backup failed, most often because it is waiting on WAL archiving. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-backup-stuck-pending` | warning | a Backup pending for over half an hour | A backup has been pending for over half an hour, which means the operator cannot start it. | `CloudNativePG >=1.30 <1.31` | `cnpg-backup-waiting-for-target`, `cnpg-backup-target-unhealthy` |
| `cnpg-backup-target-unhealthy` | warning | the operator refusing to run a backup on an unhealthy target instance | A backup cannot run because the instance it targets is not healthy, and the operator says how. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-backup-waiting-for-target` | warning | a backup waiting because its target instance is not ready or not found | A backup is waiting for its target instance to become ready. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-backup-wal-archiving` | high | a Backup blocked by failing WAL archiving | A backup is blocked because WAL archiving is not working. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-instance-archive-failing` | high | an instance whose last archive failure is more recent than its last success | An instance's last WAL archive attempt failed, more recently than its last success. | `CloudNativePG >=1.30 <1.31` | `cnpg-wal-archiving-failing` |
| `cnpg-last-backup-failed` | warning | the last backup having failed | The most recent backup failed, and the quoted message says why. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-retention-failed` | warning | a backup retention policy that failed to prune | The backup retention policy failed, so the object store keeps growing. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-schedule-adoption-refused` | warning | a schedule skipping a run because a Backup of that name is not its own | A backup schedule skipped a run: a Backup with the name it would use exists and is not owned by it. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-schedule-cluster-unhealthy` | warning | a schedule the operator holds back because the cluster is not healthy | A backup schedule is not firing because the operator is waiting for the cluster to be healthy. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-schedule-creation-failed` | warning | a schedule the operator could not create a Backup object for | A backup schedule could not create its Backup object. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-schedule-invalid` | high | a schedule expression no time satisfies | A backup schedule's expression matches no time at all, so it never fires. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-schedule-not-firing` | high | a backup schedule that has stopped firing | A backup schedule's next run is long past, so scheduling has stopped. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-schedule-suspended` | warning | a suspended backup schedule | A backup schedule is suspended, so it produces no recovery points. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-wal-archive-backlog` | high | WAL segments waiting to be archived, at least 32 of them, held for a quarter of an hour | WAL segments are piling up unarchived on an instance: at least 32 have waited a quarter of an hour. | `CloudNativePG >=1.30 <1.31` | `cnpg-wal-archiving-failing`, `cnpg-wal-archive-command-failed` |
| `cnpg-wal-archive-command-failed` | high | the archive_command wrapper failing | The WAL archive command is failing, so WAL is accumulating on the instance. | `CloudNativePG >=1.30 <1.31` | `cnpg-wal-archiving-failing` |
| `cnpg-wal-archive-plugin-missing` | high | a WAL-archive plugin whose socket is absent from the pod | The configured WAL-archive plugin is not available in this pod, so nothing is being archived. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-wal-archiving-failing` | high | the instance manager reporting continuous archiving as failing | WAL archiving is failing, so WAL is accumulating and no new recovery points are being made. | `CloudNativePG >=1.30 <1.31` | `wal-archive-not-empty`, `object-store-denied`, `object-store-forbidden`, `object-store-unreachable`, `backup-destination-conflict`, `cnpg-wal-archive-plugin-missing` |
| `cnpg-wal-files-waiting` | high | an instance counting at least 32 WAL segments waiting to be archived | WAL segments are piling up unarchived on an instance, by its own count. | `CloudNativePG >=1.30 <1.31` | `cnpg-wal-archiving-failing`, `cnpg-instance-archive-failing` |
| `object-store-denied` | high | the object store refusing the configured credentials | The object store refused the operator's credentials for the configured destination. | every version |  |
| `object-store-forbidden` | high | the object store answering 403 Forbidden | The object store answered 403 Forbidden, so the configured credentials do not grant this destination. | every version |  |
| `object-store-unreachable` | high | an unreachable object store endpoint | The operator could not reach the configured object store endpoint. | every version |  |
| `repository-coverage-unhealthy` | high | the evidence agent reporting recovery coverage as unhealthy | The evidence agent reports the observed recovery coverage as unhealthy. | every version | `repository-wal-unhealthy`, `cnpg-wal-archiving-failing` |
| `repository-retention-not-applied` | warning | the evidence agent raising the RetentionNotApplied finding | The pgBackRest stanza holds backups or WAL its retention would have expired. | every version |  |
| `repository-retention-not-configured` | note | the evidence agent raising the RetentionNotConfigured finding | No full backup retention is set for the pgBackRest repository: nothing is ever expired. | every version |  |
| `repository-retention-unhealthy` | warning | the evidence agent reporting retention as unhealthy | The evidence agent reports retention as unhealthy against the configured expectation. | every version |  |
| `repository-stanza-split` | warning | the evidence agent raising the StanzaSplit finding | The pgBackRest backups and their WAL sit under two different stanzas. | every version |  |
| `repository-wal-unhealthy` | high | the evidence agent reporting WAL continuity as unhealthy | The evidence agent reports the archived WAL sequence as unhealthy. | every version | `cnpg-wal-archiving-failing` |
| `restore-drill-overdue` | by finding | a Cluster this console can drill with no passed restore drill in its record for 90 days | *(hand-written detector; see the guide)* | every version | |
| `wal-archive-not-empty` | high | the archiver refusing a WAL archive that is not empty | The configured WAL archive is not empty, so the operator refused to start archiving into it. | every version |  |
| `wal-growing-archiving-failing` | by finding | WAL piling up on the primary's volume while archiving fails | *(hand-written detector; see the guide)* | every version | |

## Poolers

| Check | Severity | Looks for | Finding | Applies to | Follows from |
|---|---|---|---|---|---|
| `cnpg-pooler-clients-waiting` | warning | a pooler instance whose oldest queued client has waited at least five seconds | Clients are queueing at a pooler: the oldest has waited at least five seconds for a server connection. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-pooler-failed` | critical | a Pooler the operator reports as failed | A Pooler has failed: the operator cannot reconcile it, and quotes why. | `CloudNativePG >=1.30 <1.31` | `cnpg-pooler-image-catalog-error` |
| `cnpg-pooler-image-catalog-error` | warning | a Pooler whose image the operator cannot resolve from its catalog | The operator cannot resolve a Pooler's image from the catalog it names. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-pooler-inactive` | warning | a Pooler the operator reports as inactive | A Pooler is inactive: something it needs does not exist yet. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-pooler-ownership-invalid` | warning | a Pooler whose managed resources the operator found owned by something else | A Pooler's managed resources are owned by something other than the Pooler, and the operator will not touch them. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-pooler-paused` | note | a Pooler the operator reports as paused | A Pooler is paused on purpose: it runs no instances and answers no client. | `CloudNativePG >=1.30 <1.31` |  |
| `cnpg-pooler-short` | warning | a Pooler with fewer ready instances than declared | A Pooler has fewer ready instances than it declares. | `CloudNativePG >=1.30 <1.31` |  |

## Declared objects

| Check | Severity | Looks for | Finding | Applies to | Follows from |
|---|---|---|---|---|---|
| `cnpg-declared-object-failed` | warning | a declared database object the operator cannot apply | A declared database object cannot be applied. | `CloudNativePG >=1.30 <1.31` |  |

## Declined upstream signals

Signals the verified operator releases can write that no check listens for, each with the reason. The coverage verification fails on a signal that is neither listened for nor listed here.

| Signal | Why no check |
|---|---|
| `BackupStarted` | a backup in progress is not a finding; cnpg-backup-stuck-pending reads the Backup's own phase and age |
| `BootstrapCompleted` | success |
| `BootstrapPending` | cnpg-bootstrap-stuck reads the phase and how long it has held, which the condition does not carry |
| `ClientSecretNotFound` | recorded on the plugin's Service in the operator's namespace |
| `Cluster in healthy state` | the healthy phase is the absence of a finding; the sidebar has no zero state for the same reason |
| `ClusterIsReady` | success; cnpg-not-ready reads the condition's False status |
| `ContinuousArchivingFailing` | cnpg-wal-archiving-failing reads the condition's False status, whose only reason this is |
| `ContinuousArchivingSuccess` | success |
| `DiscoverImage` | the same failure sets the image-catalog phase, which cnpg-image-catalog-unusable reads with the catalog checks beneath it |
| `Failing over` | read through currentPrimary and targetPrimary by cnpg-primary-move-stuck, which also knows how long the move has taken |
| `FinalizerRemovalFailed` | recorded on the plugin's Service in the operator's namespace |
| `FindingCluster` | a Backup naming a Cluster the operator cannot find is not a member of this cluster's catalog, so its events are never attributed here |
| `InvalidClientCertificate` | recorded on the plugin's Service in the operator's namespace |
| `InvalidPortAnnotation` | recorded on the plugin's Service in the operator's namespace |
| `InvalidServerCertificate` | recorded on the plugin's Service in the operator's namespace |
| `PluginRegistrationFailed` | recorded on the plugin's Service in the operator's namespace |
| `ServerSecretNotFound` | recorded on the plugin's Service in the operator's namespace |
| `Switchover in progress` | read through currentPrimary and targetPrimary by cnpg-primary-move-stuck, which also knows how long the move has taken |
| `active` | success |

## Triage playbooks

Each step is answered by the checks named beside it; a step with no checks is a question for the reader.

### Clients cannot connect

connections to the cluster's services fail or time out.

1. **Is the cluster deliberately down?** — `cnpg-hibernated`, `cnpg-demotion-fencing`, `cnpg-instance-fenced`
2. **Does the Cluster exist, and does the operator name a primary?** — fact: the operator names no current primary — `cnpg-unrecoverable`, `cnpg-invalid-definition`, `cnpg-bootstrap-stuck`, `cnpg-no-system-id` — or run `kubectl -n <namespace> get cluster <name> -o yaml`
3. **Is the primary failing, or moving?** — `cnpg-primary-failing`, `cnpg-primary-move-stuck`, `cnpg-primary-lease-expired`, `cnpg-primary-lease-holder-mismatch`, `cnpg-primary-disagreement`, `cnpg-lease-not-acquired`
4. **Is any instance pod ready at all?** — fact: no instance pod is ready — `cnpg-instances-short`, `k8s-container-crashloop`, `k8s-container-oom`, `pod-scheduling`, `image-pull`, `k8s-container-config-error`, `k8s-volume-mount-failed`, `k8s-pod-evicted`, `cnpg-instance-failed` — or run `kubectl -n <namespace> get pods -l cnpg.io/cluster=<name> -L role -o wide`
5. **Is PostgreSQL itself down on the instances?** — `cnpg-postgres-start-failed`, `cnpg-postgres-exited`, `cnpg-wal-disk-space-phase`, `cnpg-wal-disk-full`, `postgres-panic`, `cnpg-pg-control-lost` — or run `kubectl -n <namespace> logs <pod> -c postgres --previous`
6. **Has a certificate expired?** — `cnpg-certificate-expired`, `cnpg-certificate-expiring`, `cnpg-ca-secret-unusable`
7. **Does the read-write Service exist?** — fact: the cluster has no read-write Service — `cnpg-cannot-create-objects` — or run `kubectl -n <namespace> get svc -l cnpg.io/cluster=<name>`
8. **If clients go through a pooler, is the pooler up?** — `cnpg-pooler-failed`, `cnpg-pooler-inactive`, `cnpg-pooler-short`, `cnpg-pooler-clients-waiting`
9. **Is PostgreSQL refusing the connections it receives?** — `postgres-fatal` — or run `kubectl -n <namespace> logs <pod> -c postgres | jq 'select(.record.error_severity=="FATAL")'`
10. **Is a NetworkPolicy dropping the traffic?** — `cnpg-status-unreachable`, `cnpg-liveness-isolation` — or run `kubectl -n <namespace> get networkpolicies`

### Writes are refused or hang

reads work but writes fail, or commits wait.

1. **Is this cluster a replica, or being demoted to one?** — `cnpg-demotion-fencing`, `cnpg-replica-switch-stuck`, `cnpg-promotion-stuck`
2. **Do the operator and the instances agree on who is primary?** — `cnpg-primary-disagreement`, `cnpg-primary-lease-holder-mismatch`, `cnpg-primary-move-stuck`, `cnpg-timeline-divergence`
3. **Is PostgreSQL protecting itself from wraparound?** — `postgres-xid-wraparound`, `postgres-mxid-wraparound`
4. **Are commits waiting on synchronous standbys that are not there?** — `cnpg-sync-replicas-short`, `cnpg-quorum-standbys-short`, `cnpg-replica-not-streaming`, `cnpg-replica-not-receiving`
5. **Has the WAL volume filled?** — `cnpg-wal-disk-full`, `cnpg-wal-disk-space-phase`, `cnpg-wal-archive-backlog`, `cnpg-wal-archiving-failing`
6. **Is a lock, a long transaction or a deadlock pattern holding writes?** — `postgres-backends-waiting`, `postgres-long-transaction`, `postgres-deadlocks-ongoing` — or run `kubectl cnpg psql <name> -- -c 'select pid, state, wait_event_type, query from pg_stat_activity where wait_event is not null'`

### Backups are not happening

no recent backup, or backups that fail.

1. **Is there a schedule, and is it firing?** — fact: the cluster has no backup schedule — `cnpg-schedule-suspended`, `cnpg-schedule-not-firing`, `cnpg-schedule-invalid`, `cnpg-schedule-cluster-unhealthy`, `cnpg-schedule-adoption-refused`, `cnpg-schedule-creation-failed`, `backup-cadence` — or run `kubectl -n <namespace> get scheduledbackup,backup -l cnpg.io/cluster=<name>`
2. **Is a backup stuck before it starts?** — `cnpg-backup-stuck-pending`, `cnpg-backup-target-unhealthy`, `cnpg-backup-waiting-for-target`
3. **Did the backup fail?** — `cnpg-backup-failed`, `cnpg-backup-error`, `cnpg-backup-manager-restarted`, `cnpg-backup-plugin-missing`, `cnpg-backup-stop-blocked`, `cnpg-last-backup-failed` — or run `kubectl -n <namespace> describe backup <backup>`
4. **Is WAL archiving working?** — `cnpg-wal-archiving-failing`, `cnpg-wal-archive-command-failed`, `cnpg-wal-archive-plugin-missing`, `cnpg-archiver-failing`, `cnpg-wal-archive-backlog`, `cnpg-backup-wal-archiving`, `object-store-denied`, `object-store-forbidden`, `object-store-unreachable`, `backup-destination-conflict`, `wal-archive-not-empty` — or run `kubectl -n <namespace> logs <primary-pod> -c plugin-barman-cloud`
5. **What does the object store itself hold?** — `barman-no-successful-backup`, `barman-last-backup-failed`, `repository-wal-unhealthy`, `repository-coverage-unhealthy`, `repository-retention-unhealthy`, `cnpg-retention-failed`

### The cluster will not come up

a new cluster, or a recreated instance, never becomes healthy.

1. **Has the operator refused the definition, or stopped before starting?** — fact: the API server reports no Cluster object of this name — `cnpg-invalid-definition`, `cnpg-unrecoverable`, `cnpg-cannot-create-objects`, `cnpg-unknown-plugin`, `cnpg-plugin-failure`, `cnpg-image-catalog-unusable`, `cnpg-image-catalog-missing`, `cnpg-image-catalog-lacks-major`, `cnpg-service-account-missing` — or run `kubectl -n <namespace> get cluster <name> -o jsonpath='{.status.phase}: {.status.phaseReason}'`
2. **Is the bootstrap or the clone failing?** — `cnpg-bootstrap-stuck`, `cnpg-initdb-failed`, `cnpg-restore-failed`, `cnpg-recovery-target-missing`, `cnpg-bootstrap-backup-missing`, `wal-archive-not-empty`, `cnpg-join-failed`, `cnpg-pvc-initializing-stuck` — or run `kubectl -n <namespace> logs job/<name>-1-initdb`
3. **Can the pods be scheduled and started at all?** — `pod-scheduling`, `quota-exhausted`, `resource-quota`, `image-pull`, `volume-binding`, `k8s-volume-mount-failed`, `k8s-container-config-error`, `k8s-container-crashloop`, `k8s-container-oom` — or run `kubectl -n <namespace> get events --sort-by=.metadata.creationTimestamp`
4. **Can the operator reach the instances it created?** — `cnpg-status-unreachable`, `cnpg-liveness-isolation`, `cnpg-no-system-id`, `cnpg-not-ready` — or run `kubectl -n cnpg-system logs deployment/cnpg-controller-manager | grep 'Cannot extract Pod status'`
5. **Is the data directory itself usable?** — `cnpg-pvc-unusable`, `cnpg-pvc-dangling`, `cnpg-pg-control-lost`, `cnpg-pg-rewind-failed`

### A replica is behind or not replicating

a standby lags, is out of the read service, or is on the wrong timeline.

1. **Is the replica streaming at all?** — `cnpg-replica-not-streaming`, `cnpg-replica-not-receiving`, `cnpg-replica-lagging`, `cnpg-replication-lag-high` — or run `kubectl cnpg status <name> --verbose`
2. **Is a replication slot or its synchronisation the problem?** — `cnpg-slot-retaining-wal`, `cnpg-slot-sync-failing`
3. **Did the replica fail to rejoin after a promotion?** — `cnpg-pg-rewind-failed`, `cnpg-timeline-divergence`, `cnpg-pg-control-lost`, `cnpg-wal-restore-failed`
4. **Is the instance deliberately stopped, or crashing?** — `cnpg-instance-fenced`, `k8s-container-crashloop`, `k8s-container-oom`, `cnpg-postgres-exited`, `cnpg-instance-failed`

### A switchover, failover or upgrade is stuck

the cluster has been between states for too long.

1. **Is a primary move in flight and not finishing?** — `cnpg-primary-move-stuck`, `cnpg-primary-failing`, `cnpg-primary-lease-expired`, `cnpg-lease-not-acquired`, `cnpg-lease-preempted`, `cnpg-primary-lease-conflict`, `cnpg-primary-status-check` — or run `kubectl -n <namespace> get lease <name> -o yaml`
2. **Is the operator waiting for a person?** — `cnpg-waiting-for-user`, `cnpg-switchover-required`, `cnpg-upgrade-delayed` — or run `kubectl cnpg promote <name> <instance>`
3. **Is a rollout or upgrade not finishing?** — `cnpg-rollout-stuck`, `cnpg-major-upgrade-stuck`, `cnpg-arch-binary-missing`, `cnpg-manager-upgrade-failed`, `cnpg-not-ready`, `cnpg-instances-short`
4. **Does the quorum forbid the failover?** — `cnpg-quorum-standbys-short`, `cnpg-sync-replicas-short`
5. **Is a replica-cluster promotion or demotion in progress?** — `cnpg-promotion-stuck`, `cnpg-replica-switch-stuck`, `cnpg-demotion-fencing`

### The operator seems to be doing nothing

a change to the Cluster has no effect, or the status stops moving.

1. **Has the operator been told to wait, or to stop?** — `cnpg-hibernated`, `cnpg-waiting-for-user`, `cnpg-upgrade-delayed`, `cnpg-hibernation-blocked` — or run `kubectl -n <namespace> get cluster <name> -o jsonpath='{.metadata.annotations}'`
2. **Has reconciliation stopped on a plugin or the definition?** — `cnpg-unknown-plugin`, `cnpg-plugin-failure`, `cnpg-invalid-definition`, `cnpg-image-catalog-unusable`
3. **Can the operator reach the instances?** — `cnpg-status-unreachable`, `cnpg-liveness-isolation`, `cnpg-no-system-id` — or run `kubectl -n cnpg-system logs deployment/cnpg-controller-manager --all-containers=true`
4. **Is something rewriting the definition under the operator?** — `k8s-definition-rewritten-repeatedly`, `cnpg-scale-down-refused`, `cnpg-pvc-resizing-stuck`
5. **Has the operator stopped renewing what it manages?** — `cnpg-certificate-expired`, `cnpg-certificate-expiring`, `cnpg-not-ready`, `cnpg-rollout-stuck`
6. **Is the operator itself running?** — or run `kubectl -n cnpg-system get pods && kubectl -n cnpg-system describe deployment cnpg-controller-manager`
