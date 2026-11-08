// The session as the console serves it.

import { useGetSession } from "./generated/endpoints/session/session";
import type { SessionView } from "./generated/model";

export type Session = SessionView;

/** The session (same query and cache as useGetSession). */
export function useSession() {
  return useGetSession({ query: { retry: false, refetchInterval: false } });
}
