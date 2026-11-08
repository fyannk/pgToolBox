// Time is shown relative ("3 min ago") with the absolute UTC instant on hover.
// The clock is injected so stories, tests and screenshots are deterministic.
import { createContext, type ReactNode, useContext, useEffect, useState } from "react";
import { getLocale } from "../paraglide/runtime.js";

const NowContext = createContext<number | null>(null);

/** Pins "now" for a subtree (stories, tests); without it the clock ticks. */
export function NowProvider({ now, children }: { now: number | Date; children: ReactNode }) {
  return <NowContext.Provider value={typeof now === "number" ? now : now.getTime()}>{children}</NowContext.Provider>;
}

export function useNow(tickMs = 15_000): number {
  const pinned = useContext(NowContext);
  const [now, setNow] = useState(() => pinned ?? Date.now());
  useEffect(() => {
    if (pinned !== null) return;
    const id = setInterval(() => setNow(Date.now()), tickMs);
    return () => clearInterval(id);
  }, [pinned, tickMs]);
  // Read the clock on every render too: a refetch re-renders with a fresher
  // timestamp than the last tick, which would otherwise read as the future.
  return pinned ?? Math.max(now, Date.now());
}

const UNITS: Array<[Intl.RelativeTimeFormatUnit, number]> = [
  ["day", 86_400],
  ["hour", 3_600],
  ["minute", 60],
  ["second", 1],
];

/**
 * past: the instant can only be behind us (a reading, an event); a small lead
 * from clock skew between the cluster and the browser shows as "now".
 */
export function formatRelative(iso: string, now: number, locale = getLocale(), past = false): string {
  let seconds = Math.round((Date.parse(iso) - now) / 1000);
  if (past && seconds > 0) seconds = 0;
  const fmt = new Intl.RelativeTimeFormat(locale, { numeric: "auto", style: "short" });
  for (const [unit, size] of UNITS) {
    if (Math.abs(seconds) >= size || unit === "second") return fmt.format(Math.trunc(seconds / size), unit);
  }
  return fmt.format(0, "second");
}

export function formatUtc(iso: string, locale = getLocale()): string {
  return `${new Intl.DateTimeFormat(locale, {
    dateStyle: "medium",
    timeStyle: "medium",
    timeZone: "UTC",
  }).format(new Date(iso))} UTC`;
}

/**
 * A log line's time of day in UTC at a fixed width, "11:50:00.120": the
 * container's RFC 3339 (Nano) drops trailing zeros and may carry an offset,
 * so its characters cannot be sliced. Text that is not an instant is shown
 * as written.
 */
export function formatLogTime(iso: string): string {
  const ms = Date.parse(iso);
  return Number.isNaN(ms) ? iso : new Date(ms).toISOString().slice(11, 23);
}

/** formatUtc, or the text as written when it is not an instant. */
export function formatUtcOrRaw(iso: string, locale = getLocale()): string {
  return Number.isNaN(Date.parse(iso)) ? iso : formatUtc(iso, locale);
}

/**
 * A timestamp: relative in text, absolute UTC in the title and machine-readable
 * attribute. past: see formatRelative.
 */
export function Time({ iso, className, past = false }: { iso: string; className?: string; past?: boolean }) {
  const now = useNow(past ? 5_000 : undefined);
  return (
    <time dateTime={iso} title={formatUtc(iso)} className={className}>
      {formatRelative(iso, now, undefined, past)}
    </time>
  );
}

/** An instant to the minute, in UTC: "30 Oct 2026, 18:00 UTC". For dates a sentence names. */
export function formatUtcMinute(iso: string, locale = getLocale()): string {
  return `${new Intl.DateTimeFormat(locale, {
    dateStyle: "medium",
    timeStyle: "short",
    timeZone: "UTC",
    hourCycle: "h23",
  }).format(new Date(iso))} UTC`;
}

/**
 * A span as words, rounded to the unit a reader plans with: days from two
 * days up, hours from one hour, else minutes ("46 days", "18 hours").
 */
export function formatSpan(ms: number, locale = getLocale()): string {
  const abs = Math.abs(ms);
  const [unit, size] =
    abs >= 2 * 86_400_000
      ? (["day", 86_400_000] as const)
      : abs >= 3_600_000
        ? (["hour", 3_600_000] as const)
        : (["minute", 60_000] as const);
  return new Intl.NumberFormat(locale, { style: "unit", unit, unitDisplay: "long" }).format(Math.round(abs / size));
}

/** A whole number of seconds as words ("15 minutes", "30 days"), for configured intervals. */
export function formatInterval(seconds: number, locale = getLocale()): string {
  const [unit, size] =
    seconds % 86_400 === 0
      ? (["day", 86_400] as const)
      : seconds % 3_600 === 0
        ? (["hour", 3_600] as const)
        : seconds % 60 === 0
          ? (["minute", 60] as const)
          : (["second", 1] as const);
  return new Intl.NumberFormat(locale, { style: "unit", unit, unitDisplay: "long" }).format(seconds / size);
}

/**
 * A datetime-local value read as UTC ("2026-10-29T10:00" → "2026-10-29T10:00:00Z");
 * null when the value is not a complete date and time.
 */
export function utcInputToIso(value: string): string | null {
  const match = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2})(:\d{2})?(\.\d+)?$/.exec(value);
  if (!match) return null;
  const iso = `${match[1]}${match[2] ?? ":00"}Z`;
  return Number.isNaN(Date.parse(iso)) ? null : iso;
}

/** An instant as a datetime-local value in UTC, to the minute. */
export function isoToUtcInput(iso: string): string {
  return new Date(iso).toISOString().slice(0, 16);
}

/** A duration in whole minutes or hours and minutes ("14 min", "2 h", "1 h 30 min"); under a minute, seconds. */
export function formatSeconds(seconds: number): string {
  if (seconds < 60) return `${Math.round(seconds)} s`;
  const minutes = Math.round(seconds / 60);
  const h = Math.floor(minutes / 60);
  const rest = minutes % 60;
  if (h === 0) return `${rest} min`;
  return rest === 0 ? `${h} h` : `${h} h ${rest} min`;
}
