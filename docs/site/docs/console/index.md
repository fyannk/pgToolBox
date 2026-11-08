---
sidebar_position: 1
title: The console
---

# The console

The console is one Go binary serving a JSON API and a single-page application. You
install it once per team namespace ([install](../install/chart.md)). It shows the
CloudNativePG clusters of the namespaces it watches, as the signed-in person is
allowed to see them.

## What it shows

Per Cluster, the API (`/api/v1/namespaces/{namespace}/clusters/{name}/...`)
serves these views; the application renders each one:

| View | What it answers |
|---|---|
| Overview | The roll-up verdict and what contributes to it, the primary election, instance cards, conditions, recent warnings, and the sources and blind spots the view rests on. |
| Instances | Each instance as the operator, the Pod and the instance's own `/pg/status` report it ([not-verifiable rows](configuration.md#the-instance-ca-bundle)). |
| Replication, Storage | Primary election, the synchronous configuration, walsenders, receivers and slots, and a [failover rehearsal](../guides/failover-rehearsal.md) ("if the primary failed now"), with the blind spots of `/pg/status`; declared volumes, PVCs and volume usage where an instance reports it, with [when each volume fills](forecasts-and-recovery.md). |
| Backups | The operator's `Backup` objects, the plugin's recovery window and the [repository evidence](../evidence-agent.md), side by side, with disagreements named; the [recovery window bar and a restore check](forecasts-and-recovery.md#recovery-window-backups-screen), and [restore drills](restore-drills.md). |
| Diagnostics, Triage | The [diagnostics](../guides/diagnostics.md) run for the Cluster, and playbooks that read it from a symptom. |
| Events, History | Events about the Cluster and its children (bounded by age and count); revisions of the Cluster and its declarative children that the console observed, with structural diffs. |
| Logs | The bounded tail of an instance's log, and a live follow. Read with the console's ServiceAccount after the person's own `get pods/log` check passes; the follow ends if that access is withdrawn (rechecked every 30 s), after an hour, or when the container stops. |
| Metrics | The latest exporter readings as tiles and a short retained series (`RING_RETENTION`, one hour by default), or [charts from Prometheus](prometheus.md) when `PROMETHEUS_URL` is set, plus the monitoring link-out when `console.monitoringURL` is set. Time series are Prometheus's job. |
| Objects | Databases, DatabaseRoles, managed roles, tablespaces, publications, subscriptions, poolers and the image catalog. |
| Topology | The cluster's wiring, grouped into Poolers, Cluster, Backups, Kubernetes storage and Object storage frames (poolers, Services, instances, PVCs, schedules, the object store), with its geometry computed by the console. Service boxes carry the observed type, ClusterIP and port. |
| Operations | What the signed-in person may do, with preconditions and consequences: [backup, reload, restart, promote](../operations.md). |

Across clusters: the Clusters you may see, [two of them side by side](../guides/compare.md) (`/api/v1/compare`), a live update stream (server-sent events), and `PgAdmin` and `EvidenceAgent` objects you
may list in the watched namespaces, with a link-out to pgAdmin only for people
who may `get` that `PgAdmin`.

The API is described by an OpenAPI document served with the console and kept in
the repository at `internal/console/api/openapi.yaml`.

## Why the operator waits

When a Cluster's phase is not healthy, the Overview explains why the
CloudNativePG operator is not moving on. Each explanation lists the facts it rests
on (the phase and its reason, a Pod condition, a container state, an Event, a PVC
list). Each one is marked as one of two kinds:

- **Reported.** The operator said so itself. Two sources count:
  - the reason codes of the fork's builds: Warning Events `QuotaExceeded`,
    `AdmissionDenied`, `ReconciliationFailed`, `BackupDelayed` and
    `VolumeSnapshotError`;
  - a phase whose reason the operator writes for exactly one cause, for example
    `Waiting for user action` with "User must issue a supervised switchover".
- **Heuristic.** The console derived it. Upstream CloudNativePG has no reason
  codes, so the console reads what the operator does write:
  - which instance Pod is not active, and why: unschedulable, an image it cannot
    pull, a crash loop;
  - the PVC lists in the Cluster's status;
  - a quota or admission refusal in the phase reason;
  - a pending switchover;
  - a backup waiting for its Pod.

Every rule cites the CloudNativePG source it keys on, in `pkg/cnpgstatus/waiting.go`.
The rules hold for 1.30 and 1.31. A healthy phase has no explanation, even if a PVC
list is stale.

## How it is built, in four sentences

It reads with informers over the watched namespaces using its own ServiceAccount,
which has Roles there and no Secret verb. It reads each instance's `/pg/status`
over verified TLS and each [evidence agent](../evidence-agent.md) over the agent's
own HTTPS endpoint. It decides what a person sees with a
[SelfSubjectAccessReview](authorization.md) per object. It writes only as the person,
with their token. It is stateless apart from an optional history file, so it runs
two replicas by default ([the one availability rule](configuration.md#replicas-and-history)).

## What it is not

- Not a cluster-wide console. See [watched namespaces](configuration.md#watched-namespaces).
- Not an identity provider and not a user database. There is no local password
  login. The break-glass path is a pasted API-server token
  ([authentication](authentication.md#the-token-provider-break-glass)).
- Not a metrics store. See the Metrics row above.
- Not a way to run SQL. The console never connects to PostgreSQL as a user. pgAdmin
  does, with credentials the operator offers
  ([pgAdmin](../operator/pgadmin.md)).

## Pages in this section

- [Authentication](authentication.md): providers, sessions, revocation.
- [OpenShift](openshift.md): the OAuth flow and the manual checklist.
- [Authorization](authorization.md): the access review model and the ClusterRoles.
- [Configuration](configuration.md): watched namespaces, the CA bundle, availability, exposure.
- [NetworkPolicy](networkpolicy.md): the admin's choice.
- [Metrics](metrics.md): what the console exports.
- [Prometheus charts](prometheus.md): `PROMETHEUS_URL`, the console's identity, OpenShift.
