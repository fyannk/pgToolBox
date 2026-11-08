import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import { compact, dark, darkCompact, french } from "../../../testing/variants";
import { HEALTH_STATES } from "../../health";
import { StatusChip } from "./StatusChip";

/**
 * The seven health states, each with its glyph and word; the hue only
 * reinforces them. `stale` is a modifier: the state is kept, the tint is
 * removed, the border dashed and the age said. Use it for every judgement the
 * Go view model carries; never derive a state in the UI.
 */
const meta = {
  title: "Patterns/StatusChip",
  component: StatusChip,
  args: { health: "ok" },
} satisfies Meta<typeof StatusChip>;
export default meta;
type Story = StoryObj<typeof meta>;

const OBSERVED = "2026-10-29T11:56:00Z";

export const AllStates: Story = {
  render: () => (
    <div className="flex flex-col gap-3">
      {(["chip", "text"] as const).map((variant) => (
        <div key={variant} className="flex flex-wrap gap-2">
          {HEALTH_STATES.map((h) => (
            <StatusChip key={h} health={h} variant={variant} />
          ))}
        </div>
      ))}
      <div className="flex flex-wrap gap-2">
        {HEALTH_STATES.map((h) => (
          <StatusChip key={h} health={h} stale observedAt={OBSERVED} staleReason="pod watch lost" />
        ))}
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <StatusChip health="critical" size="sm" />
        <StatusChip health="critical" size="md" />
        <StatusChip health="critical" size="lg" />
        <StatusChip health="warning" label="Failing over" />
      </div>
    </div>
  ),
  play: async ({ canvas }) => {
    // Seven words per row, never a colour alone.
    for (const word of ["ok", "in progress", "warning", "critical", "inactive", "unknown", "info"]) {
      await expect(canvas.getAllByText(word).length).toBeGreaterThanOrEqual(2);
    }
    await expect(canvas.getAllByText(/stale, as of 4 min\. ago/).length).toBe(7);
  },
};
export const AllStatesDark = dark(AllStates);
export const AllStatesCompact = compact(AllStates);
export const AllStatesDarkCompact = darkCompact(AllStates);
export const AllStatesFrench = french({ render: AllStates.render });

/** A state a newer Go build adds is drawn as unknown, never as ok. */
export const UnknownValue: Story = {
  args: { health: "degraded-by-a-future-build" },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("unknown")).toBeVisible();
  },
};
