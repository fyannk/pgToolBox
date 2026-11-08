import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import { dark } from "../../../testing/variants";
import { LiveBanner } from "./LiveBanner";

/** Says when live updates are paused and how often the page is re-read meanwhile; silent while live. */
const meta = {
  title: "Patterns/LiveBanner",
  component: LiveBanner,
  args: { connected: false, intervalMs: 2000 },
} satisfies Meta<typeof LiveBanner>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Paused: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("status")).toHaveTextContent("Live updates paused; re-reading every 2 s.");
  },
};
export const PausedDark = dark(Paused);
export const PausedSlow: Story = { args: { intervalMs: 10_000 } };
export const Live: Story = { args: { connected: true, intervalMs: false } };
