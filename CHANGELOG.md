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
  operator), kustomize, and an OLM bundle and catalog. With
  `operator.config.watchNamespaces`, the Helm chart binds the operator's
  namespaced rules (Secrets included) per namespace, keeping only cluster-scoped
  rules cluster-wide. Images for `linux/amd64`
  and `linux/arm64`: `ghcr.io/fyannk/pgtoolbox-operator`,
  `ghcr.io/fyannk/pgtoolbox-console` and `ghcr.io/fyannk/pgtoolbox-evidence-agent`.
  With `watchNamespaces` the operator also gets a name-scoped Role for its own
  ConfigMap and Secret, so it starts without its namespace listed; the cluster-wide
  role holds no `bind`; each mode has its own ClusterRoleBinding name, so `helm
  upgrade` can switch modes; a watched namespace deleted after install is left out
  at start and logged.
  The restore-drill delete guard exempts holders of `*` on restore drills in
  the Cluster's namespace (a namespaced Argo CD or OpenShift GitOps controller, a
  Flux tenant bound to cluster-admin); start-up reads have a 60 s deadline per step,
  a 401 at start fails it instead of leaving a namespace out, and
  `pgtoolbox_config_info` lists only the namespaces watched.
- The custom resources are `pgtoolbox.fyannk.dev/v1alpha1`.
- **Opening a pgAdmin** behind the OpenShift OAuth proxy takes the `connect`
  verb on `pgadmins/<name>` (granted by `pgtoolbox-console-operator` and by
  `admin`), not `get`, which OLM gives to every `view` user.
- **Release candidates** (`v0.1.0-rc.N`) publish end to end: the bundle, catalog
  and installer manifests are pinned by digest from the tree's core version, a
  pin that changes nothing fails the release, and `latest` moves only after the
  packages are verified public.
- **Release verification.** The notes of a release are its CHANGELOG section and a
  link to the commit log (the compare view, or the history for the first release),
  not the log pasted in; the OLM bundle and catalog
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
- **Admission policy, sign-in settings.** Changing the sign-in settings (`spec.auth`,
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
  size is inferred from segment names before 16 MiB is assumed, and a size only
  inferred or assumed never proves a log-id rollover or timeline switch (at most
  a warning). A Barman path toward an ancestor of `latest` ends where the default
  restore leaves it, and one whose WAL stops before the switch while the child
  already has WAL reads unhealthy, naming the segment missing from the
  repository and saying the former primary may still hold it (CloudNativePG
  archives it before that instance rejoins: do not delete that instance or its
  volume), in both formats; native fuzz targets check that removing a segment or
  a history file never moves a frontier forward. One degraded old backup no
  longer greys out newer covered spans: the console caps paths only for roll-up
  reasons no path explains, and lists the agent's coverage reason on the recovery
  bar. The pgBackRest window `end_lsn` is shown as an exclusive upper bound, not a
  restore target.
  A pgBackRest backup the agent rates not usable is drawn unknown, and "no usable
  backup" caps the window again; after a failover, an end on the parent timeline is
  bounded by the former primary's archive backlog, else by a day; the missing-WAL
  segment says a former primary may still hold the WAL, and shortened reasons keep
  "do not delete that instance or its volume".
  A former primary's readings bound only the end of the timeline it wrote, so
  a double failover or A→B→A no longer pools them, and readings after an end are no
  evidence for it; a pgBackRest backup cut from the 1,000 items read is never drawn
  covered (`backup-not-listed`); the do-not-delete advice names the former primary
  when the console knows it, and a second reason never cuts it.
  Right after a failover, the parent timeline's tail is bounded by the former primary's archiver, and the bar says the repository has no WAL of the live timeline yet. Stale kept paths no longer draw a backup the repository no longer lists as covered, and their purge band is timed from their own scan. The do-not-delete advice no longer names a former primary that has rejoined; `backup-not-listed` says how to act.
- **An archive backlog is not "healthy".** Archiving shows Warning
  `ArchiveBacklog` once segments keep waiting across sweeps (the threshold follows
  CloudNativePG's WAL archiver, `isWALArchiver`, and its ObjectStore's
  `wal.maxParallel`), and recovery screens widen the newest edge from the last
  sweep that saw no WAL waiting and name the count; a failure band is dropped once
  the archiver caught up. The window's end reads "WAL received until", beside a
  separate newest covered target (not restore-tested). The window is never healthy,
  and no target reads covered, while the primary's archiver is unread, the Cluster
  is hibernated, or the agent rates a frontier path below healthy; archiving with an
  unread primary, a hibernated Cluster or too few sweeps after a restart is
  unknown, not ok. A replica cluster's designated primary, which reports no WAL
  waiting, reads unknown (`ArchiveBacklogNotReported`), never ok; a backlog that
  holds at the threshold while nothing is archived is flagged even when the
  archiver's parallelism is unknown; a busy primary's band is bounded by its WAL
  positions, one missed read of the primary does not flicker the bar, the
  end-of-WAL band allows one sweep for archive latency, and a hibernated Cluster's
  window is unknown. The restore check says archiving is failing after the
  agent's last read, keeps archive failures across a failover, reads the archiver
  from `currentPrimary` only, clamps the instance clock, and adds an end-of-WAL
  band advising "restore to end of WAL". Phase and condition caps cover undetected
  or unverified CloudNativePG minors and say why; recovery and timeline reasons
  are translated. The failover rehearsal applies the election rules of the
  operator's own version (1.30.1's, and says when 1.30.0 differs).
  The stall rule compares the instance's clock with itself; `archivingWal` is served
  only for the archiving instance; `cnpg-wal-files-waiting` judges only primary
  readings and says why when there is none; the not-reported message is worded by
  cluster kind; a hibernated Cluster's headline reads inactive.
  The backlog rule reads only the current primary's term, a first archive counts
  as progress, and `archivingWal` is shown only on `status.currentPrimary`.
