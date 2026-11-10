import { type FormEvent, useId, useState } from "react";
import { useGetRestoreCheck } from "../api/generated/endpoints/clusters/clusters";
import type {
  ClusterRef,
  RecoveryMarker,
  RecoverySegment,
  RecoveryTimeline,
  RestoreCheck,
} from "../api/generated/model";
import { CopyCode } from "../ds/patterns/CopyCode/CopyCode";
import { Provenance } from "../ds/patterns/Provenance/Provenance";
import { ErrorState, LoadingState } from "../ds/patterns/States/States";
import { StatusChip } from "../ds/patterns/StatusChip/StatusChip";
import { formatUtcMinute, isoToUtcInput, utcInputToIso } from "../ds/time";
import { m } from "../paraglide/messages.js";
import { Panel } from "./Panel";
import { type BarSpan, recoveryBarLayout } from "./recoveryBar";
import { phraseText, reasonsText, restoreAnswerText } from "./recoveryPhrase";

type SpanKind = RecoverySegment["kind"];

/** Go's timeline state drawn on the health vocabulary (the state itself is Go's). */
const STATE_HEALTH: Record<RecoveryTimeline["state"], string> = {
  healthy: "ok",
  warning: "warning",
  unhealthy: "critical",
  unknown: "unknown",
};

/** origin: who reported a "reported" span (the operator, or the evidence agent without recovery paths). */
function spanWord(k: SpanKind, origin?: string): string {
  switch (k) {
    case "covered":
      return m.span_covered();
    case "missingWAL":
      return m.span_missing_wal();
    case "candidateGap":
      return m.span_candidate_gap();
    case "reported":
      return origin === "evidence" ? m.span_reported_agent() : m.span_reported();
    case "walEnd":
      return m.span_wal_end();
    default:
      return m.span_unknown();
  }
}

function markerWord(k: RecoveryMarker["kind"]): string {
  switch (k) {
    case "completed":
      return m.marker_completed();
    case "failed":
      return m.marker_failed();
    default:
      return m.marker_incomplete();
  }
}

/** The evidence agent's assumption codes (evidenceapi assumptionCodes), in words; an unknown code is shown as is. */
function assumptionWords(code: string): string {
  switch (code) {
    case "segment-name-presence-only":
      return m.assumption_segment_names();
    case "timeline-history-metadata-only":
      return m.assumption_timeline_history();
    case "wal-bytes-and-restore-not-verified":
      return m.assumption_wal_bytes();
    case "backup-system-identifier-unavailable":
      return m.assumption_backup_sysid();
    case "wal-system-identifier-unavailable":
      return m.assumption_wal_sysid();
    default:
      return code;
  }
}

/** Fill and outline of each span kind: solid for covered, a distinct outline or hatching for each kind of hole. */
const SPAN_CLASS: Record<SpanKind, string> = {
  covered: "fill-health-ok-icon stroke-health-ok-icon",
  missingWAL: "fill-health-critical-icon stroke-health-critical-icon",
  candidateGap: "fill-health-warning-bg stroke-health-warning-icon",
  unknown: "fill-health-unknown-bg stroke-health-unknown-icon",
  reported: "stroke-muted",
  walEnd: "fill-health-warning-icon stroke-health-warning-icon",
};
const SPAN_DASH: Partial<Record<SpanKind, string>> = {
  candidateGap: "6 3",
  unknown: "2 3",
  reported: "4 2",
  walEnd: "1 1",
};
const MARKER_CLASS: Record<RecoveryMarker["kind"], string> = {
  completed: "stroke-fg",
  failed: "stroke-health-critical-icon",
  incomplete: "stroke-health-warning-icon",
};
const MARKER_DASH: Partial<Record<RecoveryMarker["kind"], string>> = { failed: "3 2", incomplete: "1 2" };

const W = 1000;
const H = 44;
const BAR_Y = 14;
const BAR_H = 22;

