import { expect, test } from "vitest";
import { operationActing, operationOverdue, operationSettled } from "../../../../mocks/fixtures/extra";
import { renderInFrame } from "../../../testing/render";
import { OperationTracker } from "./OperationTracker";

test("each reached step names the field that showed it; the phase is announced politely", async () => {
  const screen = await renderInFrame(<OperationTracker operation={operationActing} live />);
  const status = screen.getByRole("status");
  expect(status.element().getAttribute("aria-live")).toBe("polite");
  await expect.element(status).toBeVisible();
  expect(status.element().textContent).toContain("Operator acting");
  await expect.element(screen.getByText("Cluster.status.phase")).toBeVisible();
});

test("past the deadline with no outcome it says so, without calling it a failure", async () => {
  const screen = await renderInFrame(<OperationTracker operation={operationOverdue} live />);
  await expect.element(screen.getByRole("status")).toBeVisible();
  expect(screen.getByRole("status").element().textContent).toContain("No outcome reported");
  await expect.element(screen.getByText(/This does not mean it failed/)).toBeVisible();
});

test("a settled operation shows its outcome step", async () => {
  const screen = await renderInFrame(<OperationTracker operation={operationSettled} live={false} />);
  await expect.element(screen.getByRole("status")).toBeVisible();
  expect(screen.getByRole("status").element().textContent).toContain("Settled");
  expect(screen.container.textContent).not.toContain("polling");
});
