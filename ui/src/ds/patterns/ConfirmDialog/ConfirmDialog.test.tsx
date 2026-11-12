import { useRef, useState } from "react";
import { expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { ApiError } from "../../../api/fetcher";
import { renderInFrame } from "../../../testing/render";
import { ConfirmDialog } from "./ConfirmDialog";

const base = {
  title: "Restart billing",
  consequence: "Every instance restarts.",
  confirmWord: "billing",
  actor: "dba@example.org",
};

test("pessimistic: nothing is sent until the name is typed; on 202 the dialog closes", async () => {
  const onConfirm = vi.fn(async () => {});
  const onOpenChange = vi.fn();
  await renderInFrame(<ConfirmDialog {...base} open onOpenChange={onOpenChange} onConfirm={onConfirm} />);
  const submit = page.getByRole("button", { name: "Request" });
  await expect.element(submit).toBeDisabled();
  await page.getByLabelText(/To confirm, type/).fill("billing");
  await expect.element(submit).toBeEnabled();
  await submit.click();
  expect(onConfirm).toHaveBeenCalledOnce();
  expect(onOpenChange).toHaveBeenCalledWith(false);
});

test("a refused request stays in the dialog with its reason and request id", async () => {
  const onConfirm = vi.fn(async () => {
    throw new ApiError(409, { title: "A switchover is in flight", status: 409 }, "req-42");
  });
  const onOpenChange = vi.fn();
  await renderInFrame(<ConfirmDialog {...base} open onOpenChange={onOpenChange} onConfirm={onConfirm} />);
  await page.getByLabelText(/To confirm, type/).fill("billing");
  await page.getByRole("button", { name: "Request" }).click();
  await expect.element(page.getByRole("alert")).toBeVisible();
  expect(page.getByRole("alert").element().textContent).toContain("Not started: A switchover is in flight");
  await expect.element(page.getByRole("alert")).toBeVisible();
  expect(page.getByRole("alert").element().textContent).toContain("Request id req-42");
  expect(onOpenChange).not.toHaveBeenCalledWith(false);
});

test("the dialog promises the audit log, never a Kubernetes Event", async () => {
  await renderInFrame(<ConfirmDialog {...base} open onOpenChange={vi.fn()} onConfirm={vi.fn(async () => {})} />);
  const text = page.getByRole("alertdialog").element().textContent ?? "";
  expect(text).toContain("the API server audit log records it");
  expect(text.toLowerCase()).not.toContain("event");
});

/** Opens the dialog from a trigger the outcome removes, as a disconnected card is. */
function Harness({ finalFocus }: { finalFocus?: () => HTMLElement | null | undefined }) {
  const [open, setOpen] = useState(false);
  const [gone, setGone] = useState(false);
  const note = useRef<HTMLParagraphElement>(null);
  return (
    <div>
      {!gone && (
        <button type="button" onClick={() => setOpen(true)}>
          Open
        </button>
      )}
      <p tabIndex={-1} ref={note} data-testid="note">
        note
      </p>
      <ConfirmDialog
        {...base}
        open={open}
        onOpenChange={setOpen}
        finalFocus={finalFocus ?? (() => (gone ? note.current : undefined))}
        onConfirm={async () => {
          setGone(true);
        }}
      />
    </div>
  );
}

test("finalFocus: when the outcome removed the opener, focus goes to the element the caller names", async () => {
  const s = await renderInFrame(<Harness />);
  await s.getByRole("button", { name: "Open" }).click();
  await page.getByLabelText(/To confirm, type/).fill("billing");
  await page.getByRole("button", { name: "Request" }).click();
  await expect.poll(() => document.querySelector('[role="alertdialog"]')).toBeNull();
  await expect.poll(() => document.activeElement).toBe(s.getByTestId("note").element());
  s.unmount();
});

test("finalFocus: a cancel keeps the default, back to the control that opened the dialog", async () => {
  const s = await renderInFrame(<Harness />);
  const opener = s.getByRole("button", { name: "Open" });
  await opener.click();
  await page.getByRole("button", { name: "Cancel" }).click();
  await expect.poll(() => document.querySelector('[role="alertdialog"]')).toBeNull();
  await expect.poll(() => document.activeElement).toBe(opener.element());
  s.unmount();
});
