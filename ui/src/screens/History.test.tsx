import { useState } from "react";
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

/** Two Clusters' History behind one button, as navigating between them reuses the screen. */
function TwoClusters() {
  const [cluster, setCluster] = useState("billing");
  return (
    <>
      <button type="button" onClick={() => setCluster("orders")}>
        to orders
      </button>
      <HistoryScreen namespace="team-a" cluster={cluster} search={{ range: "1h" }} onSearch={vi.fn()} />
    </>
  );
}

test("another Cluster's History is loading, never the previous Cluster's events (#117)", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input instanceof Request ? input.url : input);
    // orders never answers: what shows meanwhile is the point.
    if (url.includes("/clusters/orders/")) return new Promise<Response>(() => {});
    return new Response(JSON.stringify(timeline), { status: 200, headers: { "content-type": "application/json" } });
  });
  const screen = await renderInFrame(<TwoClusters />);
  await expect.element(screen.getByText("Backup failed")).toBeVisible();
  await screen.getByRole("button", { name: "to orders" }).click();
  await vi.waitFor(() => expect(screen.container.querySelector('[aria-busy="true"]')).not.toBeNull());
  expect(screen.container.textContent).not.toContain("Backup failed");
});
