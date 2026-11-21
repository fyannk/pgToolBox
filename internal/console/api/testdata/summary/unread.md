**Cluster `billing` in namespace `team-a`: critical**

- Verdict: critical (rests on a stale source)
- Phase: Failing over
- Rendered: 2026-10-29T12:00:00Z (UTC); the view was assembled 0s ago

**Why**
- critical · Phase: Failing over (operator, Cluster.status.phase, read 2s ago)
- unknown · Pods: stale for 4m (kubernetes, Pod, read 4m ago, stale: pod watch lost: connection refused)
- warning · Instances ready: 1 of 2 (operator, Cluster.status.readyInstances, read 2s ago)

**Findings**: the diagnostics could not be read; no finding is not a clean bill of health.

**Sources not live** (what rests on them is unknown, not healthy)
- `pods`: stale since 2026-10-29T11:56:00Z (4m ago): pod watch lost: connection refused
- `failoverQuorums`: not installed since 2026-10-29T09:00:00Z (3h ago): failoverquorums.postgresql.cnpg.io is not served by this cluster
