// Health is Go's (viewmodel.OperationHealth) for each phase.
// Operation states the API fixtures do not cover (settled, failed, timed
// out, overdue, not observable, blocked), typed against the generated model
// so they cannot drift from the OpenAPI. Anchored at fixtures.Now.
import type {
  EvidenceCheck,
  Metrics,
  Operation,
  OperationAccepted,
  RecoverySegment,
  RecoveryTimeline,
  RestoreCheck,
  Storage,
  VolumeForecast,
} from "../../src/api/generated/model";
import { backups, metrics, storageForecasts } from "./index";

const NOW = "2026-10-29T12:00:00Z";
export const NOW_MS = Date.parse(NOW);

const target = { namespace: "team-a", name: "billing" };

export const operationAccepted = {
  operationId: "op-restart-billing-1",
  kind: "restart",
  target,
  acceptedAt: "2026-10-29T11:59:30Z",
  observedGeneration: 7,
} satisfies OperationAccepted;

const requested = {
  phase: "requested" as const,
  at: "2026-10-29T11:59:29Z",
  source: "console",
  text: "Restart requested by dba@example.org",
};
const accepted = {
  phase: "accepted" as const,
  at: "2026-10-29T11:59:30Z",
  source: "Cluster.metadata.annotations",
  text: "kubectl.kubernetes.io/restartedAt set",
};
const acting = {
  phase: "acting" as const,
  at: "2026-10-29T11:59:41Z",
  source: "Cluster.status.phase",
  text: "Phase: Restarting instances",
};

export const operationActing = {
  operationId: operationAccepted.operationId,
  kind: "restart",
  target,
  phase: "acting",
  health: "progressing",
  acceptedAt: "2026-10-29T11:59:30Z",
  steps: [requested, accepted, acting],
  deadline: "2026-10-29T12:01:30Z",
} satisfies Operation;

export const operationSettled = {
  ...operationActing,
  phase: "settled",
  health: "ok",
  steps: [
    requested,
    accepted,
    acting,
    {
      phase: "settled",
      at: "2026-10-29T11:59:58Z",
      source: "Cluster.status.phase",
      text: "Phase: Cluster in healthy state",
    },
  ],
} satisfies Operation;

export const operationFailed = {
  ...operationActing,
  phase: "failed",
  health: "critical",
  steps: [
    requested,
    accepted,
    acting,
    {
      phase: "failed",
      at: "2026-10-29T11:59:58Z",
      source: "Event/Warning",
      text: "InstanceRestartFailed: billing-1 did not become ready",
    },
  ],
} satisfies Operation;

/** Past its deadline with no outcome: the server already says timedOut. */
export const operationTimedOut = {
  ...operationActing,
  phase: "timedOut",
  health: "unknown",
  deadline: "2026-10-29T11:59:50Z",
} satisfies Operation;

/** Server stopped answering: still "acting" but the deadline has passed. */
export const operationOverdue = { ...operationActing, deadline: "2026-10-29T11:59:50Z" } satisfies Operation;

/** A reload: accepted, and the operator reports no outcome for it. */
export const operationNotObservable = {
  ...operationActing,
  kind: "reload",
  phase: "notObservable",
  health: "info",
  steps: [
    requested,
    accepted,
    { ...accepted, phase: "notObservable" as const, text: "Reload requested; no outcome field exists" },
  ],
} satisfies Operation;

/** A supervised primary update waiting for the user. */
export const operationBlocked = { ...operationActing, blocked: true, health: "warning" } satisfies Operation;

// States the Go goldens do not cover, built from them: Prometheus-backed
// metrics, the evidence refusals.
/** History from the configured Prometheus: 6 h at 60 s. */
export const metricsPrometheus = {
  ...metrics,
  chartSource: "prometheus",
  prometheus: { source: "prometheus", state: "live", health: "ok" },
  rangeSeconds: 21_600,
  stepSeconds: 60,
} satisfies Metrics;

