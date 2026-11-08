import { afterEach, expect, test, vi } from "vitest";
import { timeline } from "../../mocks/fixtures";
import { renderInFrame } from "../testing/render";
import { HistoryScreen } from "./History";

afterEach(() => {
  vi.restoreAllMocks();
});

/** The `from` of every timeline request, in order. */
function timelineFroms(spy: { mock: { calls: unknown[][] } }): string[] {
  return spy.mock.calls
    .map(([input]: unknown[]) => new URL(String(input instanceof Request ? input.url : input), location.href))
    .filter((u: URL) => u.pathname.endsWith("/timeline"))
    .map((u: URL) => u.searchParams.get("from") ?? "");
}

test("choosing the preset already shown re-anchors its window at now", async () => {
  const spy = vi
    .spyOn(globalThis, "fetch")
    .mockImplementation(
      async () =>
        new Response(JSON.stringify(timeline), { status: 200, headers: { "content-type": "application/json" } }),
    );
  const screen = await renderInFrame(
    <HistoryScreen namespace="team-a" cluster="billing" search={{ range: "1h" }} onSearch={vi.fn()} />,
  );
  await expect.element(screen.getByText("Backup failed")).toBeVisible();
  const [first] = timelineFroms(spy);
  expect(first).toBeTruthy();
  await screen.getByRole("button", { name: "1 h" }).click();
  await vi.waitFor(() => expect(timelineFroms(spy).length).toBeGreaterThan(1));
  const froms = timelineFroms(spy);
  const last = froms[froms.length - 1] ?? "";
  // The fixture clock opened the view; the click anchors at the real now, later.
  expect(Date.parse(last)).toBeGreaterThan(Date.parse(first ?? ""));
});
