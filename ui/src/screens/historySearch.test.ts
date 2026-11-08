import { describe, expect, test } from "vitest";
import {
  activePreset,
  localInputToUtc,
  presetAnchor,
  REANCHOR_MS,
  timelineParams,
  utcToLocalInput,
  validateHistorySearch,
} from "./historySearch";

const NOW = Date.parse("2026-10-29T12:00:00Z");

describe("history search", () => {
  test("a hand-edited URL keeps only what the API could be sent", () => {
    expect(
      validateHistorySearch({
        view: "x",
        range: "3y",
        from: "yesterday",
        to: "2026-10-29T11:00:00Z",
        kind: "finding,bogus,primary",
      }),
    ).toEqual({ to: "2026-10-29T11:00:00Z", kind: "primary,finding" });
    expect(validateHistorySearch({ view: "revisions", range: "6h", kind: "bogus" })).toEqual({
      view: "revisions",
      range: "6h",
    });
  });

  test("no window is the API's default (24 h, ending now); a preset is anchored, the end left to the API", () => {
    expect(timelineParams({}, NOW)).toEqual({});
    expect(activePreset({})).toBe("24h");
    expect(timelineParams({ range: "6h" }, NOW)).toEqual({ from: "2026-10-29T06:00:00.000Z" });
    expect(timelineParams({ range: "7d", kind: "finding" }, NOW)).toEqual({
      from: "2026-10-22T12:00:00.000Z",
      kind: ["finding"],
    });
  });

  test("a fixed window wins over a preset and is sent as given", () => {
    const s = { range: "1h" as const, from: "2026-10-29T10:00:00Z", to: "2026-10-29T11:00:00Z" };
    expect(activePreset(s)).toBeNull();
    expect(timelineParams(s, NOW)).toEqual({ from: s.from, to: s.to });
  });

  test("the custom inputs are UTC both ways", () => {
    expect(localInputToUtc("2026-10-29T11:00")).toBe("2026-10-29T11:00:00Z");
    expect(localInputToUtc("")).toBeUndefined();
    expect(utcToLocalInput("2026-10-29T11:00:00Z")).toBe("2026-10-29T11:00");
  });

  test("a relative preset moves on with the clock: left open three hours, the last hour is still the last hour", () => {
    const chosen = NOW;
    expect(presetAnchor(chosen, NOW)).toBe(NOW);
    // Within a step the anchor (and so the query) does not change.
    expect(presetAnchor(chosen, NOW + 30_000)).toBe(NOW);
    const later = NOW + 3 * 3_600_000 + 45_000;
    const from = Date.parse(timelineParams({ range: "1h" }, presetAnchor(chosen, later)).from ?? "");
    expect(later - from).toBeGreaterThanOrEqual(3_600_000);
    expect(later - from).toBeLessThan(3_600_000 + REANCHOR_MS);
    // Chosen again mid-step: anchored at that instant, not the step before it.
    expect(presetAnchor(later, later)).toBe(later);
  });
});
