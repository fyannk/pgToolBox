// Sentences for the volume forecasts Go computes (status/forecasts.go). The
// numbers, the state, the dates and the confidence are Go's; this only words
// them, with the window and the rate every estimate must carry.
import type { AutoGrow, VolumeFillForecast } from "../api/generated/model";
import { formatBytes } from "../ds/format";
import { formatSpan, formatUtcMinute } from "../ds/time";
import { m } from "../paraglide/messages.js";
import { getLocale } from "../paraglide/runtime.js";

/** A count or a size of the forecast's measure. */
function amount(f: Pick<VolumeFillForecast, "measure">, n: number, locale = getLocale()): string {
  return f.measure === "bytes"
    ? formatBytes(n, locale)
    : m.inodes_count({ n: new Intl.NumberFormat(locale, { maximumFractionDigits: 0 }).format(n) });
}

const rate = (f: VolumeFillForecast, perDay: number) => m.rate_per_day({ rate: amount(f, perDay) });

function confidenceWords(c: VolumeFillForecast["confidence"]): string {
  switch (c) {
    case "high":
      return m.confidence_high();
    case "medium":
      return m.confidence_medium();
    case "low":
      return m.confidence_low();
    default:
      return m.confidence_not_reported();
  }
}

/** The line the forecast aims at: autoGrow.maxSize as declared, or the filesystem. */
function targetWords(f: VolumeFillForecast, autoGrow?: AutoGrow): string {
  if (f.target === "autoGrowMaxSize")
    return m.forecast_target_autogrow({ max: autoGrow?.maxSize ?? amount(f, f.capacity) });
  return f.measure === "bytes" ? m.forecast_target_filesystem() : m.forecast_target_inodes();
}

/**
 * The forecast as sentences, the first one the answer: when the volume
 * fills (or why there is no date), then whether growth is accelerating and
 * what autoGrow will do before the date. now only words the date as a span.
 */
export function forecastSentences(f: VolumeFillForecast, now: number, autoGrow?: AutoGrow): string[] {
  const out = stateSentences(f, now, autoGrow);
  if (f.resizedAt && (f.state === "forecast" || f.state === "notGrowing")) {
    const at = formatUtcMinute(f.resizedAt);
    out.push(f.fitSinceResize ? m.forecast_resized_since({ at }) : m.forecast_resized_levelled({ at }));
  }
  return out;
}

function stateSentences(f: VolumeFillForecast, now: number, autoGrow?: AutoGrow): string[] {
  const target = targetWords(f, autoGrow);
  switch (f.state) {
    case "forecast": {
      if (!f.fullAt) return [m.forecast_no_date({ target })];
      const at = formatUtcMinute(f.fullAt);
      const when =
        f.earliestAt && f.latestAt
          ? m.forecast_when_range({ at, earliest: formatUtcMinute(f.earliestAt), latest: formatUtcMinute(f.latestAt) })
          : m.forecast_when({ at });
      const how = {
        rate: rate(f, f.ratePerDay),
        hours: String(f.windowHours),
        confidence: confidenceWords(f.confidence),
      };
      const lead = Date.parse(f.fullAt) - now;
      const out = [
        lead > 0
          ? m.forecast_full_in({ target, in: formatSpan(lead), when, ...how })
          : m.forecast_full_past({ when, ...how }),
      ];
      if (f.accelerating)
        out.push(
          m.forecast_accelerating({ first: rate(f, f.firstHalfRatePerDay), recent: rate(f, f.recentRatePerDay) }),
        );
      if (f.target === "autoGrowMaxSize" && f.autoGrowSteps) out.push(m.forecast_autogrow_steps());
      if (f.target === "filesystem" && f.measure === "bytes" && f.autoGrowSteps)
        out.push(m.forecast_filesystem_autogrow());
      return out;
    }
    case "notGrowing":
      return [m.forecast_not_growing({ min: amount(f, f.minimumGrowth), hours: String(f.windowHours) })];
    case "tooFewPoints":
      return [m.forecast_too_few({ found: String(f.hoursFound), minimum: String(f.minimumHours) })];
    case "full":
      if (f.target === "autoGrowMaxSize")
        return [m.forecast_full_now_autogrow({ max: autoGrow?.maxSize ?? amount(f, f.capacity) })];
      if (f.measure === "bytes" && f.autoGrowSteps)
        return [m.forecast_full_now_bytes(), m.forecast_full_now_autogrow_set()];
      return [f.measure === "bytes" ? m.forecast_full_now_bytes() : m.forecast_full_now_inodes()];
    case "totalChanged":
      return [m.forecast_inodes_total_changed()];
    default:
      return [f.measure === "bytes" ? m.forecast_no_data_bytes() : m.forecast_no_data_inodes()];
  }
}
