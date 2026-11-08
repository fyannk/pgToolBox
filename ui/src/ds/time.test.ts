import { expect, test } from "vitest";
import {
  formatInterval,
  formatLogTime,
  formatRelative,
  formatSeconds,
  formatSpan,
  formatUtcMinute,
  isoToUtcInput,
  utcInputToIso,
} from "./time";

const now = Date.parse("2026-11-02T12:00:00Z");

test("a reading slightly ahead of the browser clock reads as now, not the future", () => {
  expect(formatRelative("2026-11-02T12:00:08Z", now, "en", true)).toBe("now");
  expect(formatRelative("2026-11-02T11:59:48Z", now, "en", true)).toBe("12 sec. ago");
});

test("a scheduled instant keeps its future", () => {
  expect(formatRelative("2026-11-02T13:00:00Z", now, "en")).toBe("in 1 hr.");
});

test("an instant to the minute is UTC whatever the browser's zone", () => {
  expect(formatUtcMinute("2026-10-30T18:00:00Z", "en")).toBe("Oct 30, 2026, 18:00 UTC");
});

test("a span is rounded to the unit a reader plans with", () => {
  expect(formatSpan(46 * 86_400_000 + 3_600_000, "en")).toBe("46 days");
  expect(formatSpan(30 * 3_600_000, "en")).toBe("30 hours");
  expect(formatSpan(20 * 60_000, "en")).toBe("20 minutes");
  expect(formatSpan(46 * 86_400_000, "fr")).toMatch(/^46\s+jours$/);
});

test("a configured interval keeps its own unit", () => {
  expect(formatInterval(900, "en")).toBe("15 minutes");
  expect(formatInterval(2_592_000, "en")).toBe("30 days");
  expect(formatInterval(7_200, "en")).toBe("2 hours");
});

test("the restore-target field is read as UTC, and an incomplete value is refused", () => {
  expect(utcInputToIso("2026-10-29T10:00")).toBe("2026-10-29T10:00:00Z");
  expect(utcInputToIso("2026-10-29T10:00:30")).toBe("2026-10-29T10:00:30Z");
  expect(utcInputToIso("2026-10-29")).toBeNull();
  expect(utcInputToIso("")).toBeNull();
  expect(isoToUtcInput("2026-10-29T11:59:15Z")).toBe("2026-10-29T11:59");
});

test("a duration drops zero units", () => {
  expect(formatSeconds(45)).toBe("45 s");
  expect(formatSeconds(840)).toBe("14 min");
  expect(formatSeconds(7200)).toBe("2 h");
  expect(formatSeconds(5400)).toBe("1 h 30 min");
});

test("a log time is UTC at a fixed millisecond width, whatever RFC 3339 dropped or offset", () => {
  expect(formatLogTime("2026-10-29T11:50:00.12Z")).toBe("11:50:00.120");
  expect(formatLogTime("2026-10-29T11:50:00Z")).toBe("11:50:00.000");
  expect(formatLogTime("2026-10-29T11:50:00.123456789Z")).toBe("11:50:00.123");
  expect(formatLogTime("2026-10-29T13:50:00.5+02:00")).toBe("11:50:00.500");
  expect(formatLogTime("not a time")).toBe("not a time");
});
