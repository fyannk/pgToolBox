import { expect, test, vi } from "vitest";
import { page, userEvent } from "vitest/browser";
import { renderInFrame } from "../../../testing/render";
import { CommandPalette, isPaletteShortcut } from "./CommandPalette";

const items = (onSelect: (id: string) => void) => [
  {
    id: "g",
    label: "Clusters",
    items: ["billing", "orders", "ledger"].map((n) => ({
      id: n,
      label: n,
      hint: "team-a",
      onSelect: () => onSelect(n),
    })),
  },
];

test("the shortcut is ⌘K or Ctrl+K, nothing else", () => {
  const k = (init: KeyboardEventInit) => isPaletteShortcut(new KeyboardEvent("keydown", init));
  expect(k({ key: "k", metaKey: true })).toBe(true);
  expect(k({ key: "K", ctrlKey: true })).toBe(true);
  expect(k({ key: "k" })).toBe(false);
  expect(k({ key: "k", ctrlKey: true, shiftKey: true })).toBe(false);
  expect(k({ key: "j", metaKey: true })).toBe(false);
});

test("arrows move the highlight, Enter selects it and closes", async () => {
  const onSelect = vi.fn();
  const onOpenChange = vi.fn();
  await renderInFrame(<CommandPalette open onOpenChange={onOpenChange} groups={items(onSelect)} />);
  const input = page.getByRole("combobox");
  await expect.element(input).toHaveFocus();
  await userEvent.keyboard("{ArrowDown}");
  await userEvent.keyboard("{Enter}");
  expect(onSelect).toHaveBeenCalledWith("orders");
  expect(onOpenChange).toHaveBeenCalledWith(false);
});

test("Escape closes without selecting", async () => {
  const onSelect = vi.fn();
  const onOpenChange = vi.fn();
  await renderInFrame(<CommandPalette open onOpenChange={onOpenChange} groups={items(onSelect)} />);
  await expect.element(page.getByRole("combobox")).toHaveFocus();
  await userEvent.keyboard("{Escape}");
  expect(onOpenChange).toHaveBeenCalledWith(false, expect.anything());
  expect(onSelect).not.toHaveBeenCalled();
});