/** Prometheus configured but not answering: Go falls back to the ring and says why. */
export const metricsPrometheusDown = {
  ...metrics,
  chartSource: "ring",
  prometheus: {
    source: "prometheus",
    state: "unavailable",
    health: "warning",
    reason: "the charts show the console's short ring: Prometheus did not answer (connection refused)",
    since: "2026-10-29T11:40:00Z",
  },
} satisfies Metrics;

// ---------------------------------------------------------------- security

// The texts are Go's verbatim (internal/console/evidence/fetch.go): no Go golden carries them
// yet, so these two rows are built from the backups golden with Go's reason.
export const evidenceAudienceMismatch = {
  ...backups.evidence,
  state: "unavailable",
  health: "warning",
  repository: undefined,
  reason:
    "not verifiable: audience mismatch (the agent accepts only the token of audience pgtoolbox-evidence-agent/<its namespace> and refused the console's; check the console's per-namespace token projection, or an agent older than the console)",
} satisfies EvidenceCheck;

export const evidenceStatusNotFromOperator = {
  ...backups.evidence,
  state: "stale",
  health: "unknown",
  reason:
    "not verifiable: status not written by the operator (status.endpoint or status.caBundle was set by another field manager than pgtoolbox-status); the console does not call this agent",
} satisfies EvidenceCheck;

// ---------------------------------------------------------------- volume forecasts

// One volume of the fork-build storage golden (getStorage.billing.json) at a
// time, so each forecast state has a story of its own.
const forecastVolume = (instance: string, volume: string): VolumeForecast => {
  const v = (storageForecasts.forecasts.volumes ?? []).find((x) => x.instance === instance && x.volume === volume);
  if (!v) throw new Error(`no ${instance}/${volume} in getStorage.billing.json`);
  return v;
};
const withVolumes = (...volumes: VolumeForecast[]) =>
  ({ ...storageForecasts, forecasts: { ...storageForecasts.forecasts, volumes } }) satisfies Storage;

/** A replica created nine hours ago: too few hourly points for a date. */
export const storageTooFewPoints = withVolumes(forecastVolume("billing-3", "pgdata"));
/** The primary's data volume, forecast toward autoGrow.maxSize. */
export const storageAutoGrow = withVolumes(forecastVolume("billing-1", "pgdata"));
/** The primary's WAL volume filling, faster than linear, while archiving fails. */
export const storageArchivingFailing = withVolumes(forecastVolume("billing-1", "wal"));
/** The trend switched off in the console's configuration (VOLUME_TREND_INTERVAL=0). */
export const storageForecastsDisabled = {
  ...storageForecasts,
  forecasts: { ...storageForecasts.forecasts, state: "disabled", reason: undefined, persistent: false, volumes: [] },
} satisfies Storage;

// ---------------------------------------------------------------- recovery window

// Built from the backups golden's timeline (getBackups.orders.json) with Go's
// own wording (internal/console/status/recovery.go).
const timeline = backups.recoveryTimeline;
const segments = timeline.segments ?? [];
const markers = timeline.markers ?? [];

/** The same repository without the hole: one covered span, no failed backup. */
export const recoveryHealthy = {
  ...timeline,
  state: "healthy",
  reason: undefined,
  segments: [
    {
      from: segments[0]?.from ?? "2026-10-22T07:08:00Z",
      to: segments.at(-1)?.to ?? "2026-10-29T11:59:15Z",
      kind: "covered",
      backupId: segments.at(-1)?.backupId,
      text: segments.at(-1)?.text,
    },
  ],
  markers: markers.filter((mk) => mk.kind === "completed"),
} satisfies RecoveryTimeline;

