---
sidebar_position: 7
title: Metrics
---

# Metrics

`/metrics` is served on the **probe port** (`console.probePort`, `3001`) beside
`/healthz` and `/readyz`, and is protected: the caller's bearer token is checked
with a `SelfSubjectReview` and then a `SelfSubjectAccessReview` (sent with that
token) for `get` on the
non-resource URL `/metrics`.

The probe port follows the console's TLS setting. With `TLS_CERT_FILE` and
`TLS_KEY_FILE` set (`console.tls.secretName`), it is **HTTPS with the same
certificate** as the main listener, probes included (the kubelet does not verify
the certificate; the chart switches the probes to `scheme: HTTPS`). Without them
it is plain HTTP, like the main listener. A scraper then uses `scheme: https` and
trusts the CA that issued the console's certificate.

## Scraping

1. Render the reader role: `console.metrics.readerClusterRole=true`. It is a
   ClusterRole allowing `get` on `/metrics`, **not** part of the console's own
   authority; the chart binds it only to `console.metrics.readerSubjects`
   (for example `{name: prometheus-k8s, namespace: monitoring}`).
2. Scrape with `console.metrics.serviceMonitor.enabled` (it sends the scraper's
   ServiceAccount token) or `console.metrics.podMonitor.enabled`. A PodMonitor
   cannot send a token, so prefer the ServiceMonitor.
3. With the [NetworkPolicy](networkpolicy.md) on, list the scraper's namespace in
   `networkPolicy.ingress.metricsFrom`.

The monitors are rendered and unit-tested but never scraped in CI (no Prometheus
Operator in the test clusters).

## What it exports

| Metric | Meaning |
|---|---|
| `pgtoolbox_console_build_info`, `pgtoolbox_console_config_info` | Version and Go version; one series per effective setting (secrets by file path, never value). |
| `pgtoolbox_console_api_request_duration_seconds` | API latency by operation and status class. |
| `pgtoolbox_console_mcp_calls_total{tool,outcome}`, `pgtoolbox_console_mcp_call_duration_seconds{tool}` | [MCP](mcp.md) tool calls by outcome (`ok`, a status such as `403`, `timeout`, `error`, `unauthenticated`) and their duration; `pgtoolbox_console_mcp_requests_total{method,outcome}` counts the other MCP requests (resource reads, prompts, lists). |
| `pgtoolbox_console_mcp_result_bytes{tool}`, `pgtoolbox_console_mcp_result_truncated_total{tool}` | Size of the successful MCP results, and how many were cut to the response cap (`MCP_MAX_RESULT_BYTES`). |
| `pgtoolbox_console_mcp_oauth_requests_total{endpoint,outcome}` | Requests to the MCP authorization server by endpoint (`register`, `authorize`, `token`, `revoke`, `client_metadata`) and outcome. |
| `pgtoolbox_console_logins_total{provider,outcome}` | Sign-in attempts. |
| `pgtoolbox_console_session_rejections_total{reason}` | Refused sessions: `expired`, `revoked`, `token_rejected`, `malformed`. |
| `pgtoolbox_console_token_refreshes_total{outcome}` | OIDC refreshes. |
| `pgtoolbox_console_revocation_list_valid` | `0` when the last revocation list was refused (the previous one stays in force). |
| `pgtoolbox_console_sar_reviews_total{outcome}`, `pgtoolbox_console_sar_review_duration_seconds`, `pgtoolbox_console_sar_cache_lookups_total{result}` | Access reviews sent (SelfSubjectAccessReviews, with the asker's token), their latency, and the positive-answer cache. |
| `pgtoolbox_console_cache_informer_calls_total{source,call,outcome}`, `pgtoolbox_console_cached_objects{source}` | Informer list/watch calls (`ok`, `forbidden`, `not_found`, `error`) and what the cache holds. |
| `pgtoolbox_console_evidence_fetches_total{outcome}` | Evidence agent fetches: `success`, `stale_epoch`, or a closed failure kind. |
| `pgtoolbox_console_evidence_agent_status{namespace,name,state}` | Per EvidenceAgent: `1` for its current evidence state (`pending`, `live`, `stale`, `unavailable`), `0` for the others. Series disappear with the agent. Alert on `state="unavailable"` or `state="stale"` held for longer than the refresh interval. |
| `pgtoolbox_console_sse_clients`, `pgtoolbox_console_sse_dropped_events_total` | Open live streams; invalidations dropped for a slow client (each answered by a resync event). |
| `pgtoolbox_console_runner_restarts_total{runner}` | Restarts of background runners. |
| `pgtoolbox_console_state_snapshot_last_success_timestamp_seconds{part}`, `pgtoolbox_console_state_snapshot_bytes{part}`, `pgtoolbox_console_state_snapshot_errors_total{part}` | The [state snapshots](configuration.md#what-survives-a-restart) beside a persistent history: when each part was last written, its size, and failed writes. Alert on an age past a few minutes. |
| `pgtoolbox_console_shared_state_writes_total{outcome}`, `pgtoolbox_console_shared_state_mode{mode}` | Writes of the state ConfigMap (`written`, `conflict`, `failed`) and its mode (`1` for the current one of `pending`, `live`, `missing`, `forbidden`, `failing`). |
| `pgtoolbox_console_digest_leader` | `1` on the replica holding the [digest Lease](timeline-and-digest.md#daily-digest), the one that delivers, else `0`. Summed over the replicas it is `1`; alert when it stays `0` with a delivery channel set (the Lease is forbidden or failing: no digest is sent). |
| `pgtoolbox_console_volume_trend_series`, `pgtoolbox_console_volume_trend_refused_total` | Volume series the trend store holds, and new ones turned away at `VOLUME_TREND_MAX_SERIES` (those volumes have no forecast; alert on any increase). |

The MCP series have a [dashboard and alerts](alerts.md) in the `pgtools` chart.

`forbidden` informer outcomes are the quickest way to see a Role that does not
match what the console expects.
