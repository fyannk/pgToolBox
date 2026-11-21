import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import {
  promoteTargetFenced,
  promoteTargetLagging,
  promoteTargetReady,
  promoteTargetUnknown,
} from "../../mocks/fixtures/extra";
import { compact, dark, darkCompact } from "../testing/variants";
import { PromoteReadiness } from "./PromoteReadiness";

/**
 * The chosen promote target's readiness, in the switchover dialog: each fact
 * with its verdict word, source and age, as Go judged it. A fact not read now
 * is unknown, never ok; what CloudNativePG would refuse is stated plainly.
 */
const meta = {
  title: "Screens/Operations/PromoteReadiness",
  component: PromoteReadiness,
  args: { target: promoteTargetReady },
  parameters: { layout: "padded" },
} satisfies Meta<typeof PromoteReadiness>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Ready: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByText("streaming from the primary")).toBeVisible();
  },
};
export const ReadyDark = dark(Ready);
export const ReadyCompact = compact(Ready);
export const ReadyDarkCompact = darkCompact(Ready);

/** Asynchronous, 48 MiB behind, no WAL receiver: facts, not a refusal. */
export const Lagging: Story = { args: { target: promoteTargetLagging } };
export const LaggingDark = dark(Lagging);
export const LaggingCompact = compact(Lagging);

/** The status endpoint timed out: the kept reading is shown stale, the verdict unknown. */
export const Unknown: Story = {
  args: { target: promoteTargetUnknown },
  play: async ({ canvasElement }) => {
    await expect(canvasElement.querySelector('[data-fact="streaming"]')).toHaveAttribute("data-health", "unknown");
  },
};
export const UnknownDark = dark(Unknown);
export const UnknownCompact = compact(Unknown);

/** Fenced: CloudNativePG would refuse, said first. */
export const Fenced: Story = {
  args: { target: promoteTargetFenced },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("CloudNativePG would refuse:")).toBeVisible();
  },
};
export const FencedDark = dark(Fenced);
export const FencedCompact = compact(Fenced);

/** A console that serves no readiness facts: unknown, never ready. */
export const NotReported: Story = { args: { target: { ...promoteTargetReady, readiness: undefined } } };
export const NotReportedDark = dark(NotReported);
