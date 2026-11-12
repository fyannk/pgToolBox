// Disconnect of one assistant answered 503, then retried.
// A 503 on a disconnection is Go's "refused on the replica that answered, not
// yet written for the others" (internal/console/api/assistant.go): never
// "Not started", and never a grant that silently vanishes.
//
// Its own file: the frame of one test file holds only a few dialog openings
// before later renders stall, so these do not share ConnectedAssistants.test.tsx.
import { expect, test, vi } from "vitest";
import { grants } from "../../mocks/fixtures";
import { ApiError } from "../api/fetcher";
import type { GrantView } from "../api/generated/model";
import { renderInFrame } from "../testing/render";
import { ConnectedAssistantsView } from "./Assistants";

const notRecorded = () =>
  new ApiError(
    503,
    { title: "Service Unavailable", status: 503, detail: "not recorded for every replica; retry" },
    "r",
  );

test("connected: a 503 closes the dialog, says refused here only, keeps a pending row and Disconnect all", async () => {
  const onRevoke = vi
    .fn<(g: GrantView) => Promise<void>>()
    .mockRejectedValueOnce(notRecorded())
    .mockRejectedValueOnce(
      new ApiError(404, { title: "Not Found", status: 404, detail: "no such grant of yours" }, "r2"),
    );
  const one = (grants.items ?? []).slice(0, 1);
  const s = await renderInFrame(
    <ConnectedAssistantsView grants={one} actor="x" onRevoke={onRevoke} onRevokeAll={vi.fn()} />,
  );
  const confirm = async () => {
    const dialog = s.getByRole("alertdialog");
    await dialog.getByLabelText(/To confirm, type/).fill("Claude Code");
    await dialog.getByRole("button", { name: "Disconnect" }).click();
    await expect.poll(() => s.container.ownerDocument.querySelector('[role="alertdialog"]')).toBeNull();
  };
  await s.getByRole("button", { name: "Disconnect Claude Code" }).click();
  await confirm();
  const status = s.container.querySelector('[role="status"]');
  expect(status?.textContent).toContain("Claude Code is disconnected on the replica that answered");
  expect(status?.textContent).not.toContain("Not started");
  expect(status?.querySelector('[data-health="warning"]')).not.toBeNull();
  // The grant stays visible (the list refetch drops it), with a way to retry.
  expect(s.container.querySelector('[data-grant-pending="grant-0c1d2e3f405162738495a6b7c8d9eaf0"]')).not.toBeNull();
  await expect.element(s.getByRole("button", { name: "Disconnect all" })).toBeVisible();
  // The retry answers 404: nothing was left to write, it is done, not a failure.
  await s.getByRole("button", { name: "Retry disconnecting Claude Code" }).click();
  await confirm();
  await expect.element(s.getByText(/Claude Code is disconnected: the console had nothing left/)).toBeVisible();
  expect(s.container.querySelector("[data-grant-pending]")).toBeNull();
  expect(onRevoke).toHaveBeenCalledTimes(2);
  s.unmount();
});

test("connected: a 404 without an earlier 503 is still a refusal shown in the dialog", async () => {
  const onRevoke = vi.fn(async () => {
    throw new ApiError(404, { title: "Not Found", status: 404, detail: "no such grant of yours" }, "r3");
  });
  const s = await renderInFrame(
    <ConnectedAssistantsView grants={grants.items ?? []} actor="x" onRevoke={onRevoke} onRevokeAll={vi.fn()} />,
  );
  await s.getByRole("button", { name: "Disconnect opencode" }).click();
  const dialog = s.getByRole("alertdialog");
  await dialog.getByLabelText(/To confirm, type/).fill("opencode");
  await dialog.getByRole("button", { name: "Disconnect" }).click();
  await expect.element(dialog.getByRole("alert")).toBeVisible();
  expect(dialog.getByRole("alert").element().textContent).toContain("no such grant of yours");
  s.unmount();
});
