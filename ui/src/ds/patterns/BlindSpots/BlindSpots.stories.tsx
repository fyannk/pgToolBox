import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import { healthyOverview, instances } from "../../../../mocks/fixtures";
import { compact, dark, darkCompact } from "../../../testing/variants";
import { BlindSpots } from "./BlindSpots";

/** What the console structurally cannot see, stated next to the panel it affects. Renders nothing when there is none. */
const meta = {
  title: "Patterns/BlindSpots",
  component: BlindSpots,
  args: { items: healthyOverview.blindSpots },
} satisfies Meta<typeof BlindSpots>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Default: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("complementary", { name: "What this console cannot see here" })).toBeVisible();
  },
};
export const Dark = dark(Default);
export const Compact = compact(Default);
export const DarkCompact = darkCompact(Default);
export const Instances: Story = { args: { items: instances.blindSpots } };
export const None: Story = { args: { items: [] } };
