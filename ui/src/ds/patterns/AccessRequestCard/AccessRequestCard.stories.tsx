import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, fn, userEvent, waitFor, within } from "storybook/test";
import {
  accessApproved,
  accessApproveSelf,
  accessBindingMissing,
  accessDeniedWithNote,
  accessDenyNoNote,
  accessExpired,
  accessGroupGranted,
  accessMinePending,
  accessRevoked,
  accessToDecide,
} from "../../../../mocks/fixtures";
import { ApiError } from "../../../api/fetcher";
import { compact, dark, darkCompact } from "../../../testing/variants";
import { AccessRequestCard } from "./AccessRequestCard";

/**
 * One PgAccessRequest as Go describes it (the Go goldens). Use it on the
 * access-requests screen; not for the per-Cluster capability list, which is
 * the API server's answer, not a request. Feedback rule:
 * pessimistic decisions, typed request name, a note required to deny, the
 * result shown once the server answered; the "undo" of an approval is Revoke.
 * What the reader may do is Go's `actions` (four-eyes rule included).
 */
const meta = {
  title: "Patterns/AccessRequestCard",
  component: AccessRequestCard,
  args: { request: accessToDecide, actor: "oidc:jane@corp.example", onDecide: fn(async () => ({})) },
} satisfies Meta<typeof AccessRequestCard>;
export default meta;
type Story = StoryObj<typeof meta>;

/** progressing: waiting for a decision; the scope says the whole namespace is bound although orders was named. */
export const Pending: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByText("Waiting for review")).toBeVisible();
    await expect(canvas.getByText(/not only orders/)).toBeVisible();
    await expect(canvas.getByRole("button", { name: /Approve/ })).toBeEnabled();
  },
};
export const PendingDark = dark(Pending);
export const PendingCompact = compact(Pending);
export const PendingDarkCompact = darkCompact(Pending);
/** The reader's own request: no approve (four-eyes, Go's words), withdraw allowed. */
export const MinePending: Story = {
  args: { request: accessMinePending },
  play: async ({ canvas }) => {
    await expect(canvas.queryByRole("button", { name: /Approve/ })).toBeNull();
    await expect(canvas.getByText(/ask someone else to decide/)).toBeVisible();
    await expect(canvas.getByRole("button", { name: /Withdraw/ })).toBeEnabled();
  },
};
/** Just approved: Go says progressing until the operator confirms the grant (ok on the next refetch). */
export const ApprovedAwaitingOperator: Story = {
  args: { request: accessApproved },
  play: async ({ canvasElement }) => {
    await expect(canvasElement.querySelector("[data-health]")?.getAttribute("data-health")).toBe("progressing");
  },
};
export const ApprovedAwaitingOperatorDark = dark(ApprovedAwaitingOperator);
/** ok: granted to a group, revocable for the life of the grant. */
export const Approved: Story = { args: { request: accessGroupGranted } };
export const ApprovedDark = dark(Approved);
export const ApprovedCompact = compact(Approved);
/** Approved but the RoleBinding is gone: the operator's condition says so, it never recreates it. */
export const BindingMissing: Story = {
  args: { request: accessBindingMissing },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("RoleBindingMissing")).toBeVisible();
  },
};
export const BindingMissingDark = dark(BindingMissing);
/** inactive: denied, with the decider's note. */
export const Denied: Story = {
  args: { request: accessDeniedWithNote },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/request viewer/)).toBeVisible();
    await expect(canvas.queryByRole("button")).toBeNull();
  },
};
export const DeniedDark = dark(Denied);
export const Revoked: Story = { args: { request: accessRevoked } };
export const Expired: Story = { args: { request: accessExpired } };
export const ExpiredDarkCompact = darkCompact(Expired);
/** Without onDecide (the console does not write): read-only. */
export const ReadOnly: Story = {
  args: { onDecide: undefined },
  play: async ({ canvas }) => {
    await expect(canvas.queryByRole("button")).toBeNull();
  },
};

/** Approve: the request's name must be typed; the note is optional. */
export const ApproveFlow: Story = {
  play: async ({ args, canvas, canvasElement }) => {
    await userEvent.click(canvas.getByRole("button", { name: /Approve/ }));
    const dialog = await within(canvasElement.ownerDocument.body).findByRole("alertdialog");
    await expect(within(dialog).getByText(/covers every Cluster of team-a/)).toBeVisible();
    await userEvent.type(within(dialog).getByLabelText(/To confirm, type/), accessToDecide.name);
    await userEvent.click(within(dialog).getByRole("button", { name: "Approve" }));
    await expect(args.onDecide).toHaveBeenCalledWith({ verb: "approve", note: undefined });
  },
};
/** Deny: the typed name is not enough, a note is required. */
export const DenyNeedsNote: Story = {
  play: async ({ args, canvas, canvasElement }) => {
    await userEvent.click(canvas.getByRole("button", { name: /Deny/ }));
    const dialog = await within(canvasElement.ownerDocument.body).findByRole("alertdialog");
    await userEvent.type(within(dialog).getByLabelText(/To confirm, type/), accessToDecide.name);
    const submit = within(dialog).getByRole("button", { name: "Deny" });
    await expect(submit).toBeDisabled();
    await userEvent.type(within(dialog).getByLabelText(/^Note/), "Ask the on-call DBA");
    await expect(submit).toBeEnabled();
    await userEvent.click(submit);
    await expect(args.onDecide).toHaveBeenCalledWith({ verb: "deny", note: "Ask the on-call DBA" });
  },
};
/** The server refuses (403, four-eyes enforced server-side too): the dialog keeps its reason and request id. */
export const DecisionRefused: Story = {
  args: {
    onDecide: fn(async () => {
      throw new ApiError(403, accessApproveSelf, "req-77");
    }),
  },
  play: async ({ canvas, canvasElement }) => {
    await userEvent.click(canvas.getByRole("button", { name: /Approve/ }));
    const dialog = await within(canvasElement.ownerDocument.body).findByRole("alertdialog");
    await userEvent.type(within(dialog).getByLabelText(/To confirm, type/), accessToDecide.name);
    await userEvent.click(within(dialog).getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(within(dialog).getByRole("alert")).toHaveTextContent("req-77"));
    await expect(within(dialog).getByRole("alert")).toHaveTextContent(/ask someone else/);
  },
};
/** The server's own check of the note (422) is shown like any refusal. */
export const DenyRefusedWithoutNote: Story = {
  args: {
    onDecide: fn(async () => {
      throw new ApiError(422, accessDenyNoNote, "req-78");
    }),
  },
  play: async ({ canvas, canvasElement }) => {
    await userEvent.click(canvas.getByRole("button", { name: /Deny/ }));
    const dialog = await within(canvasElement.ownerDocument.body).findByRole("alertdialog");
    await userEvent.type(within(dialog).getByLabelText(/To confirm, type/), accessToDecide.name);
    await userEvent.type(within(dialog).getByLabelText(/^Note/), " x");
    await userEvent.click(within(dialog).getByRole("button", { name: "Deny" }));
    await waitFor(() => expect(within(dialog).getByRole("alert")).toHaveTextContent(/say why/));
  },
};
