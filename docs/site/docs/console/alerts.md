---
sidebar_position: 17
title: Alerts and dashboard
---

# Alerts and dashboard

The console exports Prometheus [metrics](metrics.md). The `pgtools` chart ships
two PrometheusRules over them, one for the [MCP endpoint](mcp.md) and its OAuth
server, one for the [diagnostics findings](#findings), and a Grafana dashboard
for MCP. All are off by default:

```yaml
console:
  metrics:
    serviceMonitor: {enabled: true}
    prometheusRule: {enabled: true, labels: {release: kube-prometheus-stack}}  # your ruleSelector
    grafanaDashboard: {enabled: true}                                           # the sidecar's label is set
```

The rules are the plain Prometheus files `charts/pgtools/files/alerts/mcp.rules.yaml`
and `charts/pgtools/files/alerts/findings.rules.yaml`, so they also load into a
Prometheus without the operator. `make rules-test` checks them with `promtool`
against `hack/rules/mcp.rules.test.yaml` and `hack/rules/findings.rules.test.yaml`.

## MCP dashboard

"pgToolBox console: MCP / AI assistants" has four rows, filtered by namespace:

- **Tool calls**: calls per second by tool and by outcome, p95 duration by tool,
  error and timeout ratio.
- **Results and refusals**: share of results cut to the response cap and p95
  result size by tool, denied (`401`, `403`) and `unauthenticated` calls, the
  other MCP requests (resources, prompts, lists).
- **Write tools**: confirmations asked (`confirmation_requested`), declined,
  unconfirmed and done (`ok`) for `backup`, `reload`, `restart` and `promote`,
  and the share of confirmations that ended in an operation.
- **OAuth server**: token endpoint outcomes and failure ratio, authorize
  outcomes (`consent`, `approved`, `denied`), registration, client metadata
  and revocation.

The OAuth counter does not separate the authorization-code grant from the refresh
grant: both count as `endpoint="token"`, `outcome="ok"` when a token is issued.

## MCP metrics

| Metric | Labels |
|---|---|
| `pgtoolbox_console_mcp_calls_total` | `tool`, `outcome` |
| `pgtoolbox_console_mcp_call_duration_seconds` | `tool` |
| `pgtoolbox_console_mcp_result_bytes` (histogram, successful calls) | `tool` |
| `pgtoolbox_console_mcp_result_truncated_total` | `tool` |
| `pgtoolbox_console_mcp_requests_total` | `method`, `outcome` |
| `pgtoolbox_console_mcp_oauth_requests_total` | `endpoint`, `outcome` |

## MCP alerts

Severities follow the Prometheus Operator convention (`info`, `warning`,
`critical`). Each alert carries a `runbook_url` that points to its section here.
A ratio alert has a floor on the request rate, so a quiet console does not page
for one failed call.

These rules read the MCP Pod's own series, so none fires while that Pod is
down: one MCP Pod that crash-loops or stays Pending leaves no series and no
alert. Its availability is left to the cluster's Deployment alerts, for example
kube-state-metrics' `kube_deployment_status_replicas_available{deployment=~".*-mcp"} == 0`
(kube-prometheus' `KubeDeploymentReplicasMismatch`); with the operator, the
console's `WorkloadReady` condition also reports it.

### PgToolBoxConsoleMCPHighErrorRatio

Severity `warning`. More than 10% of the tool calls of a namespace ended in
`error`, `timeout` or a 5xx status for 15 minutes (and at least one call every
20 seconds). Refusals (`401`, `403`), other 4xx statuses and the write-tool
handshake do not count.

Look at the dashboard's "Tool calls per second by outcome" and the "Error and
timeout ratio" panel to find the tool, then at the console log lines named
`mcp.call` for the outcome and the reason. A `5xx` status with every tool failing
usually means the Kubernetes API server or the cache behind the console is
unhealthy; one tool failing points at the Cluster it reads.

### PgToolBoxConsoleMCPCallTimeouts

Severity `warning`. At least three calls to one tool ran past the call timeout
(`MCP_CALL_TIMEOUT`, 20 s by default) in 15 minutes.

The tool is in the alert's labels. Raising the timeout is rarely the answer: the
assistant waits for it. Check the API server latency, `pgtoolbox_console_sar_review_duration_seconds`,
and whether the tool is asked for a very large Cluster or a long log window.

### PgToolBoxConsoleMCPUnauthenticatedCalls

Severity `warning`. A tool call reached the handler without a principal. The
bearer check in front of the endpoint refuses such requests first, so this means
a defect or a bypass, not an expired token (those never get that far and show up
as a 401 on the HTTP side). Collect the `mcp.call` lines with `outcome=unauthenticated`
and report them; treat the endpoint as exposed until explained.

### PgToolBoxConsoleMCPResultsTruncated

Severity `info`. More than 25% of the successful results of one tool were cut to
fit the response cap (`MCP_MAX_RESULT_BYTES`) for an hour. The assistant reads
the cut result with its `truncated` marker, but it works from less than it
should. Raise the cap, or tell the assistants to narrow their questions (a
namespace, a Cluster, a shorter window). The "p95 result size by tool" panel
says by how much.

### PgToolBoxConsoleMCPOAuthTokenFailures

Severity `warning`. More than 25% of the requests to the token endpoint failed
for 15 minutes. The outcome label says why: `refused` (a bad or unknown code,
refresh token or verifier), `revoked` (the grant or its sign-in is revoked),
`replayed` (a code used twice), `throttled` (the rate limit), `busy` (too many
sign-ins in progress) or `error` (the console could not record the grant).

A burst of `revoked` after a console restart without a persistent session secret
or a shared state ConfigMap means the grants were lost: assistants have to connect
again. `error` points at the state store; `throttled` at a client that retries in
a loop.

### PgToolBoxConsoleMCPOAuthCodeReplayed

Severity `warning`. An authorization code was redeemed a second time. The console
revoked the grant the first redemption bought, as the OAuth rules require. A stolen
code is possible; so is a client that retried a token request after a timeout.
Check the `mcp.token` audit lines for the client and the person, and ask them to
reconnect.

## Findings

The [diagnostics](../guides/diagnostics.md) the console runs in the background
for every Cluster it watches are exported at scrape time from each Cluster's
latest run, so alerting on them does not need anyone to open the console.

| Metric | Labels | Value |
|---|---|---|
| `pgtoolbox_finding` | `cluster_namespace`, `cluster`, `check`, `severity` | `1` while the check reports a finding of that severity (`note`, `warning`, `high`, `critical`); no series otherwise |
| `pgtoolbox_finding_checks` | `cluster_namespace`, `cluster`, `outcome` | How many checks of the latest run ended `matched`, `clear`, `unavailable` (input absent, forbidden, stale or not observed yet), `off` (the source is switched off, such as log following or a metrics scrape) or `not_applicable` (outside the check's version pins); all five always exported |
| `pgtoolbox_finding_run_failed` | `cluster_namespace`, `cluster` | `1` when the latest run failed (it then reports no finding and no check) |
| `pgtoolbox_finding_run_timestamp_seconds` | `cluster_namespace`, `cluster` | When the latest run was made |

`check` is the check's stable name from [Checks and playbooks](../reference/checks.md);
the finding's text and evidence stay in the console. The Cluster's namespace is
`cluster_namespace` because the scrape adds the console's own namespace as
`namespace`.

**Silence is not health.** A check that could not run reports no finding, and
neither does a failed run or a console that stopped running them. Read
`pgtoolbox_finding` together with `pgtoolbox_finding_checks{outcome="unavailable"}`,
`pgtoolbox_finding_run_failed` and the run's age; the alerts below do. With
`DIAGNOSTICS_ENABLED=false` none of these series exist, and a console that is
not scraped exports nothing at all: keep a target-down alert on the console
(kube-prometheus' `TargetDown`, or `up == 0` on its job).

**Cardinality.** A finding series exists only while its finding is active; one
per check and severity, however many pods or backups the check names. Per
watched Cluster there are at most as many finding series as checks with a
matched finding, plus seven fixed series (five outcomes, the failure flag and
the timestamp). The Clusters are those the console's scope already bounds
(its namespaces); every replica, and the MCP Pod when it runs, exports its own
copy, which the alerts collapse with `max`. Series go with their Cluster.

**Who sees them.** The series name every watched Cluster and its active checks.
The `/metrics` reader role is not checked per Cluster the way the diagnostics
view is (`get clusters`): bind it only to the scraper
(`console.metrics.readerSubjects`).

The finding alerts carry the finding's severity mapped to the Prometheus
Operator's: `critical` alerts as `critical`, `high` as `warning`, `warning` as
`info`; a `note` does not alert.

### PgToolBoxFindingCritical

Severity `critical`. A check reports a critical finding (PostgreSQL not serving
or refusing writes, data unusable or being lost, recovery impossible) for 5
minutes, on any replica. The `check` label names the check; open the Cluster's
diagnostics in the console for the evidence it quotes and the next steps, and
the check's entry in [Checks and playbooks](../reference/checks.md).

### PgToolBoxFindingHigh

Severity `warning`. A check reports a high finding (a protection or a redundancy
gone: archiving, backups, a replica able to take over, while PostgreSQL still
serves) for 15 minutes. Left alone it becomes critical.

### PgToolBoxFindingWarning

Severity `info`. A check reports a warning for an hour.

### PgToolBoxFindingChecksUnavailable

Severity `warning`. At least one check of a Cluster could not run for 30
minutes: its input is absent, forbidden or stale, so the console cannot say
that what it would find is clear. Checks whose source is switched off on
purpose (`outcome="off"`) do not count. The Cluster's diagnostics page lists
each check that could not run and why; a `forbidden` in
`pgtoolbox_console_cache_informer_calls_total` usually names the missing
permission.

### PgToolBoxFindingRunFailed

Severity `warning`. Every diagnostics run of a Cluster failed for 15 minutes,
so it has no finding at all whatever its state. The console's log has the
error (`diagnostics run panicked`); report it with the Cluster's CloudNativePG
version.

### PgToolBoxFindingRunsStale

Severity `warning`. One replica has not run the diagnostics of a Cluster for an
hour (the safety cadence, `DIAGNOSTICS_INTERVAL`, is at most 30 minutes). The
alert names the Pod: what it exports and shows is the past. Restart it, and
look at its log for a stuck runner (`pgtoolbox_console_runner_restarts_total`).
