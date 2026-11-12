// A disconnection answered 503 because the replica could not read the shared
// state (internal/console/api/assistant.go, ProblemDisconnectionUnconfirmed):
// nothing may be disconnected, so the page never says "disconnected on the
// replica that answered" nor "it keeps retrying" (round 22, DBA). A 503 of
// any other layer (no problem type) is read the same way: it confirms nothing.
//
// Its own file: the frame of one test file holds only a few dialog openings
// before later renders stall.
import { expect, test, vi } from "vitest";
import { grants } from "../../mocks/fixtures";
import { ApiError } from "../api/fetcher";
import type { GrantView } from "../api/generated/model";
import { renderInFrame } from "../testing/render";
import { ConnectedAssistantsView } from "./Assistants";

const unconfirmed = () =>
  new ApiError(
    503,
    {
      title: "Service Unavailable",
      status: 503,
      type: "urn:pgtoolbox:disconnection-unconfirmed",
      detail: "could not read the shared state; retry",
    },
    "r",
  );

test("connected: Disconnect all unconfirmed says nothing may be disconnected and keeps every grant pending", async () => {
  const onRevokeAll = vi.fn<() => Promise<void>>().mockRejectedValueOnce(unconfirmed());
  const s = await renderInFrame(
    <ConnectedAssistantsView grants={grants.items ?? []} actor="x" onRevoke={vi.fn()} onRevokeAll={onRevokeAll} />,
  );
  await s.getByRole("button", { name: "Disconnect all" }).click();
  const dialog = s.getByRole("alertdialog");
  await dialog.getByLabelText(/To confirm, type/).fill("disconnect all");
  await dialog.getByRole("button", { name: "Disconnect all" }).click();
  await expect.poll(() => s.container.ownerDocument.querySelector('[role="alertdialog"]')).toBeNull();
  const note = s.container.querySelector('[role="status"]')?.textContent ?? "";
  expect(note).toContain("Your assistants may still be connected");
  expect(note).toContain("none may be disconnected yet");
  expect(note).not.toContain("disconnected on the replica that answered");
  expect(note).not.toContain("keeps retrying");
  expect(s.container.querySelectorAll("[data-grant-pending]")).toHaveLength(3);
  s.unmount();
});

test("connected: one disconnection answered by a 503 without a type confirms nothing", async () => {
  const onRevoke = vi
    .fn<(g: GrantView) => Promise<void>>()
    .mockRejectedValueOnce(new ApiError(503, { title: "Service Unavailable", status: 503 }, "r2"));
  const one = (grants.items ?? []).slice(0, 1);
  const s = await renderInFrame(
    <ConnectedAssistantsView grants={one} actor="x" onRevoke={onRevoke} onRevokeAll={vi.fn()} />,
  );
  await s.getByRole("button", { name: "Disconnect Claude Code" }).click();
  const dialog = s.getByRole("alertdialog");
  await dialog.getByLabelText(/To confirm, type/).fill("Claude Code");
  await dialog.getByRole("button", { name: "Disconnect" }).click();
  await expect.poll(() => s.container.ownerDocument.querySelector('[role="alertdialog"]')).toBeNull();
  const note = s.container.querySelector('[role="status"]')?.textContent ?? "";
  expect(note).toContain("Claude Code may still be connected");
  expect(note).not.toContain("disconnected on the replica that answered");
  expect(s.container.querySelector("[data-grant-pending]")).not.toBeNull();
  s.unmount();
});
