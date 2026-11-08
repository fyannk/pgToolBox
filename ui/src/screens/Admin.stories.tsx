import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, within } from "storybook/test";
import { admin } from "../../mocks/fixtures";
import { handlersFor } from "../../mocks/handlers";
import { compact, dark, darkCompact } from "../testing/variants";
import { AdminScreen, AdminView } from "./Admin";

/**
 * The namespace admin's page: PgAdmin and EvidenceAgent objects of the
 * watched namespaces the reader may list, and the console's own health. A
 * kind the reader may not list is said to be unknown, not none.
 */
const meta = {
  title: "Screens/Admin",
  component: AdminView,
  args: { data: admin },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof AdminView>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Default: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getAllByText(/unknown, not none/).length).toBeGreaterThan(0);
  },
};
export const Dark = dark(Default);
export const Compact = compact(Default);
export const DarkCompact = darkCompact(Default);
const consoleHealth = admin.console;
if (!consoleHealth?.digest) throw new Error("getAdmin.json has no console digest");
const digestStatus = consoleHealth.digest;
/** The daily digest card: schedule, and per channel configured/off, last attempt and success, retry, error. */
export const Digest: Story = {
  play: async ({ canvas }) => {
    const card = within(canvas.getByRole("region", { name: "Daily digest" }));
    await expect(card.getByText("retry pending")).toBeVisible();
    await expect(card.getByText(/does not offer STARTTLS/)).toBeVisible();
    await expect(card.getByText("off")).toBeVisible();
    await expect(card.getAllByRole("link", { name: "20261029T0700Z" }).length).toBeGreaterThan(0);
  },
};
export const DigestOff: Story = {
  args: {
    data: { ...admin, console: { ...consoleHealth, digest: { ...digestStatus, enabled: false, channels: [] } } },
  },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/DIGEST_ENABLED=false/)).toBeVisible();
  },
};
/** A server that does not report the digest: no card, rather than a guessed state. */
export const DigestNotReported: Story = {
  args: { data: { ...admin, console: { ...consoleHealth, digest: undefined } } },
};
export const ConsoleHidden: Story = { args: { data: { ...admin, console: undefined } } };
export const Empty: Story = { args: { data: { ...admin, namespaces: [] } } };
export const Loading: Story = { render: () => <AdminScreen />, parameters: { msw: handlersFor("loading") } };
export const ServerError: Story = { render: () => <AdminScreen />, parameters: { msw: handlersFor("unavailable") } };
const firstNs = admin.namespaces?.[0];
if (!firstNs) throw new Error("getAdmin.json has no namespace");
/** A PgAdmin and EvidenceAgent list that failed: the tables say "not read", never "none". */
export const ListFailed: Story = {
  args: {
    data: {
      ...admin,
      namespaces: [
        {
          ...firstNs,
          pgAdminsVisible: true,
          evidenceAgentsVisible: true,
          pgAdmins: [],
          evidenceAgents: [],
          sources: [
            {
              source: "pgAdmins",
              state: "unavailable",
              health: "warning",
              reason: "the console's cache could not be read for this view; retry",
            },
            {
              source: "evidenceAgents",
              state: "unavailable",
              health: "warning",
              reason: "the console's cache could not be read for this view; retry",
            },
          ],
        },
      ],
    },
  },
  play: async ({ canvas }) => {
    await expect(canvas.getAllByText(/Not read: the console could not list them/).length).toBe(2);
    await expect(canvas.queryByText("None in this namespace.")).toBeNull();
  },
};
export const ListFailedDark = dark(ListFailed);
