import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, userEvent } from "storybook/test";
import { dark } from "../../../testing/variants";
import { Tabs } from "./Tabs";

/** In-page tabs with a roving tabindex: one tab stop, Arrow keys move, Home/End jump. */
const meta = {
  title: "Patterns/Tabs",
  component: Tabs,
  args: {
    label: "Diagnostics",
    items: [
      { value: "incidents", label: "Incidents (1)", panel: <p>Incident list</p> },
      { value: "checks", label: "Checks (5)", panel: <p>Check table</p> },
      { value: "triage", label: "Triage", panel: <p>Playbooks</p> },
    ],
  },
} satisfies Meta<typeof Tabs>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Keyboard: Story = {
  play: async ({ canvas }) => {
    const tabs = canvas.getAllByRole("tab");
    await expect(tabs.filter((t) => t.tabIndex === 0)).toHaveLength(1);
    await userEvent.click(tabs[0] as HTMLElement);
    await userEvent.keyboard("{ArrowRight}");
    await expect(tabs[1]).toHaveFocus();
    await userEvent.keyboard("{End}");
    await expect(tabs[2]).toHaveFocus();
    await userEvent.keyboard("{Home}");
    await expect(tabs[0]).toHaveFocus();
  },
};
export const KeyboardDark = dark({ args: Keyboard.args });
