import { Link } from "@tanstack/react-router";
import type { TimelineEntry } from "../../../api/generated/model";
import { type ClusterParams, resolveClusterLink } from "../../../app/clusterLink";
import { m } from "../../../paraglide/messages.js";
import { getLocale } from "../../../paraglide/runtime.js";
import { formatUtc } from "../../time";
import { Provenance } from "../Provenance/Provenance";
import { SeverityChip } from "../Severity/Severity";
import { StatusChip } from "../StatusChip/StatusChip";

/** The word of an entry kind (the filter chips and the entry labels share it). */
export function timelineKindWord(kind: string): string {
  switch (kind) {
    case "phase":
      return m.tl_kind_phase();
    case "primary":
      return m.tl_kind_primary();
    case "restart":
      return m.tl_kind_restart();
    case "backup":
      return m.tl_kind_backup();
    case "finding":
      return m.tl_kind_finding();
    case "revision":
      return m.tl_kind_revision();
    case "event":
      return m.tl_kind_event();
    case "gap":
      return m.tl_kind_gap();
    case "console":
      return m.tl_kind_console();
    default:
      return kind;
  }
}

function revisionWord(action: string): string {
  switch (action) {
    case "created":
      return m.change_created();
    case "first-observed":
      return m.change_first_observed();
    case "spec-changed":
      return m.change_spec_changed();
    case "status-transition":
      return m.change_status_transition();
    case "deleted":
      return m.change_deleted();
    case "deleted-unobserved":
      return m.change_deleted_unobserved();
    default:
      return action;
  }
}

/** A short label for an entry, from its kind and Go's action (internal/console/timeline actions). */
export function timelineLabel(e: Pick<TimelineEntry, "kind" | "action" | "ref">): string {
  const pair = `${e.kind}:${e.action}`;
  switch (pair) {
    case "phase:changed":
      return m.tl_phase_changed();
    case "phase:firstObserved":
      return m.tl_phase_first_observed();
    case "primary:failover":
      return m.tl_primary_failover();
    case "primary:switchover":
      return m.tl_primary_switchover();
    case "primary:changed":
      return m.tl_primary_changed();
    case "primary:firstObserved":
      return m.tl_primary_first_observed();
    case "restart:restarted":
      return m.tl_restart_restarted();
    case "backup:completed":
      return m.tl_backup_completed();
    case "backup:failed":
      return m.tl_backup_failed();
    case "finding:appeared":
      return m.tl_finding_appeared();
    case "finding:cleared":
      return m.tl_finding_cleared();
    case "finding:worsened":
      return m.tl_finding_worsened();
    case "finding:improved":
      return m.tl_finding_improved();
    case "gap:began":
      return m.tl_gap_began();
    case "gap:ended":
      return m.tl_gap_ended();
    case "console:started":
      return m.tl_console_started();
  }
  if (e.kind === "revision") return m.tl_revision({ change: revisionWord(e.action) });
  // An Event: its Kubernetes type and reason, as the API server reported them.
  if (e.kind === "event") return e.ref ? `${e.action}: ${e.ref}` : e.action;
  return `${timelineKindWord(e.kind)} ${e.action}`;
}

function utcTime(iso: string): string {
  return new Intl.DateTimeFormat(getLocale(), { timeStyle: "medium", timeZone: "UTC" }).format(new Date(iso));
}

function utcDay(iso: string): string {
  return `${new Intl.DateTimeFormat(getLocale(), { dateStyle: "full", timeZone: "UTC" }).format(new Date(iso))} (UTC)`;
}

/** The UTC calendar day of an instant, as a grouping key. */
function dayKey(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toISOString().slice(0, 10);
}

/** Whose clock the time is: the console's when it observed, or the reporter's own timestamp. */
function Timing({ entry }: { entry: TimelineEntry }) {
  const observed = entry.timing === "observed";
  return (
    <span
      className="rounded-chip border border-line px-1 font-code text-caption text-muted"
      title={observed ? m.tl_timing_observed_hint() : m.tl_timing_reported_hint()}
    >
      {observed ? m.tl_timing_observed() : m.tl_timing_reported()}
    </span>
  );
}

function scopeWord(scope: string): string | null {
  if (scope === "namespace") return m.tl_scope_namespace();
  if (scope === "console") return m.tl_scope_console();
  return null;
}

