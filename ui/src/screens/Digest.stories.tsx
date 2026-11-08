import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, within } from "storybook/test";
import { digest } from "../../mocks/fixtures";
import { handlersFor } from "../../mocks/handlers";
import { compact, dark, darkCompact, french } from "../testing/variants";
import { DigestScreen, DigestView } from "./Digests";

/**
 * One digest (/digests/<id>, the link delivered messages carry): per Cluster
 * the reader may get, attention first, its verdict at close, the reasons, the
 * counts and the entries, each linking to the screen that explains it; the
 * console's own gaps; a partial digest says so; Clusters the reader may not
 * get are counted, never named.
 */
const meta = {
  title: "Screens/Digest",
  component: DigestView,
  args: { data: digest },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof DigestView>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Default: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/Partial: the console did not observe all of these 24 hours/)).toBeVisible();
    await expect(canvas.getByText("The console was down during the window.")).toBeVisible();
    const billing = within(canvas.getByRole("region", { name: "team-a/billing" }));
    await expect(billing.getByText("Needs attention")).toBeVisible();
    await expect(billing.getByText(/Phase, Pods/)).toBeVisible();
    const hrefs = billing.getAllByRole("link").map((l) => l.getAttribute("href"));
    await expect(hrefs).toContain("/c/team-a/billing/backups");
    await expect(hrefs).toContain("/c/team-a/billing/history/41");
    // orders needs no attention: its counts, no entries.
    const orders = within(canvas.getByRole("region", { name: "team-a/orders" }));
    await expect(orders.queryByRole("link", { name: "Open the screen that explains it" })).toBeNull();
    await expect(canvas.getByRole("region", { name: "Console gaps" })).toBeVisible();
  },
};
export const Dark = dark(Default);
export const Compact = compact(Default);
export const DarkCompact = darkCompact(Default);
export const French: Story = french({});
/** Clusters the reader may not get: counted, never named. */
export const Hidden: Story = {
  args: { data: { ...digest, hidden: 3, partial: false, consoleGaps: [] } },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/3 Cluster\(s\) you may not see are left out/)).toBeVisible();
    await expect(canvas.queryByText(/Partial/)).toBeNull();
  },
};
/** A Cluster whose list was cut: it says where the rest is. */
export const ClusterTruncated: Story = {
  args: {
    data: { ...digest, clusters: (digest.clusters ?? []).map((c, i) => (i === 0 ? { ...c, truncated: true } : c)) },
  },
};
export const NoClusters: Story = {
  args: { data: { ...digest, clusters: [], attention: [], consoleGaps: [], partial: false } },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/No Cluster you may get/)).toBeVisible();
  },
};
export const Live: Story = {
  render: () => <DigestScreen id="20261029T0700Z" />,
  parameters: { msw: handlersFor("default") },
};
/** A digest that left the retention: the API's 404 in its words. */
export const NotFound: Story = {
  render: () => <DigestScreen id="20250101T0700Z" />,
  parameters: { msw: handlersFor("default") },
  play: async ({ canvas }) => {
    await expect(await canvas.findByRole("alert")).toHaveTextContent(/left the retention/);
  },
};
export const Loading: Story = {
  render: () => <DigestScreen id="20261029T0700Z" />,
  parameters: { msw: handlersFor("loading") },
};
