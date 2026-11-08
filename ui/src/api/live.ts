// Live updates: one EventSource on /api/v1/stream (internal/console/api).
//
//   hello      {serverTime, heartbeatSeconds, pollMinSeconds, pollMaxSeconds}
//   invalidate {topic: clusters}                     → refetch the views across Clusters and
//                                                      every open view of any Cluster
//   invalidate {topic: cluster, namespace, name}     → refetch everything about that Cluster
//                                                      (INVALIDATION_SCOPES says which is which)
//   resync     {reason}                              → refetch everything
//   reauth     {expiresAt, refreshable}              → renew the session or ask to sign in
//
// While the stream is down every query polls instead (the backstop),
// from the server's pollMinSeconds doubling up to pollMaxSeconds,
// and the shell says so.

import type { QueryClient } from "@tanstack/react-query";
import { useSyncExternalStore } from "react";
import { getGetSessionQueryKey } from "./generated/endpoints/session/session";
import type { Hello, Invalidate, Reauth } from "./generated/model";

const STREAM_URL = "/api/v1/stream";

/** connecting: first attempt, not yet known; live: stream open; down: polling. */
export type LiveState = {
  status: "connecting" | "live" | "down";
  downSince: number | null;
  /** Backstop bounds from the server's hello; the plan's 2 s → 10 s until one arrives. */
  pollMinMs: number;
  pollMaxMs: number;
  /** The last reauth notice: the session ends at expiresAt unless renewed. */
  reauth: Reauth | null;
};

let state: LiveState = { status: "connecting", downSince: null, pollMinMs: 2_000, pollMaxMs: 10_000, reauth: null };
const listeners = new Set<() => void>();
function set(next: Partial<LiveState>) {
  state = { ...state, ...next };
  for (const l of listeners) l();
}

export const liveStore = {
  get: () => state,
  subscribe(l: () => void) {
    listeners.add(l);
    return () => {
      listeners.delete(l);
    };
  },
};

export function useLive(): LiveState {
  return useSyncExternalStore(liveStore.subscribe, liveStore.get, liveStore.get);
}

/** The polling interval while the stream is down (doubling from min to max); false otherwise. */
export function backstopInterval(s: LiveState, now = Date.now()): number | false {
  if (s.status !== "down") return false;
  const down = s.downSince === null ? 0 : now - s.downSince;
  let interval = s.pollMinMs;
  // One step per elapsed interval: min, 2·min, 4·min, … capped at max.
  for (let t = interval; t <= down && interval < s.pollMaxMs; t += interval) interval *= 2;
  return Math.min(interval, s.pollMaxMs);
}

/**
 * What makes each API path stale, one row per path of the OpenAPI document
 * (live.test.ts fails when a path is missing, so a new view cannot be
 * forgotten). Query keys start with the request path.
 *
 * - fleet: views across Clusters, refetched on `clusters` (the server's
 *   "refetch every cluster view", published with every batch of changes)
 *   and on any `cluster` event, whose row they show.
 * - cluster: one Cluster's views, refetched on `cluster` for that Cluster
 *   and on `clusters` (a change the server could not attribute to one
 *   Cluster). invalidateQueries refetches only the queries that are mounted
 *   and marks the others stale, so the cost is bounded by what is open.
 * - none: not Cluster state (session, setup, digests, operations, which
 *   poll until settled); refetched only on resync and reconnect.
 *
 * PgAccessRequests have no topic of their own: the server says to refetch
 * them on `clusters`.
 */
