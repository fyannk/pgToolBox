import type { StorageForecasts, VolumeForecast } from "../api/generated/model";
import { formatBytes } from "../ds/format";
import { Provenance } from "../ds/patterns/Provenance/Provenance";
import { Sparkline } from "../ds/patterns/Sparkline/Sparkline";
import { EmptyState } from "../ds/patterns/States/States";
import { StatusChip } from "../ds/patterns/StatusChip/StatusChip";
import { formatInterval, formatUtcMinute, useNow } from "../ds/time";
import { m } from "../paraglide/messages.js";
import { getLocale } from "../paraglide/runtime.js";
import { forecastSentences } from "./forecast";
import { Fact, Facts, Panel } from "./Panel";

const count = (n: number) => new Intl.NumberFormat(getLocale()).format(n);

function Sentences({ lines }: { lines: string[] }) {
  return (
    <span className="flex flex-col gap-0.5">
      {lines.map((l, i) => (
        <span key={l} className={i === 0 ? undefined : "text-muted"}>
          {l}
        </span>
      ))}
    </span>
  );
}

/** One instance's volume: usage now, the two forecasts as sentences, the trend toward the line it aims at. */
function VolumeCard({ v, now }: { v: VolumeForecast; now: number }) {
  const bf = v.bytesForecast;
  const gf = v.autoGrowForecast;
  const rules = [{ value: v.totalBytes, label: m.spark_rule_size({ size: formatBytes(v.totalBytes) }) }];
  if (gf && gf.capacity > 0)
    rules.push({
      value: gf.capacity,
      label: m.spark_rule_autogrow({ max: v.autoGrow?.maxSize ?? formatBytes(gf.capacity) }),
    });
  const name = `${v.instance} · ${v.volume}`;
  return (
    <article
      aria-label={name}
      className="flex flex-col gap-2 rounded-control border border-line p-3"
      data-forecast={bf.state}
    >
      <header className="flex flex-wrap items-center gap-2">
        <h3 className="font-code font-semibold text-label">{name}</h3>
        {v.primary && (
          <span className="rounded-pill border border-line-strong px-1.5 font-code text-caption">
            {m.badge_primary()}
          </span>
        )}
        {v.autoGrow && (
          <span className="rounded-pill border border-line px-1.5 font-code text-caption text-muted">
            {m.badge_autogrow({ max: v.autoGrow.maxSize ?? m.not_reported() })}
          </span>
        )}
      </header>
      {v.archivingFailing && (
        <div className="flex flex-col items-start gap-1" data-archiving="failing">
          <StatusChip health="warning" label={m.forecast_archiving_failing()} size="sm" />
          <p className="m-0 font-semibold text-label">{v.archivingText}</p>
        </div>
      )}
      <Facts>
        <Fact label={m.forecast_usage_now()}>
          {m.volume_usage_now({
            used: formatBytes(v.usedBytes),
            total: formatBytes(v.totalBytes),
            available: formatBytes(v.availableBytes),
          })}
          {v.inodes !== undefined && v.inodesFree !== undefined && (
            <span className="text-muted">
              {" · "}
              {m.volume_inodes_now({ free: count(v.inodesFree), total: count(v.inodes) })}
            </span>
          )}
        </Fact>
        <Fact label={m.forecast_space()}>
          <Sentences lines={forecastSentences(bf, now, v.autoGrow)} />
        </Fact>
        {gf && (
          <Fact label={m.forecast_autogrow_max()}>
            <Sentences lines={forecastSentences(gf, now, v.autoGrow)} />
          </Fact>
        )}
        <Fact label={m.forecast_inodes()}>
          <Sentences lines={forecastSentences(v.inodesForecast, now, v.autoGrow)} />
        </Fact>
      </Facts>
      <Sparkline
        label={m.spark_used_label({ name, since: formatUtcMinute(v.since), asOf: formatUtcMinute(v.asOf) })}
        seriesLabel={m.spark_used_series()}
        points={(v.trend ?? []).map((p) => ({ at: p.at, value: p.bytes }))}
        rules={rules}
      />
      <p className="m-0 text-caption text-muted">
        {m.forecast_samples({ samples: count(v.samples), since: formatUtcMinute(v.since) })}
      </p>
      <Provenance provenance={v.provenance} />
    </article>
  );
}

/**
 * The volume forecasts panel. On stock CloudNativePG no instance reports its
 * volumes; Go's reason says so and nothing is guessed. A forecast is an
 * estimate with its window, rate and confidence stated, never a promise.
 */
export function VolumeForecastsPanel({ f }: { f: StorageForecasts }) {
  const now = useNow();
  const volumes = f.volumes ?? [];
  return (
    <Panel title={m.panel_volume_forecasts()}>
      {f.state !== "available" ? (
        <EmptyState title={f.state === "disabled" ? m.forecast_disabled_title() : m.forecast_unavailable_title()}>
          <p className="m-0" data-forecast-reason="">
            {f.reason ?? (f.state === "disabled" ? m.forecast_disabled_text() : m.not_reported())}
          </p>
        </EmptyState>
      ) : volumes.length === 0 ? (
        <EmptyState title={m.forecast_no_volumes()} />
      ) : (
        <div className="grid grid-cols-1 gap-3 xl:grid-cols-2">
          {volumes.map((v) => (
            <VolumeCard key={`${v.instance}/${v.volume}`} v={v} now={now} />
          ))}
        </div>
      )}
      <p className="m-0 text-caption text-muted">
        {m.forecast_footnote({
          interval: formatInterval(f.intervalSeconds),
          retention: formatInterval(f.retentionSeconds),
        })}{" "}
        {f.persistent ? m.forecast_persistent() : m.forecast_not_persistent()} {m.forecast_estimate_note()}
      </p>
    </Panel>
  );
}