/** A small legend swatch drawn with the bar's own classes. */
function Swatch({ kind, hatch }: { kind: SpanKind; hatch: string }) {
  return (
    <svg viewBox="0 0 20 12" width={20} height={12} aria-hidden="true">
      <rect
        x={1}
        y={1}
        width={18}
        height={10}
        className={SPAN_CLASS[kind]}
        fill={kind === "reported" ? `url(#${hatch})` : undefined}
        strokeWidth={1.5}
        strokeDasharray={SPAN_DASH[kind]}
      />
    </svg>
  );
}

function MarkerSwatch({ kind }: { kind: RecoveryMarker["kind"] }) {
  return (
    <svg viewBox="0 0 8 14" width={8} height={14} aria-hidden="true">
      <line
        x1={4}
        x2={4}
        y1={0}
        y2={14}
        className={MARKER_CLASS[kind]}
        strokeWidth={2}
        strokeDasharray={MARKER_DASH[kind]}
      />
    </svg>
  );
}

function spanLine(s: BarSpan | RecoverySegment, origin?: string): string {
  const head = m.span_line({
    from: formatUtcMinute(s.from),
    to: formatUtcMinute(s.to),
    kind: spanWord(s.kind, origin),
  });
  const backup = s.backupId ? ` ${m.span_backup({ id: s.backupId })}` : "";
  return `${head}${backup}`;
}

/** The bar: covered spans filled, holes drawn per kind, base backups as ticks with their text as tooltip. */
function Bar({ t, hatch }: { t: RecoveryTimeline; hatch: string }) {
  const layout = recoveryBarLayout(t, W);
  if (!layout) return null;
  const kinds = [...new Set(layout.spans.map((s) => s.kind))];
  const markerKinds = [...new Set(layout.markers.map((mk) => mk.kind))];
  return (
    <div className="flex flex-col gap-1">
      <svg
        viewBox={`0 0 ${W} ${H}`}
        preserveAspectRatio="none"
        className="h-12 w-full"
        role="img"
        aria-label={m.recovery_bar_label({
          from: t.earliest ? formatUtcMinute(t.earliest) : m.not_reported(),
          to: t.latest ? formatUtcMinute(t.latest) : m.not_reported(),
        })}
      >
        <defs>
          <pattern id={hatch} patternUnits="userSpaceOnUse" width={8} height={8} patternTransform="rotate(45)">
            <line x1={0} y1={0} x2={0} y2={8} className="stroke-muted" strokeWidth={3} />
          </pattern>
        </defs>
        <rect
          x={0}
          y={BAR_Y}
          width={W}
          height={BAR_H}
          className="fill-surface-2 stroke-line"
          strokeWidth={1}
          vectorEffect="non-scaling-stroke"
        />
        {layout.spans.map((s) => (
          <rect
            key={`${s.from}-${s.kind}`}
            x={s.x}
            y={BAR_Y}
            width={s.w}
            height={BAR_H}
            className={SPAN_CLASS[s.kind]}
            fill={s.kind === "reported" ? `url(#${hatch})` : undefined}
            strokeWidth={1.5}
            strokeDasharray={SPAN_DASH[s.kind]}
            vectorEffect="non-scaling-stroke"
            data-kind={s.kind}
          >
            <title>
              {s.text
                ? `${spanLine(s, t.provenance.origin)} — ${phraseText(s.code, s.params, s.text)}`
                : spanLine(s, t.provenance.origin)}
            </title>
          </rect>
        ))}
        {layout.markers.map((mk) => (
          <g key={`${mk.at}-${mk.backupId ?? mk.name ?? ""}-${mk.kind}`} data-marker={mk.kind}>
            <title>
              {[markerWord(mk.kind), mk.backupId ?? mk.name, formatUtcMinute(mk.at), mk.text]
                .filter(Boolean)
                .join(" — ")}
            </title>
            <line
              x1={mk.x}
              x2={mk.x}
              y1={2}
              y2={BAR_Y + BAR_H}
              className={MARKER_CLASS[mk.kind]}
              strokeWidth={2}
              strokeDasharray={MARKER_DASH[mk.kind]}
              vectorEffect="non-scaling-stroke"
            />
          </g>
        ))}
      </svg>
      <div className="flex justify-between gap-2 text-caption text-muted">
        <span>{t.earliest ? m.recovery_earliest({ at: formatUtcMinute(t.earliest) }) : null}</span>
        <span className="flex flex-wrap justify-end gap-x-3">
          {t.newestCoveredTarget && (
            <span
              className="font-medium text-fg"
              data-newest-covered-target=""
              title={m.recovery_newest_covered_hint()}
            >
              {m.recovery_newest_covered_target({ at: formatUtcMinute(t.newestCoveredTarget) })}
            </span>
          )}
          {t.latest ? (
            <span data-wal-received="">
              {t.basis === "reported" && t.provenance.origin === "operator"
                ? m.recovery_last_base_backup({ at: formatUtcMinute(t.latest) })
                : m.recovery_latest({ at: formatUtcMinute(t.latest) })}
            </span>
          ) : null}
        </span>
      </div>
      <ul aria-label={m.recovery_legend()} className="m-0 flex list-none flex-wrap gap-x-4 gap-y-1 p-0 text-caption">
        {kinds.map((k) => (
          <li key={k} className="flex items-center gap-1.5">
            <Swatch kind={k} hatch={hatch} />
            {spanWord(k, t.provenance.origin)}
          </li>
        ))}
        {markerKinds.map((k) => (
          <li key={k} className="flex items-center gap-1.5">
            <MarkerSwatch kind={k} />
            {markerWord(k)}
          </li>
        ))}
      </ul>
    </div>
  );
}

