import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, fn } from "storybook/test";
import { clusterList, clusterListPartial } from "../../mocks/fixtures";
import { handlersFor } from "../../mocks/handlers";
import { compact, dark, darkCompact, french } from "../testing/variants";
import { ClusterListScreen, ClusterListView } from "./ClusterList";

/**
 * "/": one row per Cluster the caller may get, with the Go verdict, the phase
 * verbatim, the primary, readiness, backup recency and findings; namespaces
 * whose Clusters could not be listed are named, so "no row" is never read as
 * "no cluster".
 */
const meta = {
  title: "Screens/ClusterList",
  component: ClusterListView,
  args: { data: clusterList, q: "", sort: undefined, onSearchChange: fn() },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof ClusterListView>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Default: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/team-a-staging: clusters not permitted to the console/)).toBeVisible();
    await expect(canvas.getByRole("link", { name: "team-a/billing" })).toHaveAttribute("href", "/c/team-a/billing");
    await expect(canvas.getAllByText(/stale/).length).toBeGreaterThan(0);
  },
};
export const DefaultDark = dark(Default);
export const DefaultCompact = compact(Default);
export const DefaultDarkCompact = darkCompact(Default);
export const DefaultFrench = french({ args: Default.args });
export const SortedByVerdict: Story = { args: { sort: "-verdict" } };
export const Filtered: Story = { args: { q: "ord" } };
export const NoMatch: Story = { args: { q: "nothing" } };
export const Empty: Story = {
  args: { data: { ...clusterList, items: [] } },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("No Cluster in the namespaces this console could list.")).toBeVisible();
  },
};

const screen = { render: () => <ClusterListScreen q="" sort={undefined} onSearchChange={() => {}} /> };
export const Loading: Story = { ...screen, parameters: { msw: handlersFor("loading") } };
export const ServerError: Story = {
  ...screen,
  parameters: { msw: handlersFor("unavailable") },
  play: async ({ canvas }) => {
    await expect(await canvas.findByRole("alert")).toHaveTextContent("req-fixture-0001");
  },
};
/** Billing is not visible to this user: it is simply not listed (a row's presence is an authorization outcome). */
export const Denied: Story = {
  ...screen,
  parameters: { msw: handlersFor("forbidden") },
  play: async ({ canvas }) => {
    await expect(await canvas.findByRole("link", { name: "team-a/orders" })).toBeVisible();
    await expect(canvas.queryByRole("link", { name: "team-a/billing" })).toBeNull();
  },
};
/** A Cluster the API server could not be asked about is left out, and the list says how many. */
export const Partial: Story = {
  args: { data: clusterListPartial },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/1 Clusters could not be checked/)).toBeVisible();
  },
};
export const FromTheApi: Story = {
  ...screen,
  parameters: { msw: handlersFor("default") },
  play: async ({ canvas }) => {
    await expect(await canvas.findByRole("link", { name: "team-a/orders" })).toBeVisible();
  },
};
