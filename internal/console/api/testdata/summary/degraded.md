**Cluster `billing` in namespace `team-a`: critical**

- Verdict: critical (rests on a stale source)
- Phase: Failing over
- Console: https://console.example.com/c/team-a/billing
- Rendered: 2026-10-29T12:00:00Z (UTC); the view was assembled 0s ago

**Why**
- critical · Phase: Failing over (operator, Cluster.status.phase, read 2s ago)
- unknown · Pods: stale for 4m (kubernetes, Pod, read 4m ago, stale: pod watch lost: connection refused)
- warning · Instances ready: 1 of 2 (operator, Cluster.status.readyInstances, read 2s ago)

**Findings** (diagnostics run 2026-10-29T11:59:45Z, 15s ago; 2 checks could not run)
- critical · `pod-crashloop` · Instance billing-1 is restarting in a loop. Source: Kubernetes `Pod/billing-1`. Reported since 2026-10-29T11:58:20Z (1m40s ago).
  - warning · `cnpg-1.30/phase-failing-over` · The operator is failing over. (follows: same cluster · established) Source: operator `Cluster/billing`. Reported since 2026-10-29T11:58:25Z (1m35s ago).
- note · `restore-drill-overdue` · No restore drill run from this console has passed for this Cluster in the last 90 days. Source: console-recorded `restore drill record`. Reported since 2026-10-20T12:00:00Z (9d ago).

**Sources not live** (what rests on them is unknown, not healthy)
- `pods`: stale since 2026-10-29T11:56:00Z (4m ago): pod watch lost: connection refused
- `failoverQuorums`: not installed since 2026-10-29T09:00:00Z (3h ago): failoverquorums.postgresql.cnpg.io is not served by this cluster
