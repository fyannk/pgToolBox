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
 * No evidence agent and WAL archiving failing since 08:10: the operator's
 * window ends at the last base backup; the primary's own pg_stat_archiver
 * says WAL after 08:10 is likely not in the repository (Go's wording,
 * internal/console/status/recovery.go reportedTail).
 */
export const recoveryReportedArchivingFailing = {
  ...recoveryReportedOnly,
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
