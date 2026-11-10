import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import { failingOverOverview, healthyOverview, topology } from "../../mocks/fixtures";
import { handlersFor } from "../../mocks/handlers";
import { compact, dark, darkCompact } from "../testing/variants";
import { OverviewScreen, OverviewView } from "./Overview";
import { Topology } from "./Topology";

/**
 * One Cluster: the Go roll-up with every contributor first, then the primary
 * election, backups and archiving with what the method does not report,
 * instance cards, conditions, warnings, the sources the page rests on and its
 * blind spots.
 */
const meta = {
  title: "Screens/Overview",
  component: OverviewView,
  args: { data: failingOverOverview },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof OverviewView>;
export default meta;
type Story = StoryObj<typeof meta>;

export const FailingOver: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("heading", { name: "Why the operator is waiting" })).toBeVisible();
    await expect(canvas.getByText(/Status: not reachable from this console/)).toBeVisible();
    await expect(canvas.getByText(/cnpg.io\/skipWalArchiving=enabled/)).toBeVisible();
  },
};
export const FailingOverDark = dark(FailingOver);
export const FailingOverCompact = compact(FailingOver);
export const FailingOverDarkCompact = darkCompact(FailingOver);

export const Healthy: Story = {
  args: { data: healthyOverview },
  play: async ({ canvas, canvasElement }) => {
    // The archiver's own last archived WAL, with its time and its source.
    const last = canvasElement.querySelector("[data-last-archived]") as HTMLElement;
    await expect(last).toHaveTextContent(/00000003000000000000002A/);
    await expect(last).toHaveTextContent(/own report/);
    await expect(canvas.getByRole("complementary", { name: "What this console cannot see here" })).toBeVisible();
    // The failover rehearsal is one line here, with the reasoning on Replication.
    await expect(canvas.getByRole("link", { name: "See the reasoning" })).toHaveAttribute(
      "href",
      "/c/team-a/orders/replication",
    );
  },
};
export const HealthyDark = dark(Healthy);

/** The wiring drawing, full width under the roll-up: the cluster at a glance, its boxes links. */
export const WithWiring: Story = {
  args: { data: healthyOverview, wiring: <Topology data={topology} /> },
  play: async ({ canvas, canvasElement }) => {
    await expect(canvas.getByRole("heading", { name: "Wiring" })).toBeVisible();
    // React Flow is loaded on demand: wait for the drawing.
    await expect(await canvas.findByRole("region", { name: "How orders is wired" })).toBeVisible();
    // The instance is offered as a link under the drawing, resolved onto this build's routes.
    await expect(canvasElement.querySelector('figcaption a[href$="instances/orders-1"]')).toBeVisible();
  },
};
export const WithWiringDark = dark(WithWiring);
export const WithWiringCompact = compact(WithWiring);
export const WithWiringDarkCompact = darkCompact(WithWiring);
export const Deleted: Story = { args: { data: { ...healthyOverview, present: false } } };

const screen = { render: () => <OverviewScreen namespace="team-a" cluster="billing" /> };
export const Loading: Story = { ...screen, parameters: { msw: handlersFor("loading") } };
export const ServerError: Story = {
  ...screen,
  parameters: { msw: handlersFor("unavailable") },
  play: async ({ canvas }) => {
    await expect(await canvas.findByRole("button", { name: "Retry" })).toBeVisible();
  },
};

/** Capabilities as the API server decided them: one it could not be asked about is said so, not hidden. */
export const AccessUndecided: Story = {
  args: {
    data: {
      ...healthyOverview,
      capabilities: {
        view: true,
        logs: false,
        operations: false,
        backup: false,
        reload: false,
        restart: false,
        promote: false,
        undecided: ["operations", "backup", "reload", "restart", "promote"],
      },
    },
  },
  play: async ({ canvas }) => {
    const panel = canvas.getByRole("region", { name: "What you may do here" });
    await expect(panel).toHaveTextContent(/Read instance logs:\s*not allowed/);
    await expect(panel).toHaveTextContent(/could not be asked/);
  },
};

/** The pgAdmin link-out offers "my personal server"; the capability list links to an access request. */
export const PgAdminAndAccess: Story = {
  args: { data: healthyOverview },
  parameters: { msw: handlersFor("default") },
  play: async ({ canvas }) => {
    await expect(await canvas.findByRole("button", { name: /Create my personal server/ })).toBeVisible();
    await expect(await canvas.findByRole("link", { name: /Request more access/ })).toBeVisible();
  },
};
export const PgAdminAndAccessDark = dark(PgAdminAndAccess);
/** The PgAdmin list could not be read: the panel is shown and says so, rather than hidden as if there were none. */
export const PgAdminsUnread: Story = {
  args: { data: { ...healthyOverview, pgAdmins: [], pgAdminsUnread: true } },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/could not read the PgAdmins of this namespace/)).toBeVisible();
  },
};
