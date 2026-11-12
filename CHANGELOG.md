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

**Known limitation:** with NetworkPolicy enabled and no trusted proxy addresses set, the
console trusts `X-Forwarded-For` from the peers its policy admits. On a network plugin that
does not enforce NetworkPolicy, set `console.trustedProxyCIDRs` (or
`spec.exposure.trustedProxy.cidrs`): otherwise any pod can fake the client address seen by
the per-address sign-in limits and the audit log (identity is not affected).

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
  The operator probes an optional API that is not installed once at start-up, not once per watched namespace; a watched namespace whose administrator narrows the operator's Role is left out of the watch instead of turning optional features off for every namespace. The start-up steps share one 40 s budget and the manager's discovery requests time out after 10 s, so a slow or hung API server fails the start with a named error before the liveness kill. A paused console whose hostname is allowed again no longer reports `ExposurePolicyKept` "while stalled".
  A namespace whose Role grants or refuses an optional API (Routes, HTTPRoutes, Poolers, Databases, DatabaseRoles, ObjectStore, Stanza, Archive) turns that feature on or off in that namespace only; it no longer drops other namespaces from the watch, or itself. New gauges `pgtoolbox_watch_namespaces_dropped` and `pgtoolbox_optional_api_refused_namespaces`. The manager and the controller setups run under the 40 s start-up budget.
  With `watchNamespaces`, a namespace whose Role refuses any namespaced permission of the manager role (Deployments, watch on Clusters, ...) is left out at start, logged and counted in `pgtoolbox_watch_namespaces_dropped`, instead of stopping the operator for every namespace. A Route or HTTPRoute published before its API was refused in the namespace is deleted, or named in `ExposureReady` as possibly remaining. A granted namespace that refuses DatabaseRoles or Poolers says so, and no status message names another tenant's namespace.
  A watched namespace left out at start marks its PgAdmins, consoles and evidence agents Ready=Unknown (`NamespaceNotWatched`) with a Warning Event where it may still write them; new alert `PgToolBoxOperatorNamespacesDropped` (`operator.metrics.prometheusRule`); `pgtoolbox_watch_namespaces_dropped` gains a `reason` label. One namespace's slow or failing permission review no longer fails the start, and `resource/*` is no RBAC wildcard. A refused Route or HTTPRoute is reported only when one may have been published (ExposureConfined Unknown), and a Route or Gateway CRD removed at run time no longer fails every reconcile.
  The per-namespace permission review is bounded (4 MiB response), retries lost connections, and a review-failed namespace is reviewed again every minute, the operator restarting once it can watch it. The dropped-namespace report runs on the elected leader only, with one Event per transition. `status.publishedExposureKinds` records the Routes and HTTPRoutes the operator published and replaces message word matching for leftovers; an API missing for a moment is retried rather than read as nothing left.
  A namespace whose permission review is too large is dropped as `review-too-large` and reviewed again only at the next start; the restart to watch a `review-failed` namespace needs two passing reviews in a row, ten minutes of uptime and up to a minute of jitter, and exits 0 with a termination message, so a tenant can no longer keep the operator in CrashLoopBackOff. Review answers are capped at 1 MiB whatever path prefix the API server URL has. A Route or HTTPRoute withdrawn for a refused hostname is cleared from `status.publishedExposureKinds`, and the `PgToolBoxOperatorNamespacesDropped` alert says what to do for each reason.
  The start-up permission reviews are read as JSON and capped at 256 KiB each, so sixteen hostile reviews in flight stay under a 64 MiB heap budget (18–45 MiB measured). A namespace whose review gets no answer in time at both attempts is left out as `review-too-slow` and, like `review-too-large`, never reviewed in the background, so a tenant cannot drive restarts through slow reviews; a namespace that leaves the background loop moves to its reason in `pgtoolbox_watch_namespaces_dropped` and the alert.
  A watched namespace is dropped as review-too-slow only when the API server answered a check sent meanwhile; a silently dead connection now gives review-failed, which is reviewed again in the background. A server or gateway timeout late in an attempt counts as its deadline. A review too large names its size (Content-Length or bytes read) in the error and the log.
  A permission review too large that arrives slowly is now `review-too-large` with its size, not `review-too-slow` or `review-failed`; the size is counted for at most 8 MiB and one second, and never past the attempt's deadline. The check of the API server that separates `review-too-slow` from a dead connection is sent in the last third of the second attempt, so a connection that dies during that attempt, before the check answers, leaves the namespace `review-failed`, reviewed again every minute.
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
  Recovery paths kept stale are capped against the fresh snapshot's timeline and coverage, dropped on a repository change, and dropped after three refresh intervals (at least an hour). Right after a failover the recovery window is a warning (`liveTimelineNotArchived`) that says what to do, instead of healthy with no reason; once the history file is archived the tail says the repository has no WAL segment of the new timeline yet. Stale paths beside a cut backup list are not counted as covered; the purge band uses the earlier of the agent's and the console's clocks. CloudNativePG's `-infinity` last-archived time reads as "nothing archived".
  Kept recovery paths beside a fresh coverage roll-up that differs from theirs are capped at a warning. The window warns (`repositoryAheadOfLive`) when the repository's newest timeline is ahead of the live one and draws restore points past the newest live-line backup as unknown. The purge band covers the span only the oldest backup reaches once the scan is older than one refresh interval. A hibernated Cluster after a failover says it is hibernated, an unread primary gets no `pg_switch_wal()` advice, and dropped kept paths say why (`keptTooOld`, `keptRepositoryChanged`), in English and French.
  Kept healthy paths are capped beside any unhealthy coverage roll-up; the purge band covers every backup a long-stale scan may have lost and no longer flips each refresh cycle. Failing archiving after a failover says so (`archivingFailing`) instead of advising `pg_switch_wal()`; a backup on a sibling timeline no longer moves the repository-ahead span past the fork; the dropped-kept-paths reasons say what to check.
  The edge hint follows one reason map (`edgeAdvice`): no `pg_switch_wal()` while archiving fails, lags or failed recently, and nothing to run when no primary was read. With the primary unread, a False ContinuousArchiving condition is reported as the last attempt, with its age. The purge band follows pgBackRest full-backup chains and every listed Barman backup, and its missed-scan threshold follows the scan's duration, timed from its start.
  The edge hint follows what the console read: nothing to run when the primary was not read or the Cluster is hibernated, and `pg_stat_archiver` only for a designated primary, a primary mid-promotion or a repository ahead of the live timeline. The purge band catches a missed scan on short windows, covers the whole window once retention may have passed the scan, and counts pgBackRest backup-error fulls as anchors. A hibernated Cluster with ContinuousArchiving False says it is hibernated.
  A fenced or restarting primary's `/pg/status` answer (`mightBeUnavailableMaskedError`) no longer reads as archiving OK: archiving is unknown, the backlog sparklines show gaps, no SQL hint is offered and the diagnose rules do not clear. A Cluster hibernated while archiving was failing says its last archive attempt failed, at warning; the edge hint names the archiving instance. Past a missed scan, the purge band covers the whole window when the listed backups may be obsolete, and a pgBackRest time-based retention bounds it from the clock.
  An instance whose manager answered without reaching PostgreSQL (fenced or restarting) shows "PostgreSQL not read", with its error, on Instances, Replication, the topology, the failover rehearsal and the diagnostics, instead of false, zero or empty fields. The pgBackRest purge band counts the backups ended since the evidence was read, so a burst of fulls under a count retention, or an archive retention keeping fewer backups than the fulls, no longer leaves expired restore points shown as covered.
  A Backup stop time left unset by a plugin (stamped 1970) is read as unknown, in the purge band and the restore drill. The pgBackRest purge band also covers an expire still pending when the scan read the repository, and Backups in progress begun after the read. "Instance self-reports" counts a fenced or restarting instance apart, the failover rehearsal names a masked replica that reports itself primary, and Instances shows "not reported" instead of 0 for an absent ready-WAL count.
  Backup times are read through one plausibility rule. A stop time a plugin left unset (1970) or set in the future is not a stop time anywhere: the Overview's last backup, the timeline, the backups screen, the restore drill and the purge band. Such a Completed Backup is timed by CloudNativePG's completion stamp, which is named as the source. A queued Backup counts from the operator's start, and a Backup restored with its status (Velero) no longer keeps a whole-window purge band.
  Backup times older than the object are kept as restored only when the Backup's CloudNativePG stamps are as old. A Completed Backup counts at the later of its stop and CloudNativePG's completion stamp (`reconciliationTerminatedAt`: the instance manager's for a backup it completed or failed, else the operator's), and a failed Backup is timed by the latest of when CloudNativePG marked it failed, the operator's start and its creation, so a backup that waited behind a success and then failed still warns.
  A time-based pgBackRest expire whose completion stamp is before the scan no longer holds a pending purge band, so a healthy daily schedule shows none except during a scan begun within about six minutes of a backup's stamp (the rule allows five minutes of instance clock skew, so an instance 1–5 min behind the agent no longer shows a full that expire removed). On an instance whose clock runs behind, a Backup's tool times and completion stamp are all dropped: the purge band counts it as possibly ended since the scan, and the Overview times it by the earlier of its creation and the operator's start, never in the future, and says the instance's and the operator's clocks disagree.
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
  The MCP grant and code fair share no longer depends on arrival order: past 80 %, a person holding four or more is refused, so a hundred identities hold at most 1,360 codes and 1,460 grants (refresh re-records and a first replay placeholder are exempt). Among people holding equal counts, a full sessions document keeps the most recently recorded sign-outs. The size check counts `binaryData`, and a re-probe aborted by an error no longer changes source states.
  The MCP endpoint advertises no list-change notifications, so assistants hold no request open between calls; a `subscriptions/listen` request a client opens anyway counts against a cap of sixteen per person and replica, not against the four requests in flight. An MCP write confirmation is one entry of the shared codes table whatever steps it records, so past 80 % a person may still have four confirmations in flight. Shared state format 2: a sign-out read back with a recording time far in the future is kept but ordered last.
  Replicas whose clocks disagree by more than five minutes no longer rewrite the shared-state ConfigMap back and forth after a sign-out. A shared-state document of an older format version is read and taken over; a newer one is left alone, logged once and counted in `pgtoolbox_console_shared_state_foreign_documents`. An MCP confirmation whose write-through failed no longer erases another replica's spend or mark, which could let a confirmed write run twice.
  A failed MCP confirmation write over an expired copy of its code no longer leaves the code refused as spent. After a rollback, shared-state documents a newer console wrote are taken over once no newer console has written for 30 minutes (per-version annotation; `grants.json` is reset); deleting the key recovers at once. An unreadable `grants.json` is logged once per episode.
  A console replica cut off from the API server for more than 10 minutes, or one that missed a take-over, no longer brings back a revoked MCP grant, and a revocation made while cut off is kept until it reaches the state ConfigMap. A replica no longer forgets a grant it had learned when a delayed watch event arrives after a newer read (a Disconnect could fail with "no such grant"). A single-use code can no longer be spent twice during a take-over. Shared-state documents of format version 100 or more are never taken over, documents with no version are rewritten, and annotations of console versions gone for 30 minutes are removed.
  A single-use MCP code or confirmation can no longer be spent on two replicas when the watch overtakes a conflict re-read. The shared state recovers from an API server revision that went back, and exhausted writes count as failing (`pgtoolbox_console_shared_state_write_exhausted_total`). Disconnecting an assistant answers 503 "retry" until the revocation is written; unwritten and code-replay revocations are never pruned before they are written, and survive a recreated ConfigMap. A replica's batch write no longer races its own write-through, which could make a confirmation answer fail as "could not be recorded".
  A write-through its caller abandoned, or that conflicted or was refused once, no longer leaves a replica failing (which refused every MCP grant 30 s later): a failing replica retries a write until one lands, and a caller that gives up while waiting starts none. Disconnecting an assistant while the ConfigMap does not exist is answered done (logged, written when it returns). New gauge `pgtoolbox_console_shared_state_mode_since_timestamp_seconds`. On the Assistants page, a 503 on Disconnect no longer says "Not started": the grant stays listed as being written to the other replicas, with Retry, and Disconnect all is offered even with one assistant.
  A replica whose watch of the shared state is refused or broken no longer counts as live because a write landed: it refuses MCP grants 30 s after its last read and reopens the watch at once, then with jittered backoff. A replica that has not read the shared state yet answers 503 to Disconnect and Disconnect all, and with MCP OAuth on `/readyz` waits for that first read. A missing ConfigMap is read every 1–2 s. New opt-in alert `PgToolBoxConsoleMCPGrantsRefused`. On the Assistants page, focus moves to the status note after a disconnection, Retry does not ask for the name again, and the 503 note no longer promises "a few seconds".
  A replica whose watch of the shared state ended (or went silent) no longer serves MCP grants past 30 seconds after it ended; the read that reopens it is bounded at 10 seconds, and a watch that goes quiet is reopened. `/readyz` no longer drops every replica on a transient API server error after the shared state ConfigMap was found missing or forbidden; a missing ConfigMap is polled fast only while a disconnection is held or for two minutes, then about every ten seconds.
  While its watch is open, each console replica re-reads the shared state ConfigMap every 15–19 s: a permission withdrawn, or a watch that fell behind or only sends old bookmarks, now refuses or catches up grants within about 20 s (49 s if the API server stops answering) instead of up to an hour. **Disconnect all** on a replica that may not read the ConfigMap answers 503. Grace windows use the monotonic clock.
  **Disconnect all**, and disconnecting an assistant the replica does not know, read the shared state ConfigMap first and answer 503 instead of "done" on a replica that cannot read it right now (cut off from the API server or forbidden). The freshness read fetches only the ConfigMap's metadata and is counted from when the previous one was sent; the 30 s grace runs from that read's sending even if the watch ends while it is unanswered; a quiet watch is reopened after 10 minutes. The revocation list read and its watch opening time out after 10 seconds and are retried.
  A grant is written to the shared state before its first token is issued (503 `temporarily_unavailable` if that write fails), and a code from a signed-out session buys nothing, so a done **Disconnect all** also covers grants issued on a replica cut off from the API server.
  Disconnection reads are coalesced; only a 403 or 404 changes what a replica serves, so a throttled or timed-out read answers 503 without making the replica fail. A malformed grant id is refused without a read.
  The two 503s of a disconnection (not confirmed for every replica, or not recorded) have their own problem types and texts, and the signed-out page says when assistants may still be connected.
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
  A paused PgAdmin or PgToolBoxConsole withdraws a refused hostname; `ExposureReady` no longer keeps `HostnameNotAllowed` once the hostname is fixed while another stall holds; the refusal names the chart value `operator.exposure.allowedHostSuffixes`. Start-up steps fail at their deadline, naming the step, against an API server that never answers, and watched namespaces are probed sixteen at a time.
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
  CI path filters run the e2e and gate jobs for `internal/backupresolve`, `hack/lib` and the gate scripts, and turn every job on for a path they do not map. `check-toolchain.sh` requires equal `golang`/`node` digests across Dockerfiles; `release-preflight` accepts any revision; `make doctor` checks promtool and helm-unittest. Chart NOTES warn when the restore-drill guard is not enforced.
  The allowed-actions list names each action `owner/repo@*`: an exact-SHA list would refuse every Dependabot bump once `allowed_actions` is `selected`, and the SHA pins are enforced by the full-length-SHA setting.
  `make test-hack` and the file checks of `check-repo` (toolchain pins, the allowed-actions list) run in `lint-files`, so workflow, settings-doc and `hack/` changes are checked; the cache prune removes `buildkit-blob-*` caches unused for 3 days and keeps the cache under 6 GiB; `release.sh check` and `preflight` read the CHANGELOG and charts from the tagged commit, and a dispatched release must run from the tag it publishes.
  The Actions cache is pruned after every CI and Security run on main (this repository's only), with a 4 GiB budget, and buildkit layers are exported by the nightly run only; `make verify-manifests` stops when the local helm major differs from `mise.toml`; a `Chart.yaml` change runs the file and hack-script checks; the restore-drill guard NOTES name Kubernetes 1.34.
  Release candidates (`v0.1.0-rc.1`) no longer carry a `v` in the image, chart and notes version: one resolver, `release.sh resolve`, feeds the workflow and every script, and the dry run rehearses it.
  The console embeds its documentation as one deterministic gzip archive (`data/bundle.gz`), budgeted at 1 MiB compressed and 4 MiB decompressed; the generator names the limit a site build breaks.

[0.1.0]: https://github.com/fyannk/pgToolBox/releases/tag/v0.1.0
