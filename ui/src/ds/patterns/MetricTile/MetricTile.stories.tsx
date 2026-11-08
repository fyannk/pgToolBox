import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import { compact, dark, darkCompact } from "../../../testing/variants";
import { MetricTile } from "./MetricTile";

const provenance = { origin: "instance" as const, source: "/metrics", observedAt: "2026-10-29T11:59:40Z" };

/**
 * One current reading. Use it for "now" values the console keeps between
 * scrapes; a trend is a MetricChart. A missing value is "not reported", never
 * zero; the provenance says who reported it and when.
 */
const meta = {
  title: "Patterns/MetricTile",
  component: MetricTile,
  args: { label: "Database size", value: 52_428_800_000, unit: "bytes", instance: "billing-1", provenance },
} satisfies Meta<typeof MetricTile>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Bytes: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByText("48.8 GiB")).toBeVisible();
  },
};
export const BytesDark = dark(Bytes);
export const BytesCompact = compact(Bytes);
export const BytesDarkCompact = darkCompact(Bytes);
export const Ratio: Story = { args: { label: "Cache hit ratio", value: 0.9934, unit: "ratio" } };
export const Seconds: Story = { args: { label: "Replication lag", value: 0.42, unit: "seconds" } };
/** A rate of bytes is sized like a size, per second. */
export const ByteRate: Story = {
  args: { label: "WAL written", value: 1_234_567, unit: "bytes/s" },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("1.2 MiB/s")).toBeVisible();
  },
};
/** Seconds read as a duration, not "86,400 s". */
export const Uptime: Story = {
  args: { label: "Postmaster uptime", value: 86_400, unit: "s" },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("1 d")).toBeVisible();
  },
};
export const NotReported: Story = {
  args: { label: "Replication lag", value: undefined, unit: "seconds", instance: "billing-2" },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("not reported")).toBeVisible();
  },
};
export const Stale: Story = {
  args: { provenance: { ...provenance, stale: true, staleReason: "scrape failed: connection refused" } },
};
