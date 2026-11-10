---
sidebar_position: 5
title: Glossary
toc_max_heading_level: 2
---

# Glossary

The terms you meet in the console, the operator and these pages, in alphabetical
order. Each entry says what the word means in pgToolBox and links to the page that
explains it. Where the screen and the API use different words for the same thing,
both are given: the screen's words first, the API's value in `code`.

## A

### Access request

A `PgAccessRequest`: a person asks, in one namespace, for one of the two shipped
ClusterRoles (`viewer` binds `pgtoolbox-console-viewer`, `operator` binds
`pgtoolbox-console-operator`). Someone who may create RoleBindings there and update the
request's status (`pgtoolbox-console-approver`, which no request grants) approves it; the console then creates the RoleBinding with that
person's own token. A grant always covers every Cluster of the namespace. See
[Access requests](../console/access-requests.md).

Its phases are `Pending`, `Approved`, `Denied`, `Expired` and `Revoked`, and they only
move forward ([lifecycle](../console/access-requests.md#lifecycle)).

### Agent channel (`evidence/v1alpha1`)

The wire contract between the console and an [evidence agent](#evidence-agent):
HTTPS on port `8443`, media type
`application/vnd.pgtoolbox.evidence.v1alpha1+json`, defined in the Go module
`pkg/evidence`. With the operator, the console presents a token bound to the audience
`pgtoolbox-evidence-agent/<namespace>`; in [team mode](#team-mode) a shared token.
See [The channel](../evidence-agent.md#the-channel).

## B

### Baseline (insight pack)

See [Insight pack](#insight-pack).

### Blind spot

Something the console cannot see from its sources, stated on the panel where its
absence would otherwise be mistaken for "nothing there" (for example
`storage.volumeUsageNotReported`: upstream CloudNativePG reports no volume usage).
Each blind spot has a stable ID. Some hold on every supported CloudNativePG minor,
others only for the Cluster's minor or its configuration. See
[What it shows](../console/index.md#what-it-shows).

## C

### Catalog pin

The CloudNativePG releases a diagnostics rule was verified against: **1.30.0 and
1.30.1**. On any other release a pinned rule answers *does not apply*. The
diagnostics view lists the pins and the one applied to the Cluster (`catalogPins`,
`applied`). Versions are observed, never configured. See
[Version pins](../guides/diagnostics.md#version-pins).

The read model also keeps **per-minor tables** of CloudNativePG phases and condition
reasons, for 1.30 and 1.31 (1.31 read from upstream main, not yet from a
release). An unknown or unsupported minor (1.29 included) is read against the newest released one, 1.30, and
anything outside it is reported as not known.

### Check

One detector or catalog rule of the [diagnostics](../guides/diagnostics.md) run. Each
check ends a run with one of four outcomes
([the four outcomes](../guides/diagnostics.md#the-four-outcomes)):

| On screen | API value | Meaning |
|---|---|---|
| matched | `matched` | It found what it looks for; its [findings](#finding) are listed. |
| clear | `clear` | Its inputs were readable and nothing matched. |
| could not run | `unavailable` | An input is absent, forbidden, not yet observed or stale. It rules nothing out. When the input is switched off by configuration rather than failing, the check is also marked `sourceOff`. |
| does not apply | `notApplicable` | Its [catalog pins](#catalog-pin) exclude the versions observed. |

### Compare

The `/compare` page: two Clusters side by side, row by row, from what each one
declares in its `spec` plus a few status fields. The server decides each row:
**same**, **differs** or **cannot tell** (one side could not be read). See
[Comparing two Clusters](../guides/compare.md).

### ConfigurationValid, Ready, Stalled

Conditions every operator kind reports. `ConfigurationValid` says whether the
object's references resolve and its settings can be honoured; it is the first
*aspect* of `Ready`. `Ready` is derived from the aspects in the same status write:
`True` when all are `True`, otherwise it copies the first aspect that is not.
`Stalled` is `True` only when the operator cannot make progress without a change of
input; it is not retried with backoff. See
[How Ready is derived](../operator/conditions.md#how-ready-is-derived).

### Consequence

Two uses:

- In diagnostics, a [finding](#finding) nested under the root finding of an
  [incident](#incident) because a catalog relation admits the pair.
- In [operations](#operations), the one sentence that says what an operation will
  do, shown with its preconditions in the offer and the confirmation dialog
  ([Preconditions](../operations.md#preconditions)).

### Could not run

See [Check](#check).

## D

### Digest

The daily digest: a summary of the last 24 hours, closed each day at `DIGEST_HOUR`
(UTC). It lists spec changes, findings that appeared, worsened or cleared,
failovers and switchovers, failed backups, container restarts and the Clusters that
need attention. A digest that did not observe its whole window is **partial** and
never an all-clear. It can be delivered to a chat webhook, a webhook or by mail. See
[Daily digest](../console/timeline-and-digest.md#daily-digest).

### Does not apply

See [Check](#check).

### Drill

See [Restore drill](#restore-drill).

## E

### End of life

See [Lifecycle table](#lifecycle-table).

### Evidence

Repository evidence: what an [evidence agent](#evidence-agent) found in the backup
repository itself (backups, WAL ranges and gaps, recovery paths, and a conclusion
on WAL continuity, recovery coverage and retention). The Backups view shows it next
to what CloudNativePG reports, and names the disagreements. See
[The evidence agent](../evidence-agent.md#what-it-reports).

### Evidence agent

The `pgtoolbox-evidence-agent` image run in its `agent` mode: a read-only reader of one
Cluster's Barman Cloud repository (S3, Azure Blob, GCS) or pgBackRest repository
(S3, Azure Blob) that serves
[evidence](#evidence) over the [agent channel](#agent-channel-evidencev1alpha1).
The operator deploys it from an [`EvidenceAgent`](#evidenceagent); without the
operator, the `pgtools` chart does ([team mode](#team-mode)). See
[The evidence agent](../evidence-agent.md).

### EvidenceAgent

The custom resource that deploys a read-only [evidence agent](#evidence-agent) for
the backup repository of one Cluster in its namespace. Whoever may `get` it may read
the evidence it serves. See [EvidenceAgent](../operator/evidenceagent.md).

## F

### Failover rehearsal

The "If the primary failed now" panel: which replica the operator would promote (in
CloudNativePG's own order), how far behind it is, whether an acknowledged commit
could be lost, and what would block the promotion. It is computed from readings the
console already holds and triggers nothing. Its outcome is `promote`, `blocked`,
`noCandidate`, `unknown` or `notApplicable`. See
[Failover rehearsal](../guides/failover-rehearsal.md).

### Finding

A statement of the [diagnostics](../guides/diagnostics.md) about what is wrong and
where, anchored to the evidence it rests on. It names its subject, its
[severity](#severity) and the check that produced it, and carries a "since" clock:
the first run of an unbroken series that reported it, a floor rather than the moment
the fault began. See
[Findings and incidents](../guides/diagnostics.md#findings-and-incidents).

## H

### History journal

The bbolt file at `HISTORY_PATH` (on a claim, with `console.history.persistent:
true`) that keeps the revision history of Clusters and their declarative children,
and the restore-drill records, across restarts. It takes an exclusive lock, which
is why each replica of a persistent console (a StatefulSet) has its own volume. The
timeline (`timeline.db`), the volume trend (`volume-trends.gob`) and the state
snapshots are kept beside it. See
[Replicas and history](../console/configuration.md#replicas-and-history).

## I

### Incident

A root [finding](#finding) and the findings that follow from it
([consequences](#consequence)), grouped by the catalog's causal relations. Its
severity is the worst of its findings. See
[Findings and incidents](../guides/diagnostics.md#findings-and-incidents). The
*incident timeline* is a different thing: see [Timeline](#timeline).

### Insight pack

A set of CloudNativePG custom queries (a ConfigMap: `<release>-insight-pack` from the
pgtools chart, `pgtoolbox-insight-pack` from `deploy/insight-pack`) that
a team opts its Cluster into. The instances' own exporters run them and the console
reads the results from `:9187/metrics`, so the console still never connects to
PostgreSQL. It feeds the Insight screens: wraparound and vacuum, blocking, query
insight, index hygiene and connections. See [Insight pack](../console/insight-pack.md).

Two words from those screens
([What the console shows](../console/insight-pack.md#what-the-console-shows)):

- **Window**: the span of the console's own samples a figure is computed over. The
  transaction-id consumption rate uses the last hour; a statement's recent mean time
  is taken over an hour.
- **Baseline**: the same hour a day earlier, which a statement's recent mean time is
  compared with to call it a regression. A comparison needs 25 hours of samples on
  the same primary. A statement that was not among the kept top statements a day
  earlier has no baseline: it is counted, not compared.

### Instance CA bundle

The ConfigMap (key `ca.crt`) of the Clusters' server CA certificates that the
console verifies `/pg/status` with; the console reads it as a mounted file, never a
Secret. For a `PgToolBoxConsole` the operator keeps it (`<name>-instance-ca`, copied from
each Cluster's CA Secret, `ca.crt` only, following CloudNativePG's 90-day CA
renewals); with the pgtools chart alone the admin supplies it and keeps it current
([The instance CA bundle](../console/configuration.md#the-instance-ca-bundle)).

### Instance status sweep

The console's periodic read of each instance's `/pg/status` over verified TLS
(every `INSTANCE_STATUS_INTERVAL`, 30 s), which also scrapes the exporters when
metrics are on. Each instance row ends in one state
([The instance CA bundle](../console/configuration.md#the-instance-ca-bundle)):

| State | Meaning |
|---|---|
| `ok` | A verified TLS handshake and a parsed report. |
| `pending` | Not swept yet. |
| `unreachable` | Timeout or connection refused: not reachable from this console. |
| `notVerifiable` (not verifiable) | The Cluster's CA is not in the console's bundle, or no Service to check the name against: never read unverified ([the "not verifiable" row](../console/configuration.md#the-not-verifiable-row)). |
| `noTLS` | The Pod predates CloudNativePG 1.24 and its status port has no TLS: never read over HTTP. |
| `tlsError` | Another handshake failure: name mismatch, expired certificate. |
| `badResponse` | A non-200 answer or an unparsable body. |
| `notRunning` | The Pod has no IP yet or is being deleted. |
| `disabled` | The sweep is switched off (`INSTANCE_STATUS_ENABLED=false`). |

## L

### Lifecycle table

The end-of-life table the diagnostics read: for PostgreSQL, Kubernetes,
CloudNativePG and the Barman Cloud plugin, each release line and the day it leaves
upstream support. The console has one built in; the operator and the `pgtools` chart
point `LIFECYCLE_FILE` at a ConfigMap, so a support date can move without a release.
See [The lifecycle table](../operator/console.md#the-lifecycle-table).

## M

### Matched

See [Check](#check).

## N

### Not verifiable

See [Instance status sweep](#instance-status-sweep). An evidence agent is also shown
as not verifiable when its status was not written by the operator, or when it refused
the console's token as issued for another audience
([What the console shows](../evidence-agent.md#what-the-console-shows)).

## O

### Operations

The console's write endpoints: **backup**, **reload**, **restart** and **promote**
(switchover), plus the opt-in [restore drill](#restore-drill). Each is made with the
signed-in person's own token after a typed confirmation, a live read and its
preconditions; a `202` means accepted, not done. `OPERATIONS_ENABLED=false` makes
them answer 501. See [What the console may do](../operations.md).

## P

### PgAccessRequest

The custom resource behind an [access request](#access-request). See
[Access requests](../console/access-requests.md).

### PgAdmin

The custom resource for one pgAdmin in a team namespace, registered against that
namespace's Clusters. Whoever may open it (`get pgadmins`) holds the credentials of
every role it offers. See [PgAdmin](../operator/pgadmin.md).

### PgAdminAccess

A grant, created in another namespace, that lets a `PgAdmin` register that
namespace's Clusters and read their offered credentials. It has no status of its own;
the `PgAdmin` reports whether it is granted. See
[Clusters of other namespaces](../operator/pgadmin.md#clusters-of-other-namespaces).

### PgToolBoxConsole

The custom resource that runs the console for the Clusters of its namespace: the
same console the `pgtools` chart installs, kept converged by the operator. See
[PgToolBoxConsole](../operator/console.md).

### pgtools chart

The Helm chart a namespace admin installs for a team: the console in the team's
namespace, with no CRD and nothing cluster-scoped by default, and optionally pgAdmin
and the evidence agents without the operator ([team mode](#team-mode)). The operator
comes from the other chart, `pgtoolbox`. See [Helm chart](../install/chart.md).

### Playbook

An ordered list of questions for one symptom (such as `cannot-connect` or
`backups-not-happening`), each answered by checks that already ran. Triage walks
it: a step is found, ruled out, could not be judged, or not observable here. A step
is never ruled out on a check that could not run. See
[Triage](../guides/diagnostics.md#triage) and the
[playbooks](checks.md#triage-playbooks).

## R

### Read model

What the console builds from its sources and renders: informers over the watched
namespaces, the instance status sweep, the exporters, the evidence agents and the
history, published as immutable snapshots. The per-minor CloudNativePG knowledge
lives in `pkg/cnpgstatus`. The screens, the diagnostics and the drill checks read
it; an [operation](#operations) does not trust it and reads the Cluster live, as the
person, before it writes. See
[How it is built](../console/index.md#how-it-is-built-in-four-sentences).

### Ready

See [ConfigurationValid, Ready, Stalled](#configurationvalid-ready-stalled).

### Recovery window

The span a restore can target, drawn on the Backups screen. With
[evidence](#evidence) it is built from the agent's recovery paths as **covered**
spans, holes (missing WAL, candidate gap, unknown) and markers; without it, the
operator's or plugin's own window is drawn hatched as **reported**. The restore check
answers `covered`, `notCovered`, `gap`, `reported` or `unknown`. See
[Recovery window](../console/forecasts-and-recovery.md#recovery-window-backups-screen).

### Registration sidecar

In a pgAdmin installed by the `pgtools` chart, the `registration` container that
writes the declared servers into pgAdmin's "PgToolBox" group at each start, keeping
servers people added themselves. With the operator, a registration Job does this
instead (`ServersSynced`). See
[Registration, upgrades, storage](../install/pgadmin-without-operator.md#registration-upgrades-storage).

### Restore drill

An opt-in operation that restores a Cluster's latest completed backup into a
throwaway one-instance Cluster, checks it without SQL, and deletes it. Its result
(the verdict) is `running`, `passed`, `failed`, `timedOut` or `cancelled`. Three
checks decide it: **healthy**, **promoted** (the instance left recovery and reports
itself primary on a timeline) and **size** (within ±10 % of the source); a check
reads `notChecked` when it could not be judged, never passed. See
[Restore drills](../console/restore-drills.md).

## S

### SelfSubjectAccessReview (SAR)

The question "may this person do *verb* on *resource*", posted to the API server with
the person's own token. It decides every object and button the console shows. Only
positive answers are cached, for **10 seconds**: a revocation shows within that time,
a denial is asked again on the next request, and an error grants nothing. Writes go out with the person's token, so a cached
positive cannot grant a write. See
[The access review model](../console/authorization.md#the-access-review-model).

### Severity

A property of a [finding](#finding), not of its check
([Findings and incidents](../guides/diagnostics.md#findings-and-incidents)):

| Severity | Meaning |
|---|---|
| critical | Doing damage now: PostgreSQL not serving or refusing writes, data unusable or being lost, recovery impossible. |
| high | A protection or redundancy is gone while PostgreSQL still serves; left alone it becomes critical. |
| warning | Something an operator should look at. |
| note | Worth knowing; costs nothing. |

### SSE invalidation

The console's live-update stream (`/api/v1/stream`, server-sent events) carries
invalidations, never data: `invalidate` names what changed (one Cluster, or the list)
and the application refetches it through the authorized JSON API. `hello` and
`resync` mean "refetch everything" (a `resync` replaces an event the console
could not check, and is the last event when the session ends), `reauth` comes
before the session ends, and a comment arrives every 15 seconds. See [Exposure](../console/configuration.md#exposure).

### Stalled

See [ConfigurationValid, Ready, Stalled](#configurationvalid-ready-stalled).

## T

### Team mode

The `pgtools` chart deploying evidence agents itself, for the Clusters its values
list, without the operator: the agent resolves its repository from the Cluster,
authenticates the console with a shared token, and the console finds it from a fixed
list. See [Team mode](../evidence-agent.md#team-mode-without-the-operator).

### Timeline

The incident timeline that opens a Cluster's History screen: one time axis of phase
changes, primary changes, restarts, backups, findings, revisions, events, source gaps
and console starts, each with whose clock it carries. It follows the history
(`HISTORY_ENABLED`). See
[Incident timeline](../console/timeline-and-digest.md#incident-timeline).

### Triage

See [Playbook](#playbook).

## V

### Volume forecast, volume trend

The **volume trend** is the console's long record of each volume's usage, one sample
per `VOLUME_TREND_INTERVAL` kept for `VOLUME_TREND_RETENTION`, for volumes an instance
reports. The **volume forecast** fits the last 7 days of it (hourly medians,
Theil–Sen) to say when the volume fills, or refuses with "too few points" or "not
growing". See [The long trend](../console/forecasts-and-recovery.md#the-long-trend)
and [The forecast](../console/forecasts-and-recovery.md#the-forecast).

## W

### Window (insight pack)

See [Insight pack](#insight-pack).
