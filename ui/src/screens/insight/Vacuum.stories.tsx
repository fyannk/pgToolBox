import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import { insightEnabled, insightNotEnabled } from "../../../mocks/fixtures";
import {
  insightFreshConsole,
  insightHorizonHolders,
  insightPanelsUnavailable,
  insightPartial,
} from "../../../mocks/fixtures/insight";
import { handlersFor } from "../../../mocks/handlers";
import { compact, dark, darkCompact } from "../../testing/variants";
import { VacuumScreen, VacuumView } from "./Vacuum";

/**
 * Wraparound and vacuum: each database's XID and MXID age against the
 * anti-wraparound autovacuum threshold, the vacuum failsafe and the stop
 * limit, with Go's time to each at the measured rate ("no rate measured yet"
 * when there is none, never "far away"); the thresholds and where they come
 * from; the tables the pack singled out with Go's autovacuum verdict; and the
 * vacuums running, wraparound ones flagged.
 */
const meta = {
  title: "Screens/Insight/Vacuum",
  component: VacuumView,
  args: { data: insightEnabled },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof VacuumView>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Enabled: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("heading", { name: "Databases against the wraparound horizons" })).toBeVisible();
    await expect(canvas.getAllByText(/passed, by/).length).toBeGreaterThan(0);
    await expect(canvas.getByText("to prevent wraparound")).toBeVisible();
    await expect(canvas.getByText("Not started")).toBeVisible();
    // The tables fold MXID age, reltuples, the dead-row threshold and autovacuum_enabled into existing cells.
    const tables = canvas.getByRole("region", { name: "Tables the pack singled out" });
    await expect(tables.querySelectorAll("thead th").length).toBe(6);
    await expect(tables.querySelectorAll("[data-av-enabled]").length).toBe(4);
    // What holds the xmin horizon: the abandoned slot, just behind app's age.
    await expect(canvas.getByText(/oldest: holds the horizon/)).toBeVisible();
  },
};
export const EnabledDark = dark(Enabled);
export const EnabledCompact = compact(Enabled);
export const EnabledDarkCompact = darkCompact(Enabled);
/** Just after a console restart: no rate yet, PostgreSQL defaults assumed and listed. */
export const NoRateYet: Story = {
  args: { data: insightFreshConsole },
  play: async ({ canvas }) => {
    await expect(canvas.getAllByText("no rate measured yet").length).toBeGreaterThan(0);
    await expect(canvas.getByText(/PostgreSQL default assumed for: vacuum_failsafe_age/)).toBeVisible();
  },
};
export const NotEnabled: Story = {
  args: { data: insightNotEnabled },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("Not enabled")).toBeVisible();
    await expect(canvas.queryByRole("table")).toBeNull();
  },
};
export const NotEnabledDark = dark(NotEnabled);
export const PanelUnavailable: Story = {
  args: { data: insightPanelsUnavailable },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/database query has not reported yet/)).toBeVisible();
  },
};

/** What holds the xmin horizon (a prepared transaction, the oldest), the prepared transactions, a TOAST table named with its parent, and a threshold unknown. */
export const HorizonHolders: Story = {
  args: { data: insightHorizonHolders },
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("heading", { name: "What holds the xmin horizon" })).toBeVisible();
    await expect(canvas.getByText(/oldest: holds the horizon/)).toBeVisible();
    await expect(canvas.getAllByText("nothing holds one").length).toBe(1);
    await expect(canvas.getByText("TOAST table of public.documents: VACUUM public.documents")).toBeVisible();
    await expect(canvas.getByText("unknown: never vacuumed nor analyzed")).toBeVisible();
  },
};
export const HorizonHoldersDark = dark(HorizonHolders);
export const HorizonHoldersCompact = compact(HorizonHolders);
export const HorizonHoldersDarkCompact = darkCompact(HorizonHolders);
/** A query failed on one database: the notice says which, the rows read are still shown. */
export const Partial: Story = {
  args: { data: insightPartial },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/pgtoolbox_tables failed on database billing/)).toBeVisible();
    await expect(canvas.getByRole("heading", { name: "Databases against the wraparound horizons" })).toBeVisible();
  },
};
export const PartialDark = dark(Partial);

const screen = { render: () => <VacuumScreen namespace="team-a" cluster="orders" /> };
export const Live: Story = { ...screen, parameters: { msw: handlersFor("default") } };
export const Loading: Story = { ...screen, parameters: { msw: handlersFor("loading") } };
export const ServerError: Story = {
  ...screen,
  parameters: { msw: handlersFor("unavailable") },
  play: async ({ canvas }) => {
    await expect(await canvas.findByRole("button", { name: "Retry" })).toBeVisible();
  },
};