/** Every kind of hole at once, an incomplete base backup, and a collection cut at a bound (a lower bound). */
export const recoveryMixedHoles = {
  ...timeline,
  complete: false,
  reason:
    "the window has candidate gaps a second scan has not confirmed yet; the evidence collections were cut at a bound: the coverage shown is a lower bound",
  // The golden's spans up to the WAL hole, then its last covered span cut by a candidate gap and an unknown span.
  segments: [
    ...segments.slice(0, 2),
    { from: "2026-10-25T07:08:00Z", to: "2026-10-26T02:00:00Z", kind: "covered" as const, backupId: "20261025T070800" },
    {
      from: "2026-10-26T02:00:00Z",
      to: "2026-10-26T09:00:00Z",
      kind: "candidateGap" as const,
      text: "A WAL segment is not in the listing yet; a second scan has not confirmed the gap.",
    },
    { from: "2026-10-26T09:00:00Z", to: "2026-10-27T15:00:00Z", kind: "covered" as const, backupId: "20261027T070800" },
    {
      from: "2026-10-27T15:00:00Z",
      to: "2026-10-27T18:00:00Z",
      kind: "unknown" as const,
      text: "No recovery path was observed for this span.",
    },
    { from: "2026-10-27T18:00:00Z", to: "2026-10-29T11:59:15Z", kind: "covered" as const, backupId: "20261029T070001" },
  ],
  markers: [
    ...markers,
    {
      at: "2026-10-26T07:08:00Z",
      backupId: "20261026T070800-partial",
      kind: "incomplete",
      source: "repository",
      text: "backup.info has no end time",
    },
  ],
} satisfies RecoveryTimeline;

/**
 * Right after a failover: the repository holds no WAL of the timeline the new
 * primary writes yet, so the window is a warning that says to wait for the
 * next evidence refresh or switch WAL, never a green chip with no reason.
 */
export const recoveryAfterFailover = {
  ...timeline,
  state: "warning",
  targetTimeline: 1,
  reason:
    "the repository held no WAL of timeline 2, the one the current primary writes, when it was last read: the newest restore points are uncertain; wait for the next evidence refresh, or run pg_switch_wal() on the new primary to archive its first segment",
  reasons: [
    {
      code: "recovery.reason.liveTimelineNotArchived",
      params: { timeline: "2" },
      text: "the repository held no WAL of timeline 2, the one the current primary writes, when it was last read: the newest restore points are uncertain; wait for the next evidence refresh, or run pg_switch_wal() on the new primary to archive its first segment",
    },
  ],
  segments: [
    {
      from: segments[0]?.from ?? "2026-10-22T07:08:00Z",
      to: "2026-10-29T11:40:00Z",
      kind: "covered",
      backupId: segments.at(-1)?.backupId,
      text: segments.at(-1)?.text,
    },
    {
      from: "2026-10-29T11:40:00Z",
      to: "2026-10-29T12:00:00Z",
      kind: "unknown",
      code: "recovery.segment.liveTimelineNotArchived",
      params: { timeline: "2" },
      text: "The repository held no WAL of timeline 2, the one the current primary writes, when it was last read (the failover's history file may not be archived yet, or the evidence agent has not read the repository since): this time's WAL may not be in the repository yet.",
    },
  ],
  markers: markers.filter((mk) => mk.kind === "completed"),
} satisfies RecoveryTimeline;

/**
 * The failover's history file is archived (the paths target timeline 2) but
 * no WAL segment of timeline 2 yet: the tail says so, the window is a warning.
 */
export const recoveryLiveTimelineNoSegment = {
  ...recoveryAfterFailover,
  targetTimeline: 2,
  segments: [
    ...recoveryAfterFailover.segments.slice(0, 1),
    {
      from: "2026-10-29T11:40:00Z",
      to: "2026-10-29T12:00:00Z",
      kind: "unknown",
      code: "recovery.segment.liveTimelineNoSegment",
      params: { timeline: "2" },
      text: "The repository held the history file of timeline 2, the one the current primary writes, but no WAL segment of it yet when it was last read: this time's WAL may not be in the repository yet.",
    },
  ],
} satisfies RecoveryTimeline;

/**
 * Recovery paths kept stale from an earlier scan beside a backup list cut at
 * a bound that does not list the oldest backup: the span only it reaches is
 * not counted as covered until the paths are read again.
 */
