import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, userEvent, within } from "storybook/test";
import { instances } from "../../mocks/fixtures";
import { handlersFor } from "../../mocks/handlers";
import { compact, dark, darkCompact } from "../testing/variants";
import { InstancesScreen, InstancesView } from "./Instances";

/**
 * One row per instance with the three views side by side (operator, pod
 * label, instance itself). An instance whose status could not be read is an
 * error row with its reason, never omitted.
 */
const meta = {
  title: "Screens/Instances",
  component: InstancesView,
  args: { data: instances },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof InstancesView>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Default: Story = {
  play: async ({ canvasElement }) => {
    const rows = canvasElement.querySelectorAll("tr[data-error-row]");
    await expect(rows.length).toBe(3);
  },
};
export const Dark = dark(Default);
export const Compact = compact(Default);
export const DarkCompact = darkCompact(Default);

export const Detail: Story = {
  play: async ({ canvas, canvasElement }) => {
    await userEvent.click(canvas.getByRole("button", { name: "billing-1" }));
    const dialog = await within(canvasElement.ownerDocument.body).findByRole("dialog", { name: "billing-1" });
    await expect(within(dialog).getByText("CrashLoopBackOff")).toBeVisible();
  },
};
/**
 * No CA bundle configured: every instance is "not verifiable", an honest
 * configuration state with the administrator's fix, not an error.
 */
export const NoCaBundle: Story = {
  args: {
    data: {
      ...instances,
      items: (instances.items ?? []).map((r) => ({
        ...r,
        health: "unknown",
        live: undefined,
        statusRead: { state: "notVerifiable", reason: "no CA bundle configured", at: "2026-10-29T11:59:52Z" },
      })),
    },
  },
  play: async ({ canvas }) => {
    await expect(canvas.getAllByText(/console.instanceStatus.caBundleConfigMap/)).toHaveLength(3);
    await expect(canvas.queryByRole("alert")).toBeNull();
  },
};
export const NoCaBundleDark = dark(NoCaBundle);

export const Empty: Story = { args: { data: { ...instances, items: [] } } };
export const Loading: Story = {
  render: () => <InstancesScreen namespace="team-a" cluster="billing" />,
  parameters: { msw: handlersFor("loading") },
};
export const ServerError: Story = {
  render: () => <InstancesScreen namespace="team-a" cluster="billing" />,
  parameters: { msw: handlersFor("unavailable") },
};
