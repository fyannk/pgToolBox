import { expect, test } from "vitest";
import { page } from "vitest/browser";
import { renderInFrame } from "../../../testing/render";
import { fixtureLine, fixtureTimes } from "./fixture";
import { alignSeries, MetricChart } from "./MetricChart";

test("a null point is a gap said as not reported, never a zero", async () => {
  await renderInFrame(
    <MetricChart title="Lag" times={fixtureTimes(4)} lines={[fixtureLine("billing-2", () => 1, 4, [3])]} unit="s" />,
  );
  await expect.element(page.getByText("last not reported")).toBeVisible();
  await page.getByText("Show the values as a table").click();
  const cells = [...page.getByRole("cell").elements()].map((c) => c.textContent);
  expect(cells).toEqual(["1", "1", "1", "not reported"]);
});

test("draws a canvas and names every line with its last value", async () => {
  const lines = [{ ...fixtureLine("a", () => 2), label: "billing-2" }, fixtureLine("b", () => 3)];
  await renderInFrame(<MetricChart title="Lag" times={fixtureTimes()} lines={lines} unit="s" />);
  await expect.element(page.getByRole("img", { name: "Lag" })).toBeVisible();
  expect(page.getByRole("img", { name: "Lag" }).element().querySelector("canvas")).not.toBeNull();
  await expect.element(page.getByText("billing-2")).toBeVisible();
  await expect.element(page.getByText("b", { exact: true })).toBeVisible();
  await expect.element(page.getByText("last 2 s")).toBeVisible();
});

test("more than eight lines are never drawn in a cycled colour", async () => {
  const lines = Array.from({ length: 9 }, (_, i) => fixtureLine(`s${i}`, () => i));
  await renderInFrame(<MetricChart title="Many" times={fixtureTimes()} lines={lines} />);
  expect(page.getByRole("listitem").elements()).toHaveLength(8);
});

test("point series align on shared times with gaps, no value invented", () => {
  const out = alignSeries([
    {
      key: "a",
      points: [
        { at: "2026-01-01T00:00:00Z", value: 1 },
        { at: "2026-01-01T00:01:00Z", value: 2 },
      ],
    },
    { key: "b", points: [{ at: "2026-01-01T00:01:00Z", value: null }] },
  ]);
  expect(out.times).toEqual(["2026-01-01T00:00:00Z", "2026-01-01T00:01:00Z"]);
  expect(out.lines).toEqual([
    { key: "a", values: [1, 2] },
    { key: "b", values: [null, null] },
  ]);
});

test("bytes per second and seconds are written in their unit, the table keeps the exact number", async () => {
  await renderInFrame(
    <MetricChart
      title="WAL written"
      times={fixtureTimes(2)}
      lines={[fixtureLine("billing-1", () => 1_234_567, 2)]}
      unit="bytes/s"
      thresholds={[{ value: 8_388_608, health: "warning" }]}
    />,
  );
  await expect.element(page.getByText("last 1.2 MiB/s")).toBeVisible();
  await expect.element(page.getByText("threshold 8 MiB/s (warning)")).toBeVisible();
  await page.getByText("Show the values as a table").click();
  const cells = [...page.getByRole("cell").elements()].map((c) => c.textContent);
  expect(cells).toEqual(["1,234,567", "1,234,567"]);
});

test("seconds read as a duration", async () => {
  await renderInFrame(
    <MetricChart title="Uptime" times={fixtureTimes(2)} lines={[fixtureLine("billing-1", () => 86_400, 2)]} unit="s" />,
  );
  await expect.element(page.getByText("last 1 d")).toBeVisible();
});
