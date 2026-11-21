---
name: cnpg-status-contract
description: Condensed reference of what a UI must read to show the real status of a CloudNativePG cluster (Cluster.status fields and freshness, instance /pg/status payload and its blind spots, backups under plugin-based archiving, fencing/hibernation, version detection, RBAC and network per access path). Load while coding or reviewing anything under pkg/cnpgstatus, pkg/diagnose or the console status API.
---

# CNPG status contract (CNPG 1.30 → 1.31-dev)

The code is the truth: `pkg/cnpgstatus` (read model), `pkg/diagnose/catalog` (version pins),
`internal/console/status`; the user-facing text is under `docs/site/docs/console/`. Where this
sheet and the code disagree, the code wins and this sheet is the bug. pgToolBox supports
CloudNativePG 1.30 and later (`CONTRIBUTING.md` invariant 9); older minors below are named only
because a field appeared in them.

## Three origins, always shown with age
- **Operator claim** (`Cluster.status`, child CR status, conditions, events): freezes when the
  operator is down; event-driven fields keep their last value.
- **Instance self-report** (`GET https://<pod>:8000/pg/status`, unauthenticated, TLS 1.3,
  server cert from `<cluster>-ca`, `ServerName=<cluster>-rw`; metrics `:9187`): up to one
  sweep old; unreachable instances are rows, never omitted.
- **Console inference** (rules, cross-checks, history): "not applicable" outside the pinned
  CNPG minors, never "clear".

## Cluster.status essentials
`phase`/`phaseReason` (free sentences; green = Healthy, Setting up primary, Creating a new
replica; yellow = Upgrade*, Waiting for user action; red = rest; unknown phase = yellow
verbatim); conditions `Ready`, `ContinuousArchiving` (written by the instance),
`LastBackupSucceeded`, `ConsistentSystemID` (1.27), `Initialized` (1.30),
`SyncReplicationTopologySatisfied` (1.31), `cnpg.io/hibernation`, fork `TopologyUnavailable`,
`VolumeExpansionRefused`, `VolumeAutoGrowStopped`; `currentPrimary` ≠ `targetPrimary` =
switchover/failover in flight (`"pending"` = failover delay); `instances` counts PVC groups;
PVC classes healthy/initializing/resizing/dangling/unusable; `pgDataImageInfo` vs
`targetPgDataImageInfo`; `operatorCertificateFingerprint` (≥1.30); deprecated
`firstRecoverabilityPoint*`/`lastSuccessfulBackup*` are **empty under plugin backups** → render
"not reported by plugin-based backups", never "no backup". Annotations to flag:
`cnpg.io/fencedInstances`, `cnpg.io/hibernation`, `cnpg.io/reconciliationLoop: disabled`,
`cnpg.io/validation: disabled`, `cnpg.io/podPatch`, `alpha.cnpg.io/unrecoverable`,
`cnpg.io/skipWalArchiving`.

## /pg/status essentials
`isPrimary`, `pendingRestart`, `pendingRestartForDecrease`, `loadedConfigurationHash`,
`currentLsn`/`receivedLsn`/`replayLsn`, `timeLineID`, `currentWAL`, archiver fields
(`isArchivingWAL` = last archived > last failed), `readyWalFiles`, `replicationInfo[]`
(**only `<cluster>-N` `streaming_replica` walsenders; `receivedLsn` is `sent_lsn`**),
`replicationSlotsInfo[]`, `pgStatBasebackupsInfo[]`, `instanceManagerVersion`, `sessionID`
(≥1.29, changes on IM restart), `mightBeUnavailable`; `isFenced`, `pod`, `node`, `isPodReady`
are **caller-side only**; fork: `volumeUsages[]`.

## Backups
`Backup.status.phase` pending→started→running→finalizing→completed|failed; `ScheduledBackup`
uses six-field cron (seconds first); recoverability for plugin backups:
`ObjectStore.status.serverRecoveryWindow[server]`; cross-check with repository evidence.

## Version detection (best first)
`/pg/status.instanceManagerVersion` → `bootstrap-controller` init image tag (lags after
in-place upgrade; `bootstrap-instance` is another container in 1.31; bootstrap Jobs vanish in
1.31) → `cnpg.io/operatorVersion` on `cnpg-default-monitoring` → commit-hash table → feature
probes (`operatorCertificateFingerprint`, Lease, `Initialized` ≥1.30; `ConsistentSystemID`
≥1.27; `databaseroles` CRD ≥1.30; `failoverquorums` ≥1.27). Never `app.kubernetes.io/version`.

## Access paths
Watches: `get/list/watch` per kind, name-scoped where possible. `/pg/status`: egress to pod
:8000 (NetworkPolicy must allow it) or `pods/proxy get`. Metrics: :9187/:9127,
HTTPS when `monitoring.tls.enabled`. Never `pods/exec`. CNPG api types for reads only;
mutations as merge patch / SSA on precise paths with preconditions (promote: target is a
current, non-fenced replica; optimistic lock).

## Support matrix
1.30 and later; vendored CRDs per minor under `test/crds/cnpg-<minor>/` for envtest; Barman
Cloud leaves the core in 1.32 — support only the plugin path going forward.