function answerWord(a: RestoreCheck["answer"], origin?: string): string {
  switch (a) {
    case "covered":
      return m.answer_covered();
    case "notCovered":
      return m.answer_not_covered();
    case "gap":
      return m.answer_gap();
    case "reported":
      return origin === "evidence" ? m.answer_reported_agent() : m.answer_reported();
    default:
      return m.answer_unknown();
  }
}

export function RestoreAnswer({ r }: { r: RestoreCheck }) {
  return (
    <div
      className="flex flex-col gap-1 rounded-control border border-line p-3"
      aria-live="polite"
      data-answer={r.answer}
    >
      <p className="m-0 font-semibold">
        {m.answer_heading({ at: formatUtcMinute(r.at), answer: answerWord(r.answer, r.provenance.origin) })}
      </p>
      <p className="m-0 text-label" data-answer-code={r.code}>
        {restoreAnswerText(r)}
      </p>
      {r.historyFile && (
        <p className="health m-0 rounded-control px-3 py-1.5 text-label" data-health="warning" data-history-file="">
          {m.rp_check_history_file({ file: r.historyFile })}
        </p>
      )}
      {r.backupId && (
        <p className="m-0 text-label">
          {m.answer_backup()} <code className="font-code">{r.backupId}</code>
        </p>
      )}
      {r.answer === "covered" && (
        <p className="m-0 text-label" data-target-timeline-latest="">
          {m.answer_target_timeline()}
        </p>
      )}
      {(r.caveats ?? []).length > 0 && (
        <>
          <p className="m-0 text-label">{m.answer_caveats()}</p>
          <ul className="m-0 flex flex-col gap-0.5 pl-5 text-label text-muted">
            {(r.caveats ?? []).map((c) => (
              <li key={`${c.code}-${c.text}`} data-caveat={c.code}>
                {phraseText(c.code, c.params, c.text)}
              </li>
            ))}
          </ul>
        </>
      )}
      <Provenance provenance={r.provenance} />
    </div>
  );
}

/**
 * "Can I restore to…?": asks Go for one instant (UTC). The answer is Go's,
 * read off the same timeline; "structurally covered" is the strongest word.
 */
