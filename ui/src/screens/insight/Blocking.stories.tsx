import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import { insightBlocking, insightEnabled, insightNotEnabled } from "../../../mocks/fixtures";
import {
  insightBlockingMissingCycle,
  insightBlockingNoPgAdmin,
  insightBlockingNotEnabled,
  insightBlockingPartial,
  insightBlockingPrepared,
} from "../../../mocks/fixtures/insight";
import { handlersFor } from "../../../mocks/handlers";
import { compact, dark, darkCompact } from "../../testing/variants";
import { BlockingScreen, BlockingView } from "./Blocking";

/**
 * The lock graph of each instance, root blockers first: who holds the lock,
 * who waits behind it and for how long. Read again every 2 s at most while the
 * page is shown. There is no terminate button: ending a backend is done in
 * pgAdmin, which the screen links to.
 */
const meta = {
  title: "Screens/Insight/Blocking",
  component: BlockingView,
  args: { insight: insightEnabled, blocking: insightBlocking, pollMs: 2_000 },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof BlockingView>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Enabled: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByText("pid 4711")).toBeVisible();
    await expect(canvas.getByText("blocks 2")).toBeVisible();
    await expect(canvas.getByText(/Exporter data 12 s old when read/)).toBeVisible();
    await expect(canvas.queryByRole("button", { name: /terminate/i })).toBeNull();
    await expect(canvas.getByRole("link", { name: /team-a/ })).toBeVisible();
  },
};
export const EnabledDark = dark(Enabled);
export const EnabledCompact = compact(Enabled);
export const EnabledDarkCompact = darkCompact(Enabled);
export const MissingAndCycle: Story = {
  args: { blocking: insightBlockingMissingCycle },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/not among the backends the pack read/)).toBeVisible();
    await expect(canvas.getByText(/a lock cycle/)).toBeVisible();
  },
};
export const NoPgAdmin: Story = {
  args: { blocking: insightBlockingNoPgAdmin },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("No pgAdmin of this Cluster is available to you.")).toBeVisible();
  },
};
export const NotEnabled: Story = { args: { insight: insightNotEnabled, blocking: insightBlockingNotEnabled } };
export const NotEnabledDark = dark(NotEnabled);

/** Prepared transactions hold the locks: named as such (no pid), with how they end, which the console never does. */
export const PreparedBlocker: Story = {
  args: { blocking: insightBlockingPrepared },
  play: async ({ canvas }) => {
    await expect(canvas.getAllByText("prepared transaction").length).toBe(2);
    await expect(canvas.queryByText("pid 0")).toBeNull();
    await expect(canvas.getAllByText(/ends only with COMMIT PREPARED or ROLLBACK PREPARED/).length).toBe(2);
  },
};
export const PreparedBlockerDark = dark(PreparedBlocker);
export const Partial: Story = {
  args: { blocking: insightBlockingPartial },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/pgtoolbox_backends failed on orders-2/)).toBeVisible();
    await expect(canvas.getByText("pid 4711")).toBeVisible();
  },
};

const screen = { render: () => <BlockingScreen namespace="team-a" cluster="orders" /> };
export const Live: Story = { ...screen, parameters: { msw: handlersFor("default") } };
export const Loading: Story = { ...screen, parameters: { msw: handlersFor("loading") } };
export const ServerError: Story = {
  ...screen,
  parameters: { msw: handlersFor("unavailable") },
  play: async ({ canvas }) => {
    await expect(await canvas.findByRole("button", { name: "Retry" })).toBeVisible();
  },
};