export const recoveryBackupUnconfirmed = {
  ...timeline,
  state: "warning",
  reason:
    "base backup 20261022T070800 is not in the repository's backup list, which was cut at a bound, beside recovery paths kept from an earlier scan: the span only it reaches is not counted as covered until the paths are read again",
  reasons: [
    {
      code: "recovery.reason.backupUnconfirmed",
      params: { backupId: "20261022T070800", reason: "backup-not-confirmed" },
      text: "base backup 20261022T070800 is not in the repository's backup list, which was cut at a bound, beside recovery paths kept from an earlier scan: the span only it reaches is not counted as covered until the paths are read again",
    },
  ],
  segments: [
    {
      from: "2026-10-22T07:08:00Z",
      to: "2026-10-26T07:08:00Z",
      kind: "unknown",
      code: "recovery.segment.backupUnconfirmed",
      params: { backupId: "20261022T070800", reason: "backup-not-confirmed" },
      text: "Base backup 20261022T070800 reaches here in the recovery paths kept from an earlier scan, but the repository's backup list was cut at a bound and does not list it, so it may have been purged since: not counted as covered.",
    },
    {
      from: "2026-10-26T07:08:00Z",
      to: "2026-10-29T11:59:15Z",
      kind: "covered",
      backupId: segments.at(-1)?.backupId,
      text: segments.at(-1)?.text,
    },
  ],
  markers: markers.filter((mk) => mk.kind === "completed"),
} satisfies RecoveryTimeline;

/**
 * The repository's newest timeline (3) is ahead of the one the current primary
 * writes (2): another cluster may archive here, so the restore points past the
 * newest backup of the live line are not counted as covered.
 */
export const recoveryRepositoryAhead = {
  ...timeline,
  state: "warning",
  edgeAdvice: "archiver",
  targetTimeline: 3,
  reason:
    "the repository's newest timeline, 3, is ahead of timeline 2, the one the current primary writes: another cluster may archive into this repository, and a default restore follows timeline 3, so the restore points past the fork are not counted as covered; check that no other cluster's backup configuration names this repository",
  reasons: [
    {
      code: "recovery.reason.repositoryAheadOfLive",
      params: { timeline: "3", liveTimeline: "2" },
      text: "the repository's newest timeline, 3, is ahead of timeline 2, the one the current primary writes: another cluster may archive into this repository, and a default restore follows timeline 3, so the restore points past the fork are not counted as covered; check that no other cluster's backup configuration names this repository",
    },
  ],
  segments: [
    {
      from: "2026-10-22T07:08:00Z",
      to: "2026-10-27T07:08:00Z",
      kind: "covered",
      backupId: segments.at(-1)?.backupId,
      text: segments.at(-1)?.text,
    },
    {
      from: "2026-10-27T07:08:00Z",
      to: "2026-10-29T11:59:15Z",
      kind: "unknown",
      code: "recovery.segment.repositoryAheadOfLive",
      params: { timeline: "3", liveTimeline: "2" },
      text: "The repository's newest timeline, 3, is ahead of timeline 2, the one the current primary writes: another cluster may archive into this repository, and a default restore follows timeline 3, so a time past the fork may restore that cluster's data.",
    },
  ],
  markers: markers.filter((mk) => mk.kind === "completed"),
} satisfies RecoveryTimeline;

/**
 * The Cluster is hibernated: no instance runs, so nothing confirms that the
 * WAL written before it slept reached the repository. The window is unknown,
 * not amber, and gives no advice to act on a primary (Go's wording,
 * internal/console/status/recovery.go timelineVerdict).
 */
export const recoveryHibernated = {
  ...recoveryHealthy,
  state: "unknown",
  edgeAdvice: "none",
  reason:
    "the Cluster is hibernated: whether the WAL written before it reached the repository is not confirmed, so the newest restore points are uncertain",
  reasons: [
    {
      code: "recovery.reason.archiverHibernated",
      text: "the Cluster is hibernated: whether the WAL written before it reached the repository is not confirmed, so the newest restore points are uncertain",
    },
  ],
  segments: [
    { ...(recoveryHealthy.segments[0] as RecoverySegment), to: "2026-10-28T11:59:15Z" },
    {
      from: "2026-10-28T11:59:15Z",
      to: "2026-10-29T11:59:15Z",
      kind: "unknown",
      code: "recovery.segment.archiverHibernated",
      text: "The Cluster is hibernated, and no sweep the console keeps saw its primary with nothing waiting before it slept: WAL that reached the repository may have waited on it first, so its arrival time may overstate how recent a restore can be.",
    },
  ],
} satisfies RecoveryTimeline;