function RestoreCheckForm({ cluster, initial }: { cluster: ClusterRef; initial?: string }) {
  const inputId = useId();
  const hintId = useId();
  const [value, setValue] = useState(initial ? isoToUtcInput(initial) : "");
  const [at, setAt] = useState<string | null>(null);
  const [invalid, setInvalid] = useState(false);
  const query = useGetRestoreCheck(
    cluster.namespace,
    cluster.name,
    { at: at ?? "" },
    { query: { enabled: at !== null, retry: false } },
  );
  const submit = (e: FormEvent) => {
    e.preventDefault();
    const iso = utcInputToIso(value);
    setInvalid(iso === null);
    if (iso) setAt(iso);
  };
  return (
    <div className="flex flex-col gap-2">
      <h3 className="m-0 font-semibold text-label">{m.restore_check_title()}</h3>
      <form className="flex flex-wrap items-end gap-2" onSubmit={submit} noValidate>
        <span className="flex flex-col gap-1">
          <label htmlFor={inputId} className="text-label">
            {m.restore_check_label()}
          </label>
          <input
            id={inputId}
            type="datetime-local"
            step={60}
            value={value}
            aria-describedby={hintId}
            aria-invalid={invalid || undefined}
            onChange={(e) => setValue(e.currentTarget.value)}
            className="rounded-control border border-line-strong bg-surface px-2 py-1 font-code text-label"
          />
        </span>
        <button
          type="submit"
          className="rounded-control border border-line-strong bg-surface-2 px-3 py-1 font-medium text-label hover:bg-accent-weak"
        >
          {m.restore_check_submit()}
        </button>
      </form>
      <p id={hintId} className="m-0 text-caption text-muted">
        {invalid ? m.restore_check_invalid() : m.restore_check_hint()}
      </p>
      {at !== null &&
        (query.isPending ? (
          <LoadingState label={m.restore_check_title()} rows={2} />
        ) : query.data ? (
          <RestoreAnswer r={query.data} />
        ) : (
          <ErrorState error={query.error} what={m.restore_check_title()} onRetry={() => void query.refetch()} />
        ))}
    </div>
  );
}

/**
 * The recovery window of one Cluster: the instants a restore could target,
 * from the repository evidence or, without it, the operator's own report
 * (drawn hatched and labelled unverified). The bar has a text twin: every
 * span and base backup is listed below it.
 */
/**
 * The edge of the window is uncertain when the evidence cannot place the
 * newest restorable point: the newest span is neither covered nor the
 * end of the archived WAL (which every frontier ends with), or the
 * recent end is not restorable or unknown. A reported window's end is
 * never a WAL frontier: its edge is always uncertain.
 */
function uncertainEdge(t: RecoveryTimeline): boolean {
  if (t.basis === "reported") return true;
  if (t.basis !== "evidence") return false;
  const last = (t.segments ?? []).at(-1);
  return (
    t.state === "unhealthy" ||
    t.state === "unknown" ||
    (last !== undefined && last.kind !== "covered" && last.kind !== "walEnd")
  );
}

/** What a DBA can check by hand at an uncertain edge; the console runs none of it. Without an agent there is no evidence to refresh. */
function PitrEdgeHint({ basis }: { basis: RecoveryTimeline["basis"] }) {
  return (
    <div className="flex flex-col gap-1 text-label" data-pitr-edge-hint={basis}>
      <p className="m-0">{m.pitr_edge_hint_archiver()}</p>
      <CopyCode code="SELECT * FROM pg_stat_archiver;" label="pg_stat_archiver" />
      <p className="m-0">{basis === "reported" ? m.pitr_edge_hint_switch_reported() : m.pitr_edge_hint_switch()}</p>
      <CopyCode code="SELECT pg_switch_wal();" label="pg_switch_wal" />
    </div>
  );
}