/** One entry: time and whose clock, health with the label, what, the honesty about gaps, the source, the screen that explains it. */
function TimelineItem({ entry: e, params }: { entry: TimelineEntry; params?: ClusterParams }) {
  const target = params && e.link ? resolveClusterLink(e.link, params) : null;
  const scope = scopeWord(e.scope);
  const unobservedSpan = e.since && (e.kind === "console" || e.kind === "gap");
  return (
    <li className="grid grid-cols-1 gap-x-3 gap-y-1 border-line border-t py-2 first:border-t-0 sm:grid-cols-[8rem_1fr]">
      <div className="flex flex-wrap items-baseline gap-1.5 sm:flex-col sm:items-start">
        <time dateTime={e.at} title={formatUtc(e.at)} className="num font-code text-label">
          {utcTime(e.at)}
        </time>
        <Timing entry={e} />
      </div>
      <div className="flex min-w-0 flex-col gap-1">
        <div className="flex flex-wrap items-center gap-2">
          <StatusChip health={e.health} label={timelineLabel(e)} size="sm" />
          {e.severity && <SeverityChip severity={e.severity} />}
          {e.subject && (
            <code className="font-code text-label">
              {e.subject.kind}/{e.subject.name}
            </code>
          )}
          {(e.from || e.to) && (
            <span className="text-label">
              <span className="font-code">{e.from ?? m.not_reported()}</span>
              <span aria-hidden="true"> → </span>
              <span className="sr-only"> {m.tl_to()} </span>
              <span className="font-code">{e.to ?? m.not_reported()}</span>
            </span>
          )}
          {e.count !== undefined && e.count > 1 && (
            <span className="num text-caption text-muted">{m.tl_count({ count: String(e.count) })}</span>
          )}
          {scope && <span className="rounded-chip border border-line px-1 text-caption text-muted">{scope}</span>}
        </div>
        {e.detail && <p className="m-0 break-words text-label">{e.detail}</p>}
        {e.ref && e.kind === "finding" && <code className="font-code text-caption text-muted">{e.ref}</code>}
        {e.uncertain && e.action !== "changed" && (
          <p className="health m-0 self-start rounded-control px-2 py-0.5 text-caption" data-health="unknown">
            {m.tl_uncertain()}
          </p>
        )}
        {e.afterGap && e.since ? (
          <p className="health m-0 self-start rounded-control px-2 py-0.5 text-caption" data-health="unknown">
            {m.tl_after_gap({ since: formatUtc(e.since), at: formatUtc(e.at) })}
          </p>
        ) : unobservedSpan && e.since ? (
          <p className="health m-0 self-start rounded-control px-2 py-0.5 text-caption" data-health="unknown">
            {m.tl_unobserved_span({ since: formatUtc(e.since), at: formatUtc(e.at) })}
          </p>
        ) : e.since ? (
          <p className="m-0 text-caption text-muted">{m.tl_since({ since: formatUtc(e.since) })}</p>
        ) : null}
        <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
          <Provenance provenance={e.provenance} />
          {target ? (
            <Link {...target} className="text-caption text-accent hover:underline">
              {m.tl_open_link()}
            </Link>
          ) : e.link && params ? (
            <span className="text-caption text-muted">{e.link}</span>
          ) : null}
        </div>
      </div>
    </li>
  );
}

/**
 * Timeline — what happened, newest first, grouped by UTC day
 * (history + events). Every entry says whose clock its time is (the console
 * observed it, or the operator, kubelet or API server reported it), and an
 * entry the console could only place between two observations says so
 * instead of pretending to an instant. Order and grouping are the API's
 * (newest first); the pattern only draws the day headings.
 */
export function Timeline({
  items,
  params,
  label,
  headingLevel = 3,
}: {
  items: TimelineEntry[];
  /** The Cluster the entries' links are relative to; without it links are not drawn. */
  params?: ClusterParams;
  /** Accessible name of the list. */
  label: string;
  headingLevel?: 3 | 4;
}) {
  const days: Array<{ key: string; at: string; items: TimelineEntry[] }> = [];
  for (const e of items) {
    const key = dayKey(e.at);
    const last = days[days.length - 1];
    if (last && last.key === key) last.items.push(e);
    else days.push({ key, at: e.at, items: [e] });
  }
  const Heading = headingLevel === 3 ? "h3" : "h4";
  return (
    // biome-ignore lint/a11y/useSemanticElements: a group, not a landmark or a form: a screen may hold several timelines (a digest, one per Cluster).
    <div role="group" aria-label={label} className="flex flex-col gap-3">
      {days.map((d) => (
        <div key={d.key} className="flex flex-col">
          <Heading className="m-0 border-line border-b pb-1 font-semibold text-label text-muted">
            {utcDay(d.at)}
          </Heading>
          <ol className="m-0 flex list-none flex-col p-0">
            {d.items.map((e) => (
              <TimelineItem key={e.id} entry={e} params={params} />
            ))}
          </ol>
        </div>
      ))}
    </div>
  );
}
