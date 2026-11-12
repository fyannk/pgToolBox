---
sidebar_position: 5
title: Configuration
---

# Configuration

Chart values become environment variables; the binary reads them
(`internal/console/config`, `internal/console/auth`) and refuses to start on a
setting it cannot honour, with all problems in one message. Sign-in settings are
on [Authentication](authentication.md). `console.extraEnv` appends more.

## Watched namespaces

`console.watchNamespaces` (`WATCH_NAMESPACES`) lists the namespaces the console
watches and gets a Role in. Empty means the release namespace
(`POD_NAMESPACE`). The console **refuses to start** with an empty list or "all
namespaces"; there is no cluster-scoped console. At most 16
namespaces: past that, run one console per team.

`console.clusterSelector` (`CLUSTER_SELECTOR`, a label selector) narrows the
Clusters shown on top of what each person's RBAC allows. "I only want to see these
two clusters" is a console with a `clusterSelector`.

## The instance CA bundle

The console reads each instance's `/pg/status` over HTTPS (TLS 1.3 only). It
verifies the instance's certificate against a CA bundle: a ConfigMap, key
`ca.crt`, holding the PEM server CAs of the watched Clusters, mounted into the
console and named by `INSTANCE_STATUS_CA_FILE`. The kubelet refreshes the mounted
file when the ConfigMap changes and the console re-reads it without a restart.
The console never reads the CloudNativePG `<cluster>-ca` Secret itself: it holds
`ca.key` next to `ca.crt`, and the console reads no Secret.

CloudNativePG gives the self-signed CA it creates for each Cluster **90 days** of
validity and renews it 7 days before it expires. Whatever fills the bundle must
therefore follow the renewals; who does it depends on how the console was
installed.

### Console run by the operator (`PgToolBoxConsole`)

Nothing to do. The operator keeps the bundle in `<name>-instance-ca`: it copies
the `ca.crt` (and only that key) of each selected Cluster's server CA Secret,
follows renewals and Clusters coming and going within seconds, and reports what
is missing in `status.instanceCABundle` and the `InstanceCABundleComplete`
condition. See [PgToolBoxConsole: the instance CA bundle](../operator/console.md#the-instance-ca-bundle).
Set `spec.instanceCABundleRef` only to supply a bundle of your own, which you then
keep current as below.

### Console installed by the pgtools chart

No operator is involved, so **you** supply the bundle: a ConfigMap named by
`console.instanceStatus.caBundleConfigMap` (default `pgtoolbox-console-instance-ca`). The
chart only mounts it. Ordinary namespace RBAC is enough to build it:

```bash
for c in $(kubectl -n team-a get clusters.postgresql.cnpg.io -o name | cut -d/ -f2); do
  kubectl -n team-a get secret "$c-ca" -o jsonpath='{.data.ca\.crt}' | base64 -d
done > ca.crt
kubectl -n team-a create configmap pgtoolbox-console-instance-ca --from-file=ca.crt=ca.crt \
  --dry-run=client -o yaml | kubectl apply -f -
```

:::warning A copy made once expires within 90 days
CloudNativePG's self-signed CA is valid 90 days and renewed (same key, new
certificate) 7 days before it expires. A bundle built once by the commands above
holds the old certificate: on the day it expires, at most 90 days after it was
issued, every instance of that Cluster reads `tlsError` until the bundle is
rebuilt. Re-running the commands on a schedule works but is easy to forget;
prefer one of these:

- **cert-manager trust-manager**: a `Bundle` whose sources are the Clusters'
  `<cluster>-ca` Secrets (key `ca.crt`) and whose target is a ConfigMap
  `pgtoolbox-console-instance-ca`, key `ca.crt`, in the console's namespace. trust-manager
  follows every renewal. It reads the Secrets in its own trust namespace, so the
  CA Secrets must be there or be copied there; check its documentation for the
  namespace and RBAC it needs.
- **One long-lived CA for the namespace**: create a CA yourself (or with
  cert-manager) with the validity you choose, store it as a Secret with `ca.crt`
  and `ca.key`, and name it in `spec.certificates.serverCASecret` of every Cluster
  of the namespace. CloudNativePG signs the server certificates with it and does
  not renew a CA it did not create; the bundle has one entry and changes only when
  you rotate that CA.
