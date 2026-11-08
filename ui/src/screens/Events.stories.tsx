import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import { events } from "../../mocks/fixtures";
import { handlersFor } from "../../mocks/handlers";
import { compact, dark, darkCompact } from "../testing/variants";
import { EventsScreen, EventsView } from "./Events";

/** Events about the Cluster and its children, newest first, with the watch state and the window applied. */
const meta = {
  title: "Screens/Events",
  component: EventsView,
  args: { data: events },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof EventsView>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Default: Story = {};
export const Dark = dark(Default);
export const Compact = compact(Default);
export const DarkCompact = darkCompact(Default);
export const Empty: Story = { args: { data: { ...events, items: [] } } };
/** An empty list from a watch that is not live is not "no event". */
export const SourceNotLive: Story = {
  args: {
    data: {
      ...events,
      items: [],
      source: { source: "events", state: "unavailable", health: "warning", reason: "watch failed: 503" },
    },
  },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/this is not “no event”/)).toBeVisible();
  },
};
/** Two Events with the same kind, name, reason and second (Events carry no UID): two rows, neither hidden. */
export const SameSecond: Story = {
  args: {
    data: {
      ...events,
      items: events.items?.[0] ? [events.items[0], { ...events.items[0], message: "the second one, same second" }] : [],
    },
  },
  play: async ({ canvasElement, canvas }) => {
    await expect(canvas.getByText("the second one, same second")).toBeVisible();
    await expect(canvasElement.querySelectorAll("tbody tr")).toHaveLength(2);
  },
};
export const Truncated: Story = { args: { data: { ...events, truncated: true } } };
export const Loading: Story = {
  render: () => <EventsScreen namespace="team-a" cluster="billing" />,
  parameters: { msw: handlersFor("loading") },
};
export const ServerError: Story = {
  render: () => <EventsScreen namespace="team-a" cluster="billing" />,
  parameters: { msw: handlersFor("unavailable") },
};