/**
 * After a failover the repository holds no WAL of timeline 2 yet, and the new
 * primary was not read (fenced, unreachable): the warning gives no advice to
 * run pg_switch_wal() on it.
 */
export const recoveryPrimaryUnreadAfterFailover = {
  ...recoveryAfterFailover,
  edgeAdvice: "none",
  reason:
    "the repository held no WAL of timeline 2, the one the current primary writes, when it was last read, and the primary was not read: the newest restore points are uncertain until the primary is read again and the agent reads WAL of timeline 2",
  reasons: [
    {
      code: "recovery.reason.liveTimelineNotArchivedPrimaryUnread",
      params: { timeline: "2" },
      text: "the repository held no WAL of timeline 2, the one the current primary writes, when it was last read, and the primary was not read: the newest restore points are uncertain until the primary is read again and the agent reads WAL of timeline 2",
    },
  ],
} satisfies RecoveryTimeline;

/**
 * WAL archiving is failing with the primary read and no recent failure on
 * record (ContinuousArchiving False): the newest restore points are
 * uncertain, the hint offers pg_stat_archiver only; a WAL switch would
 * wait like every other segment (Go's wording and edgeAdvice,
 * internal/console/status/recovery.go timelineVerdict, edgeAdviceOf).
 */
export const recoveryArchivingFailing = {
  ...recoveryHealthy,
  state: "warning",
  edgeAdvice: "archiver",
  reason:
    "WAL archiving is failing: WAL written since the newest segment the repository holds is likely not in it, so the newest restore points are uncertain; the Cluster's ContinuousArchiving condition says why",
  reasons: [
    {
      code: "recovery.reason.archivingFailing",
      text: "WAL archiving is failing: WAL written since the newest segment the repository holds is likely not in it, so the newest restore points are uncertain; the Cluster's ContinuousArchiving condition says why",
    },
  ],
  segments: [
    { ...(recoveryHealthy.segments[0] as RecoverySegment), to: "2026-10-29T11:40:00Z" },
    {
      from: "2026-10-29T11:40:00Z",
      to: "2026-10-29T12:00:00Z",
      kind: "unknown",
      code: "recovery.segment.afterFrontierFailing",
      text: "After the newest archived WAL the evidence shows, and WAL archiving is failing: this time's WAL is likely not in the repository.",
    },
  ],
} satisfies RecoveryTimeline;

/**
 * The evidence agent missed its scans: the paths come from a scan more than
 * a day old, so retention may have purged the oldest base backup and the
 * WAL only it needs. The purge band at the oldest edge runs to the next
 * backup's lower bound, not just the age of the scan (Go's wording,
 * internal/console/status/recovery.go purgeBand).
 */
export const recoveryPurgeBandExtended = {
  ...recoveryHealthy,
  segments: [
    {
      from: "2026-10-22T07:08:00Z",
      to: "2026-10-23T07:08:00Z",
      kind: "unknown",
      code: "recovery.segment.purgeEdge",
      params: { marginSeconds: "86400" },
      text: "Within 24 h of the oldest restore point: a retention purge since the repository was last read may have removed this base backup.",
    },
    { ...(recoveryHealthy.segments[0] as RecoverySegment), from: "2026-10-23T07:08:00Z" },
  ],
  provenance: { ...timeline.provenance, stale: true },
} satisfies RecoveryTimeline;

/**
 * The archiving instance does not report itself primary (a replica cluster's
 * designated primary, or a primary mid-promotion): it is in recovery, where
 * pg_switch_wal() fails, so the hint offers pg_stat_archiver only (Go's
 * wording and edgeAdvice, internal/console/status/recovery.go
 * timelineVerdict, edgeAdvice).
 */