- **Run the console with the operator** (`PgToolBoxConsole`), which keeps the bundle
  itself.
:::

A Cluster whose CA is missing from the bundle is shown as "not verifiable" (below);
a bundle that holds an expired CA shows `tlsError`.

### The "not verifiable" row

A Cluster whose CA is not in the bundle is shown as **not verifiable** with the
reason "status port not verifiable: CA not in the console's bundle", and the
instance's own report is not shown. It is never read unverified, never read over
plain HTTP, and the console has no insecure mode (`INSTANCE_STATUS_INSECURE` is a
start-up error). The same row appears when no bundle is configured at all. A row
is also "not verifiable" when the Cluster exposes no read-write, read or read-only
Service to check the certificate name against.

The other states of an instance's status read, each with its reason:

| State | Meaning |
|---|---|
| `ok` | A verified TLS handshake and a parsed report. |
| `pending` | Not swept yet. |
| `unreachable` | Timeout or connection refused: "not reachable from this console" (a NetworkPolicy, a Pod down). |
| `notVerifiable` | See above. |
| `noTLS` | The Pod was created before CloudNativePG 1.24 and its status port has no TLS. Never read over HTTP; restart the instance. |
| `tlsError` | Another handshake failure: name mismatch, expired certificate, a port that does not speak TLS. |
| `badResponse` | A non-200 answer or an unparsable body. |
| `notRunning` | The Pod has no IP yet or is being deleted. |
| `disabled` | `INSTANCE_STATUS_ENABLED=false`. |

A read can be `ok` and still not read PostgreSQL. While an instance is fenced or
restarting, CloudNativePG's instance manager answers `/pg/status` without
reaching PostgreSQL and says so (`mightBeUnavailable`, with the error it hit).
Such a report is shown as "PostgreSQL not read", with that error: its positions,
timeline, pending restart, replay pause, WAL receiver, archiver, walsenders and
slots are not read, never false, zero or empty. The Instances row and detail mark
them unread; the Replication screen says the primary's walsenders and slots, or a
replica's receiver, were not read; the topology places no replica against the
primary's walsenders; the diagnostics on those fields, the failover rehearsal and
the archiving fact (`PrimaryUnread`, "answered without reading PostgreSQL") do not
judge it. Only the manager's own flags (`mightBeUnavailable`, a rewind in
progress, its version) and the primary role it reads from the data directory
stand.

