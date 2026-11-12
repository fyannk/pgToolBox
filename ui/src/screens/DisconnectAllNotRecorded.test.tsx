// Disconnect all answered 503, then retried.
// A 503 on a disconnection is Go's "refused on the replica that answered, not
// yet written for the others" (internal/console/api/assistant.go): never
// "Not started", and never a grant that silently vanishes.
//
// Its own file: the frame of one test file holds only a few dialog openings
// before later renders stall, so these do not share ConnectedAssistants.test.tsx.
import { expect, test, vi } from "vitest";
import { grants } from "../../mocks/fixtures";
import { ApiError } from "../api/fetcher";
import { renderInFrame } from "../testing/render";
import { ConnectedAssistantsView } from "./Assistants";

const notRecorded = () =>
  new ApiError(
    503,
    { title: "Service Unavailable", status: 503, detail: "not recorded for every replica; retry" },
    "r",
  );

test("connected: Disconnect all answered 503 leaves every grant pending, then a 204 clears them", async () => {
  const onRevokeAll = vi.fn<() => Promise<void>>().mockRejectedValueOnce(notRecorded()).mockResolvedValueOnce();
  const s = await renderInFrame(
    <ConnectedAssistantsView grants={grants.items ?? []} actor="x" onRevoke={vi.fn()} onRevokeAll={onRevokeAll} />,
  );
  const run = async () => {
    await s.getByRole("button", { name: "Disconnect all" }).click();
    const dialog = s.getByRole("alertdialog");
    await dialog.getByLabelText(/To confirm, type/).fill("disconnect all");
    await dialog.getByRole("button", { name: "Disconnect all" }).click();
    await expect.poll(() => s.container.ownerDocument.querySelector('[role="alertdialog"]')).toBeNull();
  };
  await run();
  expect(s.container.querySelector('[role="status"]')?.textContent).toContain("retry Disconnect all to be sure");
  expect(s.container.querySelectorAll("[data-grant-pending]")).toHaveLength(3);
  await run();
  await expect.element(s.getByText(/Every assistant is disconnected/)).toBeVisible();
  expect(s.container.querySelectorAll("[data-grant-pending]")).toHaveLength(0);
  s.unmount();
});
