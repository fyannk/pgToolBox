import type { Meta, StoryObj } from "@storybook/react-vite";
import { useState } from "react";
import { expect, userEvent, waitFor, within } from "storybook/test";
import { dark } from "../../../testing/variants";
import { Drawer } from "./Drawer";

function Demo({ initiallyOpen }: { initiallyOpen: boolean }) {
  const [open, setOpen] = useState(initiallyOpen);
  return (
    <>
      <button type="button" onClick={() => setOpen(true)} className="rounded-control border border-line px-2 py-1">
        Open billing-1
      </button>
      <Drawer open={open} onOpenChange={setOpen} title="billing-1" description="Instance detail">
        <p>Pod, status read and the instance's own report.</p>
      </Drawer>
    </>
  );
}

/**
 * A side panel for a detail that keeps the list behind it. Focus moves in,
 * Escape closes, focus returns to the opener.
 */
const meta = { title: "Patterns/Drawer", component: Demo, args: { initiallyOpen: false } } satisfies Meta<typeof Demo>;
export default meta;
type Story = StoryObj<typeof meta>;

export const OpenAndClose: Story = {
  play: async ({ canvas, canvasElement }) => {
    const opener = canvas.getByRole("button", { name: "Open billing-1" });
    await userEvent.click(opener);
    const body = within(canvasElement.ownerDocument.body);
    const dialog = await body.findByRole("dialog", { name: "billing-1" });
    await expect(dialog).toBeVisible();
    await userEvent.keyboard("{Escape}");
    await waitFor(() => expect(body.queryByRole("dialog")).toBeNull());
    await waitFor(() => expect(opener).toHaveFocus());
  },
};

export const Open: Story = { args: { initiallyOpen: true } };
export const OpenDark = dark(Open);
