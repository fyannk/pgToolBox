import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import { digests, digestsDenied } from "../../mocks/fixtures";
import { handlersFor } from "../../mocks/handlers";
import { compact, dark, darkCompact, french } from "../testing/variants";
import { DigestsScreen, DigestsView } from "./Digests";

/**
 * The console's daily digests (/digests): the schedule and one row per
 * digest — its 24 hours, how many Clusters, which need attention, what
 * happened. Counts are the reader's own: a digest shows only the Clusters
 * they may get.
 */
const meta = {
  title: "Screens/Digests",
  component: DigestsView,
  args: { data: digests },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof DigestsView>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Default: Story = {
  play: async ({ canvas }) => {
    const links = canvas.getAllByRole("link").map((l) => l.getAttribute("href"));
    await expect(links).toContain("/digests/20261029T0700Z");
    await expect(links).toContain("/c/team-a/billing");
    await expect(canvas.getByText("partial")).toBeVisible();
    await expect(canvas.getByText("07:00 UTC")).toBeVisible();
  },
};
export const Dark = dark(Default);
export const Compact = compact(Default);
export const DarkCompact = darkCompact(Default);
export const French: Story = french({});
/** A reader who may get fewer Clusters: their counts, not the console's. */
export const FewerClusters: Story = { args: { data: digestsDenied } };
export const Empty: Story = {
  args: { data: { ...digests, items: [] } },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/No digest yet/)).toBeVisible();
  },
};
export const Disabled: Story = {
  args: { data: { ...digests, items: [], source: { source: "digest", state: "disabled", health: "inactive" } } },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("Digests are switched off")).toBeVisible();
  },
};
export const Live: Story = { render: () => <DigestsScreen />, parameters: { msw: handlersFor("default") } };
export const Loading: Story = { render: () => <DigestsScreen />, parameters: { msw: handlersFor("loading") } };
export const ServerError: Story = { render: () => <DigestsScreen />, parameters: { msw: handlersFor("unavailable") } };
