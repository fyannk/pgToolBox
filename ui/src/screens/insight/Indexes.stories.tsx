import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import { insightEnabled, insightNotEnabled } from "../../../mocks/fixtures";
import { insightIndexesConstraints, insightPanelsUnavailable, insightPartial } from "../../../mocks/fixtures/insight";
import { handlersFor } from "../../../mocks/handlers";
import { compact, dark, darkCompact } from "../../testing/variants";
import { IndexesScreen, IndexesView } from "./Indexes";

/**
 * Index hygiene of the primary: unused (no scan on the primary since the
 * statistics reset, with the primary-only caveat), invalid and duplicate
 * indexes, with their size.
 */
const meta = {
  title: "Screens/Insight/Indexes",
  component: IndexesView,
  args: { data: insightEnabled },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof IndexesView>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Enabled: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByText("events_payload_gin")).toBeVisible();
    await expect(canvas.getByText("orders_customer_id_idx")).toBeVisible();
    await expect(canvas.getByText(/used by queries on replicas only/)).toBeVisible();
    await expect(canvas.getAllByText(/no scan since/).length).toBeGreaterThan(0);
  },
};
export const EnabledDark = dark(Enabled);
export const EnabledCompact = compact(Enabled);
export const EnabledDarkCompact = darkCompact(Enabled);
export const Empty: Story = {
  args: { data: { ...insightEnabled, indexes: { ...insightEnabled.indexes, unused: [], invalid: [], duplicate: [] } } },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("No unused index reported.")).toBeVisible();
  },
};
export const NotEnabled: Story = { args: { data: insightNotEnabled } };
export const NotEnabledDark = dark(NotEnabled);
export const PanelUnavailable: Story = { args: { data: insightPanelsUnavailable } };

/** A duplicate pair both backing a constraint: reported, "not droppable"; an unused index serving a foreign key. */
export const Constraints: Story = {
  args: { data: insightIndexesConstraints },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("Not droppable: both back a constraint")).toBeVisible();
    await expect(canvas.getByText("serves a foreign key's checks: few scans expected")).toBeVisible();
    await expect(canvas.queryByText(/drop index/i)).toBeNull();
  },
};
export const ConstraintsDark = dark(Constraints);
export const Partial: Story = {
  args: { data: insightPartial },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/pgtoolbox_indexes failed on database billing/)).toBeVisible();
    await expect(canvas.getByText("events_payload_gin")).toBeVisible();
  },
};

const screen = { render: () => <IndexesScreen namespace="team-a" cluster="orders" /> };
export const Live: Story = { ...screen, parameters: { msw: handlersFor("default") } };
export const Loading: Story = { ...screen, parameters: { msw: handlersFor("loading") } };
export const ServerError: Story = {
  ...screen,
  parameters: { msw: handlersFor("unavailable") },
  play: async ({ canvas }) => {
    await expect(await canvas.findByRole("button", { name: "Retry" })).toBeVisible();
  },
};