export const INVALIDATION_SCOPES = {
  "/api/v1/clusters": "fleet",
  "/api/v1/compare": "fleet",
  "/api/v1/insight/statements": "fleet",
  "/api/v1/access-requests": "fleet",
  "/api/v1/namespaces/{namespace}/access-requests": "fleet",
  "/api/v1/namespaces/{namespace}/access-requests/{name}": "fleet",
  "/api/v1/namespaces/{namespace}/access-requests/{name}/approve": "none",
  "/api/v1/namespaces/{namespace}/access-requests/{name}/deny": "none",
  "/api/v1/namespaces/{namespace}/access-requests/{name}/revoke": "none",
  "/api/v1/namespaces/{namespace}/clusters/{name}": "cluster",
  "/api/v1/namespaces/{namespace}/clusters/{name}/backups": "cluster",
  "/api/v1/namespaces/{namespace}/clusters/{name}/backups/restore-check": "cluster",
  "/api/v1/namespaces/{namespace}/clusters/{name}/database-roles": "cluster",
  "/api/v1/namespaces/{namespace}/clusters/{name}/diagnostics": "cluster",
  "/api/v1/namespaces/{namespace}/clusters/{name}/events": "cluster",
  "/api/v1/namespaces/{namespace}/clusters/{name}/history": "cluster",
  "/api/v1/namespaces/{namespace}/clusters/{name}/history/{seq}": "cluster",
  "/api/v1/namespaces/{namespace}/clusters/{name}/insight": "cluster",
  "/api/v1/namespaces/{namespace}/clusters/{name}/insight/blocking": "cluster",
  "/api/v1/namespaces/{namespace}/clusters/{name}/instances": "cluster",
  "/api/v1/namespaces/{namespace}/clusters/{name}/instances/{pod}/logs": "cluster",
  "/api/v1/namespaces/{namespace}/clusters/{name}/instances/{pod}/logs/stream": "cluster",
  "/api/v1/namespaces/{namespace}/clusters/{name}/metrics": "cluster",
  "/api/v1/namespaces/{namespace}/clusters/{name}/objects": "cluster",
  "/api/v1/namespaces/{namespace}/clusters/{name}/operations": "cluster",
  "/api/v1/namespaces/{namespace}/clusters/{name}/replication": "cluster",
  "/api/v1/namespaces/{namespace}/clusters/{name}/storage": "cluster",
  "/api/v1/namespaces/{namespace}/clusters/{name}/timeline": "cluster",
  "/api/v1/namespaces/{namespace}/clusters/{name}/topology": "cluster",
  "/api/v1/namespaces/{namespace}/clusters/{name}/triage": "cluster",
  "/api/v1/admin": "none",
  "/api/v1/assistant-setup": "none",
  "/api/v1/assistant-setup/agents": "none",
  "/api/v1/assistant-setup/skill": "none",
  "/api/v1/digests": "none",
  "/api/v1/digests/{id}": "none",
  "/api/v1/operations/{id}": "none",
  "/api/v1/session": "none",
  "/api/v1/session/grants": "none",
  "/api/v1/session/grants/{id}": "none",
  "/api/v1/stream": "none",
  "/api/v1/vocabulary": "none",
} as const satisfies Record<string, "fleet" | "cluster" | "none">;

type Scope = (typeof INVALIDATION_SCOPES)[keyof typeof INVALIDATION_SCOPES];

/** The table compiled once: each template's parameters match one path segment. */
const MATCHERS: ReadonlyArray<{ re: RegExp; scope: Scope }> = Object.entries(INVALIDATION_SCOPES).map(
  ([template, scope]) => ({
    re: new RegExp(`^${template.replace(/\{[^}]+\}/g, "[^/?]+")}$`),
    scope,
  }),
);

/** The scope of a query key's path, with its namespace and name when it belongs to one Cluster. */
export function scopeOf(path: string): { scope: Scope; namespace?: string; name?: string } | null {
  const bare = path.split("?")[0] ?? "";
  const hit = MATCHERS.find((m) => m.re.test(bare));
  if (!hit) return null;
  if (hit.scope !== "cluster") return { scope: hit.scope };
  const [, , , , namespace, , name] = bare.split("/");
  return { scope: "cluster", namespace, name };
}

/** A path segment against a Kubernetes name, raw or percent-encoded. */
const same = (segment: string | undefined, value: string) => segment === value || segment === encodeURIComponent(value);

/**
 * Whether a query is made stale by an invalidation. A path not in the table
 * (none of ours) is refetched rather than left stale.
 */
