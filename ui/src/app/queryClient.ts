import { QueryClient } from "@tanstack/react-query";
import { ApiError } from "../api/fetcher";
import { backstopInterval, liveStore } from "../api/live";

/** Errors a retry cannot fix: the caller must act (sign in, ask for a role) or the object is gone. */
function retryable(error: unknown): boolean {
  return !(error instanceof ApiError && [401, 403, 404].includes(error.status));
}

export function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        // Live updates invalidate; while the stream is down every query polls.
        refetchInterval: () => backstopInterval(liveStore.get()),
        refetchIntervalInBackground: false,
        staleTime: 5_000,
        retry: (count, error) => retryable(error) && count < 2,
      },
    },
  });
}
