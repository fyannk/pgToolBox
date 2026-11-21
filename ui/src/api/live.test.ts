import { QueryClient, QueryObserver } from "@tanstack/react-query";
import { expect, test, vi } from "vitest";
import spec from "../../../internal/console/api/openapi.yaml?raw";
import { getGetAccessRequestQueryKey, getListAccessRequestsQueryKey } from "./generated/endpoints/access/access";
import {
  getCompareClustersQueryKey,
  getGetClusterQueryKey,
  getGetFleetStatementsQueryKey,
  getGetMetricsQueryKey,
  getListClustersQueryKey,
  getListEventsQueryKey,
} from "./generated/endpoints/clusters/clusters";
import { getGetOperationQueryKey } from "./generated/endpoints/operations/operations";
import { getGetSessionQueryKey } from "./generated/endpoints/session/session";
import {
  backstopInterval,
  batchInvalidations,
  INVALIDATION_SCOPES,
  INVALIDATION_WINDOW_MS,
  invalidates,
  type LiveState,
  liveStore,
  scopeOf,
  startLive,
} from "./live";

const base: LiveState = { status: "down", downSince: 0, pollMinMs: 2_000, pollMaxMs: 10_000, reauth: null };

test("no polling while live or still connecting (connecting is bounded: see the watchdog tests)", () => {
  expect(backstopInterval({ ...base, status: "live", downSince: null })).toBe(false);
  expect(backstopInterval({ ...base, status: "connecting", downSince: null })).toBe(false);
});

/** An EventSource that delivers only what the test emits. */
class FakeSource {
  static readonly CLOSED = 2;
  static all: FakeSource[] = [];
  readyState = 0;
  closed = false;
  private handlers = new Map<string, ((ev: Event) => void)[]>();
  constructor(readonly url: string) {
    FakeSource.all.push(this);
  }
  addEventListener(type: string, h: (ev: Event) => void) {
    this.handlers.set(type, [...(this.handlers.get(type) ?? []), h]);
  }
  emit(type: string, data?: unknown) {
    const ev = data === undefined ? new Event(type) : new MessageEvent(type, { data: JSON.stringify(data) });
    for (const h of this.handlers.get(type) ?? []) h(ev);
  }
  close() {
    this.closed = true;
    this.readyState = FakeSource.CLOSED;
  }
}

function withFakeStream(run: (src: () => FakeSource) => Promise<void>) {
  return async () => {
    vi.useFakeTimers();
    vi.stubGlobal("EventSource", FakeSource);
    const client = new QueryClient();
    const stop = startLive(client);
    try {
      await run(() => {
        const s = FakeSource.all.at(-1);
        if (!s) throw new Error("no stream opened");
        return s;
      });
    } finally {
      stop();
      client.clear();
      FakeSource.all = [];
      vi.unstubAllGlobals();
      vi.useRealTimers();
    }
  };
}

test(
  "a first open that never answers is down after twice the default heartbeat, and is reopened",
  withFakeStream(async (src) => {
    const first = src();
    expect(liveStore.get().status).toBe("connecting");
    await vi.advanceTimersByTimeAsync(29_999);
    expect(liveStore.get().status).toBe("connecting");
    await vi.advanceTimersByTimeAsync(1);
    expect(liveStore.get().status).toBe("down");
    expect(first.closed).toBe(true);
    // Reopened after the same backoff as a closed stream, not at once.
    expect(FakeSource.all).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(2_000);
    expect(FakeSource.all).toHaveLength(2);
  }),
);

test(
  "heartbeats keep the stream live; silence for twice the hello's interval marks it down and reopens it",
  withFakeStream(async (src) => {
    const first = src();
    first.emit("open");
    first.emit("hello", {
      serverTime: "2026-11-20T00:00:00Z",
      heartbeatSeconds: 5,
      pollMinSeconds: 2,
      pollMaxSeconds: 10,
    });
    for (let i = 0; i < 5; i++) {
      await vi.advanceTimersByTimeAsync(9_000);
      first.emit("heartbeat", { serverTime: "2026-11-20T00:00:00Z" });
    }
    expect(liveStore.get().status).toBe("live");
    expect(FakeSource.all).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(10_000);
    expect(liveStore.get().status).toBe("down");
    expect(first.closed).toBe(true);
    expect(FakeSource.all).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(2_000);
    expect(FakeSource.all).toHaveLength(2);
    // Headers alone are not a working stream; its hello brings the state back.
    src().emit("open");
    expect(liveStore.get().status).toBe("down");
    src().emit("hello", HELLO);
    expect(liveStore.get().status).toBe("live");
  }),
);

const HELLO = { serverTime: "2026-11-20T00:00:00Z", heartbeatSeconds: 15, pollMinSeconds: 2, pollMaxSeconds: 10 };