export function invalidates(inv: Invalidate, queryKey: readonly unknown[]): boolean {
  // A cluster event that names no Cluster cannot be scoped: everything.
  if (inv.topic === "cluster" && (!inv.namespace || !inv.name)) return true;
  const s = scopeOf(String(queryKey[0] ?? ""));
  if (!s) return true;
  if (s.scope === "none") return false;
  if (s.scope === "fleet") return true;
  // The server's "refetch every cluster view": only mounted ones refetch.
  if (inv.topic === "clusters") return true;
  if (!inv.namespace || !inv.name) return false;
  return same(s.namespace, inv.namespace) && same(s.name, inv.name);
}

/** How long invalidations gather before one refetch round (see batchInvalidations). */
export const INVALIDATION_WINDOW_MS = 500;

/**
 * Gathers invalidations for a short window and refetches once for all of
 * them, never cancelling a fetch already in flight. Sent one by one, a fleet
 * whose Clusters change a few times a second would restart the list fetch
 * before it could finish (a 1,600-Cluster list takes a few hundred
 * milliseconds to build), so the list would never settle and the server would
 * build it again and again. `null` stands for "everything".
 */
export function batchInvalidations(
  client: Pick<QueryClient, "invalidateQueries">,
  windowMs = INVALIDATION_WINDOW_MS,
): { add: (inv: Invalidate | null) => void; stop: () => void } {
  let pending: Invalidate[] | "all" = [];
  let timer: ReturnType<typeof setTimeout> | undefined;
  const flush = () => {
    timer = undefined;
    const batch = pending;
    pending = [];
    if (batch === "all") {
      void client.invalidateQueries(undefined, { cancelRefetch: false });
    } else if (batch.length > 0) {
      void client.invalidateQueries(
        { predicate: (q) => batch.some((inv) => invalidates(inv, q.queryKey)) },
        { cancelRefetch: false },
      );
    }
  };
  return {
    add(inv) {
      if (inv === null) pending = "all";
      else if (pending !== "all") pending.push(inv);
      timer ??= setTimeout(flush, windowMs);
    },
    stop() {
      clearTimeout(timer);
      timer = undefined;
      pending = [];
    },
  };
}

function parse<T>(ev: Event): T | null {
  try {
    return JSON.parse((ev as MessageEvent<string>).data) as T;
  } catch {
    return null;
  }
}

/**
 * Opens the stream and keeps it open. EventSource reconnects by itself after a
 * network error; when the server closes it for good (401, 5xx) this retries
 * with a bounded backoff. Returns a stop function.
 */
export function startLive(client: QueryClient): () => void {
  let es: EventSource | null = null;
  let retry: ReturnType<typeof setTimeout> | undefined;
  let attempts = 0;
  let stopped = false;
  const batch = batchInvalidations(client);

  const down = () => {
    if (state.status === "down") return;
    set({ status: "down", downSince: Date.now() });
    // Start the backstop now: each query re-reads its interval after a fetch.
    void client.invalidateQueries();
  };

  const open = () => {
    if (stopped) return;
    es = new EventSource(STREAM_URL);
    es.addEventListener("open", () => {
      attempts = 0;
      const wasDown = state.status === "down";
      set({ status: "live", downSince: null });
      // Whatever changed while the stream was down was not announced.
      if (wasDown) void client.invalidateQueries();
    });
    es.addEventListener("hello", (ev) => {
      const h = parse<Hello>(ev);
      if (h) set({ pollMinMs: h.pollMinSeconds * 1000, pollMaxMs: h.pollMaxSeconds * 1000 });
    });
    es.addEventListener("invalidate", (ev) => {
      batch.add(parse<Invalidate>(ev));
    });
    es.addEventListener("resync", () => batch.add(null));
    es.addEventListener("reauth", (ev) => {
      const r = parse<Reauth>(ev);
      set({ reauth: r });
      // A refreshable session renews when the session endpoint is read.
      if (r?.refreshable) void client.invalidateQueries({ queryKey: getGetSessionQueryKey() });
    });
    es.addEventListener("error", () => {
      down();
      if (es && es.readyState === EventSource.CLOSED) {
        es = null;
        attempts += 1;
        retry = setTimeout(open, Math.min(30_000, 1_000 * 2 ** attempts));
      }
    });
  };

  set({ status: "connecting", downSince: null });
  open();
  return () => {
    stopped = true;
    clearTimeout(retry);
    batch.stop();
    es?.close();
  };
}
