import { expect, test } from "vitest";
import { formatCount, formatDuration, formatMillis, formatPercent } from "./format";

test("durations keep their two largest units", () => {
  expect(formatDuration(4.5, "en")).toBe("4.5 s");
  expect(formatDuration(42, "en")).toBe("42 s");
  expect(formatDuration(2_280, "en")).toBe("38 min");
  expect(formatDuration(3_600, "en")).toBe("1 h");
  expect(formatDuration(6_666.67, "en")).toBe("1 h 51 min");
  expect(formatDuration(840_000, "en")).toBe("9 d 17 h");
  expect(formatDuration(86_400, "en")).toBe("1 d");
  // The sign is the caller's words, not the number's.
  expect(formatDuration(-90, "en")).toBe("1 min");
});

test("milliseconds stay milliseconds under a second", () => {
  expect(formatMillis(40.27, "en")).toBe("40.27 ms");
  expect(formatMillis(41_000, "en")).toBe("41 s");
  expect(formatMillis(205_000_000_000, "en")).toBe("2,372 d 16 h");
});

test("big counts are short; ratios are percentages", () => {
  expect(formatCount(340_000_000, "en")).toBe("340M");
  expect(formatCount(1_500, "en")).toBe("1.5K");
  expect(formatPercent(0.86, "en")).toBe("86%");
});