- **Sign-in under a flood.** Behind a shared proxy address, unauthenticated junk
  (bad OAuth codes, unknown tokens, MCP token and registration requests) no
  longer spends the budgets genuine sign-ins, known MCP clients and token
  refreshes use; the per-replica caps protect the IdP and the API server. With
  `TRUSTED_PROXY_HOPS=0` and a public URL the peer address counts as shared.
  `pgtoolbox_console_throttled_requests_total{bucket}` counts refusals. A replayed,
  spent or expired MCP code or refresh token is checked before it spends any
  shared budget, and each live grant has its own; sign-outs are bounded per person
  so the state ConfigMap stays under 1 MiB
  (`pgtoolbox_console_shared_state_bytes`,
  `pgtoolbox_console_shared_state_signouts_refused_total`); one person cannot fill
  the set of known MCP bearers. A sign-out counts against the person sealed in
  the session at sign-in (an HMAC tag); the state ConfigMap never drops a
  sign-out it holds and refuses new ones past its bounds (`reason` `subject`,
  `unnamed` or `budget`), and a dead token-only session is not shared. The MCP
  token endpoint, known-bearer reviews and OIDC exchange reviews each have a
  per-person share (`oauth_subject_tokens`, `known_reviews_subject`,
  `exchange_reviews_subject`); a replayed refresh token is refused before the
  review; store-outage refusals are bounded; each replica's signed-out table is
  bounded.
  A sign-out of a session sealed with its person's tag is shared even when the API
  server refuses the token at sign-out; a person past 16 sign-outs keeps their latest
  ones (each replaces that person's own earliest-ending one, nobody else's entry
  moves); `signouts_refused_total` reasons are `replaced`, `unnamed`, `budget` and
  `merge`; new gauge `pgtoolbox_console_shared_state_signouts{bound}`.
  A full sessions document is shared fairly: a newcomer's sign-out takes the
  place of an entry of the person holding the most (`reason="evicted"`), and
  concurrent replacements a merge drops are counted under `merge`; the shared-state
  ConfigMap stays within 1 MiB, dropping the oldest restore drill results first
  (`pgtoolbox_console_shared_state_dropped_total`).
  A full sessions document gives up a person's oldest sign-out, never their newest, so a sign-out after a cookie theft stays shared. Past 80 % of the MCP grant index or codes table, a person holding at least the mean share is refused. "Since" times keep the room the state ConfigMap leaves instead of all being dropped, and prune and merge hold the state lock about half as long.