export const recoveryArchiverNotReported = {
  ...recoveryHealthy,
  state: "warning",
  edgeAdvice: "archiver",
  reason:
    "the archiving instance does not report itself primary (a replica cluster's designated primary, or a primary mid-promotion), so it does not report its archive backlog: how late recent WAL reached the repository is not known, so the newest restore points are uncertain",
  reasons: [
    {
      code: "recovery.reason.archiverNotReported",
      text: "the archiving instance does not report itself primary (a replica cluster's designated primary, or a primary mid-promotion), so it does not report its archive backlog: how late recent WAL reached the repository is not known, so the newest restore points are uncertain",
    },
  ],
  segments: [
    { ...(recoveryHealthy.segments[0] as RecoverySegment), to: "2026-10-28T11:59:15Z" },
    {
      from: "2026-10-28T11:59:15Z",
      to: "2026-10-29T11:59:15Z",
      kind: "unknown",
      code: "recovery.segment.archiverNotReported",
      text: "The archiving instance does not report itself primary (a replica cluster's designated primary, or a primary mid-promotion), so it does not report how many WAL segments wait to be archived: WAL that reached the repository may have waited on it first, so its arrival time may overstate how recent a restore can be.",
    },
  ],
} satisfies RecoveryTimeline;

/** No evidence agent: the operator's window, drawn hatched and labelled unverified. */
export const recoveryReportedOnly = {
  ...timeline,
  basis: "reported",
  state: "unknown",
  targetTimeline: undefined,
  complete: false,
  reason: "the window is the operator's report, not repository evidence; no repository evidence",
  assumptions: [],
  segments: [
    {
      from: "2026-10-22T07:08:00Z",
      to: "2026-10-29T11:59:15Z",
      kind: "reported",
      text: "Reported by Cluster.status.firstRecoverabilityPoint / lastSuccessfulBackup; not verified against the repository.",
    },
  ],
  markers: markers.filter((mk) => mk.source === "backupObject"),
  provenance: { origin: "operator", source: "Cluster.status", observedAt: "2026-10-29T11:59:58Z" },
} satisfies RecoveryTimeline;

/**
 * No evidence agent and a hibernated Cluster: no instance runs, so the hint
 * offers nothing to run on a primary (Go's edgeAdvice, from the facts on the
 * reported basis too).
 */
export const recoveryReportedHibernated = {
  ...recoveryReportedOnly,
  edgeAdvice: "none",
  reasons: [
    { code: "recovery.reason.reportedWindow", text: "the window is the operator's report, not repository evidence" },
    { code: "recovery.reason.noEvidence", text: "no repository evidence" },
  ],
} satisfies RecoveryTimeline;

/**
 * No evidence agent and WAL archiving failing since 08:10: the operator's
 * window ends at the last base backup; the primary's own pg_stat_archiver
 * says WAL after 08:10 is likely not in the repository (Go's wording,
 * internal/console/status/recovery.go reportedTail).
 */
export const recoveryReportedArchivingFailing = {
  ...recoveryReportedOnly,
  edgeAdvice: "archiver",
  latest: "2026-10-29T07:08:00Z",
  segments: [
    { ...(recoveryReportedOnly.segments[0] as RecoverySegment), to: "2026-10-29T07:08:00Z" },
    {
      from: "2026-10-29T07:08:00Z",
      to: "2026-10-29T08:10:00Z",
      kind: "unknown",
      code: "recovery.reportedTail.beforeArchivedFailing",
      params: {
        windowOrigin: "operator",
        endAt: "2026-10-29T07:08:00Z",
        archivedAt: "2026-10-29T08:10:00Z",
        ageSeconds: "8",
      },
      text: "After the last base backup the operator reports (2026-10-29T07:08:00Z): the operator does not report the archived WAL beyond it. The primary last archived a segment at 2026-10-29T08:10:00Z, after this time (its own report on /pg/status, read 1 min ago), but WAL archiving is failing: this time's WAL may be in the repository; deploy an evidence agent to place it.",
    },
    {
      from: "2026-10-29T08:10:00Z",
      to: "2026-10-29T12:00:00Z",
      kind: "unknown",
      code: "recovery.reportedTail.afterArchivedFailing",
      params: {
        windowOrigin: "operator",
        endAt: "2026-10-29T07:08:00Z",
        archivedAt: "2026-10-29T08:10:00Z",
        ageSeconds: "8",
      },
      text: "After the last base backup the operator reports (2026-10-29T07:08:00Z): the operator does not report the archived WAL beyond it. WAL archiving is failing: WAL after the primary's last archived segment (2026-10-29T08:10:00Z, its own report on /pg/status, read 1 min ago) is likely not in the repository.",
    },
  ],
} satisfies RecoveryTimeline;

