import { expect, test } from "vitest";
import { storageForecasts } from "../../mocks/fixtures";
import { NOW_MS } from "../../mocks/fixtures/extra";
import type { VolumeForecast } from "../api/generated/model";
import { forecastSentences } from "./forecast";

const volume = (instance: string, name: string): VolumeForecast => {
  const v = (storageForecasts.forecasts.volumes ?? []).find((x) => x.instance === instance && x.volume === name);
  if (!v) throw new Error(`no ${instance}/${name} in getStorage.billing.json`);
  return v;
};

test("a forecast states the date, its range, the rate, the window and the confidence", () => {
  const wal = volume("billing-1", "wal");
  const [lead, accel, ...rest] = forecastSentences(wal.bytesForecast, NOW_MS, wal.autoGrow);
  expect(lead).toBe(
    "Full in about 30 hours (around Oct 30, 2026, 18:00 UTC, between Oct 30, 2026, 10:00 UTC and Oct 31, 2026, 08:00 UTC), growing 1.6 GiB/day over the last 96 h, medium confidence.",
  );
  expect(accel).toBe(
    "Growing faster than linear: 409.6 MiB/day in the first half of the window, 1.6 GiB/day recently; the date uses the recent rate.",
  );
  expect(rest).toEqual([]);
});

test("with autoGrow the filesystem and maxSize are two dates", () => {
  const data = volume("billing-1", "pgdata");
  const fs = forecastSentences(data.bytesForecast, NOW_MS, data.autoGrow);
  expect(fs[0]).toMatch(/^Full in about 6 days/);
  expect(fs.at(-1)).toMatch(/^This is the current filesystem: autoGrow should grow the PVC before then/);
  if (!data.autoGrowForecast) throw new Error("no autoGrow forecast in getStorage.billing.json");
  const s = forecastSentences(data.autoGrowForecast, NOW_MS, data.autoGrow);
  expect(s[0]).toMatch(/^Reaches autoGrow\.maxSize \(100Gi\) in about 46 days \(around Dec 14, 2026, 12:00 UTC,/);
  expect(s[0]).toMatch(/growing 1\.5 GiB\/day over the last 168 h, high confidence\.$/);
  expect(s.at(-1)).toMatch(/autoGrow will grow the PVC in steps before then/);
});

test("the states without a date say why", () => {
  expect(forecastSentences(volume("billing-2", "pgdata").bytesForecast, NOW_MS)).toEqual([
    "Not growing: less than 20.5 MiB over the last 168 h.",
    "The volume was resized at Oct 27, 2026, 00:00 UTC: the fit uses only the readings since then, as a resize shifts the space counted as used by the reserved blocks it adds.",
  ]);
  const xfs = { ...volume("billing-1", "pgdata").inodesForecast, state: "totalChanged" as const };
  expect(forecastSentences(xfs, NOW_MS)).toEqual([
    "No inode forecast: the inode total changed over the window (XFS allocates inodes on demand), so there is no fixed limit to run toward.",
  ]);
  expect(forecastSentences(volume("billing-3", "pgdata").bytesForecast, NOW_MS)).toEqual([
    "Too few points to forecast: 9 of 24 hourly points.",
  ]);
  expect(forecastSentences(volume("billing-3", "pgdata").inodesForecast, NOW_MS)).toEqual([
    "Inode counts are not reported by this filesystem.",
  ]);
  const full = { ...volume("billing-1", "wal").bytesForecast, state: "full" as const };
  expect(forecastSentences(full, NOW_MS)).toEqual(["Full now: the filesystem has no space left."]);
  // autoGrow set does not soften a full filesystem.
  const fullGrow = { ...volume("billing-1", "pgdata").bytesForecast, state: "full" as const };
  expect(forecastSentences(fullGrow, NOW_MS)).toEqual([
    "Full now: the filesystem has no space left.",
    "autoGrow is set but did not grow this filesystem in time: check the VolumeExpansionRefused and VolumeAutoGrowStopped conditions.",
  ]);
});

test("a date already behind the clock is not worded as a future", () => {
  const wal = volume("billing-1", "wal").bytesForecast;
  const [lead] = forecastSentences(wal, Date.parse("2026-11-01T00:00:00Z"));
  expect(lead).toMatch(/^At this rate the limit would already be reached/);
});
