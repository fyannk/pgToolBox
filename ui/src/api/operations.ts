// Operation trackers survive navigation and reload: the ids of
// accepted operations are kept in sessionStorage and looked up again on the
// server by id. Storage may be unavailable (private mode); trackers then live
// for the page only.

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useSyncExternalStore } from "react";
import { getGetOperationQueryKey, getOperation, requestOperation } from "./generated/endpoints/operations/operations";
import type { Operation, OperationAccepted, OperationRequest } from "./generated/model";
import { backstopInterval, type LiveState, liveStore } from "./live";

const KEY = "pgtoolbox-console.operations";

function load(): string[] {
  try {
    const raw = sessionStorage.getItem(KEY);
    const parsed: unknown = raw ? JSON.parse(raw) : [];
    return Array.isArray(parsed) ? parsed.filter((x): x is string => typeof x === "string") : [];
  } catch {
    return [];
  }
}

let ids = load();
const listeners = new Set<() => void>();
function save(next: string[]) {
  ids = next;
  try {
    sessionStorage.setItem(KEY, JSON.stringify(next));
  } catch {
    // Page-lifetime tracking only.
  }
  for (const l of listeners) l();
}

export const trackedOperations = {
  get: () => ids,
  subscribe(l: () => void) {
    listeners.add(l);
    return () => {
      listeners.delete(l);
    };
  },
  track(id: string) {
    if (!ids.includes(id)) save([...ids, id]);
  },
  dismiss(id: string) {
    save(ids.filter((x) => x !== id));
  },
};

export function useTrackedOperations(): string[] {
  return useSyncExternalStore(trackedOperations.subscribe, trackedOperations.get, trackedOperations.get);
}

const TERMINAL: ReadonlyArray<Operation["phase"]> = ["settled", "failed", "timedOut", "notObservable"];

/**
 * How often an operation is read again. An outcome is final: never again.
 * In flight it is polled whatever the stream says, because the server
 * publishes no topic for operations (only `clusters` and `cluster`), and a
 * missed or coalesced cluster event must not freeze the tracker until its
 * deadline turns it into a false "timed out". The interval starts at the
 * server's pollMinSeconds and doubles per read up to pollMaxSeconds; while the
 * stream is down the shared backstop applies when it is shorter.
 */
export function operationPollInterval(
  operation: Pick<Operation, "phase"> | undefined,
  reads: number,
  s: LiveState,
): number | false {
  if (operation && TERMINAL.includes(operation.phase)) return false;
  const steps = Math.max(0, reads - 1);
  const own = Math.min(s.pollMinMs * 2 ** Math.min(steps, 30), s.pollMaxMs);
  const backstop = backstopInterval(s);
  return backstop === false ? own : Math.min(own, backstop);
}

/** One tracked operation: polled until it reports an outcome (live or not), also refetched on invalidation. */
export function useOperation(id: string) {
  return useQuery({
    queryKey: getGetOperationQueryKey(id),
    queryFn: ({ signal }) => getOperation(id, { signal }),
    refetchInterval: (q) => operationPollInterval(q.state.data, q.state.dataUpdateCount, liveStore.get()),
    // An outcome never changes; until then every read may be out of date.
    staleTime: (q) => (q.state.data && TERMINAL.includes(q.state.data.phase) ? Number.POSITIVE_INFINITY : 0),
  });
}

/** Sends the request (CSRF header from apiFetch); on 202 the operation is tracked. Throws ApiError otherwise. */
export function useRequestOperation(namespace: string, name: string) {
  const client = useQueryClient();
  return useCallback(
    async (body: OperationRequest): Promise<OperationAccepted> => {
      const accepted: OperationAccepted = await requestOperation(namespace, name, body);
      trackedOperations.track(accepted.operationId);
      await client.invalidateQueries();
      return accepted;
    },
    [client, namespace, name],
  );
}
