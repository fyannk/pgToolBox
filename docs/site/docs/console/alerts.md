---
sidebar_position: 17
title: MCP alerts and dashboard
---

# MCP alerts and dashboard

The [MCP endpoint](mcp.md) and its OAuth server export Prometheus
[metrics](metrics.md). The `pgtools` chart ships a Grafana dashboard and a
PrometheusRule over them. Both are off by default:

```yaml
console:
  metrics:
    serviceMonitor: {enabled: true}
    prometheusRule: {enabled: true, labels: {release: kube-prometheus-stack}}  # your ruleSelector
    grafanaDashboard: {enabled: true}                                           # the sidecar's label is set
```

The rules are the plain Prometheus file `charts/pgtools/files/alerts/mcp.rules.yaml`,
so they also load into a Prometheus without the operator. `make rules-test` checks
them with `promtool` against `hack/rules/mcp.rules.test.yaml`.

## The dashboard

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

## Metrics

| Metric | Labels |
|---|---|
| `pgtoolbox_console_mcp_calls_total` | `tool`, `outcome` |
| `pgtoolbox_console_mcp_call_duration_seconds` | `tool` |
| `pgtoolbox_console_mcp_result_bytes` (histogram, successful calls) | `tool` |
| `pgtoolbox_console_mcp_result_truncated_total` | `tool` |
| `pgtoolbox_console_mcp_requests_total` | `method`, `outcome` |
| `pgtoolbox_console_mcp_oauth_requests_total` | `endpoint`, `outcome` |

## Alerts

Severities follow the Prometheus Operator convention (`info`, `warning`,
`critical`). Each alert carries a `runbook_url` that points to its section here.
A ratio alert has a floor on the request rate, so a quiet console does not page
for one failed call.

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