The sweep runs every `INSTANCE_STATUS_INTERVAL` (30 s), each read bounded by
`INSTANCE_STATUS_TIMEOUT` (5 s, shorter than the interval), at most
`INSTANCE_STATUS_CONCURRENCY` (8) at a time. It also scrapes the instances'
exporters (`:9187`) when `METRICS_ENABLED` is true. `RING_RETENTION` (default 1 h,
between 5 min and 6 h) is how long samples are kept for "since when" and
sparklines. `VOLUME_TREND_INTERVAL`, `VOLUME_TREND_RETENTION` and
`VOLUME_TREND_MAX_SERIES` bound the long volume trend behind the
[storage forecasts](forecasts-and-recovery.md#the-long-trend).

## Replicas and history

The one availability rule: two replicas, rolling, with or without persistence.

| Setting | Workload | Replicas | History |
|---|---|---|---|
| in memory (the pgtools chart's default) | a Deployment, `RollingUpdate` with `maxUnavailable: 0` | `console.replicas` / `spec.replicas` (2) | in memory |
| persistent (`console.history.persistent: true`; the default of a `PgToolBoxConsole`) | a StatefulSet, `podManagementPolicy: Parallel`, rolling one Pod at a time | the same (2) | a bbolt file (`HISTORY_PATH`) on each replica's own claim, `history-<console>-<ordinal>`, of `console.history.size` / `spec.history.size` (1Gi) |

Both keep a PodDisruptionBudget (`minAvailable: 1`) with more than one replica.
The history file takes an exclusive lock, so the replicas never share a volume:
each keeps its own history, snapshots, timeline and digests. Sessions are sealed
in the cookie, revocation is in a watched ConfigMap and the few facts replicas
must agree on are in the state ConfigMap (below), so the edge's cookie affinity
(see [Exposure](#exposure)) only keeps the charts a person sees from one replica.
The claims are deleted with the StatefulSet and kept when scaling down.

**Sizing a claim.** The journal holds up to `HISTORY_MAX_REVISIONS` revisions
(tens of MiB at the default), the timeline and the digests a few MiB at the
default retention, the sweep's snapshot about 100 KiB per instance (bounded at
512 MiB) and the log rules' one at most 64 MiB; a snapshot is written beside the
previous one before it replaces it. 1Gi covers a console of a few hundred
instances; above a thousand, give it 2Gi.

Each replica's claim is its own; see the
[pgtools chart README](https://github.com/fyannk/pgToolBox/blob/main/charts/pgtools/README.md#persistent-history)
or, for a `PgToolBoxConsole`, the [operator page](../operator/console.md#persistent-history).
`HISTORY_ENABLED=false` switches the revision history off. `HISTORY_MAX_REVISIONS`
(5000) and `HISTORY_MAX_BYTES` (64 MiB) bound it.
The incident timeline and the daily digest follow the history (and its
persistence); see [Timeline, change preview and daily digest](timeline-and-digest.md).

The console being down never affects the clusters, pgAdmin or the evidence agents.

## What survives a restart

Two mechanisms, each optional; without them, each replica keeps its state in
memory only.

**State snapshots (persistent history only).** With `HISTORY_PATH` set, the
console writes the in-memory state it would otherwise rebuild next to the
history file, every minute and once more at a graceful shutdown, and reads it
back at start:

| File | What it holds |
|---|---|
| `sweep.snapshot` | the per-instance rings (lag, WAL, connections), the last good `/pg/status` reading and when it was read, each instance's "since", the metrics window and the insight tracker (transaction-ID age, table and statement samples) |
| `log-matches.snapshot` | the [log rules'](../guides/diagnostics.md) observations |
| `volume-trends.gob` | the long volume trend |

Each file starts with a version header and is written to a temporary file,
synced and renamed, so the volume always holds one whole snapshot. A file that
is unreadable, of another version or larger than its bound is logged once and
that part starts empty; the console never fails to start because of one.
On load, samples older than their window are dropped, and state is restored
only onto the **same object**: a Cluster or a pod is matched by its UID, so a
Cluster deleted and recreated under the same name inherits nothing. A counter
(WAL bytes, transactions) is never derived across the restart: the gap is a
reset, as on any restart of PostgreSQL. The admin page lists the snapshots as
the source `stateSnapshots`; `pgtoolbox_console_state_snapshot_*` [metrics](metrics.md)
give each part's age, size and failures.

Without `HISTORY_PATH` nothing is written and a restart starts empty.

**The state ConfigMap (`STATE_CONFIGMAP`).** One ConfigMap per console, named
`<console>-state`, created empty by the pgtools chart and by the operator, and
written by the console itself (with the digest Lease, the only writes it makes
with its own identity besides the opt-in drill teardown: `get`, `watch` and
`update` on that name only). Every replica watches it and merges what the
others wrote:

| Key | What it holds |
|---|---|
| `sessions.json` | signed-out sessions, as the SHA-256 of the session ID with its expiry, when it was recorded and an HMAC tag of the person; a sign-out on one replica is refused by every other, and pruned once expired; bounded per person and in all, a full document shared fairly among the people holding entries ([sign-out](authentication.md#sessions)) |
| `since.json` | when each state began: an instance's `/pg/status` result, a Cluster's phase, a diagnostics finding; keyed by object UID, the earliest time wins, a changed state replaces it, and entries go when their object does |
| `digests.json` | which [digest](timeline-and-digest.md#daily-digest) went to which channel and when (`<digest id>/<channel>`): the earliest time wins, kept for the digest retention plus two days, so a replica taking over the delivery never sends one twice |

Writes are batched (about every 10 seconds, a sign-out or a delivered digest
within a second), use the ConfigMap's `resourceVersion` and re-read and merge on
a conflict. The documents stay under 1 MiB. The sessions, digests and "since"
documents share one budget, and past it the oldest "since" entries go first;
"since" also keeps only the room every other document leaves. Should the
whole ConfigMap (every document counted) near 1 MiB, the oldest restore drill
results go; sign-outs, MCP grants and delivery marks never. A missing or forbidden ConfigMap is logged once and
shown on the admin page (source `sharedState`); each replica then keeps its own
sign-outs and clocks, as a single replica does. `pgtoolbox_console_shared_state_*` metrics count
the writes and give the mode.

Each document carries a format version. A console reads the documents of
older versions, and rewrites them at its own on its next write; it leaves a
document of a newer version (written by a newer console during a rolling
upgrade, or before a rollback) exactly as it is, logs it once and counts it in
`pgtoolbox_console_shared_state_foreign_documents{document}`: what that
document holds is not shared with the older replica (a sign-out in it is not
refused there, and MCP is refused over a newer `grants.json`).

Every write a console makes also changes the ConfigMap annotation
`pgtoolbox.fyannk.dev/shared-state-format-<version>` of its own version. A
replica that leaves a newer document alone, or that has seen another version's
annotation change in the last 30 minutes, rewrites its own every 10 minutes
even with nothing else to write; replicas of one version alone write no such
heartbeat. A replica takes a newer document over once it has seen no write by a
console of another version for 30 minutes, counted on its own clock from when
it last saw one (or from its own start): after a rollback, about half an hour
after the rolled-back replicas start, sharing resumes on its own. During a
rolling upgrade the newer replicas write at least every 10 minutes, so the
older ones never take over; and a take-over is written with the
`resourceVersion` it was decided on, so a newer replica's write in between makes
it conflict, and the re-read leaves the document alone. The trade-off: a newer
replica unable to reach the API server for 30 minutes has its documents taken
over, and takes them back (merging) once it writes again. A document taken over
is read in this version's layout, so the sign-outs it holds stay refused: every
later version keeps the fields an earlier one reads, and a version that cannot
is numbered 100 or more, which no console of this version ever takes over (it
stays foreign until its key is deleted). `grants.json` is reset instead (a
revocation recorded in a newer layout could otherwise be lost): a replica drops
the MCP grants it holds once it finds a newer `grants.json`, and every assistant
connects again after the take-over. An annotation of another version that has
not changed for 30 minutes is removed with a replica's next write, so a replica
started later sends no heartbeat for a version long gone. To recover at once,
delete the document's key from the ConfigMap; what it held is lost (for
`grants.json`, every assistant connects again). A document of no version (`{}`)
is read as empty and rewritten.

MCP grants are merged against what each replica last read, not added up: a
grant the ConfigMap no longer holds was pruned, reset or deleted by another
replica, and is dropped. So a replica cut off from the API server while a grant
was revoked, and the revocation pruned 10 minutes later, does not bring the
grant back when it reconnects, nor does one that missed a take-over. A grant a
replica recorded itself is kept while its own write is on the way, for at most 8
minutes; a revocation it made is kept until the ConfigMap holds it, however
long that takes, past its own expiry too. A replica cut
off longer than that loses the grants it recorded meanwhile (their assistants
connect again), and it refuses every grant from 30 seconds into the outage
anyway. A ConfigMap deleted and recreated (a new UID) resets the grants, but
every replica writes the revocations it knows into the new one, so a grant
still recorded on another replica does not come back unrevoked.

A ConfigMap older than the one a replica last merged (a watch event still
queued) is not merged. Should the API server's revision go back (an etcd
restore that did not bump it), two reads in a row older than what a replica
merged are taken as the ConfigMap, logged once: the replicas then read and write
again. A write still conflicting after five re-reads counts as a failure
(`pgtoolbox_console_shared_state_write_exhausted_total`): the replica is in the
failing mode, and refuses MCP grants 30 seconds later, until a write lands.
A replica in the failing mode retries a write about a second later and then
with backoff (up to a minute apart), with nothing of its own pending too, so a
failed write leaves it failing only while the API server keeps refusing. A
request whose client gives up is no failure: one still waiting for another
write starts none, and one already sent is completed (within 10 seconds).

Replicas whose clocks disagree still converge: a sign-out's
recording time is kept as written, and a replica that finds it more than five
minutes in its own future only ranks it last when a bound of the sessions
document must choose. What each replica's clock decides on its own (when an
entry ends, and which entries a full document keeps) can make two replicas
rewrite each other's copy for at most as long as their clocks disagree.

The chart's ConfigMap and Role need only what a namespace admin holds
(`configmaps` in the release namespace), so the team install keeps working
without a cluster administrator.

## Exposure

`console.exposure.type`: `none` (a ClusterIP Service, the default), `ingress`,
`route` (OpenShift) or `httpRoute` (Gateway API). The console streams events over
SSE, so each edge needs a long timeout and no buffering; the chart sets what it
knows (`console.exposure.sse.timeout`, default `1h`): the nginx timeout and
buffering annotations on an Ingress, the HAProxy `timeout` annotation on a Route,
`timeouts.request` on an HTTPRoute. Set the equivalents yourself for any other
ingress controller. The stream sends a comment every 15 seconds and a `reauth`
event before a token expires.

**Cookie affinity** (`console.exposure.sticky`, on by default; always on for a
`PgToolBoxConsole`): the edge keeps a person on one replica with a cookie,
`pgtoolbox-console-replica`, so the charts and sparklines they see come from one
replica's rings and history. Rendered: ingress-nginx's cookie affinity on the
Ingress, Traefik's sticky cookie on the Service, the cookie name on an OpenShift
Route (sticky by default). Gateway API session persistence is not in the
standard channel: configure it on your gateway. Without affinity nothing
breaks: the session cookie is self-contained and sign-outs and "since" times are
shared, so only the charts may differ between two reloads.

Set `console.publicURL` (or `exposure.host`, from which it is derived) so cookies,
redirects and the CSRF check use the right origin. To serve HTTPS from the pod,
point `console.tls.secretName` at a `kubernetes.io/tls` Secret; it is mounted as
files. Behind an edge, name the edge's addresses in `console.trustedProxyCIDRs`
(or enable `console.networkPolicy` with no `ingress.extraFrom`: it then admits
only the router namespace and trusts every peer it admits; not on OpenShift,
below). Without either, or with `extraFrom` (its peers are not the router), the
console ignores
`X-Forwarded-For`: every client shares the edge's address, so no per-address
sign-in budget applies, only the per-flow, per-token and per-replica bounds
([rate limits](authentication.md#sessions)). NetworkPolicies add up, and the
chart cannot see the namespace's others: one that admits more Pods to the
console port (an `allow-same-namespace`, the OpenShift project template's)
lets them forge the header under the any-peer trust, so list the proxies in
`trustedProxyCIDRs` where such a policy exists. On OpenShift (the Route API is
served, or `exposure.type` is `route`) the project template adds
`allow-from-same-namespace`, so the chart never trusts every peer there: without
`trustedProxyCIDRs` the header is ignored and the install notes say so.
`console.trustedProxyHops` (default 1 when exposed) counts the proxies in front.

## What else can be switched

| Variable | Default | |
|---|---|---|
| `ALLOW_LOGS` | `true` | Serve log reads. The chart sets `false` when `console.rbac.podLogs=false`. |
| `ALLOW_CLUSTER_CATALOGS` | `false` | Read `ClusterImageCatalog`s; needs the opt-in ClusterRole. Without it the panel says "catalog not readable", never an empty list. |
| `OPERATIONS_ENABLED` | `true` | `false` makes the [write endpoints](../operations.md) answer 501. |
| `MCP_ENABLED`, `MCP_*` | `false` | The read-only [MCP endpoint](mcp.md) at `/mcp` for AI agents, bearer tokens only. |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `TRACING_*` | unset: off | [OpenTelemetry tracing](tracing.md) over OTLP/HTTP: HTTP requests, the Kubernetes API calls they make, MCP tool calls. |
| `RESTORE_DRILL_ENABLED`, `RESTORE_DRILL_*` | `false` | [Restore drills](restore-drills.md): a throwaway Cluster restoring the latest backup, checked and deleted by the console. |
| `EVIDENCE_ENABLED`, `EVIDENCE_INTERVAL` | `true`, `1m` | Read [repository evidence](../evidence-agent.md). Off, the backups view says "disabled". |
| `EVIDENCE_TOKEN_DIR` | `/var/run/secrets/pgtoolbox.fyannk.dev/evidence-agent` | Directory of the projected evidence-agent tokens, one per watched namespace `N` at `<dir>/N/token` with audience `pgtoolbox-evidence-agent/N`. Set by the chart. The v2.0 variable `EVIDENCE_TOKEN_PATH` is refused at start. |
| `DIAGNOSTICS_ENABLED`, `_INTERVAL`, `_DEBOUNCE` | `true`, `1m`, `2s` | The [background diagnostics](../guides/diagnostics.md) runner. |
| `STATE_CONFIGMAP` | empty | The [state ConfigMap](#what-survives-a-restart) the replicas share sign-outs, "since" times and digest delivery marks through, in the console's namespace. Set by the operator and the pgtools chart; empty keeps them per replica. |
| `INSIGHT_PACK_CONFIGMAP` | `pgtoolbox-insight-pack` | The name of the [insight pack](insight-pack.md)'s ConfigMap, a DNS-1123 subdomain: the console matches it against each Cluster's `spec.monitoring.customQueriesConfigMap` and names it in the opt-in instruction. It reads no ConfigMap for it. The pgtools chart sets its per-release name (`<release>-insight-pack`); the operator and `config/console` keep the default, the name of `deploy/insight-pack/insight-pack.yaml`. |
| `LIFECYCLE_FILE` | empty | The end-of-life table (JSON) of PostgreSQL, Kubernetes, CloudNativePG and Barman Cloud plugin releases that the diagnostics read; reloaded when the file changes. Set by the operator and the pgtools chart to their lifecycle ConfigMap; empty or absent is the table built into the console. |
| `EVENTS_MAX_AGE`, `EVENTS_MAX_PER_CLUSTER` | `1h`, `200` | Bound the events served. |
| `MONITORING_URL` | empty | An https or root-relative link-out to your monitoring. |
| `PGADMIN_URL` | empty | A fixed https or root-relative link to a pgAdmin installed without the operator ([pgtools chart](../install/pgadmin-without-operator.md)), offered on every Cluster after the PgAdmin objects' links. |
| `ALLOW_INSECURE_LINKS` | `false` | Accept `http://` in `MONITORING_URL` and `PGADMIN_URL`. For a lab only: a plain-http link sends readers to a page whose traffic, and any password typed there, crosses the network unencrypted. Root-relative links need no exception. |
| `PROMETHEUS_URL`, `PROMETHEUS_TOKEN_FILE`, `PROMETHEUS_CA_FILE`, `PROMETHEUS_NAMESPACED`, `PROMETHEUS_RANGE` | empty, empty, empty, `false`, `1h` | Chart the metrics from [Prometheus](prometheus.md), queried with the console's own identity. |
| `RESYNC_PERIOD`, `REPROBE_INTERVAL`, `API_REQUEST_TIMEOUT` | `10h`, `5m`, `10s` | Informer safety resync; how often optional APIs are probed again; one API request. |
| `LOG_LEVEL` | | `debug`, `info`, `warn` or `error`. |
| `LISTEN_ADDR`, `PROBE_LISTEN_ADDR` | `:3000`, `:3001` | The probe listener carries `/healthz`, `/readyz` and [`/metrics`](metrics.md). `/readyz` is ready once, among its other checks, the informer cache has synced and the revocation list was read; with MCP OAuth on and `STATE_CONFIGMAP` set, also once the shared state ConfigMap was read (or is known missing or forbidden: the console then refuses MCP grants and logs why), and that check does not fail again afterwards: a later API server failure refuses MCP grants instead. |
| `SHUTDOWN_DELAY` | `3s` | On SIGTERM the console answers not ready at once but keeps serving for this long (0s to 10s), so the endpoints and the edge stop routing to it before its listener closes. Requests in flight then have 25 s to complete; the event stream and log follows keep running through the delay and end when the drain begins; the informers and the change feed behind them keep running until the drain is over, so a stream open during the delay still receives updates. Delay plus drain must stay under the pod's `terminationGracePeriodSeconds` (30 s by default). |

Optional APIs (Routes, Gateway API, the barman-cloud `ObjectStore` kind) are
probed at start and re-probed every `REPROBE_INTERVAL`; the console degrades
instead of failing when one is missing or not readable.

`pgtoolbox_console_config_info` exports every effective setting as a metric; secrets are
named by file path, never by value.
