import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, userEvent } from "storybook/test";
import { compact, dark, darkCompact } from "../../../testing/variants";
import { fixtureLine, fixtureTimes, lagLines } from "./fixture";
import { MetricChart } from "./MetricChart";

/**
 * One measure over time. Use it where Go serves a series (instance trends,
 * the metrics screen); a single current value is a tile, not a chart. One
 * axis only: two measures of different units are two charts sharing a
 * syncKey. A gap is a reading not reported. Thresholds and their states come
 * from Go and are named in the legend.
 */
const meta = {
  title: "Patterns/MetricChart",
  component: MetricChart,
  args: {
    title: "Replication lag",
    times: fixtureTimes(),
    lines: lagLines,
    unit: "s",
    note: "Lag is shown, not judged: a replica can lag during a large transaction.",
    thresholds: [{ value: 10, health: "warning" }],
  },
} satisfies Meta<typeof MetricChart>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Lag: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("img", { name: "Replication lag" })).toBeVisible();
    await expect(canvas.getByText("threshold 10 s (warning)")).toBeVisible();
    await userEvent.click(canvas.getByText("Show the values as a table"));
    await expect(canvas.getAllByText("not reported").length).toBeGreaterThanOrEqual(3);
  },
};
export const LagDark = dark(Lag);
export const LagCompact = compact(Lag);
export const LagDarkCompact = darkCompact(Lag);

/** Every slot of the palette, in order: the eight-series cap. */
export const EightSeries: Story = {
  args: {
    title: "Connections by database",
    thresholds: [],
    unit: "count",
    lines: Array.from({ length: 8 }, (_, k) =>
      fixtureLine(`db-${k + 1}`, (i) => 10 + k * 6 + 4 * Math.sin((i + k * 5) / 6)),
    ),
  },
};
export const EightSeriesDark = dark(EightSeries);
export const Empty: Story = {
  args: { times: [], lines: [{ key: "billing-2", values: [] }] },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("No point reported in this window.")).toBeVisible();
  },
};
/** A rate of bytes: the legend, the threshold and the axis are sized ("MiB/s"), the table keeps the exact numbers. */
export const ByteRate: Story = {
  args: {
    title: "WAL written",
    unit: "bytes/s",
    note: undefined,
    thresholds: [{ value: 8_388_608, health: "warning" }],
    lines: [fixtureLine("billing-1", (i) => 1_234_567 + i * 20_000)],
  },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("last 2.3 MiB/s")).toBeVisible();
    await expect(canvas.getByText("threshold 8 MiB/s (warning)")).toBeVisible();
  },
};
export const ByteRateDark = dark(ByteRate);
/** The last good reading, said as such. */
export const Stale: Story = { args: { stale: true } };