/** Closes the current stream for good, as the browser does on a 401 or a 5xx. */
function failHard(s: FakeSource) {
  s.close();
  s.emit("error");
}

test(
  "a stream closed for good is retried after 2, 4, 8, 16, 30, 30 s; a hello resets the backoff",
  withFakeStream(async (src) => {
    const cases = [2_000, 4_000, 8_000, 16_000, 30_000, 30_000];
    for (const [i, delay] of cases.entries()) {
      failHard(src());
      expect(liveStore.get().status, `attempt ${i + 1}`).toBe("down");
      await vi.advanceTimersByTimeAsync(delay - 1);
      expect(FakeSource.all, `attempt ${i + 1} before ${delay} ms`).toHaveLength(i + 1);
      await vi.advanceTimersByTimeAsync(1);
      expect(FakeSource.all, `attempt ${i + 1} at ${delay} ms`).toHaveLength(i + 2);
    }
    // An open without a hello does not reset it ...
    src().emit("open");
    failHard(src());
    await vi.advanceTimersByTimeAsync(29_999);
    expect(FakeSource.all).toHaveLength(cases.length + 1);
    await vi.advanceTimersByTimeAsync(1);
    expect(FakeSource.all).toHaveLength(cases.length + 2);
    // ... a hello does: the next failure retries after 2 s again.
    src().emit("open");
    src().emit("hello", HELLO);
    expect(liveStore.get().status).toBe("live");
    failHard(src());
    await vi.advanceTimersByTimeAsync(2_000);
    expect(FakeSource.all).toHaveLength(cases.length + 3);
  }),
);

test(
  "an error the browser recovers from by itself (not closed) schedules no retry of its own",
  withFakeStream(async (src) => {
    src().emit("hello", HELLO);
    src().emit("error");
    expect(liveStore.get().status).toBe("down");
    await vi.advanceTimersByTimeAsync(5_000);
    expect(FakeSource.all).toHaveLength(1);
    src().emit("hello", HELLO);
    expect(liveStore.get().status).toBe("live");
  }),
);

test(
  "a resync refetches everything after the batching window",
  withFakeStream(async (src) => {
    src().emit("hello", HELLO);
    const spy = vi.spyOn(QueryClient.prototype, "invalidateQueries");
    try {
      src().emit("resync", { reason: "watch restarted" });
      expect(spy).not.toHaveBeenCalled();
      await vi.advanceTimersByTimeAsync(INVALIDATION_WINDOW_MS);
      expect(spy).toHaveBeenCalledWith(undefined, { cancelRefetch: false });
    } finally {
      spy.mockRestore();
    }
  }),
);

test("the backstop doubles from the server's minimum to its maximum while the stream stays down", () => {
  expect(backstopInterval(base, 500)).toBe(2_000);
  expect(backstopInterval(base, 2_500)).toBe(4_000);
  expect(backstopInterval(base, 6_500)).toBe(8_000);
  expect(backstopInterval(base, 60_000)).toBe(10_000);
  expect(backstopInterval({ ...base, pollMinMs: 5_000, pollMaxMs: 30_000 }, 1_000)).toBe(5_000);
});

test("a cluster invalidation covers every view of that Cluster and the list, nothing else", () => {
  const inv = { topic: "cluster" as const, namespace: "team-a", name: "billing" };
  expect(invalidates(inv, getGetClusterQueryKey("team-a", "billing"))).toBe(true);
  expect(invalidates(inv, getListEventsQueryKey("team-a", "billing"))).toBe(true);
  expect(invalidates(inv, getListClustersQueryKey())).toBe(true);
  expect(invalidates(inv, getGetClusterQueryKey("team-a", "orders"))).toBe(false);
  expect(invalidates(inv, getGetClusterQueryKey("team-a", "billing-2"))).toBe(false);
});

test("a clusters invalidation covers every view across Clusters and every Cluster's views", () => {
  const inv = { topic: "clusters" as const };
  expect(invalidates(inv, getListClustersQueryKey())).toBe(true);
  expect(invalidates(inv, getCompareClustersQueryKey())).toBe(true);
  expect(invalidates(inv, getGetFleetStatementsQueryKey())).toBe(true);
  expect(invalidates(inv, getGetClusterQueryKey("team-a", "billing"))).toBe(true);
  expect(invalidates(inv, getGetMetricsQueryKey("team-b", "orders"))).toBe(true);
  // PgAccessRequests ride on the clusters topic (no topic of their own).
  expect(invalidates(inv, getListAccessRequestsQueryKey())).toBe(true);
  expect(invalidates(inv, getGetAccessRequestQueryKey("team-a", "viewer-q8w3d"))).toBe(true);
  // Not Cluster state.
  expect(invalidates(inv, getGetSessionQueryKey())).toBe(false);
  expect(invalidates(inv, getGetOperationQueryKey("op-1"))).toBe(false);
});

