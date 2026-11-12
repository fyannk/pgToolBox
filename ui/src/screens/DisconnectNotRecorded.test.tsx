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
  const status = s.container.querySelector<HTMLElement>('[role="status"]');
  // The card that opened the dialog is gone: focus lands on the note, not on nothing.
  await expect.poll(() => s.container.ownerDocument.activeElement).toBe(status);
  expect(status?.textContent).toContain("Claude Code is disconnected on the replica that answered");
  expect(status?.textContent).not.toContain("Not started");
  expect(status?.querySelector('[data-health="warning"]')).not.toBeNull();
  // The grant stays visible (the list refetch drops it), with a way to retry.
  expect(s.container.querySelector('[data-grant-pending="grant-0c1d2e3f405162738495a6b7c8d9eaf0"]')).not.toBeNull();
  await expect.element(s.getByRole("button", { name: "Disconnect all" })).toBeVisible();
  // The retry answers 404: nothing was left to write, it is done, not a failure.
  // It is already confirmed (the name was typed): no second dialog.
  await s.getByRole("button", { name: "Retry disconnecting Claude Code" }).click();
  await expect.element(s.getByText(/Claude Code is disconnected: the console had nothing left/)).toBeVisible();
  expect(s.container.ownerDocument.querySelector('[role="alertdialog"]')).toBeNull();
  expect(s.container.querySelector("[data-grant-pending]")).toBeNull();
  // The pending row and its Retry button are gone: focus is on the note.
  await expect.poll(() => s.container.ownerDocument.activeElement).toBe(status);
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

test("connected: a retry refused for another reason keeps the pending row and says why, with the request id", async () => {
  const onRevoke = vi
    .fn<(g: GrantView) => Promise<void>>()
    .mockRejectedValueOnce(notRecorded())
    .mockRejectedValueOnce(notRecorded())
    .mockRejectedValueOnce(
      new ApiError(403, { title: "Forbidden", status: 403, detail: "not allowed to disconnect" }, "req-retry-403"),
    );
  const one = (grants.items ?? []).slice(0, 1);
  const s = await renderInFrame(
    <ConnectedAssistantsView grants={one} actor="x" onRevoke={onRevoke} onRevokeAll={vi.fn()} />,
  );
  await s.getByRole("button", { name: "Disconnect Claude Code" }).click();
  const dialog = s.getByRole("alertdialog");
  await dialog.getByLabelText(/To confirm, type/).fill("Claude Code");
  await dialog.getByRole("button", { name: "Disconnect" }).click();
  await expect.poll(() => s.container.ownerDocument.querySelector('[role="alertdialog"]')).toBeNull();
  const retry = s.getByRole("button", { name: "Retry disconnecting Claude Code" });
  // A second 503: still pending, still the honest warning, and focus stays on Retry.
  await retry.click();
  await expect.poll(() => onRevoke.mock.calls.length).toBe(2);
  await expect.element(retry).toBeEnabled();
  expect(s.container.querySelector('[role="status"] [data-health="warning"]')).not.toBeNull();
  expect(s.container.ownerDocument.activeElement).toBe(retry.element());
  // A refusal: the row stays, the note is critical with Go's detail and the request id.
  await retry.click();
  await expect.element(s.getByText("Request id req-retry-403")).toBeVisible();
  const status = s.container.querySelector('[role="status"]');
  expect(status?.querySelector('[data-health="critical"]')).not.toBeNull();
  expect(status?.textContent).toContain("not allowed to disconnect");
  expect(s.container.querySelector("[data-grant-pending]")).not.toBeNull();
  s.unmount();
});
