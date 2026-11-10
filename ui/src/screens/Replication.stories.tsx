import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import { replication } from "../../mocks/fixtures";
import { handlersFor } from "../../mocks/handlers";
import { compact, dark, darkCompact } from "../testing/variants";
import { ReplicationScreen, ReplicationView } from "./Replication";

/** Who is primary, the synchronous configuration declared and the set PostgreSQL applied, every WAL sender, receiver and slot. Lag is shown, not judged. */
const meta = {
  title: "Screens/Replication",
  component: ReplicationView,
  args: { data: replication },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof ReplicationView>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Default: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("heading", { name: "WAL senders of the primary" })).toBeVisible();
    await expect(canvas.getAllByText("not verifiable").length).toBeGreaterThan(0);
    await expect(canvas.getByRole("heading", { name: "If the primary failed now" })).toBeVisible();
    await expect(canvas.getByText(/On the primary's node node-a: orders-3/)).toBeVisible();
  },
};
export const Dark = dark(Default);
export const Compact = compact(Default);
export const DarkCompact = darkCompact(Default);
/** Nothing reported: every list says so, never "none". */
export const Empty: Story = {
  args: { data: { ...replication, standbys: [], receivers: [], slots: [], quorum: undefined } },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/FailoverQuorum kind is not served/)).toBeVisible();
  },
};
/** The rehearsal when the primary's position was not read: the panel says it cannot tell. */
export const FailoverUnknown: Story = {
  args: {
    data: {
      ...replication,
      failover: {
        ...replication.failover,
        outcome: "unknown",
        health: "unknown",
        text: "No replica's status could be read, so the console cannot tell which one the operator would promote.",
        candidate: undefined,
        behindBytes: undefined,
        dataLoss: "unknown",
        dataLossText: "not known",
        quorum: undefined,
        ranking: [],
        sameNodeAsPrimary: [],
      },
    },
  },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("No replica reported.")).toBeVisible();
  },
};
/**
 * The sweep the rehearsal rests on is older than three sweep intervals: Go
 * no longer says "none" (unknown), and the panel shows the sweep's age.
 */
export const FailoverStaleSweep: Story = {
  args: {
    data: {
      ...replication,
      failover: {
        ...replication.failover,
        dataLoss: "unknown",
        health: "unknown",
        dataLossText:
          "The primary reported orders-2 as a synchronous standby (sync_state sync): commits waited for it. But the sweep has not completed for three intervals: whether that still holds is not known.",
        provenance: {
          ...replication.failover.provenance,
          observedAt: "2026-10-29T11:50:00Z",
          stale: true,
          staleReason: "the sweep has not completed for three intervals",
        },
      },
    },
  },
  play: async ({ canvasElement }) => {
    const asOf = canvasElement.querySelector("[data-loss-as-of]") as HTMLElement;
    await expect(asOf).toHaveTextContent(/as of the sweep/);
    await expect(asOf).toHaveTextContent(/three intervals/);
  },
};
/** Every instance fenced: the operator promotes none. */
export const FailoverBlocked: Story = {
  args: {
    data: {
      ...replication,
      failover: {
        ...replication.failover,
        outcome: "blocked",
        health: "critical",
        fencing: "all",
        text: "Every instance is fenced: the operator would promote none.",
      },
    },
  },
};
const screen = { render: () => <ReplicationScreen namespace="team-a" cluster="orders" /> };
export const Live: Story = { ...screen, parameters: { msw: handlersFor("default") } };
export const Loading: Story = { ...screen, parameters: { msw: handlersFor("loading") } };
export const ServerError: Story = {
  ...screen,
  parameters: { msw: handlersFor("unavailable") },
  play: async ({ canvas }) => {
    await expect((await canvas.findAllByRole("button", { name: "Retry" }))[0]).toBeVisible();
  },
};
export const Forbidden: Story = { ...screen, parameters: { msw: handlersFor("forbidden") } };
