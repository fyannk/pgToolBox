import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import { timeline } from "../../../../mocks/fixtures";
import type { TimelineEntry } from "../../../api/generated/model";
import { compact, dark, darkCompact } from "../../../testing/variants";
import { HEALTH_STATES } from "../../health";
import { Timeline } from "./Timeline";

const items = timeline.items ?? [];
const params = { namespace: "team-a", cluster: "billing" };
const first = items[0];
if (!first) throw new Error("getTimeline.billing.json has no item");

/** The same entry in each of the seven health states (state + glyph + word, never colour alone). */
const everyState: TimelineEntry[] = HEALTH_STATES.map((h, i) => ({
  ...first,
  id: `state-${h}`,
  health: h,
  at: new Date(Date.parse(first.at) - i * 60_000).toISOString(),
}));

/**
 * Timeline — entries newest first, grouped by UTC day. Use it for anything
 * Go serves as TimelineEntry (the History screen's timeline, a digest's
 * Clusters and console gaps); not for a list the reader sorts (DataTable).
 * Feedback rule: every time says whose clock it is ("observed" by the
 * console, "reported" by the operator, kubelet or API server), and an entry
 * the console could only place between two observations says so.
 */
const meta = {
  title: "Patterns/Timeline",
  component: Timeline,
  args: { items, params, label: "Timeline entries" },
} satisfies Meta<typeof Timeline>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Default: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("heading", { level: 3 })).toHaveTextContent("(UTC)");
    await expect(canvas.getAllByRole("listitem")).toHaveLength(items.length);
  },
};
export const Dark = dark(Default);
export const Compact = compact(Default);
export const DarkCompact = darkCompact(Default);
export const HealthStates: Story = { args: { items: everyState } };
export const HealthStatesDark = dark(HealthStates);
/** Without a Cluster (a digest's console gaps): no link is drawn. */
export const WithoutLinks: Story = {
  args: { params: undefined },
  play: async ({ canvas }) => {
    await expect(canvas.queryAllByRole("link")).toHaveLength(0);
  },
};
