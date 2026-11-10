---
sidebar_position: 8
title: Prometheus charts
---

# Prometheus charts

Without configuration, the Metrics view charts a short ring of the readings the
console takes itself (`RING_RETENTION`, one hour by default). With
`PROMETHEUS_URL` set, the console builds the charts from Prometheus instead. The
tiles keep showing the latest readings, and `MONITORING_URL` stays the link-out
to your dashboards.

| Variable | Default | |
|---|---|---|
| `PROMETHEUS_URL` | empty | Base of the Prometheus HTTP API, the part before `/api/v1`. Use `https`; `http` is accepted only without a token. |
| `PROMETHEUS_TOKEN_FILE` | empty | A bearer token file sent with every query and re-read when it rotates. Requires `https`. |
| `PROMETHEUS_CA_FILE` | system roots | The CA that signed the Prometheus server's certificate. |
| `PROMETHEUS_NAMESPACED` | `false` | Adds `namespace=<ns>` to every query. OpenShift's tenancy port needs it. |
| `PROMETHEUS_RANGE` | `1h` | The charts' window, from `5m` to `24h`. The console asks for about 120 points, and never steps below 15 s. |

## What the console asks

The console sends one fixed range query per charted series. Each query is scoped
to one Cluster's instance Pods:

```
sum by (pod) (rate(cnpg_pg_stat_database_xact_commit_total{namespace="team-a",pod=~"orders-[0-9]+"}[5m]))
```

Nobody's text reaches Prometheus. Only people who may `get` the Cluster can open
its Metrics view. The console caches each Cluster's answers for 30 seconds, and concurrent viewers of one Cluster share a single run.

The scrape must keep the `namespace` and `pod` labels on every sample. The
Prometheus Operator's PodMonitor does this, whether CloudNativePG's
`monitoring.enablePodMonitor` creates it or you write your own. OpenShift
user-workload monitoring does too.

## Which identity

The console queries Prometheus as **itself**, never as the signed-in person. A
person's token never leaves for a third party.

Without `PROMETHEUS_TOKEN_FILE`, no credential is sent. That suits a Prometheus
reachable only from inside the namespace or through your NetworkPolicy.

With a token, mount a projected ServiceAccount token and point
`PROMETHEUS_TOKEN_FILE` at it. A token for the API server's audience (the default)
lets whatever receives it act as the console's ServiceAccount against the API
server. When your Prometheus front lets you configure the accepted audience
(kube-rbac-proxy `--auth-token-audiences`), use a dedicated audience instead.

On a `PgToolBoxConsole`, `spec.prometheus.projectedToken: true` mounts that token
for you, and `spec.prometheus.tokenAudience` sets its audience. Omitted, the token
carries the API server's audience, which OpenShift's thanos-querier expects. The
operator's admission policy treats the projected token as a credential: setting
`projectedToken`, or changing the `url`, the `caConfigMapRef` or the
`tokenAudience` it goes with, requires `create` on `serviceaccounts/token` for the
console's ServiceAccount ([security](../security.md#secret-references-need-get)).

### OpenShift

On OpenShift, query `thanos-querier` on its tenancy port:

```
PROMETHEUS_URL=https://thanos-querier.openshift-monitoring.svc:9092
PROMETHEUS_NAMESPACED=true
PROMETHEUS_CA_FILE=/etc/prometheus-ca/service-ca.crt   # the injected service CA bundle
PROMETHEUS_TOKEN_FILE=/var/run/secrets/kubernetes.io/serviceaccount/token
```

The tenancy port authorizes each query for the namespace it names. Bind the
console's ServiceAccount to the `view` ClusterRole in each watched namespace with
a RoleBinding. Check the requirement against the monitoring documentation of your
OpenShift version; the console was not tested against a live thanos-querier.

### NetworkPolicy

With the console's [NetworkPolicy](networkpolicy.md) enabled, add the Prometheus
endpoint to `networkPolicy.extraEgress`. Without that rule, every query fails.

## When Prometheus does not answer

The view says so and falls back to the ring's charts. `chartSource` is `ring`,
and the `prometheus` source state gives the reason:

- unreachable
- refused the console's identity (HTTP 401 or 403)
- answer too large, malformed, or an error status
- holds no series for this Cluster's Pods, which usually means a scrape without
  the `namespace` and `pod` labels

If only some series fail, the charts that answered stay, and the state reports
the failure.

Answers are bounded to 4 MiB, 64 Pods and 1441 points per Pod. A missing or
non-finite sample is a gap, never a zero.
