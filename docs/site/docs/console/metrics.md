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
| `pgtoolbox_console_throttled_requests_total{bucket}` | Requests refused by a budget ([throttled requests](#throttled-requests)). |
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
| `pgtoolbox_console_shared_state_writes_total{outcome}`, `pgtoolbox_console_shared_state_mode{mode}` | Writes of the state ConfigMap (`written`, `conflict`, `failed`) and its mode (`1` for the current one of `pending`, `live`, `missing`, `forbidden`, `failing`). Alert on `increase(...{outcome="failed"}[10m]) > 0` or a mode other than `live` held for ten minutes: sign-outs and spent MCP codes stop replicating and MCP token requests are refused. |
| `pgtoolbox_console_shared_state_bytes{document}`, `pgtoolbox_console_shared_state_dropped_total{document}`, `pgtoolbox_console_shared_state_signouts_refused_total{reason}`, `pgtoolbox_console_shared_state_signouts{bound}` | The size of each document of the state ConfigMap, and `total`: its whole data, keys and values, every document counted (the API server refuses a ConfigMap past 1 MiB; the console drops the oldest "since" times when the sessions, digests and since documents pass 768 KiB together or when the whole would pass 1,032,192 bytes, so they give way to every other document and take the room it leaves, and keeps the whole within 1,032,192 bytes by then dropping the oldest restore drill results, each source's newest pass last: both counted in `dropped_total`, by document); sign-outs the sessions document did not keep, each still refused by its own replica: `replaced` (a person at 32 entries, 16 sign-outs, or holding as much as the most in a full document, gave up their own earliest-ending one for the new one), `unnamed` (sessions that name nobody, 128 entries together), `evicted` (the document full at 1,536 entries: a newcomer took the place of the entry recorded longest ago of the person holding the most), `budget` (the document full and the new sign-out's person holding as much as the most with nothing of their own to give up), `merge` (two replicas filled the last place, or replaced the same entry, at once; logged once); and how full each bound is (`subject`: the most one person holds, `unnamed`, `total`). `merge` is counted by every replica that merges the drop, so its sum across replicas overstates the sign-outs lost (with three replicas, up to three times); read it per replica, or take the maximum. Alert on any `budget`, `merge` or `dropped_total{document="drills.json"}` increase; a steady `evicted` rate means some identities keep the document full: put a stolen session's person on the revocation list's `users` key ([revoking sessions](authentication.md#revoking-sessions)), which no identity can fill. |
| `pgtoolbox_console_shared_state_foreign_documents{document}` | `1` for each document of the state ConfigMap this replica leaves as it is, neither merged nor rewritten: written in a newer format version than it reads (during a rolling upgrade, or after a rollback), or, for `grants.json`, unreadable. What such a document holds is not shared with this replica (a sign-out in it is not refused here; MCP grants are refused over a foreign `grants.json`), and each one is logged once. A document of an older version is read and rewritten at the current one, so it is never counted. A newer document is taken over once no console of another version has written the ConfigMap for 30 minutes, counted on each replica's own clock from when it last saw such a write (after a rollback; see [the state ConfigMap](configuration.md#what-survives-a-restart)); to recover at once, delete the key from the ConfigMap (what it holds is lost: for `grants.json`, every MCP assistant connects again). Alert when it stays `1` for longer than a rollout plus 30 minutes: replicas of two versions are running, or the document does not parse. |
| `pgtoolbox_console_digest_leader` | `1` on the replica holding the [digest Lease](timeline-and-digest.md#daily-digest), the one that delivers, else `0`. Summed over the replicas it is `1`; alert when it stays `0` with a delivery channel set (the Lease is forbidden or failing: no digest is sent). |
| `pgtoolbox_console_volume_trend_series`, `pgtoolbox_console_volume_trend_refused_total` | Volume series the trend store holds, and new ones turned away at `VOLUME_TREND_MAX_SERIES` (those volumes have no forecast; alert on any increase). |

The MCP series have a [dashboard and alerts](alerts.md) in the `pgtools` chart.

## Throttled requests

`pgtoolbox_console_throttled_requests_total{bucket}` counts the requests a
budget refused, with a fixed set of `bucket` values (never anything a request
carries):

| `bucket` | Budget |
|---|---|
| `sign_in_address` | OAuth callbacks and unrecognised MCP bearers, per client address |
| `token_form_token`, `token_form_address` | The token form, per pasted token and per client address |
| `code_exchanges` | OAuth code exchanges with the IdP, per replica |
| `exchange_reviews` | Reviews of tokens the IdP has just issued, per replica |
| `exchange_reviews_subject` | The same, per person the IdP vouched for (OIDC: 12 a minute, bursts of 6), charged first |
| `token_reviews` | Reviews of tokens the replica never accepted (token form, raw MCP bearers), per replica |
| `known_reviews` | Re-reviews of raw MCP bearers the replica accepted before, per replica |
| `known_reviews_subject` | The same, per person the API server named (32 a minute, bursts of 16), charged first |
| `mcp_calls`, `mcp_attempts`, `mcp_writes`, `mcp_page_views`, `mcp_page_answers` | The [MCP endpoint's budgets](mcp.md#budgets) |
| `oauth_requests`, `oauth_registry`, `oauth_tokens`, `oauth_unknown_tokens`, `oauth_grant_tokens`, `oauth_subject_tokens`, `oauth_registrations`, `oauth_decisions` | The MCP authorization server's budgets (`oauth_grant_tokens`: per grant; `oauth_subject_tokens`: per person) |
| `metrics_scrapes` | `/metrics` per client address |

A per-address bucket rising is one client retrying. A per-replica bucket
rising (`code_exchanges`, `token_reviews`, `oauth_unknown_tokens`,
`oauth_registrations`) is a flood that new sign-ins on that replica are now
waiting behind; `exchange_reviews`, `known_reviews` or `oauth_tokens` rising
means genuine sign-ins or clients themselves are being refused. A starting
point for an alert:

```promql
sum by (namespace, bucket) (
  rate(pgtoolbox_console_throttled_requests_total{bucket=~"code_exchanges|exchange_reviews|token_reviews|known_reviews|oauth_tokens"}[5m])
) > 1
```

held for ten minutes. What such a flood can and cannot do is in
[security](../security.md).

`forbidden` informer outcomes are the quickest way to see a Role that does not
match what the console expects.
