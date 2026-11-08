import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import { compact, dark, darkCompact } from "../../../testing/variants";
import { Sparkline, type SparkPoint } from "./Sparkline";

const GIB = 1024 ** 3;
const at = (h: number) => new Date(Date.UTC(2026, 9, 22, h)).toISOString();
const growing: SparkPoint[] = Array.from({ length: 42 }, (_, i) => ({ at: at(i * 4), value: (24 + i * 0.25) * GIB }));
const withGaps: SparkPoint[] = growing.map((p, i) => (i >= 12 && i < 16 ? { at: p.at } : i === 30 ? { at: p.at } : p));

/**
 * A trend read at a glance next to a sentence that states the figures (a
 * volume's used space beside its forecast). Use a MetricChart to explore a
 * series; use this where the line only has to show the shape and the line it
 * aims at. A gap is a break, never an interpolation; the legend says when
 * the line has one. The figures are always stated in text by the caller.
 */
const meta = {
  title: "Patterns/Sparkline",
  component: Sparkline,
  args: {
    label: "Used space of billing-1 pgdata over 7 days",
    seriesLabel: "used space",
    points: growing,
    rules: [{ value: 40 * GIB, label: "filesystem size now (40 GiB)" }],
  },
} satisfies Meta<typeof Sparkline>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Growing: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("img", { name: /Used space of billing-1/ })).toBeVisible();
  },
};
export const GrowingDark = dark(Growing);
export const GrowingCompact = compact(Growing);
export const GrowingDarkCompact = darkCompact(Growing);
/** Gaps (nothing recorded) stay gaps; an isolated reading is a dot. */
export const WithGaps: Story = {
  args: { points: withGaps },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/gaps/)).toBeVisible();
  },
};
export const WithGapsDark = dark(WithGaps);
/** Nothing reported in the window: said in words, no empty box. */
export const Empty: Story = {
  args: { points: [{ at: at(0) }, { at: at(4) }] },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("No point reported in this window.")).toBeVisible();
  },
};
