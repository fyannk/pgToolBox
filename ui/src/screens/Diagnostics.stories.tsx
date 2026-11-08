import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, userEvent } from "storybook/test";
import { diagnostics, triage } from "../../mocks/fixtures";
import { handlersFor } from "../../mocks/handlers";
import { compact, dark, darkCompact } from "../testing/variants";
import { DiagnosticsScreen, DiagnosticsView, TriageView } from "./Diagnostics";

/**
 * The latest diagnostics run: incidents (root finding and consequences, with
 * verbatim evidence and "look yourself" commands), every check with its
 * outcome — so an empty list never reads as "nothing wrong" when checks could
 * not run — and the triage playbooks walked against the same run.
 */
const meta = {
  title: "Screens/Diagnostics",
  component: DiagnosticsView,
  args: { data: diagnostics, triage: <TriageView data={triage} /> },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof DiagnosticsView>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Default: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByText("Instance billing-1 is restarting in a loop.")).toBeVisible();
    await userEvent.click(canvas.getByRole("tab", { name: /Checks/ }));
    await expect(canvas.getByRole("table", { name: "Checks of the last run" })).toBeVisible();
    await userEvent.click(canvas.getByRole("tab", { name: "Triage" }));
    await expect(canvas.getByText("Applications cannot write")).toBeVisible();
  },
};
export const Dark = dark({ args: Default.args });
export const Compact = compact({ args: Default.args });
export const DarkCompact = darkCompact({ args: Default.args });
export const NoFinding: Story = {
  args: {
    data: { ...diagnostics, incidents: [], counts: { ...diagnostics.counts, critical: 0, warning: 0, note: 0 } },
  },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/this is not a clean bill of health/)).toBeVisible();
  },
};
/** A finding linking to a screen this build does not have yet is plain text, not a dead link. */
export const LinkToLaterScreen: Story = {
  args: {
    data: {
      ...diagnostics,
      incidents: (diagnostics.incidents ?? []).map((i) => ({
        ...i,
        root: { ...i.root, link: "poolers/metrics", linkLabel: "Pooler metrics" },
      })),
    },
  },
  play: async ({ canvas }) => {
    const texts = canvas.getAllByText(/Pooler metrics \(that screen is not in this console version yet\)/);
    await expect(texts).toHaveLength(diagnostics.incidents?.length ?? 0);
    for (const text of texts) await expect(text).toBeVisible();
    await expect(canvas.queryByRole("link", { name: "Pooler metrics" })).toBeNull();
  },
};
/** A finding linking to a cluster section resolves onto its route. */
export const LinkToBackups: Story = {
  args: {
    data: {
      ...diagnostics,
      incidents: (diagnostics.incidents ?? []).map((i) => ({
        ...i,
        root: { ...i.root, link: "backups", linkLabel: "Backups" },
      })),
    },
  },
  play: async ({ canvas }) => {
    await expect(canvas.getAllByRole("link", { name: "Backups" })[0]).toHaveAttribute(
      "href",
      "/c/team-a/billing/backups",
    );
  },
};
export const NotApplicable: Story = { args: { data: { ...diagnostics, applied: undefined } } };
export const Loading: Story = {
  render: () => <DiagnosticsScreen namespace="team-a" cluster="billing" />,
  parameters: { msw: handlersFor("loading") },
};
export const ServerError: Story = {
  render: () => <DiagnosticsScreen namespace="team-a" cluster="billing" />,
  parameters: { msw: handlersFor("unavailable") },
};
