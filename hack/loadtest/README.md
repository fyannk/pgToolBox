# Load-test harness

Measures the memory, latency and throughput limits of the console, the
operator and (later) the evidence agent against thousands of fake CloudNativePG
Clusters, on one kind node. Nothing here runs in CI. It is not a benchmark of
PostgreSQL: the Clusters, their Pods and their `/pg/status` are generated.

**Run it on an idle host.** `cluster.sh up` refuses to start while a CI e2e
kind cluster or build container runs or the load average is high
(`ALLOW_BUSY_HOST=1` overrides, for a smoke test only). It touches only the kind
cluster `pgtoolbox-load`, with a private kubeconfig in `hack/loadtest/.state/`;
`loadgen` refuses any other context.

## What is in the cluster

| Where | What |
|---|---|
| the real kind node (`type=real`) | console (`pgtools` chart, 1 replica, log streaming off, `cookieSecure: "false"`), operator (`pgtoolbox` chart, 1 replica), MinIO, `loadgen instance-sim`, KWOK, metrics-server |
| `kwok-node-NN` (`type=kwok`, tainted) | the Pods of the fake Clusters: KWOK gives them Running status, container statuses and an IP |
| CRDs | CloudNativePG 1.30, Barman `ObjectStore`, the pgToolBox CRDs (from the operator chart). **No CloudNativePG operator**: `loadgen` writes the Clusters and their status |
| RBAC | ServiceAccount `loadtest` (namespace `loadtest`): viewer in every watched namespace, `get /metrics` on the console and the operator. The group `system:serviceaccounts:loadtest` is a viewer too: `loadgen http` signs in as one ServiceAccount per virtual user |

`instance-sim` exists because the console sweeps `https://<podIP>:8000/pg/status`
of every member Pod (and `:9187/metrics`). On KWOK nothing listens there, and
the sweep (8 at a time, 5 s timeout) would measure dead addresses. cluster.sh
routes the fake Pod CIDRs to the node's loopback, and the simulator (hostNetwork)
answers with a certificate for `<cluster>-rw.<ns>.svc` signed by the CA in the
console's `pgtoolbox-console-instance-ca` ConfigMap, a primary/replica report by Pod IP,
and a small exporter page. Every instance reads `ok`.

## Steps

```sh
# 1. The cluster (builds the console and operator images from this tree the
#    first time: a few minutes). Namespaces lt-01..lt-NN are the ones the console watches.
NAMESPACES=16 KWOK_NODES=10 hack/loadtest/cluster.sh up
hack/loadtest/cluster.sh status

# 2. Population. --clusters is per namespace: 16 x 100 x 3 = 4800 Pods.
hack/loadtest/loadgen.sh populate --namespaces 16 --clusters 100 --instances 3

# 3. In other shells, while measuring:
hack/loadtest/loadgen.sh scrape --every 5s                                   # one line per sample
hack/loadtest/loadgen.sh churn --rate 50 --duration 5m                       # status writes
hack/loadtest/loadgen.sh http --users 20 --duration 5m --mix 1:4 --sse 100   # API + event streams
hack/loadtest/loadgen.sh s3fill --objects 1000000 --servers 50 --backups 30  # evidence-agent scans

# 4. Tear down (keeps the images and the download cache; PURGE=1 drops .state/).
hack/loadtest/cluster.sh down
```

`loadgen.sh` loads `.state/env` (kubeconfig, console, MinIO and metrics
endpoints) and runs `.state/loadgen`. `kubectl --kubeconfig hack/loadtest/.state/kubeconfig ...`
for anything else. `cluster.sh console` re-applies the console release after you
change `CONSOLE_MEM_LIMIT`, `INSTANCE_STATUS_INTERVAL`, ... in the environment.

## What each subcommand measures

