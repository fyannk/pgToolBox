import { expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import {
  accessBindingMissing,
  accessDeniedWithNote,
  accessGroupGranted,
  accessMinePending,
  accessToDecide,
} from "../../../../mocks/fixtures";
import { renderInFrame } from "../../../testing/render";
import { AccessRequestCard } from "./AccessRequestCard";

test("the phase is glyph + word + hue, never colour alone", async () => {
  const screen = await renderInFrame(<AccessRequestCard request={accessToDecide} actor="me" />);
  await expect.element(screen.getByText("Waiting for review")).toBeVisible();
  const chip = screen.container.querySelector("[data-health]");
  expect(chip?.getAttribute("data-health")).toBe("progressing");
  expect(chip?.textContent).toContain("◐");
});

test("the scope is Go's words: a named Cluster still binds the namespace", async () => {
  const screen = await renderInFrame(<AccessRequestCard request={accessToDecide} actor="me" />);
  await expect.element(screen.getByText(/every Cluster of namespace team-a, not only orders/)).toBeVisible();
});

test("decided-by, the note and a missing RoleBinding are shown", async () => {
  const denied = await renderInFrame(<AccessRequestCard request={accessDeniedWithNote} actor="me" />);
  await expect.element(denied.getByText(/by oidc:bob@corp\.example/)).toBeVisible();
  await expect.element(denied.getByText(/request viewer/)).toBeVisible();
  denied.unmount();
  const missing = await renderInFrame(<AccessRequestCard request={accessBindingMissing} actor="me" />);
  await expect.element(missing.getByText("RoleBindingMissing")).toBeVisible();
  expect(missing.container.querySelector("[data-condition]")?.getAttribute("data-health")).toBe("warning");
});

test("four-eyes: no approve on a request naming the reader, Go's reason shown; withdraw offered", async () => {
  const screen = await renderInFrame(<AccessRequestCard request={accessMinePending} actor="me" onDecide={vi.fn()} />);
  await expect.element(screen.getByRole("button", { name: /Withdraw/ })).toBeVisible();
  expect(screen.container.textContent).not.toContain("Approve…");
  expect(screen.container.textContent).toContain("ask someone else to decide");
});

test("revoke: typed name, optional note, sent as the decision", async () => {
  const onDecide = vi.fn(async () => ({}));
  const screen = await renderInFrame(<AccessRequestCard request={accessGroupGranted} actor="me" onDecide={onDecide} />);
  await screen.getByRole("button", { name: /Revoke/ }).click();
  const dialog = page.getByRole("alertdialog");
  await expect.element(dialog.getByRole("button", { name: "Revoke" })).toBeDisabled();
  await dialog.getByLabelText(/To confirm, type/).fill(accessGroupGranted.name);
  await dialog.getByLabelText(/^Note/).fill("moved to team-c");
  await dialog.getByRole("button", { name: "Revoke" }).click();
  expect(onDecide).toHaveBeenCalledWith({ verb: "revoke", note: "moved to team-c" });
});
