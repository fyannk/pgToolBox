import { afterEach, expect, test, vi } from "vitest";
import { ApiError } from "../api/fetcher";
import { renderInFrame } from "../testing/render";
import { CopySummary } from "./CopySummary";

afterEach(() => vi.restoreAllMocks());

const summary = "**Cluster `billing` in namespace `team-a`: critical**\n";

test("reads the summary on the click, copies it and announces it", async () => {
  const write = vi.spyOn(navigator.clipboard, "writeText").mockResolvedValue();
  const load = vi.fn().mockResolvedValue(summary);
  const s = await renderInFrame(<CopySummary namespace="team-a" cluster="billing" load={load} />);
  expect(load).not.toHaveBeenCalled();
  await s.getByRole("button", { name: "Copy as Markdown: the incident summary of billing" }).click();
  await vi.waitFor(() => expect(s.getByRole("status").element().textContent).toMatch(/Copied/));
  expect(load).toHaveBeenCalledWith("team-a", "billing");
  expect(write).toHaveBeenCalledWith(summary);
  s.unmount();
});

test("a refused clipboard shows the summary selected for a manual copy", async () => {
  vi.spyOn(navigator.clipboard, "writeText").mockRejectedValue(new Error("denied"));
  const s = await renderInFrame(
    <CopySummary namespace="team-a" cluster="billing" load={() => Promise.resolve(summary)} />,
  );
  await s.getByRole("button", { name: /Copy as Markdown: the incident summary/ }).click();
  const area = s.getByRole("textbox", { name: "Incident summary of billing as Markdown" });
  await expect.element(area).toHaveValue(summary);
  await vi.waitFor(() => expect(s.getByRole("status").element().textContent).toMatch(/copy it by hand/));
  await vi.waitFor(() => {
    const el = area.element() as HTMLTextAreaElement;
    expect(el.selectionEnd - el.selectionStart).toBe(summary.length);
  });
  s.unmount();
});

test("a summary that cannot be read says so with the request id, and copies nothing", async () => {
  const write = vi.spyOn(navigator.clipboard, "writeText").mockResolvedValue();
  const err = new ApiError(403, { title: "Forbidden", status: 403, detail: "you may not get this Cluster" }, "req-7");
  const s = await renderInFrame(<CopySummary namespace="team-a" cluster="billing" load={() => Promise.reject(err)} />);
  await s.getByRole("button", { name: /Copy as Markdown: the incident summary/ }).click();
  const alert = s.getByRole("alert");
  await vi.waitFor(() => expect(alert.element().textContent).toMatch(/could not be rendered/));
  await vi.waitFor(() => expect(alert.element().textContent).toMatch(/you may not get this Cluster/));
  await vi.waitFor(() => expect(alert.element().textContent).toMatch(/req-7/));
  expect(write).not.toHaveBeenCalled();
  s.unmount();
});
