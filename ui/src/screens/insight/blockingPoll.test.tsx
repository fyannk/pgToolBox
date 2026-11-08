// The blocking screen re-reads the lock graph (refresh=true) every 2 s at
// most, and not while the tab is hidden. Its own file: fake timers and a
// stubbed fetch stay out of the other screen tests.
import { focusManager } from "@tanstack/react-query";
import { afterEach, expect, test, vi } from "vitest";
import { insightBlocking, insightEnabled } from "../../../mocks/fixtures";
import { renderInFrame } from "../../testing/render";
import { BlockingScreen } from "./Blocking";

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
  focusManager.setFocused(undefined);
});

test("the blocking screen reads the graph again every 2 s with refresh=true, and pauses while hidden", async () => {
  const calls: string[] = [];
  vi.spyOn(window, "fetch").mockImplementation(async (input) => {
    const url = String(input instanceof Request ? input.url : input);
    calls.push(url);
    const body = url.includes("/insight/blocking") ? insightBlocking : insightEnabled;
    return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
  });
  // Time still flows (the router and Testing Library need it); the steps below are what move the clock.
  vi.useFakeTimers({ shouldAdvanceTime: true, toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval"] });
  const s = await renderInFrame(<BlockingScreen namespace="team-a" cluster="orders" />);
  const reads = () => calls.filter((u) => u.includes("/insight/blocking")).length;
  await vi.waitFor(() => expect(reads()).toBe(1));
  expect(calls.find((u) => u.includes("/insight/blocking"))).toContain("refresh=true");
  await vi.advanceTimersByTimeAsync(1_800);
  expect(reads()).toBe(1);
  await vi.advanceTimersByTimeAsync(300);
  await vi.waitFor(() => expect(reads()).toBe(2));
  await vi.advanceTimersByTimeAsync(2_000);
  await vi.waitFor(() => expect(reads()).toBe(3));
  focusManager.setFocused(false);
  await vi.advanceTimersByTimeAsync(10_000);
  expect(reads()).toBe(3);
  s.unmount();
});
