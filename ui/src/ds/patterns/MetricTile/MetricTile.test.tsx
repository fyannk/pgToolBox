import { expect, test } from "vitest";
import { formatUnit } from "../../format";

test("units are formatted for reading, the number never changed", () => {
  expect(formatUnit(1536, "bytes", "en")).toBe("1.5 KiB");
  expect(formatUnit(1_234_567, "bytes/s", "en")).toBe("1.2 MiB/s");
  expect(formatUnit(0.5, "ratio", "en")).toBe("50%");
  expect(formatUnit(12, "count", "en")).toBe("12");
  expect(formatUnit(1_500, undefined, "en")).toBe("1,500");
  expect(formatUnit(86_400, "s", "en")).toBe("1 d");
  expect(formatUnit(5_400, "seconds", "en")).toBe("1 h 30 min");
  expect(formatUnit(0.25, "seconds", "en")).toBe("250 ms");
  expect(formatUnit(0, "s", "en")).toBe("0 s");
  expect(formatUnit(-90, "s", "en")).toBe("-1 min");
  expect(formatUnit(40.5, "ms", "en")).toBe("40.5 ms");
  // Go's own unit words follow the number as served.
  expect(formatUnit(1_234.5, "tx/s", "en")).toBe("1,234.5 tx/s");
  expect(formatUnit(3, "connections", "en")).toBe("3 connections");
});
