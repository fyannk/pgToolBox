import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import { fleetStatements, fleetStatementsDenied } from "../../../mocks/fixtures";
import { fleetStatementsTruncated, fleetStatementsUnchecked } from "../../../mocks/fixtures/insight";
import { handlersFor } from "../../../mocks/handlers";
import { compact, dark, darkCompact } from "../../testing/variants";
import { FleetStatementsPage, FleetStatementsScreen } from "./Fleet";

/**
 * Team-wide query insight: the regressions and costliest statements of every
 * Cluster the reader may see, each linking to its Cluster's statements screen,
 * and each Cluster's coverage (not opted in, no comparison yet, and why).
 */
const meta = {
  title: "Screens/Insight/Fleet",
  component: FleetStatementsPage,
  args: { data: fleetStatements },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof FleetStatementsPage>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Default: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("heading", { name: "Coverage per Cluster" })).toBeVisible();
    await expect(canvas.getByText("Not enabled")).toBeVisible();
    const links = canvas.getAllByRole("link", { name: "team-a/orders" });
    await expect(links[0]).toHaveAttribute("href", "/c/team-a/orders/insight/statements");
  },
};
export const Dark = dark(Default);
export const Compact = compact(Default);
export const DarkCompact = darkCompact(Default);
/** Only the Clusters the reader may see (billing is not). */
export const Denied: Story = { args: { data: fleetStatementsDenied } };
export const Unchecked: Story = {
  args: { data: fleetStatementsUnchecked },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/2 Clusters could not be checked/)).toBeVisible();
  },
};
export const Empty: Story = { args: { data: { ...fleetStatements, clusters: [], regressions: [], top: [] } } };

/** The team-wide top cut at 100: the page says how many there were. */
export const TopTruncated: Story = {
  args: { data: fleetStatementsTruncated },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/Showing 3 of 240 statements/)).toBeVisible();
  },
};
export const TopTruncatedDark = dark(TopTruncated);

const screen = { render: () => <FleetStatementsScreen /> };
export const Live: Story = { ...screen, parameters: { msw: handlersFor("default") } };
export const Loading: Story = { ...screen, parameters: { msw: handlersFor("loading") } };
export const ServerError: Story = {
  ...screen,
  parameters: { msw: handlersFor("unavailable") },
  play: async ({ canvas }) => {
    await expect(await canvas.findByRole("button", { name: "Retry" })).toBeVisible();
  },
};
