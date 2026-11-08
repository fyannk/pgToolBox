// Presentation helpers of the insight screens. Every threshold, verdict and
// time-to-horizon is Go's (viewmodel/insight.go); these only choose which of
// Go's facts a cell states, and fill the queryid into Go's SQL template.
import type { InsightHorizon, InsightRate } from "../../api/generated/model";

/** What one horizon cell says, from Go's fields only. */
export type HorizonReading =
  /** Past the threshold, by `over` (Go's remaining, as a magnitude). */
  | { kind: "past"; over: number }
  /** Reached in `seconds` at the measured rate (Go's inSeconds). */
  | { kind: "in"; seconds: number }
  /** A rate was measured and it is zero: the age did not grow over the window. */
  | { kind: "notApproaching" }
  /** No rate measured yet: the time to the threshold is unknown, never "far away". */
  | { kind: "noRate" };

export function readHorizon(h: InsightHorizon, rate: InsightRate | null | undefined): HorizonReading {
  if (h.past) return { kind: "past", over: Math.abs(h.remaining) };
  if (h.inSeconds !== undefined && h.inSeconds !== null) return { kind: "in", seconds: h.inSeconds };
  if (rate && rate.perSecond <= 0) return { kind: "notApproaching" };
  return { kind: "noRate" };
}

/**
 * Go's textQuery with QUERYID replaced by the statement's queryid. A queryid
 * is a signed 64-bit integer; anything else is not substituted (null), so a
 * malformed value never ends up in SQL someone pastes into pgAdmin.
 */
export function statementSql(textQuery: string, queryId: string): string | null {
  if (!/^-?\d{1,20}$/.test(queryId) || !textQuery.includes("QUERYID")) return null;
  return textQuery.replaceAll("QUERYID", queryId);
}

/** The least interval between two lock-graph reads from this screen, whatever the server allows. */
const BLOCKING_POLL_FLOOR_MS = 2_000;

/** How often the blocking screen reads the lock graph: Go's minRefreshSeconds, never under the floor. */
export function blockingPollMs(minRefreshSeconds: number | null | undefined): number {
  return Math.max(BLOCKING_POLL_FLOOR_MS, Math.ceil((minRefreshSeconds ?? 0) * 1000));
}

/** Only https links leave the console (pgAdmin URLs); anything else is shown as text. */
export const isHttps = (u: string | null | undefined): u is string => !!u && /^https:\/\//i.test(u);