test("a cluster invalidation also refreshes the views across Clusters, which show its row", () => {
  const inv = { topic: "cluster" as const, namespace: "team-a", name: "billing" };
  expect(invalidates(inv, getCompareClustersQueryKey())).toBe(true);
  expect(invalidates(inv, getGetFleetStatementsQueryKey())).toBe(true);
  expect(invalidates(inv, getGetMetricsQueryKey("team-a", "billing"))).toBe(true);
  expect(invalidates(inv, getGetMetricsQueryKey("team-b", "billing"))).toBe(false);
  expect(invalidates(inv, getGetSessionQueryKey())).toBe(false);
});

test("every path of the OpenAPI document has an invalidation scope, and no other", () => {
  const paths = [...spec.matchAll(/^ {2}(\/api\/[^\s:]+):\s*$/gm)].map((m) => m[1]).sort();
  expect(paths.length).toBeGreaterThan(30);
  expect(Object.keys(INVALIDATION_SCOPES).sort()).toEqual(paths);
  for (const p of paths) {
    const concrete = (p ?? "").replace(/\{[^}]+\}/g, "x");
    expect(scopeOf(concrete)?.scope, p).toBe(INVALIDATION_SCOPES[p as keyof typeof INVALIDATION_SCOPES]);
  }
});

test("invalidations within a window refetch once, without cancelling a fetch in flight", () => {
  vi.useFakeTimers();
  try {
    const invalidateQueries = vi.fn(async () => {});
    const batch = batchInvalidations({ invalidateQueries });
    batch.add({ topic: "cluster", namespace: "team-a", name: "orders" });
    batch.add({ topic: "cluster", namespace: "team-a", name: "billing" });
    expect(invalidateQueries).not.toHaveBeenCalled();
    vi.advanceTimersByTime(INVALIDATION_WINDOW_MS);
    expect(invalidateQueries).toHaveBeenCalledTimes(1);
    const [filters, options] = invalidateQueries.mock.calls[0] as unknown as [
      { predicate: (q: { queryKey: readonly unknown[] }) => boolean },
      { cancelRefetch: boolean },
    ];
    expect(options).toEqual({ cancelRefetch: false });
    expect(filters.predicate({ queryKey: getGetClusterQueryKey("team-a", "billing") })).toBe(true);
    expect(filters.predicate({ queryKey: getGetClusterQueryKey("team-b", "billing") })).toBe(false);

    // A resync in the window widens the round to everything; stop drops what is pending.
    batch.add({ topic: "clusters" });
    batch.add(null);
    vi.advanceTimersByTime(INVALIDATION_WINDOW_MS);
    expect(invalidateQueries).toHaveBeenLastCalledWith(undefined, { cancelRefetch: false });
    batch.add({ topic: "clusters" });
    batch.stop();
    vi.advanceTimersByTime(INVALIDATION_WINDOW_MS);
    expect(invalidateQueries).toHaveBeenCalledTimes(2);
  } finally {
    vi.useRealTimers();
  }
});

test("a clusters event refetches a mounted Cluster view, and only marks an unmounted one stale", async () => {
  vi.useFakeTimers();
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const open = vi.fn(async () => "open");
  const closed = vi.fn(async () => "closed");
  const session = vi.fn(async () => "session");
  try {
    const openKey = getGetMetricsQueryKey("team-a", "billing");
    const closedKey = getGetMetricsQueryKey("team-b", "orders");
    const observer = new QueryObserver(client, { queryKey: openKey, queryFn: open });
    const unsubscribe = observer.subscribe(() => {});
    await client.fetchQuery({ queryKey: closedKey, queryFn: closed });
    await client.fetchQuery({ queryKey: getGetSessionQueryKey(), queryFn: session });
    await vi.waitFor(() => expect(open).toHaveBeenCalledTimes(1));

    const batch = batchInvalidations(client);
    batch.add({ topic: "clusters" });
    await vi.advanceTimersByTimeAsync(INVALIDATION_WINDOW_MS);
    await vi.waitFor(() => expect(open).toHaveBeenCalledTimes(2));
    expect(closed).toHaveBeenCalledTimes(1);
    expect(client.getQueryState(closedKey)?.isInvalidated).toBe(true);
    expect(session).toHaveBeenCalledTimes(1);
    expect(client.getQueryState(getGetSessionQueryKey())?.isInvalidated).toBe(false);
    unsubscribe();
  } finally {
    client.clear();
    vi.useRealTimers();
  }
});
