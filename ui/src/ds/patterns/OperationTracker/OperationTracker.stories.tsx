import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import {
  operationActing,
  operationBlocked,
  operationFailed,
  operationNotObservable,
  operationOverdue,
  operationSettled,
  operationTimedOut,
} from "../../../../mocks/fixtures/extra";
import { compact, dark, darkCompact } from "../../../testing/variants";
import { OperationTracker } from "./OperationTracker";

/**
 * Follows one operation from Requested to an outcome; each step names the
 * field that showed it. Past the deadline with no outcome it says that no
 * outcome was reported, which does not mean it failed. While live updates are
 * paused it says it is polling.
 */
const meta = {
  title: "Patterns/OperationTracker",
  component: OperationTracker,
  args: { operation: operationActing, live: true },
} satisfies Meta<typeof OperationTracker>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Acting: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("status")).toHaveTextContent("Operator acting");
    await expect(canvas.getByText("Cluster.status.phase")).toBeVisible();
  },
};
export const ActingDark = dark(Acting);
export const ActingCompact = compact(Acting);
export const ActingDarkCompact = darkCompact(Acting);
export const Settled: Story = { args: { operation: operationSettled } };
export const Failed: Story = { args: { operation: operationFailed } };
export const FailedDark = dark(Failed);
export const TimedOut: Story = {
  args: { operation: operationTimedOut },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/This does not mean it failed/)).toBeVisible();
  },
};
/** The server stopped answering: still "acting" but past the deadline. */
export const Overdue: Story = {
  args: { operation: operationOverdue },
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("status")).toHaveTextContent("No outcome reported");
  },
};
export const Polling: Story = {
  args: { live: false },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/following this operation by polling/)).toBeVisible();
  },
};

/** The operator reports no outcome for a reload: said, not shown as settled. */
export const NotObservable: Story = {
  args: { operation: operationNotObservable },
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("status")).toHaveTextContent("Sent, outcome not observable");
  },
};
/** The operator waits for the user. */
export const Blocked: Story = {
  args: { operation: operationBlocked },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/waiting for an action from you/)).toBeVisible();
  },
};

/** The chip's health is Go's Operation.health (blocked: warning), not a table in the UI. */
export const HealthFromGo: Story = {
  args: { operation: operationBlocked },
  play: async ({ canvasElement }) => {
    await expect(canvasElement.querySelector('[role="status"] [data-health]')?.getAttribute("data-health")).toBe(
      "warning",
    );
  },
};