| Command | Does | Reports |
|---|---|---|
| `populate --namespaces N --clusters C --instances I` | Per namespace C Clusters (status: healthy, primary `-1`, conditions, instance names), I Pods on KWOK nodes with the controller ownerRef, `postgres` container and `--status-port-tls`, a PV and PVC each, 3 Services, completed Backups, a ScheduledBackup and Events. Idempotent (re-run = no creates), parallel. | objects created/existing/failed, elapsed, objects/s, time until every Pod is Running with an IP |
| `churn --rate R --duration D [--mode timestamp\|condition\|phase]` | Merge-patches Cluster status at R/s over random Clusters. Every informer event becomes an SSE invalidation, so this drives the hub and the verdict recomputation. `phase` flips the phase away from healthy and back; the end state is restored. | achieved rate, errors, writes dropped because workers lagged, patch latency |
| `http --users U --duration D --mix list:detail [--sse S]` | Signs U ServiceAccounts in (`POST /auth/token`), **paced under the console's 10 logins/min**, then each user runs `--concurrency` workers on `GET /api/v1/clusters` and Cluster detail (`overview`, `/instances`, `/diagnostics`; `--detail-paths`). `--rps` makes it open loop. `--sse S` opens S `/api/v1/stream` streams (8 per session at most) and reconnects them like the SPA. `--same-user` shares one SelfSubjectAccessReview cache. | per endpoint: count, errors, req/s, p50/p95/p99/max, non-2xx codes; SSE events/s by type, reconnects, time to `hello`; `--json` file |
| `scrape [--every 5s]` | Reads console `/metrics` (bearer token of `loadtest`), operator `/metrics`, and `kubectl top` for the harness namespaces (metrics-server). | one line: cached objects by source, `sse_clients`, `sse_dropped`, SAR reviews and cache lookups, API request count, operator RSS/heap/goroutines, CPU and working set of every Pod |
| `s3fill --objects N` | Writes a Barman Cloud layout to MinIO: per server `base/<id>/backup.info` and `data.tar.gz`, and `wals/<16 hex>/<24 hex>.gz` segments; then lists the bucket as a scan does. The layout is held to the evidence agent's own parsers by a unit test. | objects/s written, full listing time |

## Limits to know before reading the numbers

* **The console exports no `go_*` or `process_*` metrics** (checked on `main`: `/metrics` carries only `pgtoolbox_console_*` and `certwatcher_*`). `scrape` therefore shows `rss=-` for it; memory is the working set from metrics-server (`top[pgtoolbox-console=...]`, 15 s resolution), and goroutines are unknown. The operator does export them.
* **Fake Pods are minimal.** KWOK sets phase, conditions, container statuses and the IP; no restarts, no real node facts. Backups carry no `ObjectStore` or archiving configuration, so the backup views show a Cluster without WAL archiving. `/pg/status` is a canned report (LSN advances 16 KiB/s) and the exporter a dozen series.
* **Services have no selectors by default** (`populate --service-selectors` adds them): with them the control plane builds EndpointSlices and kube-proxy programs iptables rules for every fake Pod on the real node, which loads the host the console is measured on.
* **Events expire after an hour** (kube-apiserver default), so the cached Event count falls during a long run; re-run `populate` to recreate them.
* **KWOK nodes carry a `/16` Pod CIDR each** (`10.(192+i).0.0/16`). Without one, the controller manager gives a node a `/24` (254 Pods). kindnet routes that CIDR through the KWOK controller Pod; the two `/17` routes cluster.sh adds on the node are more specific and win. After a restart of the node container run `cluster.sh routes`.
* **The cluster range is widened to `10.192.0.0/10`** (`kind.yaml`) so it holds those CIDRs: with kind's default `10.244.0.0/16` the controller manager's node-IPAM controller refuses them on its next restart and crash-loops, and no ReplicaSet is created any more. A busy control plane does restart it (seen at 4,000 Clusters and 1,000 writes/s). kindnet's memory limit is raised from 50Mi to 256Mi for the same reason: it watches every Pod and was OOM-killed with 12,000 fake ones.
* **The control plane is the ceiling for writes.** On a 16-CPU host the kind API server accepted about 410 Cluster status patches/s; `churn` reports the writes it had to drop.
* **MinIO lists faster than a cloud store.** It runs on the node's disk, so `s3fill` and evidence-agent scan times are a lower bound; S3, Azure and GCS answer each listing page over the network.
* `metrics-server` runs with `--kubelet-insecure-tls`; it logs scrape errors for the fake nodes, which have no kubelet.
* The operator's `/metrics` is HTTPS with a self-signed certificate; `scrape` skips verification only with `--operator-skip-verify` (`LOADTEST_OPERATOR_SKIP_VERIFY=1`, which `cluster.sh` writes into `.state/env` for its throwaway cluster); the token still decides.

## Settings

`cluster.sh` reads `NAMESPACES` (1..16), `KWOK_NODES` (1..32), `CONSOLE_IMG`,
`OPERATOR_IMG`, `BUILD_IMAGES=auto|always|never`, `CONSOLE_CPU_LIMIT`,
`CONSOLE_MEM_LIMIT` (default 2Gi, raised from the chart's 512Mi so the limit
you find is the console's and not the Pod's), `OPERATOR_CPU_LIMIT`,
`OPERATOR_MEM_LIMIT`, `INSTANCE_STATUS_INTERVAL`, `KWOK_VERSION` (v0.8.0) and
`METRICS_SERVER_VERSION` (v0.8.0). Downloads are cached in `.state/cache`.
