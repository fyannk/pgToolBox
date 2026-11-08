import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, fn, userEvent, waitFor, within } from "storybook/test";
import { operationBadConfirmation, operationRefused, operationsOperator, operationsViewer } from "../../mocks/fixtures";
import { handlersFor } from "../../mocks/handlers";
import { ApiError } from "../api/fetcher";
import { compact, dark, darkCompact } from "../testing/variants";
import { OperationsScreen, OperationsView } from "./Operations";

/**
 * What the reader may do on this Cluster, why not when not, and the
 * operations this console replica issued. Every action goes through a typed
 * confirmation and is followed by an OperationTracker; nothing is shown as
 * done before the operator says so.
 */
const meta = {
  title: "Screens/Operations",
  component: OperationsView,
  args: { data: operationsOperator, actor: "oidc:jane@corp.example", onRequest: fn(async () => ({})) },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof OperationsView>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Operator: Story = {};
export const OperatorDark = dark(Operator);
export const OperatorCompact = compact(Operator);
export const OperatorDarkCompact = darkCompact(Operator);
/** A viewer: every offer refused by the API server, with the verbs it needs. */
export const Viewer: Story = {
  args: { data: operationsViewer },
  play: async ({ canvas }) => {
    for (const b of canvas.getAllByRole("button", { name: /Request…/ })) await expect(b).toBeDisabled();
  },
};

/** Backup: typed name → request sent with the confirmation. */
export const ConfirmBackup: Story = {
  play: async ({ args, canvas, canvasElement }) => {
    await userEvent.click(canvas.getByRole("button", { name: /on-demand backup/ }));
    const dialog = await within(canvasElement.ownerDocument.body).findByRole("alertdialog");
    await userEvent.type(within(dialog).getByLabelText(/To confirm, type/), "orders");
    await userEvent.click(within(dialog).getByRole("button", { name: "Request" }));
    await expect(args.onRequest).toHaveBeenCalledWith({ kind: "backup", target: undefined, confirmation: "orders" });
  },
};

/** Promote: an instance is chosen first; its name is what must be typed. */
export const ConfirmPromote: Story = {
  play: async ({ args, canvas, canvasElement }) => {
    await userEvent.click(canvas.getByRole("button", { name: /Promote a replica/ }));
    const dialog = await within(canvasElement.ownerDocument.body).findByRole("alertdialog");
    await expect(within(dialog).getByText("Choose an instance first.")).toBeVisible();
    const eligible = within(dialog)
      .getAllByRole("radio")
      .find((r) => !(r as HTMLInputElement).disabled) as HTMLInputElement;
    await userEvent.click(eligible);
    await userEvent.type(within(dialog).getByLabelText(/To confirm, type/), eligible.value);
    await userEvent.click(within(dialog).getByRole("button", { name: "Request" }));
    await expect(args.onRequest).toHaveBeenCalledWith({
      kind: "promote",
      target: eligible.value,
      confirmation: eligible.value,
    });
  },
};

/** A refused request (409) stays in the dialog with its reason. */
export const Refused: Story = {
  args: {
    onRequest: fn(async () => {
      throw new ApiError(409, operationRefused, "req-9");
    }),
  },
  play: async ({ canvas, canvasElement }) => {
    await userEvent.click(canvas.getByRole("button", { name: /Restart the cluster/ }));
    const dialog = await within(canvasElement.ownerDocument.body).findByRole("alertdialog");
    await userEvent.type(within(dialog).getByLabelText(/To confirm, type/), "orders");
    await userEvent.click(within(dialog).getByRole("button", { name: "Request" }));
    await waitFor(() => expect(within(dialog).getByRole("alert")).toHaveTextContent(operationRefused.title ?? ""));
  },
};
/** The server's own check of the typed name (422) is shown like any refusal. */
export const BadConfirmation: Story = {
  args: {
    onRequest: fn(async () => {
      throw new ApiError(422, operationBadConfirmation, "req-10");
    }),
  },
  play: async ({ canvas, canvasElement }) => {
    await userEvent.click(canvas.getByRole("button", { name: /Reload configuration/ }));
    const dialog = await within(canvasElement.ownerDocument.body).findByRole("alertdialog");
    await userEvent.type(within(dialog).getByLabelText(/To confirm, type/), "orders");
    await userEvent.click(within(dialog).getByRole("button", { name: "Request" }));
    await waitFor(() => expect(within(dialog).getByRole("alert")).toHaveTextContent("req-10"));
  },
};
export const Empty: Story = { args: { data: { ...operationsOperator, offers: [], recent: [] } } };
const screen = { render: () => <OperationsScreen namespace="team-a" cluster="orders" /> };
export const Loading: Story = { ...screen, parameters: { msw: handlersFor("loading") } };
export const ServerError: Story = { ...screen, parameters: { msw: handlersFor("unavailable") } };

/** Restore drill: where it runs, which backup, from which source, and the time limit, before and in the dialog. */
export const RestoreDrillOffer: Story = {
  args: {
    data: {
      ...operationsOperator,
      offers: (operationsOperator.offers ?? []).map((o) => (o.kind === "restoreDrill" ? { ...o, allowed: true } : o)),
    },
  },
  play: async ({ args, canvas, canvasElement }) => {
    await expect(canvas.getByText(/ObjectStore orders-store/)).toBeVisible();
    await userEvent.click(canvas.getByRole("button", { name: /Run a restore drill/ }));
    const dialog = await within(canvasElement.ownerDocument.body).findByRole("alertdialog");
    await expect(within(dialog).getByText("2 h")).toBeVisible();
    await userEvent.type(within(dialog).getByLabelText(/To confirm, type/), "orders");
    await userEvent.click(within(dialog).getByRole("button", { name: "Request" }));
    await expect(args.onRequest).toHaveBeenCalledWith({
      kind: "restoreDrill",
      target: undefined,
      confirmation: "orders",
    });
  },
};
