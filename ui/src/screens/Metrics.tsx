import { useGetMetrics } from "../api/generated/endpoints/clusters/clusters";
import type { Metrics } from "../api/generated/model";
import { BlindSpots } from "../ds/patterns/BlindSpots/BlindSpots";
import { MetricChart } from "../ds/patterns/MetricChart/MetricChart";
import { MetricTile } from "../ds/patterns/MetricTile/MetricTile";
import { StatusChip } from "../ds/patterns/StatusChip/StatusChip";
import { m } from "../paraglide/messages.js";
import { Panel } from "./Panel";
import { QueryState } from "./QueryState";
import { sourceStateWord } from "./sources";

type Params = { namespace: string; cluster: string };

/** Only http(s) links leave the console; anything else is shown as text. */
const safeUrl = (u: string) => /^https?:\/\//i.test(u);

/**
 * Where the charts come from (Go's chartSource): the configured Prometheus
 * over rangeSeconds at stepSeconds, or the console's short ring. When a
 * Prometheus is configured its SourceState is drawn with Go's health, and
 * its reason says why the charts fell back to the ring.
 */
function ChartSource({ data }: { data: Metrics }) {
  const prom = data.prometheus;
  const fromPrometheus = data.chartSource === "prometheus";
  return (
    <div className="flex flex-col gap-1 text-label" data-chart-source={data.chartSource ?? "ring"}>
      <p>
        {fromPrometheus && data.rangeSeconds && data.stepSeconds
          ? m.metrics_from_prometheus({
              range: formatDuration(data.rangeSeconds),
              step: String(data.stepSeconds),
            })
          : m.metrics_from_ring()}
      </p>
      {prom && (
        <p className="flex flex-wrap items-center gap-2" data-prometheus={prom.state}>
          <span>{m.metrics_prometheus()}:</span>
          <StatusChip health={prom.health} label={sourceStateWord(prom.state)} size="sm" />
          {prom.reason && <span className="text-muted">{prom.reason}</span>}
        </p>
      )}
    </div>
  );
}

function formatDuration(seconds: number): string {
  if (seconds % 86_400 === 0) return m.duration_days({ n: String(seconds / 86_400) });
  if (seconds % 3_600 === 0) return m.duration_hours({ n: String(seconds / 3_600) });
  return m.duration_minutes({ n: String(Math.round(seconds / 60)) });
}

/**
 * The metrics screen: the latest readings per instance, the history as charts
 * (from the configured Prometheus when Go serves it, otherwise the console's
 * short ring) and the way out to real monitoring.
 */
export function MetricsView({ data, params }: { data: Metrics; params: Params }) {
  const tiles = data.tiles ?? [];
  const charts = data.charts ?? [];
  const mon = data.monitoring;
  const sync = `metrics:${params.namespace}/${params.cluster}`;
  return (
    <div className="flex flex-col gap-4">
      <p className="text-label">
        {m.metrics_source()}: <span className="font-semibold">{sourceStateWord(data.source.state)}</span>
        {data.source.reason && <span className="text-muted"> — {data.source.reason}</span>}
        {data.intervalSeconds && data.retentionSeconds ? (
          <span className="text-muted">
            {" "}
            ·{" "}
            {m.metrics_ring({
              interval: String(data.intervalSeconds),
              retention: String(Math.round(data.retentionSeconds / 60)),
            })}
          </span>
        ) : null}
      </p>
      <ChartSource data={data} />
      <Panel title={m.monitoring_title()}>
        {mon.state === "configured" && mon.url && safeUrl(mon.url) ? (
          <ul className="flex flex-wrap gap-3 text-label">
            <li>
              <a href={mon.url} rel="noreferrer noopener" target="_blank" className="text-accent underline">
                {m.monitoring_open()}
              </a>
            </li>
            {(mon.links ?? [])
              .filter((l) => safeUrl(l.url))
              .map((l) => (
                <li key={l.url}>
                  <a href={l.url} rel="noreferrer noopener" target="_blank" className="text-accent underline">
                    {l.label}
                  </a>
                </li>
              ))}
          </ul>
        ) : (
          <p className="text-label text-muted">{m.monitoring_not_configured()}</p>
        )}
      </Panel>
      <Panel title={m.metrics_now()}>
        {tiles.length === 0 ? (
          <p className="text-label text-muted">{m.metrics_none()}</p>
        ) : (
          <ul className="grid grid-cols-1 gap-2 sm:grid-cols-2 lg:grid-cols-4">
            {tiles.map((t) => (
              <li key={`${t.key}/${t.instance}`}>
                <MetricTile
                  label={t.label}
                  value={t.value}
                  unit={t.unit}
                  instance={t.instance}
                  provenance={t.provenance}
                />
              </li>
            ))}
          </ul>
        )}
      </Panel>
      <Panel title={m.metrics_trends()}>
        {charts.length === 0 ? (
          <p className="text-label text-muted">{m.metrics_none()}</p>
        ) : (
          <div className="grid grid-cols-1 gap-6 xl:grid-cols-2">
            {charts.map((c) => (
              <MetricChart
                key={c.key}
                title={c.label}
                unit={c.unit}
                note={c.note}
                times={c.times ?? []}
                lines={(c.lines ?? []).map((l) => ({ key: l.instance, values: l.values ?? [] }))}
                syncKey={sync}
              />
            ))}
          </div>
        )}
      </Panel>
      <BlindSpots items={data.blindSpots} />
    </div>
  );
}

export function MetricsScreen({ namespace, cluster }: Params) {
  const query = useGetMetrics(namespace, cluster);
  return (
    <QueryState query={query} what={m.nav_metrics()}>
      {(data) => <MetricsView data={data} params={{ namespace, cluster }} />}
    </QueryState>
  );
}
