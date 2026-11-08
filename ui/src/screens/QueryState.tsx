import type { UseQueryResult } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { ApiError } from "../api/fetcher";
import { ErrorState, LoadingState } from "../ds/patterns/States/States";
import { m } from "../paraglide/messages.js";

/**
 * Renders a query's three states. A failed refresh over data already shown
 * keeps the data and says it could not be refreshed: the last good reading is
 * never replaced by an error, nor passed off as current.
 */
export function QueryState<T>({
  query,
  what,
  children,
}: {
  query: Pick<UseQueryResult<T, unknown>, "data" | "error" | "isPending" | "refetch">;
  what: string;
  children: (data: T) => ReactNode;
}) {
  if (query.isPending) return <LoadingState label={what} />;
  if (query.data === undefined)
    return <ErrorState error={query.error} what={what} onRetry={() => void query.refetch()} />;
  const refreshFailed = query.error && !(query.error instanceof ApiError && query.error.reauth);
  return (
    <>
      {refreshFailed && (
        <p role="status" className="health rounded-control px-3 py-1.5 text-label" data-health="unknown" data-stale="">
          {m.refresh_failed({ what })}
        </p>
      )}
      {children(query.data)}
    </>
  );
}
