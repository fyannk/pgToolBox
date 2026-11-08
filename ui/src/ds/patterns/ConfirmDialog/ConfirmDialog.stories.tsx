import type { Meta, StoryObj } from "@storybook/react-vite";
import { useState } from "react";
import { expect, fn, userEvent, waitFor, within } from "storybook/test";
import { ApiError } from "../../../api/fetcher";
import { dark } from "../../../testing/variants";
import { ConfirmDialog, type ConfirmDialogProps } from "./ConfirmDialog";

function Demo(props: Omit<ConfirmDialogProps, "open" | "onOpenChange">) {
  const [open, setOpen] = useState(true);
  return (
    <>
      <button type="button" onClick={() => setOpen(true)} className="rounded-control border border-line px-2 py-1">
        Restart billing
      </button>
      <ConfirmDialog {...props} open={open} onOpenChange={setOpen} />
    </>
  );
}

/**
 * Typed confirmation for a cluster operation. The submit button
 * stays disabled until the cluster name is typed; a refusal stays in the
 * dialog with its reason and request id; on 202 the dialog closes and an
 * OperationTracker takes over. Nothing is shown as done here.
 */
const meta = {
  title: "Patterns/ConfirmDialog",
  component: Demo,
  args: {
    title: "Restart billing",
    consequence: "Every instance of billing restarts, replicas first, then a switchover before the primary restarts.",
    expectedDuration: "2 to 5 minutes",
    confirmWord: "billing",
    actor: "dba@example.org",
    onConfirm: fn(async () => {}),
  },
} satisfies Meta<typeof Demo>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Confirm: Story = {
  play: async ({ args, canvasElement }) => {
    const body = within(canvasElement.ownerDocument.body);
    const dialog = await body.findByRole("alertdialog", { name: "Restart billing" });
    // the trail is the API server audit log, not a Kubernetes Event.
    await expect(dialog).toHaveTextContent(
      "Performed with your identity (dba@example.org); the API server audit log records it.",
    );
    const submit = within(dialog).getByRole("button", { name: "Request" });
    await expect(submit).toBeDisabled();
    await userEvent.type(within(dialog).getByLabelText(/To confirm, type/), "billin");
    await expect(submit).toBeDisabled();
    await userEvent.type(within(dialog).getByLabelText(/To confirm, type/), "g");
    await expect(submit).toBeEnabled();
    await userEvent.click(submit);
    await expect(args.onConfirm).toHaveBeenCalledOnce();
    await waitFor(() => expect(body.queryByRole("alertdialog")).toBeNull());
  },
};
export const ConfirmDark = dark({ args: Confirm.args });

export const Refused: Story = {
  args: {
    onConfirm: fn(async () => {
      throw new ApiError(409, { title: "A switchover is already in flight", status: 409 }, "req-7f3a");
    }),
  },
  play: async ({ canvasElement }) => {
    const body = within(canvasElement.ownerDocument.body);
    const dialog = await body.findByRole("alertdialog");
    await userEvent.type(within(dialog).getByLabelText(/To confirm, type/), "billing");
    await userEvent.click(within(dialog).getByRole("button", { name: "Request" }));
    const alert = await within(dialog).findByRole("alert");
    await expect(alert).toHaveTextContent("Not started: A switchover is already in flight");
    await expect(alert).toHaveTextContent("Request id req-7f3a");
  },
};
