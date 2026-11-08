import type { Meta, StoryObj } from "@storybook/react-vite";
import { useState } from "react";
import { expect } from "storybook/test";
import { history } from "../../mocks/fixtures";
import { handlersFor } from "../../mocks/handlers";
import { compact, dark, darkCompact } from "../testing/variants";
import { HistoryScreen, HistoryView } from "./History";
import type { HistorySearch } from "./historySearch";

const params = { namespace: "team-a", cluster: "orders" };

/** The History screen with its URL state held locally (the route keeps it in the search params). */
function Stateful({ cluster, initial = {} }: { cluster: string; initial?: HistorySearch }) {
  const [search, setSearch] = useState<HistorySearch>(initial);
  return <HistoryScreen namespace="team-a" cluster={cluster} search={search} onSearch={setSearch} />;
}

/**
 * The revision list (the History screen's "Revisions" tab): what changed on
 * the Cluster and its children as this console observed it: observation
 * times, never server timestamps; the field manager is self-declared, never
 * an authenticated identity.
 */
const meta = {
  title: "Screens/History",
  component: HistoryView,
  args: { data: history, params },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof HistoryView>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Default: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("link", { name: /^#42 / })).toHaveAttribute("href", "/c/team-a/orders/history/42");
    await expect(canvas.getByText(/not an authenticated identity/)).toBeVisible();
  },
};
export const Dark = dark(Default);
export const Compact = compact(Default);
export const DarkCompact = darkCompact(Default);
export const Evicted: Story = { args: { data: { ...history, evicted: true, persistent: false } } };
export const Empty: Story = { args: { data: { ...history, items: [] } } };
/** The screen opened on the Revisions tab (?view=revisions), on the API fixtures. */
export const RevisionsTab: Story = {
  render: () => <Stateful cluster="orders" initial={{ view: "revisions" }} />,
  parameters: { msw: handlersFor("default") },
  play: async ({ canvas }) => {
    await expect(await canvas.findByRole("link", { name: /^#42 / })).toBeVisible();
    await expect(canvas.getByRole("tab", { name: "Revisions" })).toHaveAttribute("aria-selected", "true");
  },
};
const screen = { render: () => <Stateful cluster="orders" initial={{ view: "revisions" }} /> };
export const Loading: Story = { ...screen, parameters: { msw: handlersFor("loading") } };
export const ServerError: Story = { ...screen, parameters: { msw: handlersFor("unavailable") } };
