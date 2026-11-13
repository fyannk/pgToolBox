import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, userEvent, within } from "storybook/test";
import { instances } from "../../mocks/fixtures";
import { handlersFor } from "../../mocks/handlers";
import type { InstanceRow } from "../api/generated/model";
import { compact, dark, darkCompact, french } from "../testing/variants";
import { InstancesScreen, InstancesView } from "./Instances";

/**
 * One row per instance with the three views side by side (operator, pod
 * label, instance itself). An instance whose status could not be read is an
 * error row with its reason, never omitted.
 */
const meta = {
  title: "Screens/Instances",
  component: InstancesView,
  args: { data: instances },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof InstancesView>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Default: Story = {
  play: async ({ canvasElement }) => {
    const rows = canvasElement.querySelectorAll("tr[data-error-row]");
    await expect(rows.length).toBe(3);
  },
};
export const Dark = dark(Default);
export const Compact = compact(Default);
export const DarkCompact = darkCompact(Default);

export const Detail: Story = {
  play: async ({ canvas, canvasElement }) => {
    await userEvent.click(canvas.getByRole("button", { name: "billing-1" }));
    const dialog = await within(canvasElement.ownerDocument.body).findByRole("dialog", { name: "billing-1" });
    await expect(within(dialog).getByText("CrashLoopBackOff")).toBeVisible();
    const ready = within(dialog).getByText("WAL files waiting to be archived").parentElement as HTMLElement;
    await expect(ready).toHaveTextContent("2");
  },
};

/** A primary read with no readyWalFiles in its report: "not reported", never an invented 0. */
function withoutReadyWal(): InstanceRow[] {
  return (instances.items ?? []).map((r) =>
    r.name === "billing-1" && r.live ? { ...r, live: { ...r.live, readyWalFiles: undefined } } : r,
  );
}
export const ReadyWalFilesNotReported: Story = {
  args: { data: { ...instances, items: withoutReadyWal() } },
  play: async ({ canvas, canvasElement }) => {
    await userEvent.click(canvas.getByRole("button", { name: "billing-1" }));
    const dialog = await within(canvasElement.ownerDocument.body).findByRole("dialog", { name: "billing-1" });
    const ready = within(dialog).getByText("WAL files waiting to be archived").parentElement as HTMLElement;
    await expect(ready).toHaveTextContent("not reported");
    await expect(ready).not.toHaveTextContent("0");
  },
};
export const ReadyWalFilesNotReportedFrench: Story = french({
  args: { data: { ...instances, items: withoutReadyWal() } },
  play: async ({ canvas, canvasElement }) => {
    await userEvent.click(canvas.getByRole("button", { name: "billing-1" }));
    const dialog = await within(canvasElement.ownerDocument.body).findByRole("dialog", { name: "billing-1" });
    const ready = within(dialog).getByText("Fichiers WAL en attente d’archivage").parentElement as HTMLElement;
    await expect(ready).toHaveTextContent("non communiqué");
  },
});
/**
 * No CA bundle configured: every instance is "not verifiable", an honest
 * configuration state with the administrator's fix, not an error.
 */
export const NoCaBundle: Story = {
  args: {
    data: {
      ...instances,
      items: (instances.items ?? []).map((r) => ({
        ...r,
        health: "unknown",
        live: undefined,
        statusRead: { state: "notVerifiable", reason: "no CA bundle configured", at: "2026-10-29T11:59:52Z" },
      })),
    },
  },
  play: async ({ canvas }) => {
    await expect(canvas.getAllByText(/console.instanceStatus.caBundleConfigMap/)).toHaveLength(3);
    await expect(canvas.queryByRole("alert")).toBeNull();
  },
};
export const NoCaBundleDark = dark(NoCaBundle);

export const Empty: Story = { args: { data: { ...instances, items: [] } } };
export const Loading: Story = {
  render: () => <InstancesScreen namespace="team-a" cluster="billing" />,
  parameters: { msw: handlersFor("loading") },
};
export const ServerError: Story = {
  render: () => <InstancesScreen namespace="team-a" cluster="billing" />,
  parameters: { msw: handlersFor("unavailable") },
};

const maskedError = "FATAL: the database system is shutting down (SQLSTATE 57P03)";
/** A report whose instance manager answered without reaching PostgreSQL (fenced, restarting or rewinding). */
function masked(name: string, isPrimary: boolean): InstanceRow {
  const base = (instances.items ?? [])[0] as InstanceRow;
  return {
    ...base,
    name,
    health: "warning",
    role: {
      operator: isPrimary ? "primary" : "replica",
      label: isPrimary ? "primary" : "replica",
      live: isPrimary ? "primary" : "replica",
    },
    statusRead: { state: "ok", at: "2026-10-29T11:59:52Z", since: "2026-10-29T11:40:00Z" },
    live: {
      isPrimary,
      mightBeUnavailable: true,
      instanceManagerVersion: "1.30.1",
      postgresUnread: { instance: name, error: maskedError },
      provenance: { origin: "instance", source: "/pg/status", observedAt: "2026-10-29T11:59:52Z" },
    },
  };
}
const maskedData = { ...instances, items: [masked("billing-1", true), masked("billing-2", false)] };

/**
 * A fenced or restarting primary: the status read is ok, but PostgreSQL was
 * not read. Its PostgreSQL fields say "not read", never "no" or 0, and the
 * masked error is shown.
 */
export const MaskedPrimary: Story = {
  args: { data: maskedData },
  play: async ({ canvas, canvasElement }) => {
    await expect(canvas.getAllByText("PostgreSQL not read (fenced, restarting or rewinding)")).toHaveLength(2);
    await userEvent.click(canvas.getByRole("button", { name: "billing-1" }));
    const dialog = await within(canvasElement.ownerDocument.body).findByRole("dialog", { name: "billing-1" });
    await expect(
      within(dialog).getByText(/answered without reaching it \(fenced, restarting or rewinding\): FATAL/),
    ).toBeVisible();
    await expect(within(dialog).getAllByText("not read").length).toBeGreaterThanOrEqual(7);
    await expect(within(dialog).getByText("Archives WAL")).toBeVisible();
    await expect(within(dialog).queryByText("no, or not reported")).toBeNull();
  },
};
export const MaskedPrimaryDark = dark(MaskedPrimary);
/** A masked replica: its WAL receiver and replay pause are not read, never "not active". */
export const MaskedReplica: Story = {
  args: { data: maskedData },
  play: async ({ canvas, canvasElement }) => {
    await userEvent.click(canvas.getByRole("button", { name: "billing-2" }));
    const dialog = await within(canvasElement.ownerDocument.body).findByRole("dialog", { name: "billing-2" });
    await expect(within(dialog).getByText("WAL receiver")).toBeVisible();
    await expect(within(dialog).getAllByText("not read").length).toBeGreaterThanOrEqual(7);
    await expect(within(dialog).queryByText("not active")).toBeNull();
  },
};
export const MaskedReplicaFrench: Story = french({
  args: { data: maskedData },
  play: async ({ canvas, canvasElement }) => {
    await userEvent.click(canvas.getByRole("button", { name: "billing-2" }));
    const dialog = await within(canvasElement.ownerDocument.body).findByRole("dialog", { name: "billing-2" });
    await expect(
      within(dialog).getByText(/PostgreSQL non lu : le gestionnaire d’instance a répondu sans l’atteindre/),
    ).toBeVisible();
    await expect(within(dialog).getAllByText("non lu").length).toBeGreaterThanOrEqual(7);
  },
});
