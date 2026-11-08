import { expect, test } from "vitest";
import { userEvent } from "vitest/browser";
import { renderInFrame } from "../../../testing/render";
import { Tabs } from "./Tabs";

const items = ["one", "two", "three"].map((v) => ({ value: v, label: v, panel: <p>panel {v}</p> }));

test("roving tabindex: one tab stop; arrows, Home and End move focus and selection", async () => {
  const screen = await renderInFrame(<Tabs label="Sections" items={items} />);
  const tabs = screen
    .getByRole("tab")
    .all()
    .map((l) => l.element() as HTMLElement);
  expect(tabs.filter((t) => t.tabIndex === 0)).toHaveLength(1);
  tabs[0]?.focus();
  await userEvent.keyboard("{ArrowRight}");
  expect(document.activeElement).toBe(tabs[1]);
  await userEvent.keyboard("{Enter}");
  await expect.element(screen.getByText("panel two")).toBeVisible();
  await userEvent.keyboard("{End}");
  expect(document.activeElement).toBe(tabs[2]);
  await userEvent.keyboard("{Home}");
  expect(document.activeElement).toBe(tabs[0]);
});
