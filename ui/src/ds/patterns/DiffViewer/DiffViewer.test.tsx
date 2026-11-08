import { expect, test } from "vitest";
import { page } from "vitest/browser";
import { renderInFrame } from "../../../testing/render";
import { DiffViewer } from "./DiffViewer";

test("every changed line carries its sign and a spoken word, not only a tint", async () => {
  await renderInFrame(<DiffViewer hasBase entries={[]} before={"a\nb\nc\n"} after={"a\nB\nc\n"} />);
  const rows = [...document.querySelectorAll(".diff-line")].map((r) => [
    r.getAttribute("data-kind"),
    r.textContent?.replace(/\s+/g, " ").trim(),
  ]);
  expect(rows).toEqual([
    ["context", "a"],
    ["removed", "−removed line: b"],
    ["added", "+added line: B"],
    ["context", "c"],
  ]);
});

test("no base: nothing drawn, the reason said", async () => {
  await renderInFrame(<DiffViewer hasBase={false} entries={[]} />);
  await expect.element(page.getByText(/Nothing to compare with/)).toBeVisible();
  expect(document.querySelectorAll(".diff-line")).toHaveLength(0);
});

test("a structural entry without a manager says not reported", async () => {
  await renderInFrame(<DiffViewer hasBase entries={[{ path: "spec.x", op: "added", after: "1" }]} />);
  await expect.element(page.getByRole("cell", { name: "not reported" })).toBeVisible();
});

test("jsdiff's end-of-file marker is not shown as a line of the definition", async () => {
  await renderInFrame(<DiffViewer hasBase entries={[]} before={'{\n  "a": 1\n}'} after={'{\n  "a": 2\n}'} />);
  expect(document.body.textContent).not.toContain("No newline at end of file");
});
