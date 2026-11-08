import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import { dark } from "../../../testing/variants";
import { Provenance } from "./Provenance";

/** Who claims a fact, read from where, and when; stale with its reason. */
const meta = {
  title: "Patterns/Provenance",
  component: Provenance,
  args: { provenance: { origin: "operator", source: "Cluster.status.phase", observedAt: "2026-10-29T11:59:58Z" } },
} satisfies Meta<typeof Provenance>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Current: Story = {};
export const Stale: Story = {
  args: {
    provenance: {
      origin: "kubernetes",
      source: "Pod",
      observedAt: "2026-10-29T11:56:00Z",
      stale: true,
      staleReason: "pod watch lost: connection refused",
    },
  },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/stale: pod watch lost/)).toBeVisible();
  },
};
export const StaleDark = dark(Stale);
export const NeverRead: Story = { args: { provenance: { origin: "instance", source: "/pg/status" } } };
