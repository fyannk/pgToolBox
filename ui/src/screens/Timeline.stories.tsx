import type { Meta, StoryObj } from "@storybook/react-vite";
import { useState } from "react";
import { expect, fn, waitFor } from "storybook/test";
import { timeline, timelineFiltered } from "../../mocks/fixtures";
import { handlersFor } from "../../mocks/handlers";
import { compact, dark, darkCompact, french } from "../testing/variants";
import { HistoryScreen, TimelineView } from "./History";
import type { HistorySearch } from "./historySearch";

const params = { namespace: "team-a", cluster: "billing" };

function Stateful({ initial = {} }: { initial?: HistorySearch }) {
  const [search, setSearch] = useState<HistorySearch>(initial);
  return <HistoryScreen {...params} search={search} onSearch={setSearch} />;
}

/**
 * The incident timeline (History screen, first tab): phase and primary
 * changes, restarts, backup results, findings, revisions, Events and the
 * console's own gaps on one axis, newest first, grouped by UTC day. Each
 * entry says whose clock its time is; an entry the console could only place
 * between two observations says so. The kind chips count the whole window.
 */
const meta = {
  title: "Screens/Timeline",
  component: TimelineView,
  args: { data: timeline, params, search: {}, onSearch: fn() },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof TimelineView>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Default: Story = {
  play: async ({ canvas, args }) => {
    await expect(canvas.getByText("Failover")).toBeVisible();
    await expect(canvas.getAllByText("reported").length).toBeGreaterThan(0);
    await expect(canvas.getAllByText("observed").length).toBeGreaterThan(0);
    await expect(canvas.getByText(/the console was not observing then/)).toBeVisible();
    const links = canvas.getAllByRole("link", { name: "Open the screen that explains it" });
    const hrefs = links.map((l) => l.getAttribute("href"));
    await expect(hrefs).toContain("/c/team-a/billing/replication");
    await expect(hrefs).toContain("/c/team-a/billing/history/41");
    await expect(hrefs).toContain("/c/team-a/billing/instances/billing-1");
    await expect(canvas.getByRole("button", { name: /^Findings/ })).toHaveAttribute("aria-pressed", "false");
    canvas.getByRole("button", { name: /^Findings/ }).click();
    await waitFor(() => expect(args.onSearch).toHaveBeenCalledWith({ kind: "finding" }));
  },
};
export const Dark = dark(Default);
export const Compact = compact(Default);
export const DarkCompact = darkCompact(Default);
export const French: Story = french({});
/** Kinds selected: the chips still count the whole window; the list holds the selected kinds only. */
export const Filtered: Story = {
  args: { data: timelineFiltered, search: { kind: "primary,finding" } },
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("button", { name: /^Primary/ })).toHaveAttribute("aria-pressed", "true");
    await expect(canvas.getByRole("button", { name: /^Backups/ })).toHaveAttribute("aria-pressed", "false");
  },
};
export const Empty: Story = {
  args: { data: { ...timeline, items: [] } },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/Nothing happened in this window/)).toBeVisible();
  },
};
export const EmptyFiltered: Story = {
  args: { data: { ...timelineFiltered, items: [] }, search: { kind: "primary,finding" } },
};
/** History is off on this console: there is no timeline, said as such. */
export const Disabled: Story = {
  args: {
    data: { ...timeline, items: [], counts: [], source: { source: "timeline", state: "disabled", health: "inactive" } },
  },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("The timeline is switched off")).toBeVisible();
  },
};
/** In memory, retention reached and the limit hit: the three honesty notes. */
export const TruncatedEvicted: Story = {
  args: { data: { ...timeline, truncated: true, evicted: true, persistent: false } },
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("status")).toHaveTextContent(/newest 9 entries/);
    await expect(canvas.getByText(/dropped by retention/)).toBeVisible();
  },
};
/** The whole screen on the API fixtures: choosing kinds changes the query (the filtered golden answers finding+primary). */
export const Screen: Story = {
  render: () => <Stateful />,
  parameters: { msw: handlersFor("default") },
  play: async ({ canvas }) => {
    await expect(await canvas.findByText("Backup failed")).toBeVisible();
    (await canvas.findByRole("button", { name: /^Primary/ })).click();
    await waitFor(() =>
      expect(canvas.getByRole("button", { name: /^Primary/ })).toHaveAttribute("aria-pressed", "true"),
    );
    (await canvas.findByRole("button", { name: /^Findings/ })).click();
    await waitFor(() => expect(canvas.queryByText("Backup failed")).toBeNull());
    await expect(await canvas.findByText("Failover")).toBeVisible();
  },
};
/**
 * "Last 1 h" chosen again while shown: the window re-anchors at now (it
 * also moves on once a minute), the list stays on screen while it is read.
 */
export const PresetChosenAgain: Story = {
  render: () => <Stateful initial={{ range: "1h" }} />,
  parameters: { msw: handlersFor("default") },
  play: async ({ canvas }) => {
    await expect(await canvas.findByText("Backup failed")).toBeVisible();
    const lastHour = canvas.getByRole("button", { name: "1 h" });
    await expect(lastHour).toHaveAttribute("aria-pressed", "true");
    lastHour.click();
    await expect(canvas.getByText("Backup failed")).toBeVisible();
    await waitFor(() => expect(canvas.getByRole("button", { name: "1 h" })).toHaveAttribute("aria-pressed", "true"));
  },
};
/** A window that ends before it starts: the API's 422 in its words, the controls still there to fix it. */
export const BadWindow: Story = {
  render: () => <Stateful initial={{ from: "2026-10-29T12:00:00Z", to: "2026-10-29T11:00:00Z" }} />,
  parameters: { msw: handlersFor("default") },
  play: async ({ canvas }) => {
    await expect(await canvas.findByRole("alert")).toHaveTextContent(/from must be before to/);
    await expect(canvas.getByLabelText("From (UTC)")).toHaveValue("2026-10-29T12:00");
  },
};
export const Loading: Story = { render: () => <Stateful />, parameters: { msw: handlersFor("loading") } };
export const ServerError: Story = { render: () => <Stateful />, parameters: { msw: handlersFor("unavailable") } };
