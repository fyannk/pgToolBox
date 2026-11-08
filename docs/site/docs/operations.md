---
sidebar_position: 6
title: Operations
---

# What the console may do

The console is read-mostly. It offers exactly **four operations** on a
CloudNativePG Cluster, each one a fixed object or patch, plus the opt-in
[restore drill](console/restore-drills.md), and nothing else. Every
one is made **with the signed-in person's own token**: the API server authorizes
it, runs admission on it and records it in its audit log under the person's name.
The console's ServiceAccount has no write permission (the one exception, off by
default, is deleting the restore drill Clusters it sealed itself) and there is no
impersonation ([security model](security.md#writes-token-passthrough-never-impersonation)).

| Operation | What the console writes | The person needs | Outcome observable? |
|---|---|---|---|
| **Backup** | One `Backup` named `ondemand-<timestamp>-…` with the Cluster's own method (barman-cloud plugin, `barmanObjectStore` or `volumeSnapshot`) | `create backups` in the namespace | Yes: the `Backup` object's phase |
| **Reload configuration** | The annotation `cnpg.io/reloadedAt` on the Cluster | `patch clusters/<name>` | No: CloudNativePG reports no outcome for a reload (the console says so) |
| **Restart** | The annotation `kubectl.kubernetes.io/restartedAt` on the Cluster: instances roll, replicas first, then the primary | `patch clusters/<name>` | Yes |
| **Promote (switchover)** | A status patch on the Cluster setting `targetPrimary` to the chosen replica | `patch clusters/<name>/status` | Yes |
| **Restore drill** (opt-in) | One throwaway Cluster recovering the latest backup, with no archiver and no backup section; the console deletes it after its check | `create clusters` in the drill namespace | Yes: the drill's checks |

These are the same interactions the `kubectl cnpg` plugin makes, verified against
CloudNativePG 1.30. The `pgtoolbox-console-operator` ClusterRole grants exactly these
verbs ([authorization](console/authorization.md#the-two-clusterroles)); the
viewer role grants none.

## How a request is handled

1. **Typed confirmation.** Each operation needs the Cluster's name typed back (for
   a promote, the target instance's name). A mismatch is a 422.
2. **A live read, as the person.** The console reads the Cluster (and, for a
   promote, its pods) from the API server with the person's token, never from its
   cache: a precondition must not be judged on a snapshot seconds old.
3. **Preconditions on that read.** A failed one is a 409 that names the reasons
   in plain words. Nothing is written.
4. **A locked write.** Restart, reload and promote carry the `resourceVersion` of
   the read, so a Cluster that changed in between (a switchover that just began)
   makes the write a 409 ("the cluster changed since it was checked; reload the
   page and retry") instead of acting on stale ground.
5. **202 means accepted, not done.** The API server accepted the write; the
   operator may not have acted. The response carries an operation id.

## Preconditions

| Operation | Refused when |
|---|---|
| Backup | The Cluster is hibernated; a plugin backup but the Cluster configures no plugin; otherwise no `spec.backup` section, or none for the chosen method (`barmanObjectStore` or `volumeSnapshot`) |
| Restart | A switchover or failover is in flight; the Cluster is hibernated or hibernating; `cnpg.io/reconciliationLoop` is disabled (nothing would roll out) |
| Reload | The Cluster is hibernated; reconciliation is disabled |
| Promote | The same Cluster-level reasons as restart, and a target that: does not exist; is not an instance of this Cluster (listed in `status.instanceNames` and controlled by the Cluster, not just labelled); is being deleted; is already the primary; is not a replica; is fenced (the target, or every instance), or the fencing annotation cannot be read |

The offers list shows each operation with the verbs it needs, whether *this person*
has them (or whether the console could not ask: "could not check"), the
preconditions not currently met, the consequence in one sentence and the expected
duration.

## Following an operation

`GET /api/v1/operations/{id}` resolves an operation, from the Cluster's current
state, on **any** console replica: the id encodes what to look for. It reports a
phase ladder with each step attributed to the field that showed it. `timedOut`
means no outcome was reported before the deadline (an hour for a backup, 30 minutes
for a restart, 2 minutes for a promote), not that it failed. The offers list also
shows the operations *this replica* issued, since it started; that is a
convenience, not a record. **The API server's audit log is the complete record.**

## Switching it off

`console.operations.enabled=false` (`OPERATIONS_ENABLED=false`) makes the operation
endpoints answer 501. It removes the buttons for everyone; it is not what
protects the Cluster, RBAC is. To stop one person, remove the RoleBinding.

## What the console does not do

It does not connect to PostgreSQL, run SQL, edit a Cluster spec, delete anything
but its own sealed drill Clusters,
fence or unfence instances, or resize storage. Temporary access is not a console
operation either: it is a `PgAccessRequest` that the operator turns into an
expiring RoleBinding ([access requests](console/access-requests.md)).
