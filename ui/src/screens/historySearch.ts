// The History screen's URL state: which tab, the timeline's window and its
// kind filter. Kept in the search params so a view is a link; validated so a
// hand-edited URL never reaches the API as something it would refuse
// silently (an invalid instant or kind is dropped, the API's own 422 says
// what is wrong with a window it is sent).
import { GetTimelineKindItem, type GetTimelineParams } from "../api/generated/model";

/** The window presets, relative to when the view was opened; 24h is the API's default window. */
export const RANGE_PRESETS = ["1h", "6h", "24h", "7d"] as const;
export type RangePreset = (typeof RANGE_PRESETS)[number];
const PRESET_MS: Record<RangePreset, number> = {
  "1h": 3_600_000,
  "6h": 6 * 3_600_000,
  "24h": 24 * 3_600_000,
  "7d": 7 * 86_400_000,
};

export type TimelineKind = (typeof GetTimelineKindItem)[keyof typeof GetTimelineKindItem];
/** Every kind, in the order the API documents them. */
const TIMELINE_KINDS = Object.values(GetTimelineKindItem) as TimelineKind[];

export type HistorySearch = {
  /** The revision list instead of the timeline. */
  view?: "revisions";
  /** A preset window ending now; absent with no from/to means the API's default (24 hours). */
  range?: Exclude<RangePreset, "24h">;
  /** A fixed window (RFC 3339, UTC); takes precedence over range. */
  from?: string;
  to?: string;
  /** Kinds kept, comma-separated in the API's order. */
  kind?: string;
};

const isInstant = (v: unknown): v is string => typeof v === "string" && v !== "" && !Number.isNaN(Date.parse(v));

/** Kinds from a comma-separated list: unknown ones dropped, in the API's order. */
export function parseKinds(kind: string | undefined): TimelineKind[] {
  const wanted = new Set((kind ?? "").split(","));
  return TIMELINE_KINDS.filter((k) => wanted.has(k));
}

export function formatKinds(kinds: readonly string[]): string | undefined {
  const s = parseKinds(kinds.join(",")).join(",");
  return s || undefined;
}

export function validateHistorySearch(s: Record<string, unknown>): HistorySearch {
  const out: HistorySearch = {};
  if (s.view === "revisions") out.view = "revisions";
  if (s.range === "1h" || s.range === "6h" || s.range === "7d") out.range = s.range;
  if (isInstant(s.from)) out.from = s.from;
  if (isInstant(s.to)) out.to = s.to;
  if (typeof s.kind === "string") out.kind = formatKinds(s.kind.split(","));
  if (out.kind === undefined) delete out.kind;
  return out;
}

/** The preset a search shows as selected, or null for a fixed window. */
export function activePreset(s: HistorySearch): RangePreset | null {
  if (s.from || s.to) return null;
  return s.range ?? "24h";
}

/**
 * The API parameters of a search. A preset is anchored at `anchor` (when the
 * view was opened or the preset chosen) so the query does not change with
 * every tick of the clock; the window's end is left to the API (now).
 */
export function timelineParams(s: HistorySearch, anchor: number): GetTimelineParams {
  const p: GetTimelineParams = {};
  if (s.from || s.to) {
    if (s.from) p.from = s.from;
    if (s.to) p.to = s.to;
  } else if (s.range) {
    p.from = new Date(anchor - PRESET_MS[s.range]).toISOString();
  }
  const kinds = parseKinds(s.kind);
  if (kinds.length > 0) p.kind = kinds;
  return p;
}

/** How far a relative preset's start may lag behind now before it moves (one minute). */
export const REANCHOR_MS = 60_000;

/**
 * The anchor of a relative preset: when it was chosen (choosing it again,
 * the one already shown included, moves it to then), and then the last
 * whole re-anchor step, so "last hour" left open for three hours still
 * starts at most a minute more than an hour ago, while the query stays
 * the same between steps.
 */
export function presetAnchor(chosenAt: number, now: number, stepMs = REANCHOR_MS): number {
  return Math.max(chosenAt, Math.floor(now / stepMs) * stepMs);
}

/** A `datetime-local` value read as UTC (the form says so), as RFC 3339. */
export function localInputToUtc(v: string): string | undefined {
  if (!v) return undefined;
  const iso = `${v.length === 16 ? `${v}:00` : v}Z`;
  return Number.isNaN(Date.parse(iso)) ? undefined : new Date(iso).toISOString().replace(/\.000Z$/, "Z");
}

/** An RFC 3339 instant as a `datetime-local` value in UTC. */
export function utcToLocalInput(iso: string | undefined): string {
  if (!iso || Number.isNaN(Date.parse(iso))) return "";
  return new Date(iso).toISOString().slice(0, 16);
}
