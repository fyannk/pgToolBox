import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import { dark, french } from "../../../testing/variants";
import { FindingSquares, SEVERITIES, SeverityChip } from "./Severity";

/**
 * A finding's severity, and a run's counts by severity. Four severities:
 * critical (doing damage now), high (a protection is gone, nothing lost yet),
 * warning, note. Their colours are their own, not the health states'.
 */
const meta = {
  title: "Patterns/Severity",
  component: SeverityChip,
  args: { severity: "critical" },
} satisfies Meta<typeof SeverityChip>;
export default meta;
type Story = StoryObj<typeof meta>;

const RUN = "2026-10-29T11:56:00Z";

export const All: Story = {
  render: () => (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap gap-2">
        {SEVERITIES.map((s) => (
          <SeverityChip key={s} severity={s} />
        ))}
      </div>
      <FindingSquares counts={{ critical: 1, high: 2, warning: 5, note: 3, unavailable: 6, runAt: RUN }} />
      <FindingSquares counts={{ critical: 0, high: 0, warning: 1, note: 0, unavailable: 0, runAt: RUN }} />
      <FindingSquares counts={{ critical: 0, high: 0, warning: 0, note: 0, unavailable: 0, runAt: RUN }} />
      <FindingSquares counts={{ critical: 0, high: 0, warning: 0, note: 0, unavailable: 0 }} />
    </div>
  ),
  play: async ({ canvas }) => {
    await expect(
      canvas.getByRole("img", { name: "1 critical, 2 high, 5 warning, 3 note, 6 checks could not run" }),
    ).toBeVisible();
    await expect(canvas.getByRole("img", { name: "no findings" })).toBeVisible();
    await expect(canvas.getByText("not run yet")).toBeInTheDocument();
  },
};

export const AllDark = dark({ render: All.render });
export const AllFrench = french({ render: All.render });