- **Exposure trust.** The pgtools chart trusts every peer only when
  `networkPolicy.ingress.extraFrom` is empty. `networkPolicy.from` selectors are
  validated by the CRD, and a policy the API server refuses stalls with
  `ExposurePolicyInvalid`. More broad peers are detected (selectors pinning no
  value, wide, private-range or Pod-network ipBlocks), other policies are matched
  against live Pod labels and rechecked while they bypass ours, OpenShift's
  allow-from-openshift-ingress is not a bypass, and the operator's own any-peer
  trust is marked on the Pod template. That trust is granted only once verified:
  an ipBlock peer waits for a console Pod address, a truncated Pod list grants
  none (`ExposureTrustUnverified`), and a widened `from` waits until no Pod trusts
  every peer. Unique-local IPv6 blocks wider than /64 and namespace selectors
  pinning only shared labels are broad; a refused NetworkPolicy is caught by a
  dry run before anything is exposed. On OpenShift the pgtools chart trusts no
  peer unless `console.trustedProxyCIDRs` is set, and `console.platform`
  (`auto`, `kubernetes`, `openshift`) makes that hold under offline `helm
  template`. A single-replica console with an ipBlock peer no longer restarts in
  a loop: the verified blocks are kept while no Pod has an address. An ipBlock
  overlapping a Node's podCIDR is broad (the operator gains `list` on Nodes); an
  admission policy or webhook that denies the console's NetworkPolicy stalls with
  `ExposurePolicyInvalid`; the dry run is skipped when nothing changed; a Pod
  stuck terminating that holds an exposure change is named
  (`ExposurePolicyKept`).
  A widened `networkPolicy.from` is held even when the cache still shows an older
  policy or workload; the Node podCIDR check runs one lookup at a time, lists 100
  Nodes a page and says when Nodes have no `spec.podCIDRs`; a `HostnameNotAllowed`
  stall reports `ExposurePolicyKept` while the console's ingress policy remains.
  A console whose hostname and image are both outside the allow-lists withdraws
  its Ingress, Route or HTTPRoute (the hostname is checked first) and names both;
  `ExposureConfined` says when the Node podCIDR check is not meaningful (a console
  Pod outside its Node's podCIDRs); the Node lookup runs within 10 s and resumes.
  PgAdmin, like the console, checks its hostname first and withdraws a published Ingress or Route under a disallowed name; both kinds report `ExposureReady=False` (`HostnameNotAllowed`) with an empty `status.url`. Operator start-up steps have a 20 s deadline, below the liveness kill, and a 401 from an optional-API probe fails the start.
- **Admission.** A PgAdmin with `configDatabase` needs `get` on the
  namespace's Secrets and its database must start with `pgadmin_` (CloudNativePG
  adopts and resets an existing role); raising `sessionMaxAge` counts as widening
  the audience; exposure annotations an ingress controller runs as configuration
  (snippets, `auth-*`, mirroring, middlewares, plugins, other hosts, source
  ranges, redirects, canaries, CORS, load-balancer scheme, group and entry point,
  for ingress-nginx, AWS Load Balancer Controller, HAProxy, the OpenShift router,
  Traefik and Kong) need `create` on the exposure kind, on the Service as well as
  the exposure; the list is documented in security.md and kept equal by a test.
  The restore-drill delete guard also holds ServiceAccounts of other namespaces,
  the operator's included, unless they hold `*` on restore drills cluster-wide.
- **Release pipeline hardening.** GHCR pruning is opt-in (repository variable
  `PRUNE_GHCR`) so a release cannot turn red after it is public; the 30-minute net
  now dispatches `ci.yml` and `security.yml` independently (the dead
  `pull_request: closed` trigger is gone); `wait-ci` names the exact dispatch
  commands and the checklist says to run `make release-preflight` before pushing a
  tag; npm installs of the docs site and the Headlamp plugin run with
  `ignore-scripts` and a minimum release age; the actions allow-list for the
  repository is documented. The daily Actions cache prune keeps the newest member
  of every logical cache (module, toolchain, node, CodeQL, each job's build cache),
  deletes superseded copies and closed pull requests' caches, evicts rebuildable
  build caches first only if the newest members still exceed the budget, and
  survives a 404 on one delete.
- **Release gates.** `wait-ci` and `release-preflight` accept only a
  `workflow_dispatch` or `schedule` run of `ci.yml` and `security.yml` on the tagged
  commit, because a docs-only push to main skips the image, e2e and OLM jobs; each
  image carries its own OCI title and description (the repository's had replaced
  them); the verify docs name cosign v3 as the minimum; the docs say a pushed tag
  cannot be deleted; the CI image cache has one scope per architecture; the
  anonymous-pull check polls for 20 minutes so package visibility can be flipped
  during the run and, while it waits, names the settings page of every private
  package in a `::notice`, and a registry outage (5xx, timeout) is reported as
  such rather than as a private package. `release-preflight` refuses a commit
  that is not an ancestor of `origin/main` before any check-run call, and its
  Unreleased message names the core `## [X.Y.Z]` heading for a candidate. A release candidate and its final tag are cut on the
  same commit with the CHANGELOG already dated (`release-preflight` and the
  workflow refuse `Unreleased` for a candidate too); the release's `prepare` job
  holds `actions: read`, which the full-tier check needs; Pages after a release
  builds the tag; a daily `cache-prune.yml` keeps the Actions cache under its
  limit; `verify-pins-cnpgstatus` runs every pin test; the repository-settings page
  matches the live rulesets; the e2e clients drop idle connections before the
  console closes them.
  `check` requires a real `YYYY-MM-DD` date, `wait-ci` names a failing API call,
  the draft release is no longer created with `--latest`, `make doctor` reads
  govulncheck's own version, `hack/check-action-pins.sh` keeps the documented
  allowed-actions list (now with `jdx/mise-action`) in step with the pins, the
  watchNamespaces e2e also runs on chart changes, and cache pruning retires mise
  caches of an old `mise.toml`.
  Built with Go 1.27.4 (GO-2026-6859, an HTTP/1 server crash in `net/http`).
  `check-toolchain.sh` ties the Dockerfiles' `golang` and `node` images to `mise.toml`, and Dependabot leaves them to the toolchain bump; `release-preflight` checks that the tag points at the SHA. The restore-drill guard is on by default and rendered by plain `helm template`; the kustomize install passes the default images by digest.

[0.1.0]: https://github.com/fyannk/pgToolBox/releases/tag/v0.1.0
