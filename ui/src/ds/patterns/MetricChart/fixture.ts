import type { ChartLine } from "./MetricChart";

const T0 = Date.parse("2026-10-29T11:30:00Z");

/** n shared instants 30 s apart ending near the fixture clock. */
export const fixtureTimes = (n = 60) =>
  Array.from({ length: n }, (_, i) => new Date(T0 + i * 30_000).toISOString().replace(".000Z", "Z"));

/** A deterministic line: f(i) rounded, null where `gaps` says. */
export function fixtureLine(key: string, f: (i: number) => number, n = 60, gaps: number[] = []): ChartLine {
  return { key, values: Array.from({ length: n }, (_, i) => (gaps.includes(i) ? null : Math.round(f(i) * 100) / 100)) };
}

/** Replication lag of two replicas in seconds, one drifting past the threshold, with a reporting gap. */
export const lagLines: ChartLine[] = [
  fixtureLine("billing-2", (i) => 0.2 + 0.1 * Math.sin(i / 4)),
  fixtureLine("billing-3", (i) => (i < 30 ? 0.4 : 0.4 + (i - 30) * 0.5), 60, [41, 42, 43]),
];