export function RecoveryWindowPanel({
  t,
  cluster,
  windowDays,
}: {
  t: RecoveryTimeline;
  cluster: ClusterRef;
  windowDays?: number;
}) {
  const hatch = `hatch${useId().replace(/[^a-zA-Z0-9_-]/g, "")}`;
  const segments = t.segments ?? [];
  const markers = t.markers ?? [];
  const assumptions = t.assumptions ?? [];
  return (
    <Panel title={m.panel_recovery_window()}>
      <div className="flex flex-wrap items-center gap-2">
        <StatusChip
          health={STATE_HEALTH[t.state] ?? "unknown"}
          stale={t.provenance.stale}
          observedAt={t.provenance.observedAt}
        />
        <span className="text-label">
          {t.basis === "evidence"
            ? m.recovery_basis_evidence()
            : t.basis === "reported"
              ? m.recovery_basis_reported()
              : m.recovery_basis_none()}
        </span>
        {t.targetTimeline !== undefined && (
          <span className="text-caption text-muted">{m.recovery_timeline({ tli: String(t.targetTimeline) })}</span>
        )}
      </div>
      {t.reason && (
        <p className="m-0 text-label" data-recovery-reason="">
          {reasonsText(t.reasons, t.reason)}
        </p>
      )}
      {t.timelineWarning && (
        <p className="health m-0 rounded-control px-3 py-1.5 text-label" data-health="warning" data-timeline-warning="">
          <span className="font-semibold">{m.recovery_timeline_warning()}:</span>{" "}
          {phraseText(t.timelineWarning.code, t.timelineWarning.params, t.timelineWarning.text)}
        </p>
      )}
      {t.basis !== "none" && <Bar t={t} hatch={hatch} />}
      {t.basis === "evidence" && !t.complete && (
        <p className="health m-0 rounded-control px-3 py-1.5 text-label" data-health="unknown">
          {m.recovery_lower_bound()}
        </p>
      )}
      {segments.length > 0 && (
        <ol aria-label={m.recovery_spans()} className="m-0 flex flex-col gap-1 pl-5 text-label">
          {segments.map((s) => (
            <li key={`${s.from}-${s.kind}`} data-kind={s.kind} data-code={s.code}>
              {spanLine(s, t.provenance.origin)}
              {s.text && <span className="text-muted"> — {phraseText(s.code, s.params, s.text)}</span>}
            </li>
          ))}
        </ol>
      )}
      {(t.offTarget ?? []).length > 0 && (
        <div className="flex flex-col gap-1 text-label" data-off-target="">
          <p className="m-0 font-semibold">{m.recovery_off_target()}</p>
          <ul className="m-0 flex flex-col gap-0.5 pl-5">
            {(t.offTarget ?? []).map((o) => (
              <li key={`${o.backupId}-${o.targetTimeline}`} data-target-timeline={o.targetTimeline}>
                {formatUtcMinute(o.from)} → {formatUtcMinute(o.to)}{" "}
                <span className="text-muted">— {phraseText(o.code, o.params, o.text)}</span>
              </li>
            ))}
          </ul>
        </div>
      )}
      {markers.length > 0 && (
        <details className="text-label">
          <summary className="cursor-pointer text-accent">{m.recovery_markers({ n: String(markers.length) })}</summary>
          <ul className="mt-1 flex flex-col gap-0.5 pl-5">
            {markers.map((mk) => (
              <li key={`${mk.at}-${mk.backupId ?? mk.name ?? ""}-${mk.kind}`}>
                {formatUtcMinute(mk.at)} — {markerWord(mk.kind)}{" "}
                <code className="font-code">{mk.backupId ?? mk.name}</code>
                {mk.text && <span className="text-muted"> — {mk.text}</span>}
              </li>
            ))}
          </ul>
        </details>
      )}
      {assumptions.length > 0 && (
        <ul aria-label={m.recovery_assumptions()} className="m-0 flex flex-col gap-0.5 pl-5 text-caption text-muted">
          {assumptions.map((a) => (
            <li key={a}>{m.not_verified({ what: assumptionWords(a) })}</li>
          ))}
        </ul>
      )}
      {uncertainEdge(t) && <PitrEdgeHint basis={t.basis} />}
      <p className="m-0 text-caption text-muted">{m.recovery_strongest()}</p>
      {t.basis !== "none" && !t.restoreTested && (
        <p className="m-0 text-label font-semibold" data-restore-tested="false">
          {m.not_restore_tested({ days: String(windowDays ?? 90) })}
        </p>
      )}
      <Provenance provenance={t.provenance} />
      <RestoreCheckForm cluster={cluster} initial={t.latest} />
    </Panel>
  );
}
