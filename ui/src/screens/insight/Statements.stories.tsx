import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, userEvent } from "storybook/test";
import { insightEnabled, insightNotEnabled } from "../../../mocks/fixtures";
import {
  insightFreshConsole,
  insightPanelsUnavailable,
  insightPartial,
  insightStatementsUncertain,
} from "../../../mocks/fixtures/insight";
import { handlersFor } from "../../../mocks/handlers";
import { compact, dark, darkCompact } from "../../testing/variants";
import { StatementsScreen, StatementsView } from "./Statements";

/**
 * Query insight of the primary's pg_stat_statements: the costliest statements
 * and the regressions against the same hour a day earlier. Statement text
 * never leaves the database: each row offers Go's SQL, with its queryid, to
 * run in pgAdmin.
 */
const meta = {
  title: "Screens/Insight/Statements",
  component: StatementsView,
  args: { data: insightEnabled },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof StatementsView>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Enabled: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getAllByText("3.25×")[0]).toBeVisible();
    const summaries = canvas.getAllByText("SQL to read the text in pgAdmin");
    await userEvent.click(summaries[0] as HTMLElement);
    // The regression's queryid is also among the top statements: the first match is the regression row.
    await expect(
      canvas.getAllByText("SELECT query FROM pg_stat_statements WHERE queryid = 7766023351299184043 LIMIT 1;")[0],
    ).toBeVisible();
  },
};
export const EnabledDark = dark(Enabled);
export const EnabledCompact = compact(Enabled);
export const EnabledDarkCompact = darkCompact(Enabled);
export const NotComparableYet: Story = {
  args: { data: insightFreshConsole },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/holds 1 h of the 1 d 1 h of history/)).toBeVisible();
    await expect(canvas.getByText("No comparison made yet: see above why.")).toBeVisible();
  },
};
export const NotEnabled: Story = { args: { data: insightNotEnabled } };
/** The PgAdmin list could not be read: the pgAdmin panel says so instead of "no pgAdmin". */
export const PgAdminsUnread: Story = {
  args: { data: { ...insightEnabled, pgAdmins: [], pgAdminsUnread: true } },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/could not read the PgAdmins of this namespace/)).toBeVisible();
    await expect(canvas.queryByText("No pgAdmin of this Cluster is available to you.")).toBeNull();
  },
};
export const PgAdminsUnreadDark = dark(PgAdminsUnread);
export const NotEnabledDark = dark(NotEnabled);
export const PanelUnavailable: Story = {
  args: { data: insightPanelsUnavailable },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/pg_stat_statements is not loaded/)).toBeVisible();
  },
};

/** pg_stat_statements evicted entries: the regression is flagged uncertain, and what was not compared is counted. */
export const Uncertain: Story = {
  args: { data: insightStatementsUncertain },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("Uncertain")).toBeVisible();
    await expect(canvas.getByText(/evicted entries during the window/)).toBeVisible();
    await expect(canvas.getByText("130")).toBeVisible();
  },
};
export const UncertainDark = dark(Uncertain);
export const Partial: Story = {
  args: { data: insightPartial },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/cut at the console's row bound/)).toBeVisible();
  },
};

const screen = { render: () => <StatementsScreen namespace="team-a" cluster="orders" /> };
export const Live: Story = { ...screen, parameters: { msw: handlersFor("default") } };
export const Loading: Story = { ...screen, parameters: { msw: handlersFor("loading") } };
export const ServerError: Story = {
  ...screen,
  parameters: { msw: handlersFor("unavailable") },
  play: async ({ canvas }) => {
    await expect(await canvas.findByRole("button", { name: "Retry" })).toBeVisible();
  },
};
