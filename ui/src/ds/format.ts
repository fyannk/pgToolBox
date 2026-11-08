// Display formatting only: numbers and sizes the Go side reports raw.
import { m } from "../paraglide/messages.js";
import { getLocale } from "../paraglide/runtime.js";

const UNITS = ["B", "KiB", "MiB", "GiB", "TiB"];

export function formatBytes(n: number, locale = getLocale()): string {
  let v = n;
  let i = 0;
  while (Math.abs(v) >= 1024 && i < UNITS.length - 1) {
    v /= 1024;
    i++;
  }
  return `${new Intl.NumberFormat(locale, { maximumFractionDigits: i === 0 ? 0 : 1 }).format(v)} ${UNITS[i]}`;
}

/**
 * Sort order of health states for a sortable column: AllHealth of
 * internal/console/viewmodel/vocabulary.go, best first. Ordering for display,
 * not a judgement; the states themselves come from Go.
 */
const HEALTH_ORDER = ["info", "ok", "inactive", "progressing", "unknown", "warning", "critical"];
export function healthRank(h: string): number {
  const i = HEALTH_ORDER.indexOf(h);
  return i < 0 ? HEALTH_ORDER.indexOf("unknown") : i;
}

/** A count as read ("1,500"); fractions kept to two digits. */
export function formatNumber(n: number, locale = getLocale()): string {
  return new Intl.NumberFormat(locale, { maximumFractionDigits: 2 }).format(n);
}

/** A large count, short ("340M"); pair it with formatNumber in a title for the exact value. */
export function formatCount(n: number, locale = getLocale()): string {
  return new Intl.NumberFormat(locale, { notation: "compact", maximumFractionDigits: 1 }).format(n);
}

/** A ratio in [0, 1] as a percentage ("86%"). */
export function formatPercent(ratio: number, locale = getLocale()): string {
  return new Intl.NumberFormat(locale, { style: "percent", maximumFractionDigits: 1 }).format(ratio);
}

/**
 * A duration in seconds, in its two largest units ("9 d 17 h", "38 min",
 * "4.5 s"). A negative value is shown as its magnitude: the caller's words say
 * whether it is behind or ahead.
 */
export function formatDuration(seconds: number, locale = getLocale()): string {
  const s = Math.abs(seconds);
  const n = (v: number, digits = 0) => new Intl.NumberFormat(locale, { maximumFractionDigits: digits }).format(v);
  if (s < 60) return m.duration_seconds({ n: n(s, s < 10 ? 1 : 0) });
  const total = Math.round(s);
  const d = Math.floor(total / 86_400);
  const h = Math.floor((total % 86_400) / 3_600);
  const min = Math.floor((total % 3_600) / 60);
  const days = m.duration_days({ n: n(d) });
  const hours = m.duration_hours({ n: n(h) });
  const minutes = m.duration_minutes({ n: n(min) });
  if (d > 0) return h > 0 ? `${days} ${hours}` : days;
  if (h > 0) return min > 0 ? `${hours} ${minutes}` : hours;
  return minutes;
}

/** A time in milliseconds: "40.27 ms" under a second, a duration from there on. */
export function formatMillis(ms: number, locale = getLocale()): string {
  if (Math.abs(ms) < 1000) return m.duration_millis({ n: formatNumber(ms, locale) });
  return formatDuration(ms / 1000, locale);
}

/**
 * A reading in the unit Go served it in, for reading; the one formatter of
 * MetricTile and MetricChart. Sizes are sized ("1.2 MiB", "1.2 MiB/s"),
 * seconds are durations ("1 d", "250 ms"), ratios percentages; any other
 * unit (Go's own word: "connections", "tx/s") follows the number as served.
 * The value is never changed, only written.
 */
export function formatUnit(value: number, unit?: string, locale = getLocale()): string {
  const sign = value < 0 ? "-" : "";
  switch (unit) {
    case "bytes":
      return formatBytes(value, locale);
    case "bytes/s":
      return m.unit_per_second({ value: formatBytes(value, locale) });
    case "s":
    case "seconds":
      return value !== 0 && Math.abs(value) < 1
        ? `${sign}${formatMillis(Math.abs(value) * 1000, locale)}`
        : `${sign}${formatDuration(value, locale)}`;
    case "ms":
      return `${sign}${formatMillis(Math.abs(value), locale)}`;
    case "ratio":
      return formatPercent(value, locale);
    case undefined:
    case "":
    case "count":
      return formatNumber(value, locale);
    default:
      return `${formatNumber(value, locale)} ${unit}`;
  }
}
