import type { Meta, StoryObj } from "@storybook/react-vite";
import { useState } from "react";
import { expect, fn, userEvent, waitFor, within } from "storybook/test";
import { compact, dark, darkCompact } from "../../../testing/variants";
import { CommandPalette, type PaletteGroup } from "./CommandPalette";

const select = fn();
const groups = (loading = false): PaletteGroup[] => [
  {
    id: "pages",
    label: "Pages of billing",
    items: ["Overview", "Instances", "Backups", "Diagnostics"].map((p) => ({
      id: `page:${p}`,
      label: p,
      hint: "team-a/billing",
      onSelect: () => select(p),
    })),
  },
  {
    id: "instances",
    label: "Instances of billing",
    loading,
    items: loading
      ? []
      : [
          {
            id: "pod:billing-1",
            label: "billing-1",
            hint: "team-a/billing",
            health: "critical",
            onSelect: () => select("billing-1"),
          },
          {
            id: "pod:billing-2",
            label: "billing-2",
            hint: "team-a/billing",
            health: "unknown",
            onSelect: () => select("billing-2"),
          },
        ],
  },
  {
    id: "clusters",
    label: "Clusters",
    items: [
      { id: "c:billing", label: "billing", hint: "team-a", health: "critical", onSelect: () => select("billing") },
      { id: "c:orders", label: "orders", hint: "team-a", health: "ok", onSelect: () => select("orders") },
      { id: "c:ledger", label: "ledger", hint: "team-b", health: "progressing", onSelect: () => select("ledger") },
    ],
  },
];

function Demo({ loading = false }: { loading?: boolean }) {
  const [open, setOpen] = useState(true);
  return (
    <>
      <button type="button" onClick={() => setOpen(true)} className="rounded-control border border-line px-2 py-1">
        Search ⌘K
      </button>
      <CommandPalette open={open} onOpenChange={setOpen} groups={groups(loading)} />
    </>
  );
}

/**
 * ⌘K / Ctrl+K: jump to a cluster, a page of the current cluster or one of its
 * instances. Use it for navigation only; an operation is never started from
 * here without its ConfirmDialog. Health chips are the Go states of the
 * targets, glyph + word as everywhere else.
 */
const meta = {
  title: "Patterns/CommandPalette",
  component: Demo,
  beforeEach: () => select.mockClear(),
} satisfies Meta<typeof Demo>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Open: Story = {
  play: async ({ canvasElement }) => {
    const body = within(canvasElement.ownerDocument.body);
    const dialog = await body.findByRole("dialog", { name: "Command palette" });
    // The Dialog moves focus into the input once it has opened, a frame after
    // the dialog is in the DOM: wait for it rather than read it at once.
    const input = within(dialog).getByRole("combobox", { name: /Search clusters/ });
    await waitFor(() => expect(input).toHaveFocus());
    await expect(within(dialog).getByText("Clusters")).toBeVisible();
  },
};
export const OpenDark = dark(Open);
export const OpenCompact = compact(Open);
export const OpenDarkCompact = darkCompact(Open);

/** Typing filters across groups; Enter opens the highlighted entry and closes the palette. */
export const FilterAndOpen: Story = {
  play: async ({ canvasElement }) => {
    const body = within(canvasElement.ownerDocument.body);
    const dialog = await body.findByRole("dialog", { name: "Command palette" });
    await userEvent.type(within(dialog).getByRole("combobox"), "ord");
    await waitFor(() => expect(within(dialog).getAllByRole("option")).toHaveLength(1));
    await expect(within(dialog).getByRole("option")).toHaveTextContent("orders");
    await userEvent.keyboard("{Enter}");
    await expect(select).toHaveBeenCalledWith("orders");
    await waitFor(() => expect(body.queryByRole("dialog")).toBeNull());
  },
};

export const NothingMatches: Story = {
  play: async ({ canvasElement }) => {
    const body = within(canvasElement.ownerDocument.body);
    const dialog = await body.findByRole("dialog", { name: "Command palette" });
    await userEvent.type(within(dialog).getByRole("combobox"), "zzz");
    await expect(await within(dialog).findByText("Nothing matches.")).toBeVisible();
  },
};

/** A list still being read is said, not shown as empty. */
export const Loading: Story = {
  args: { loading: true },
  play: async ({ canvasElement }) => {
    const body = within(canvasElement.ownerDocument.body);
    await expect(await body.findByText("Loading Instances of billing…")).toBeVisible();
  },
};
