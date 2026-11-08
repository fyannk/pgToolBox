import type { ReactNode } from "react";
import { ApiError } from "../../../api/fetcher";
import { m } from "../../../paraglide/messages.js";

/** LoadingState — skeleton rows; the region is aria-busy and says what it waits for. */
export function LoadingState({ label, rows = 4 }: { label: string; rows?: number }) {
  return (
    <div aria-busy="true" aria-live="polite" className="flex flex-col gap-2">
      <p className="sr-only">{m.loading({ what: label })}</p>
      {Array.from({ length: rows }, (_, i) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: static skeleton rows have no identity.
        <div key={i} className="skeleton h-row rounded-control" />
      ))}
    </div>
  );
}

/** EmptyState — an empty list that was read successfully, said in words. */
export function EmptyState({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <div className="rounded-panel border border-line border-dashed bg-surface p-6 text-center">
      <p className="font-medium">{title}</p>
      {children && <div className="mt-1 text-label text-muted">{children}</div>}
    </div>
  );
}

/**
 * ErrorState — a request that failed: what failed, the request id for the
 * report and a retry. 403 says which role would allow it; 401 is handled by
 * the shell's re-authentication banner and only noted here.
 */
export function ErrorState({ error, what, onRetry }: { error: unknown; what: string; onRetry?: () => void }) {
  const api = error instanceof ApiError ? error : null;
  if (api?.forbidden) {
    return (
      <div role="alert" className="health rounded-panel p-4" data-health="inactive">
        <p className="font-semibold">{m.forbidden_title({ what })}</p>
        <p className="text-label">{api.problem?.detail ?? m.forbidden_detail()}</p>
      </div>
    );
  }
  return (
    <div role="alert" className="health flex flex-col gap-1 rounded-panel p-4" data-health="critical">
      <p className="font-semibold">{api?.reauth ? m.reauth_inline() : m.error_title({ what })}</p>
      {!api?.reauth && (
        <p className="text-label">{api?.problem?.detail ?? (error instanceof Error ? error.message : String(error))}</p>
      )}
      {api?.requestId && <p className="text-caption">{m.request_id({ id: api.requestId })}</p>}
      {onRetry && !api?.reauth && (
        <button
          type="button"
          onClick={onRetry}
          className="mt-1 self-start rounded-control border border-line-strong bg-surface px-3 py-1 text-fg text-label hover:bg-surface-2"
        >
          {m.retry()}
        </button>
      )}
    </div>
  );
}
