import { afterEach, expect, test, vi } from "vitest";
import { renderHook } from "vitest-browser-react";
import { StoryFrame } from "../../.storybook/StoryFrame";
import { operationAccepted, operationActing, operationSettled } from "../../mocks/fixtures/extra";
import type { LiveState } from "./live";
import { operationPollInterval, trackedOperations, useOperation, useRequestOperation } from "./operations";

afterEach(() => {
  vi.restoreAllMocks();
  for (const id of trackedOperations.get()) trackedOperations.dismiss(id);
});

test("a 202 tracks the operation and survives a reload through sessionStorage", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(JSON.stringify(operationAccepted), { status: 202, headers: { "content-type": "application/json" } }),
  );
  const { result } = await renderHook(() => useRequestOperation("team-a", "billing"), {
    wrapper: ({ children }) => <StoryFrame>{() => children}</StoryFrame>,
  });
  const accepted = await result.current({ kind: "restart", confirmation: "billing" });
  expect(accepted.operationId).toBe(operationAccepted.operationId);
  expect(trackedOperations.get()).toEqual([operationAccepted.operationId]);
  expect(JSON.parse(sessionStorage.getItem("pgtoolbox-console.operations") ?? "[]")).toEqual([
    operationAccepted.operationId,
  ]);
});

test("a refused request tracks nothing", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(JSON.stringify({ title: "Conflict", status: 409 }), {
      status: 409,
      headers: { "content-type": "application/problem+json" },
    }),
  );
  const { result } = await renderHook(() => useRequestOperation("team-a", "billing"), {
    wrapper: ({ children }) => <StoryFrame>{() => children}</StoryFrame>,
  });
  await expect(result.current({ kind: "restart", confirmation: "billing" })).rejects.toThrow("Conflict");
  expect(trackedOperations.get()).toEqual([]);
});

const live: LiveState = { status: "live", downSince: null, pollMinMs: 2_000, pollMaxMs: 10_000, reauth: null };

test("an operation in flight is polled while the stream is live, with a bounded backoff", () => {
  // The server publishes no operation topic: SSE alone would never refresh it.
  expect(operationPollInterval(undefined, 0, live)).toBe(2_000);
  expect(operationPollInterval({ phase: "accepted" }, 1, live)).toBe(2_000);
  expect(operationPollInterval({ phase: "acting" }, 2, live)).toBe(4_000);
  expect(operationPollInterval({ phase: "acting" }, 3, live)).toBe(8_000);
  expect(operationPollInterval({ phase: "acting" }, 4, live)).toBe(10_000);
  expect(operationPollInterval({ phase: "acting" }, 500, live)).toBe(10_000);
  expect(operationPollInterval({ phase: "requested" }, 1, { ...live, status: "connecting" })).toBe(2_000);
});

test("an outcome stops the polling, live or not", () => {
  for (const phase of ["settled", "failed", "timedOut", "notObservable"] as const) {
    expect(operationPollInterval({ phase }, 1, live)).toBe(false);
    expect(operationPollInterval({ phase }, 1, { ...live, status: "down", downSince: 0 })).toBe(false);
  }
});

test("while the stream is down the shorter of the backoff and the backstop applies", () => {
  const down: LiveState = { ...live, status: "down", downSince: Date.now() };
  expect(operationPollInterval({ phase: "acting" }, 4, down)).toBe(2_000);
});

test("the tracker reads the operation again until it settles, without any stream event", async () => {
  let calls = 0;
  vi.spyOn(globalThis, "fetch").mockImplementation(async () => {
    calls += 1;
    return new Response(JSON.stringify(calls === 1 ? operationActing : operationSettled), {
      status: 200,
      headers: { "content-type": "application/json" },
    });
  });
  const { result } = await renderHook(() => useOperation(operationActing.operationId), {
    wrapper: ({ children }) => <StoryFrame>{() => children}</StoryFrame>,
  });
  await vi.waitFor(() => expect(result.current.data?.phase).toBe("acting"));
  await vi.waitFor(() => expect(result.current.data?.phase).toBe("settled"), { timeout: 5_000 });
  const after = calls;
  await new Promise((r) => setTimeout(r, 2_500));
  expect(calls).toBe(after);
});