/** The restore check's answer for 11:55 on that window: unknown, never covered, with the archiving caveat. */
export const restoreCheckArchivingFailing: RestoreCheck = {
  ref: { namespace: "team-a", name: "orders" },
  generatedAt: NOW,
  at: "2026-10-29T11:55:00Z",
  answer: "unknown",
  basis: "reported",
  code: "recovery.reportedTail.afterArchivedFailing",
  params: recoveryReportedArchivingFailing.segments[2]?.params,
  text: recoveryReportedArchivingFailing.segments[2]?.text ?? "",
  caveats: [
    {
      code: "recovery.caveat.archivingFailingSince",
      params: { archivedAt: "2026-10-29T08:10:00Z" },
      text: "WAL archiving is failing: the primary last archived a segment at 2026-10-29T08:10:00Z; newer WAL is on the instance, not in the repository.",
    },
    {
      code: "recovery.caveat.notRestoreTested",
      params: { days: "90" },
      text: "Not restore-tested: no restore drill of this Cluster passed in the last 90 days.",
    },
    { code: "recovery.caveat.fromTheFuture", text: "A caveat this SPA does not know yet, shown in Go's English." },
  ],
  provenance: recoveryReportedOnly.provenance,
};

/** Neither evidence nor an operator window: nothing to draw, Go says why. */
export const recoveryNone = {
  basis: "none",
  state: "unknown",
  complete: false,
  reason: "no repository evidence",
  assumptions: [],
  segments: [],
  markers: [],
  provenance: { origin: "evidence" },
} satisfies RecoveryTimeline;

/**
 * Reading the recovery paths keeps failing, and the last good ones are more
 * than three refresh intervals older than the latest scan: they are dropped,
 * and the bar says so and what to check (Go's wording,
 * internal/console/status/recovery.go keptDroppedReason).
 */
export const recoveryKeptTooOld = {
  ...recoveryNone,
  reason:
    "the recovery paths page timed out; the last good recovery paths are more than three refresh intervals (at least an hour) older than the latest scan and are no longer shown; check the evidence agent’s logs for why reading its recovery paths fails, and that the object store answers",
  reasons: [
    {
      code: "recovery.reason.pathsNotCoverage",
      params: { detail: "the recovery paths page timed out" },
      text: "the recovery paths page timed out",
    },
    {
      code: "recovery.reason.keptTooOld",
      text: "the last good recovery paths are more than three refresh intervals (at least an hour) older than the latest scan and are no longer shown; check the evidence agent’s logs for why reading its recovery paths fails, and that the object store answers",
    },
  ],
} satisfies RecoveryTimeline;

/** The repository's identity changed: the last good paths describe another repository (Go's wording). */
export const recoveryKeptRepositoryChanged = {
  ...recoveryKeptTooOld,
  reason:
    "the recovery paths page timed out; the repository’s identity changed since the last good recovery paths were read: they describe another repository and are no longer shown; check the evidence agent’s logs and that its object store configuration still names this cluster’s repository",
  reasons: [
    ...recoveryKeptTooOld.reasons.slice(0, 1),
    {
      code: "recovery.reason.keptRepositoryChanged",
      text: "the repository’s identity changed since the last good recovery paths were read: they describe another repository and are no longer shown; check the evidence agent’s logs and that its object store configuration still names this cluster’s repository",
    },
  ],
} satisfies RecoveryTimeline;
