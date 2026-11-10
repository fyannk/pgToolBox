import { expect, test } from "vitest";
import { backups } from "../../mocks/fixtures";
import type { RecoveryTimeline } from "../api/generated/model";
import { MIN_SPAN_WIDTH, recoveryBarLayout } from "./recoveryBar";

const base: RecoveryTimeline = {
  basis: "evidence",
  state: "healthy",
  complete: true,
  assumptions: [],
  markers: [],
  segments: [],
  provenance: { origin: "evidence" },
};

test("the window fills the box between a 3 % margin on each side", () => {
  const layout = recoveryBarLayout(
    {
      ...base,
      earliest: "2026-10-01T00:00:00Z",
      latest: "2026-10-11T00:00:00Z",
      segments: [{ from: "2026-10-01T00:00:00Z", to: "2026-10-11T00:00:00Z", kind: "covered" }],
    },
    1060,
  );
  expect(layout?.spans).toEqual([expect.objectContaining({ x: 30, w: 1000, kind: "covered" })]);
});

test("spans and markers keep their order and their kinds; a hole stays where it is", () => {
  const layout = recoveryBarLayout(backups.recoveryTimeline);
  if (!layout) throw new Error("no layout");
  // The golden's newest edge: unknown near the end, then where the archived WAL ends.
  expect(layout.spans.map((s) => s.kind)).toEqual(["covered", "missingWAL", "covered", "unknown", "walEnd"]);
  const [a, hole, b, near, end] = layout.spans;
  if (!a || !hole || !b || !near || !end) throw new Error("five spans expected");
  expect(hole.x).toBeCloseTo(a.x + a.w, 1);
  expect(b.x).toBeCloseTo(hole.x + hole.w, 1);
  expect(near.x).toBeGreaterThanOrEqual(b.x);
  expect(end.x).toBeGreaterThanOrEqual(near.x);
  expect(layout.markers).toHaveLength(8);
  expect(layout.markers.find((mk) => mk.kind === "failed")?.x).toBeGreaterThan(b.x);
});

test("a hole too narrow to see is widened, never dropped", () => {
  const layout = recoveryBarLayout({
    ...base,
    segments: [
      { from: "2026-10-01T00:00:00Z", to: "2026-10-30T00:00:00Z", kind: "covered" },
      { from: "2026-10-30T00:00:00Z", to: "2026-10-30T00:00:01Z", kind: "candidateGap" },
    ],
  });
  expect(layout?.spans[1]?.w).toBe(MIN_SPAN_WIDTH);
});

test("a timeline that names no instant has no bar", () => {
  expect(recoveryBarLayout({ ...base, basis: "none", state: "unknown" })).toBeNull();
});

test("a single instant is padded by an hour on each side", () => {
  const layout = recoveryBarLayout(
    { ...base, markers: [{ at: "2026-10-01T00:00:00Z", kind: "failed", source: "backupObject" }] },
    100,
  );
  expect(layout?.markers[0]?.x).toBe(50);
  expect(layout?.end).toBeGreaterThan(layout?.start ?? 0);
});
