import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import { insightEnabled, insightNotEnabled } from "../../../mocks/fixtures";
import { insightPanelsUnavailable, insightPartial } from "../../../mocks/fixtures/insight";
import { handlersFor } from "../../../mocks/handlers";
import { compact, dark, darkCompact } from "../../testing/variants";
import { ConnectionsScreen, ConnectionsView } from "./Connections";

/**
 * Connection saturation per instance: client connections against the usable
 * slots (max_connections less the reserved ones) and Go's health, sessions idle in a
 * transaction, lock waits, the oldest transaction, and who holds the
 * connections (database, application, state).
 */
const meta = {
  title: "Screens/Insight/Connections",
  component: ConnectionsView,
  args: { data: insightEnabled },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof ConnectionsView>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Enabled: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByText("172 of 197 usable client slots (87.3%)")).toBeVisible();
    await expect(canvas.getAllByText("0 (no timeout)").length).toBe(3);
  },
};
export const EnabledDark = dark(Enabled);
export const EnabledCompact = compact(Enabled);
export const EnabledDarkCompact = darkCompact(Enabled);
export const NotEnabled: Story = { args: { data: insightNotEnabled } };
export const NotEnabledDark = dark(NotEnabled);
export const PanelUnavailable: Story = { args: { data: insightPanelsUnavailable } };

export const Partial: Story = {
  args: { data: insightPartial },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/pgtoolbox_backends failed on orders-2/)).toBeVisible();
  },
};
export const PartialDark = dark(Partial);

const screen = { render: () => <ConnectionsScreen namespace="team-a" cluster="orders" /> };
export const Live: Story = { ...screen, parameters: { msw: handlersFor("default") } };
export const Loading: Story = { ...screen, parameters: { msw: handlersFor("loading") } };
export const ServerError: Story = {
  ...screen,
  parameters: { msw: handlersFor("unavailable") },
  play: async ({ canvas }) => {
    await expect(await canvas.findByRole("button", { name: "Retry" })).toBeVisible();
  },
};
