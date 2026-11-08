import { expect, test } from "vitest";
import { renderInFrame } from "../../../testing/render";
import { Sparkline, sparkGeometry } from "./Sparkline";

const at = (h: number) => new Date(Date.UTC(2026, 9, 29, h)).toISOString();

test("a gap ends the run: nothing is interpolated across it", () => {
  const g = sparkGeometry(
    [
      { at: at(0), value: 0 },
      { at: at(1), value: 10 },
      { at: at(2), value: null },
      { at: at(3), value: 10 },
      { at: at(4) },
      { at: at(5), value: 5 },
      { at: at(6), value: 10 },
    ],
    [],
    60,
    26,
  );
  expect(g.runs).toEqual(["0,23 10,3", "50,13 60,3"]);
  expect(g.dots).toEqual([{ x: 30, y: 3 }]);
});

test("the scale includes the rules, so a limit above every reading stays on the box", () => {
  const g = sparkGeometry(
    [
      { at: at(0), value: 10 },
      { at: at(1), value: 20 },
    ],
    [{ value: 40, label: "size" }],
    100,
    46,
  );
  expect(g.rules).toEqual([{ y: 3, label: "size" }]);
  expect(g.runs[0]).toBe("0,33 100,23");
});

test("a single reading is a dot at the right edge", () => {
  expect(sparkGeometry([{ at: at(0), value: 1 }], [], 100, 46).dots).toEqual([{ x: 100, y: 3 }]);
});

test("no reading says so instead of drawing an empty box", async () => {
  const screen = await renderInFrame(<Sparkline label="used" seriesLabel="used" points={[{ at: at(0) }]} />);
  await expect.element(screen.getByText("No point reported in this window.")).toBeVisible();
  expect(screen.container.querySelector("svg[role=img]")).toBeNull();
});

test("the line is named for screen readers and a gap is announced in the legend", async () => {
  const screen = await renderInFrame(
    <Sparkline
      label="Used space of billing-1 pgdata"
      seriesLabel="used"
      points={[{ at: at(0), value: 1 }, { at: at(1) }, { at: at(2), value: 2 }, { at: at(3), value: 3 }]}
      rules={[{ value: 4, label: "size now" }]}
    />,
  );
  await expect.element(screen.getByRole("img", { name: "Used space of billing-1 pgdata" })).toBeVisible();
  await expect.element(screen.getByText("size now")).toBeVisible();
  await expect.element(screen.getByText(/gap/)).toBeVisible();
});
