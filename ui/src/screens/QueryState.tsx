import type { UseQueryResult } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { ApiError } from "../api/fetcher";
import { ErrorState, LoadingState } from "../ds/patterns/States/States";
import { formatRelative, formatUtc, useNow } from "../ds/time";
import { m } from "../paraglide/messages.js";

/**
 * Renders a query's three states. A failed refresh over data already shown
 * keeps the data and says it could not be refreshed: the last good reading is
 * never replaced by an error, nor passed off as current, and the line says how
 * old that reading is.
 */
export function QueryState<T>({
  query,
  what,
  children,
}: {
  query: Pick<UseQueryResult<T, unknown>, "data" | "dataUpdatedAt" | "error" | "isPending" | "refetch">;
  what: string;
  children: (data: T) => ReactNode;
}) {
  if (query.isPending) return <LoadingState label={what} />;
  if (query.data === undefined)
    return <ErrorState error={query.error} what={what} onRetry={() => void query.refetch()} />;
  const refreshFailed = query.error && !(query.error instanceof ApiError && query.error.reauth);
  return (
    <>
      {refreshFailed && <RefreshFailed what={what} updatedAt={query.dataUpdatedAt} />}
      {children(query.data)}
    </>
  );
}

/** Its own component, so the ticking clock re-renders this line, not the screen. */
function RefreshFailed({ what, updatedAt }: { what: string; updatedAt: number }) {
  const now = useNow(5_000);
  const iso = new Date(updatedAt).toISOString();
  return (
    <p
      role="status"
      className="health rounded-control px-3 py-1.5 text-label"
      data-health="unknown"
      data-stale=""
      title={formatUtc(iso)}
    >
      {m.refresh_failed({ what, age: formatRelative(iso, now, undefined, true) })}
    </p>
  );
}
