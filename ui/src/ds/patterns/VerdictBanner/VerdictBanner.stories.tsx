import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import { failingOverOverview, healthyOverview } from "../../../../mocks/fixtures";
import { compact, dark, darkCompact } from "../../../testing/variants";
import { VerdictBanner } from "./VerdictBanner";

/**
 * The roll-up headline and every contributor, worst first. The worst state is
 * computed in Go; unknown never rolls up to ok. A stale verdict
 * keeps its state and says that a contributor rests on a stale source.
 */
const meta = {
  title: "Patterns/VerdictBanner",
  component: VerdictBanner,
  args: {
    title: "Overall health",
    verdict: failingOverOverview.verdict,
    params: { namespace: "team-a", cluster: "billing" },
  },
} satisfies Meta<typeof VerdictBanner>;
export default meta;
type Story = StoryObj<typeof meta>;

export const CriticalAndStale: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("heading", { name: "Overall health" })).toBeVisible();
    await expect(canvas.getByText("At least one contributor rests on a stale source.")).toBeVisible();
    await expect(canvas.getByRole("link", { name: "Pods" })).toHaveAttribute("href", "/c/team-a/billing/instances");
  },
};
export const CriticalAndStaleDark = dark(CriticalAndStale);
export const CriticalAndStaleCompact = compact(CriticalAndStale);
export const CriticalAndStaleDarkCompact = darkCompact(CriticalAndStale);

export const Healthy: Story = {
  args: { verdict: healthyOverview.verdict, params: { namespace: "team-a", cluster: "orders" } },
};
export const HealthyDark = dark(Healthy);

/** No contributor reported: nothing was confirmed, and the banner says so. */
export const Empty: Story = {
  args: { verdict: { health: "unknown", contributors: [] } },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/nothing was confirmed/)).toBeVisible();
  },
};
