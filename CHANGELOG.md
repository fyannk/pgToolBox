# Changelog

All notable changes to pgToolBox are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[semantic versioning](https://semver.org/). Before 1.0.0, and while the custom
resources are `v1alpha1`, a field can change between minor releases; every such
change is announced here. Pin an exact image tag, or better a digest, and read
the notes before upgrading.

## [0.1.0] - Unreleased

The first public release: a console, an optional operator and an evidence agent
for [CloudNativePG](https://cloudnative-pg.io) 1.30 and later, on Kubernetes and
OpenShift.

### Added

- **The console**, installed once per team namespace by the `pgtools` chart or
  declared as a `PgToolBoxConsole`. It shows the CloudNativePG Clusters of the
  namespaces it watches: an overview with the sources and blind spots each verdict
  rests on, instances as the operator, the Pod and the instance's own `/pg/status`
  report them, replication and a failover rehearsal, storage with a forecast of
  when each volume fills, backups next to the repository evidence, diagnostics
  and triage, events, a revision history with structural diffs, an incident
  timeline, logs, metrics (built-in or from Prometheus), the declarative objects,
  the topology, and two Clusters side by side.
- **Authorization by the API server.** People sign in with OIDC, OpenShift OAuth
  or a pasted API-server token; every read is checked with a
  SelfSubjectAccessReview sent with the person's own token. The console stores
  no level, role or group of its own.
- **Four operations** (backup, reload, restart, promote), off unless enabled,
  each sent to the API server with the signed-in person's token, plus opt-in
  restore drills that restore the latest backup into a throwaway Cluster, check
  it and delete it.
- **Access requests** (`PgAccessRequest`): a person asks for a role in the
  console, someone who may grant it approves, and the grant is a RoleBinding the
  operator removes when the request expires or is revoked.
- **An MCP endpoint** (`/mcp`, off by default, read-only unless its write tools
  are enabled) that serves the same facts as the screens to AI agents, as the
  token's owner.
- **A daily digest**, alerting rules and Grafana dashboards, the insight pack
  (PostgreSQL metrics from the instances' exporter), and OpenTelemetry tracing.
- **The operator** (optional), reconciling `PgAdmin` (one pgAdmin per team,
  registered against many Clusters, with libpq credential files written by a
  sidecar), `EvidenceAgent`, `PgToolBoxConsole` and `PgAccessRequest`, and honouring
  `PgAdminAccess` grants across namespaces. It never writes to a CloudNativePG
  `Cluster` and puts no secret material in status, events or logs.
- **The evidence agent**, a read-only reader of backup repositories: the Barman
  Cloud layout on S3, Azure Blob Storage and Google Cloud Storage, and the
  pgBackRest layout (repository format 5) for Clusters backed up through the
  Dalibo or Opera CNPG-I plugin. It reports what the repository actually holds,
  with conservative roll-ups, so the console can compare it with what
  CloudNativePG claims. It is the only component that holds object-store
  credentials.
- **Install paths**: the `pgtoolbox` chart (the operator and its CRDs), the
  `pgtools` chart (a team's console, evidence agents and pgAdmin, without the
  operator), kustomize, and an OLM bundle and catalog. Images for `linux/amd64`
  and `linux/arm64`: `ghcr.io/fyannk/pgtoolbox-operator`,
  `ghcr.io/fyannk/pgtoolbox-console` and `ghcr.io/fyannk/pgtoolbox-evidence-agent`.
- The custom resources are `pgtoolbox.fyannk.dev/v1alpha1`.
- **Opening a pgAdmin** behind the OpenShift OAuth proxy takes the `connect`
  verb on `pgadmins/<name>` (granted by `pgtoolbox-console-operator` and by
  `admin`), not `get`, which OLM gives to every `view` user.
- **Release candidates** (`v0.1.0-rc.N`) publish end to end: the bundle, catalog
  and installer manifests are pinned by digest from the tree's core version, a
  pin that changes nothing fails the release, and `latest` moves only after the
  packages are verified public.
- **Release verification.** The notes of the final tag list every commit since the
  previous stable tag (release candidates included); the OLM bundle and catalog
  images are scanned on every pull request and push, not first in the release; the
  release rehearsal pins the bundle and catalog on every run; after the prune, and
  weekly, `verify-release.yml` pulls every image and both charts with no
  credentials and verifies signatures and attestations. A failed candidate is
  superseded by the next one (`-rc.N+1`), never moved. The images are not claimed
  to be bit-for-bit reproducible. The catalog image builds opm and
  `grpc_health_probe` from source (opm v1.74.0) with the repository's Go and
  dependency versions, since the upstream opm image ships fixable high
  vulnerabilities.
- **Release gates that hold on every commit.** Merges made by auto-merge start no
  workflow of their own, so `automerge.yml` now dispatches the full `ci.yml` and
  `security.yml` on `main` for a commit that has no run (after the merge, and every
  30 minutes as a net); `make release-preflight TAG=vX.Y.Z` checks the dated
  CHANGELOG, the annotated tag and green `ci-ok` and `security-gate` on the tagged
  commit before the tag is pushed. A nested-module release is never marked latest, a
  release candidate never is either, and the product's stable release is. The
  signature and attestation check of each image is retried, `npm ci` and `pnpm
  install` in the image build are retried, and the anonymous verification still runs
  when the prune fails. The envtest suites run on Kubernetes 1.35 and 1.36 in the
  nightly and dispatched runs (1.34 on every change). Dependabot proposes the
  Playwright npm packages as one group, and the e2e image moves with them.
- **The restore check without an evidence agent** reads the primary's last
  archived WAL time: "may well be possible" only while archiving is healthy, and
  "likely not in the repository" when it fails. With an agent, a time between
  its WAL frontier and its scan is unknown, not "not covered". Restore-check and
  recovery-window sentences are translated; the Overview shows the last archived
  WAL; the failover data-loss answer shows the age of its sweep and is unknown
  when the sweep is stale.
- **Backup cadence** is read with CloudNativePG's own cron parser (robfig/cron
  v1.2.0), so descriptors, `@every`, lists, ranges and day-restricted schedules
  are checked. On a CloudNativePG minor whose phase table was not verified, a
  healthy phase reads as a warning. Restore-drill pruning keeps the newest pass
  of a recreated Cluster.
- **Admission policy**: an EvidenceAgent's `allowedSecrets` needs `get` on those
  Secrets; a kept Secret reference whose destination changes is checked again;
  a PgAdmin that offers Cluster credentials needs `get` on the namespace's
  Secrets. A kustomize component (`config/components/admission`) ships the same
  policies; the OLM bundle has none, as its page says. On OpenShift, pgAdmin's
  OAuth proxy sessions last one hour.
- **Recovery evidence follows `recovery_target_timeline 'latest'`** as PostgreSQL
  resolves it, in both formats: a missing middle history file, or an unreadable
  newest one, no longer reads as healthy coverage, and an off-target path names
  the timeline a restore must ask for. Snapshots carry `producer.features`; the
  console takes no coverage from an agent without `recovery-timeline-latest`, or
  whose timeline evidence is not healthy.
- **Client addresses behind a proxy**: `X-Forwarded-For` is read only from peers
  in `TRUSTED_PROXY_CIDRS`; hops without CIDRs ignore it, so a pod in the cluster
  cannot forge its address. `spec.exposure.trustedProxy` sets hops and CIDRs, and
  an exposed console gets an ingress NetworkPolicy (by default behind a Route,
  with `spec.exposure.networkPolicy.from` behind an Ingress or Gateway), reported
  by the advisory `ExposureConfined` condition. MCP bearer reviews share the
  process-wide review budget, streams keep their feed through the shutdown
  drain, and the console warns at start about hops 0 behind a public URL.
- **Sign-in budgets behind a shared proxy address.** Behind a proxy declared
  without `TRUSTED_PROXY_CIDRS`, junk callbacks on self-started OIDC flows, junk
  pasted tokens, or floods of the MCP and OAuth endpoints no longer lock everyone
  out: sign-ins are bounded per flow, per token, per person and per replica (20/s,
  burst 40) instead of per shared address. The docs say what stays open after an
  access request expires (the console's caches, the pgAdmin session, open
  connections) and that the four-eyes check covers only the console's path.
- **Exposure trust is confined, or withdrawn.** A PgToolBoxConsole trusts every
  peer's `X-Forwarded-For` only while no other NetworkPolicy or broad `from` peer
  reopens its port (new reasons `ExposurePolicyBypassed`, `ExposurePeersBroad`),
  and keeps its ingress policy until the rollout to a narrower trust completes.
  `networkPolicy.from` peers are checked by CEL and, on a Route, add to the
  OpenShift routers; the sample names ingress-nginx. The `pgtools` chart's
  policies admit host-network routers by their policy-group label. `PgAdmin`
  `spec.auth.openshiftOAuthProxy.sessionMaxAge` (5m to 12h, default 1h) sets the
  pgAdmin session lifetime.
- **Admission policy, round four.** Changing the sign-in settings (`spec.auth`,
  `spec.admins`) of a PgAdmin that offers Cluster credentials needs `get` on the
  namespace's Secrets; CA ConfigMaps, the SMTP TLS mode and an EvidenceAgent's
  Cluster, ObjectStore and pgBackRest endpoint count as part of a Secret's
  destination; pointing `spec.prometheus.projectedToken` at a new URL needs
  `create` on the console's `serviceaccounts/token`. `spec.prometheus.tokenAudience`
  gives that token a dedicated audience.
- **Recovery evidence follows the timeline ancestry.** Barman Cloud no longer
  shows a sibling timeline as covered by a default restore: a sibling path is off
  target with "needs targetTLI N", which the recovery panel and restore check
  show. The console caps paths at the agent's own coverage conclusion and requires
  agents with `recovery-timeline-ancestry`; a malformed history no default restore
  reads is a warning naming the file instead of hiding all coverage; an absent
  history of the lowest timeline is no longer a gap; the pgBackRest WAL segment
  size is inferred from segment names before 16 MiB is assumed.
- **An archive backlog is not "healthy".** Archiving shows Warning
  `ArchiveBacklog` once segments wait, and recovery screens widen the newest edge
  and name the count. The restore check says archiving is failing after the
  agent's last read, keeps archive failures across a failover, reads the archiver
  from `currentPrimary` only, clamps the instance clock, and adds an end-of-WAL
  band advising "restore to end of WAL". Phase and condition caps cover undetected
  or unverified CloudNativePG minors and say why; recovery and timeline reasons
  are translated.

[0.1.0]: https://github.com/fyannk/pgToolBox/releases/tag/v0.1.0
