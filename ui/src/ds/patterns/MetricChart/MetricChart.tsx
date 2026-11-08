import { useEffect, useId, useLayoutEffect, useRef, useState } from "react";
import uPlot from "uplot";
import "uplot/dist/uPlot.min.css";
import { m } from "../../../paraglide/messages.js";
import { getLocale } from "../../../paraglide/runtime.js";
import { formatUnit } from "../../format";
import { asHealth, healthWord } from "../../health";
import { formatUtc } from "../../time";
import { StatusChip } from "../StatusChip/StatusChip";

/** A horizontal reference line the Go view model sets (e.g. a lag threshold) and the state beyond it. */
export type Threshold = { value: number; health: string };

/** One line: a value per shared time, null where nothing was reported (a gap, not a zero). */
export type ChartLine = { key: string; label?: string; values: (number | null)[] };

const SLOTS = 8;
/** Literal class names, so Tailwind emits them (slot i → class i). */
const SWATCH = [
  "stroke-series-1",
  "stroke-series-2",
  "stroke-series-3",
  "stroke-series-4",
  "stroke-series-5",
  "stroke-series-6",
  "stroke-series-7",
  "stroke-series-8",
] as const;
const HEIGHT = 220;

/** Reads a token's current value: canvas cannot take var(), so the chart asks the cascade. */
function token(name: string): string {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

/** Bumps when the theme changes (pinned attribute or OS preference), so the canvas repaints in the new tokens. */
function useThemeVersion(): number {
  const [v, setV] = useState(0);
  useEffect(() => {
    const bump = () => setV((x) => x + 1);
    const mo = new MutationObserver(bump);
    mo.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    mq.addEventListener("change", bump);
    return () => {
      mo.disconnect();
      mq.removeEventListener("change", bump);
    };
  }, []);
  return v;
}

/** The exact number, for the table (its header names the unit). */
const number = (v: number) => new Intl.NumberFormat(getLocale()).format(v);
/** A reading for the legend and the axis: in its unit, sized (formatUnit, shared with MetricTile). */
const fmt = (v: number | null | undefined, unit?: string) =>
  v === null || v === undefined ? m.not_reported() : formatUnit(v, unit);

/**
 * MetricChart — one measure over time, uPlot behind this
 * wrapper. Series take the categorical slots in the order given, never
 * cycled (a ninth folds upstream); a null point is a gap, never a zero.
 * Thresholds and their states come from Go. A legend names every series with
 * its last value, and the same numbers are one click away as a table (the
 * palette's relief rule). syncKey links the cursors of charts that share it.
 */
export function MetricChart({
  title,
  times,
  lines,
  unit,
  note,
  thresholds = [],
  syncKey,
  stale = false,
}: {
  title: string;
  /** Shared sample instants (ISO), ascending, as Go serves them. */
  times: string[];
  lines: ChartLine[];
  unit?: string;
  /** How to read the chart, from Go. */
  note?: string;
  thresholds?: Threshold[];
  syncKey?: string;
  stale?: boolean;
}) {
  const host = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);
  /** The point under the cursor, or null for the last reading. */
  const [cursor, setCursor] = useState<number | null>(null);
  const themeVersion = useThemeVersion();
  const titleId = useId();
  const shown = lines.slice(0, SLOTS);
  const hasPoints = times.length > 0 && shown.some((l) => l.values.some((v) => v !== null));
  const name = (l: ChartLine) => l.label ?? l.key;

  // biome-ignore lint/correctness/useExhaustiveDependencies: rebuilt when the data or the theme changes.
  useLayoutEffect(() => {
    const el = host.current;
    if (!el || !hasPoints) return;
    const xs = times.map((t) => Date.parse(t) / 1000);
    const ys = shown.map((l) => times.map((_, i) => l.values[i] ?? null));
    const muted = token("--text-muted");
    const grid = token("--border");
    const opts: uPlot.Options = {
      width: Math.max(el.clientWidth, 280),
      height: HEIGHT,
      legend: { show: false },
      cursor: syncKey ? { sync: { key: syncKey } } : {},
      scales: { x: { time: true } },
      // Times read in UTC, as every other time in the console.
      tzDate: (ts: number) => uPlot.tzDate(new Date(ts * 1e3), "UTC"),
      axes: [
        { stroke: muted, grid: { stroke: grid, width: 1 }, ticks: { stroke: grid, width: 1 } },
        {
          stroke: muted,
          grid: { stroke: grid, width: 1 },
          ticks: { stroke: grid, width: 1 },
          // Ticks in the reading's unit ("512 MiB", "2 h"), wide enough for the longest.
          values: (_u: uPlot, splits: number[]) => splits.map((v) => formatUnit(v, unit)),
          size: (_u: uPlot, values: string[] | null) =>
            Math.max(50, 16 + 7 * Math.max(0, ...(values ?? []).map((v) => v.length))),
        },
      ],
      series: [
        {},
        ...shown.map((s, i) => ({
          label: name(s),
          stroke: token(`--series-${i + 1}`),
          width: 2,
          spanGaps: false,
        })),
      ],
      hooks: {
        setCursor: [(u: uPlot) => setCursor(u.cursor.idx ?? null)],
        draw: [
          (u: uPlot) => {
            const ctx = u.ctx;
            for (const th of thresholds) {
              const y = u.valToPos(th.value, "y", true);
              ctx.save();
              ctx.strokeStyle = token(`--health-${asHealth(th.health)}-icon`);
              ctx.lineWidth = 1.5 * devicePixelRatio;
              ctx.setLineDash([6 * devicePixelRatio, 4 * devicePixelRatio]);
              ctx.beginPath();
              ctx.moveTo(u.bbox.left, y);
              ctx.lineTo(u.bbox.left + u.bbox.width, y);
              ctx.stroke();
              ctx.restore();
            }
          },
        ],
      },
    };
    plot.current = new uPlot(opts, [xs, ...ys] as uPlot.AlignedData, el);
    const ro = new ResizeObserver(() =>
      plot.current?.setSize({ width: Math.max(el.clientWidth, 280), height: HEIGHT }),
    );
    ro.observe(el);
    return () => {
      ro.disconnect();
      plot.current?.destroy();
      plot.current = null;
    };
  }, [times, lines, thresholds, syncKey, themeVersion, hasPoints, unit]);

  return (
    <figure aria-labelledby={titleId} className="flex flex-col gap-2" data-stale={stale ? "" : undefined}>
      <figcaption id={titleId} className="flex flex-wrap items-center gap-2 font-semibold text-label">
        {title}
        {stale && <StatusChip health="unknown" stale label={m.chart_stale()} size="sm" />}
      </figcaption>
      {hasPoints ? (
        <div ref={host} role="img" aria-label={title} className="min-w-0" />
      ) : (
        <p className="rounded-control border border-line border-dashed p-4 text-label text-muted">
          {m.chart_no_points()}
        </p>
      )}
      {shown.length > 0 && (
        <ul className="flex flex-wrap gap-x-4 gap-y-1 text-label">
          {cursor !== null && times[cursor] && (
            <li className="text-muted">{m.chart_at({ time: formatUtc(times[cursor]) })}</li>
          )}
          {shown.map((l, i) => {
            const at = cursor !== null ? times[cursor] : undefined;
            const value = fmt(cursor !== null ? l.values[cursor] : l.values.at(-1), unit);
            return (
              <li key={l.key} className="flex items-center gap-1.5">
                <svg viewBox="0 0 16 4" width={16} height={4} aria-hidden="true">
                  <line x1={0} y1={2} x2={16} y2={2} className={SWATCH[i]} strokeWidth={3} />
                </svg>
                <span>{name(l)}</span>
                <span className="num text-muted">{at ? value : m.chart_last({ value })}</span>
              </li>
            );
          })}
          {thresholds.map((t) => (
            <li key={`${t.value}-${t.health}`} className="text-muted">
              {m.chart_threshold({ value: formatUnit(t.value, unit), state: healthWord(asHealth(t.health)) })}
            </li>
          ))}
        </ul>
      )}
      {note && <p className="text-caption text-muted">{note}</p>}
      {hasPoints && (
        <details className="text-label">
          <summary className="cursor-pointer text-accent">{m.chart_table()}</summary>
          <p className="mt-1 text-caption text-muted">{m.chart_gap_note()}</p>
          {/* biome-ignore lint/a11y/noNoninteractiveTabindex: a scrolling table must be reachable by keyboard. */}
          <section className="mt-1 max-h-64 overflow-auto" tabIndex={0} aria-label={m.chart_table()}>
            <table className="w-full text-left">
              <caption className="sr-only">{title}</caption>
              <thead>
                <tr>
                  <th scope="col" className="pr-4 font-semibold">
                    {m.chart_time()}
                  </th>
                  {shown.map((l) => (
                    <th key={l.key} scope="col" className="pr-4 font-semibold">
                      {name(l)}
                      {unit && <span className="font-normal text-muted"> ({unit})</span>}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {times.map((t, row) => (
                  <tr key={t} className="border-line border-t">
                    <th scope="row" className="pr-4 font-normal">
                      {formatUtc(t)}
                    </th>
                    {shown.map((l) => {
                      const v = l.values[row];
                      return (
                        <td key={l.key} className={v === null || v === undefined ? "pr-4 text-muted" : "pr-4"}>
                          {v === null || v === undefined ? m.not_reported() : number(v)}
                        </td>
                      );
                    })}
                  </tr>
                ))}
              </tbody>
            </table>
          </section>
        </details>
      )}
    </figure>
  );
}

/** Aligns point series (InstanceRow.series) on shared times; a missing point is a gap. Reshaping only. */
export function alignSeries(series: { key: string; points: { at: string; value: number | null }[] | null }[]): {
  times: string[];
  lines: ChartLine[];
} {
  const times = [...new Set(series.flatMap((s) => (s.points ?? []).map((p) => p.at)))].sort();
  return {
    times,
    lines: series.map((s) => {
      const byAt = new Map((s.points ?? []).map((p) => [p.at, p.value]));
      return { key: s.key, values: times.map((t) => byAt.get(t) ?? null) };
    }),
  };
}
