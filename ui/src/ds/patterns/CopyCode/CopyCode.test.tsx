import { afterEach, expect, test, vi } from "vitest";
import { renderInFrame } from "../../../testing/render";
import { CopyCode } from "./CopyCode";

afterEach(() => vi.restoreAllMocks());

test("copies the code and announces it", async () => {
  const write = vi.spyOn(navigator.clipboard, "writeText").mockResolvedValue();
  const s = await renderInFrame(<CopyCode code="SELECT 1;" label="the query" />);
  await s.getByRole("button", { name: "Copy the query" }).click();
  expect(write).toHaveBeenCalledWith("SELECT 1;");
  await expect.element(s.getByRole("status")).toHaveTextContent("Copied.");
  s.unmount();
});

test("a refused clipboard selects the text and says so", async () => {
  vi.spyOn(navigator.clipboard, "writeText").mockRejectedValue(new Error("denied"));
  const s = await renderInFrame(<CopyCode code="SELECT 2;" label="the query" />);
  await s.getByRole("button", { name: "Copy the query" }).click();
  const status = s.container.querySelector('[role="status"]');
  await vi.waitFor(() => expect(status?.textContent).toMatch(/copy it by hand/));
  expect(window.getSelection()?.toString()).toBe("SELECT 2;");
  s.unmount();
});
