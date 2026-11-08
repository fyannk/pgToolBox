import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import { insightEnabled, insightNotEnabled } from "../../../mocks/fixtures";
import {
  insightOutdated,
  insightPackTruncated,
  insightPackUnknown,
  insightPackUnsupported,
  insightPanelsUnavailable,
  insightPartial,
} from "../../../mocks/fixtures/insight";
import { handlersFor } from "../../../mocks/handlers";
import { compact, dark, darkCompact } from "../../testing/variants";
import { InsightPackScreen, InsightPackView } from "./Pack";

/**
 * The Insight landing: whether the Cluster runs the insight pack (CloudNativePG
 * custom queries), the version each instance reports, the pack queries that
 * failed, and which insight screens have data. When the pack is not enabled it
 * gives the one-line spec change, copyable; the console never applies it.
 */
const meta = {
  title: "Screens/Insight/Pack",
  component: InsightPackView,
  args: { data: insightEnabled },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof InsightPackView>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Enabled: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByText("The insight pack version 1 is enabled.")).toBeVisible();
    await expect(canvas.getByRole("heading", { name: "Pack version per instance" })).toBeVisible();
  },
};
export const EnabledDark = dark(Enabled);
export const EnabledCompact = compact(Enabled);
export const EnabledDarkCompact = darkCompact(Enabled);

export const NotEnabled: Story = {
  args: { data: insightNotEnabled },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(insightNotEnabled.pack.specChange)).toBeVisible();
    await expect(canvas.getByRole("button", { name: "Copy the Cluster spec change" })).toBeVisible();
    await expect(canvas.getAllByText("no pack reported").length).toBe(2);
  },
};
export const NotEnabledDark = dark(NotEnabled);
export const Outdated: Story = { args: { data: insightOutdated } };
export const PanelsUnavailable: Story = {
  args: { data: insightPanelsUnavailable },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/pg_stat_statements is not loaded/)).toBeVisible();
  },
};

/** The console cut the exporters' responses before the pack's series: "response cut", never "not enabled", and no spec change offered. */
export const Truncated: Story = {
  args: { data: insightPackTruncated },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("Response cut")).toBeVisible();
    await expect(canvas.queryByText("Not enabled")).toBeNull();
    await expect(canvas.queryByRole("button", { name: "Copy the Cluster spec change" })).toBeNull();
    await expect(canvas.getAllByText("the exporter response, at the size bound").length).toBe(2);
  },
};
export const TruncatedDark = dark(Truncated);
/** Pack series without their version. */
export const VersionUnknown: Story = {
  args: { data: insightPackUnknown },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("Version unknown")).toBeVisible();
    await expect(canvas.getByText("no pack reported")).toBeVisible();
  },
};
/** PostgreSQL 13: the pack does not cover it; server_version_num is shown. */
export const Unsupported: Story = {
  args: { data: insightPackUnsupported },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("PostgreSQL not covered")).toBeVisible();
    await expect(canvas.getAllByText("130016").length).toBe(3);
  },
};
export const UnsupportedCompact = compact(Unsupported);
/** A query failed on one database: the panels it feeds are marked partial, the database named in the errors. */
export const PartialPanels: Story = {
  args: { data: insightPartial },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("billing")).toBeVisible();
    await expect(canvas.getAllByText("Partial").length).toBe(5);
  },
};
export const PartialPanelsDark = dark(PartialPanels);

const screen = { render: () => <InsightPackScreen namespace="team-a" cluster="orders" /> };
export const Live: Story = { ...screen, parameters: { msw: handlersFor("default") } };
export const Loading: Story = { ...screen, parameters: { msw: handlersFor("loading") } };
export const ServerError: Story = {
  ...screen,
  parameters: { msw: handlersFor("unavailable") },
  play: async ({ canvas }) => {
    await expect(await canvas.findByRole("button", { name: "Retry" })).toBeVisible();
  },
};
export const Forbidden: Story = { ...screen, parameters: { msw: handlersFor("forbidden") } };
