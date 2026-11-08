// Geometry of the recovery-window bar: where Go's spans and markers fall on
// a box of a given width. Times in, positions out; nothing is judged here.
import type { RecoveryMarker, RecoverySegment, RecoveryTimeline } from "../api/generated/model";

export type BarSpan = RecoverySegment & { x: number; w: number };
type BarMarker = RecoveryMarker & { x: number };
export type BarLayout = { start: number; end: number; spans: BarSpan[]; markers: BarMarker[] };

/** A hole narrower than this (in box units) is widened so it can be seen; the list states the real times. */
export const MIN_SPAN_WIDTH = 2;
const HOUR = 3_600_000;
const round = (n: number) => Math.round(n * 100) / 100;

/**
 * Lays the timeline out on [0, width]: the domain runs from the earliest to
 * the latest instant the timeline names (window, spans, markers), padded by
 * 3 % on each side (an hour when everything is one instant) so the ends do
 * not touch the frame. null when the timeline names no instant.
 */
export function recoveryBarLayout(t: RecoveryTimeline, width = 1000): BarLayout | null {
  const segments = t.segments ?? [];
  const marks = t.markers ?? [];
  const instants = [t.earliest, t.latest, ...segments.flatMap((s) => [s.from, s.to]), ...marks.map((mk) => mk.at)]
    .filter((v): v is string => Boolean(v))
    .map(Date.parse)
    .filter((n) => !Number.isNaN(n));
  if (instants.length === 0) return null;
  const lo = Math.min(...instants);
  const hi = Math.max(...instants);
  const pad = hi > lo ? (hi - lo) * 0.03 : HOUR;
  const start = lo - pad;
  const end = hi + pad;
  const x = (iso: string) => round(((Date.parse(iso) - start) / (end - start)) * width);
  return {
    start,
    end,
    spans: segments.map((s) => {
      const from = x(s.from);
      return { ...s, x: from, w: Math.max(round(x(s.to) - from), MIN_SPAN_WIDTH) };
    }),
    markers: marks.map((mk) => ({ ...mk, x: x(mk.at) })),
  };
}
