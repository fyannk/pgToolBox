import { m } from "../../../paraglide/messages.js";

/** One reading; a missing or null value is a gap, drawn as a break in the line. */
export type SparkPoint = { at: string; value?: number | null };
/** A horizontal line the caller names, e.g. the size a forecast aims at. */
export type SparkRule = { value: number; label: string };

const W = 300;
const H = 48;
const PAD = 3;

export type SparkGeometry = {
  /** Each unbroken run of two points or more, as SVG polyline points. */
  runs: string[];
  /** Points with a gap on both sides: drawn as a dot so they are not lost. */
  dots: Array<{ x: number; y: number }>;
  rules: Array<{ y: number; label: string }>;
};

const round = (n: number) => Math.round(n * 100) / 100;

/**
 * Lays out a series on a width × height box: time on x (from the first to the
 * last reading), zero to the largest value or rule on y. Gaps stay gaps: a
 * missing value ends a run and nothing is interpolated across it.
 */
export function sparkGeometry(points: SparkPoint[], rules: SparkRule[] = [], width = W, height = H): SparkGeometry {
  const times = points.map((p) => Date.parse(p.at));
  const t0 = Math.min(...times);
  const t1 = Math.max(...times);
  const values = points.flatMap((p) => (typeof p.value === "number" ? [p.value] : []));
  const top = Math.max(0, ...values, ...rules.map((r) => r.value)) || 1;
  const x = (t: number) => round(t1 === t0 ? width : ((t - t0) / (t1 - t0)) * width);
  const y = (v: number) => round(height - PAD - (v / top) * (height - 2 * PAD));
  const runs: string[] = [];
  const dots: Array<{ x: number; y: number }> = [];
  let run: Array<[number, number]> = [];
  const flush = () => {
    const [only] = run;
    if (run.length === 1 && only) dots.push({ x: only[0], y: only[1] });
    else if (run.length > 1) runs.push(run.map(([a, b]) => `${a},${b}`).join(" "));
    run = [];
  };
  points.forEach((p, i) => {
    if (typeof p.value === "number") run.push([x(times[i] ?? t0), y(p.value)]);
    else flush();
  });
  flush();
  return { runs, dots, rules: rules.map((r) => ({ y: y(r.value), label: r.label })) };
}

/**
 * Sparkline — a small line of one measure over time with the reference lines
 * that give it meaning (a capacity, a limit). For a trend read at a glance
 * beside a sentence that states the numbers; a chart to explore is a
 * MetricChart. A gap is drawn as a gap. The label names the line for screen
 * readers; the caller states the figures in text next to it.
 */
export function Sparkline({
  label,
  points,
  rules = [],
  seriesLabel,
}: {
  label: string;
  points: SparkPoint[];
  rules?: SparkRule[];
  /** What the line is, for the legend. */
  seriesLabel: string;
}) {
  if (!points.some((p) => typeof p.value === "number")) {
    return (
      <p className="rounded-control border border-line border-dashed p-2 text-caption text-muted">
        {m.chart_no_points()}
      </p>
    );
  }
  const g = sparkGeometry(points, rules);
  const hasGap = points.some((p) => typeof p.value !== "number");
  return (
    <figure className="m-0 flex flex-col gap-1">
      <svg
        viewBox={`0 0 ${W} ${H}`}
        preserveAspectRatio="none"
        className="h-12 w-full rounded-control bg-surface-2"
        role="img"
        aria-label={label}
      >
        {g.rules.map((r) => (
          <line
            key={r.label}
            x1={0}
            x2={W}
            y1={r.y}
            y2={r.y}
            className="stroke-muted"
            strokeWidth={1}
            strokeDasharray="4 3"
            vectorEffect="non-scaling-stroke"
          />
        ))}
        {g.runs.map((pts) => (
          <polyline
            key={pts}
            points={pts}
            className="fill-none stroke-series-1"
            strokeWidth={2}
            strokeLinejoin="round"
            vectorEffect="non-scaling-stroke"
          />
        ))}
        {g.dots.map((d) => (
          <line
            key={`${d.x},${d.y}`}
            x1={d.x}
            x2={d.x}
            y1={d.y}
            y2={d.y}
            className="stroke-series-1"
            strokeWidth={4}
            strokeLinecap="round"
            vectorEffect="non-scaling-stroke"
          />
        ))}
      </svg>
      <figcaption className="flex flex-wrap gap-x-4 gap-y-0.5 text-caption text-muted">
        <span className="flex items-center gap-1.5">
          <svg viewBox="0 0 16 4" width={16} height={4} aria-hidden="true">
            <line x1={0} y1={2} x2={16} y2={2} className="stroke-series-1" strokeWidth={3} />
          </svg>
          {seriesLabel}
        </span>
        {rules.map((r) => (
          <span key={r.label} className="flex items-center gap-1.5">
            <svg viewBox="0 0 16 4" width={16} height={4} aria-hidden="true">
              <line x1={0} y1={2} x2={16} y2={2} className="stroke-muted" strokeWidth={2} strokeDasharray="4 3" />
            </svg>
            {r.label}
          </span>
        ))}
        {hasGap && <span>{m.spark_gaps()}</span>}
      </figcaption>
    </figure>
  );
}
