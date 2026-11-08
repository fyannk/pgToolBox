import { useLive } from "../api/live";
import { trackedOperations, useOperation, useTrackedOperations } from "../api/operations";
import { OperationTracker } from "../ds/patterns/OperationTracker/OperationTracker";
import { ErrorState, LoadingState } from "../ds/patterns/States/States";
import { m } from "../paraglide/messages.js";

function Tracked({ id }: { id: string }) {
  const query = useOperation(id);
  const live = useLive();
  return (
    <li className="flex flex-col gap-1">
      {query.data ? (
        <OperationTracker operation={query.data} live={live.status === "live"} />
      ) : query.isPending ? (
        <LoadingState label={m.op_lookup({ id })} rows={1} />
      ) : (
        <ErrorState error={query.error} what={m.op_lookup({ id })} onRetry={() => void query.refetch()} />
      )}
      <button
        type="button"
        onClick={() => trackedOperations.dismiss(id)}
        className="self-end rounded-control border border-line px-2 py-0.5 text-caption hover:bg-surface-2"
      >
        {m.op_dismiss()}
      </button>
    </li>
  );
}

/**
 * The operations this reader started in this browser session, looked up again
 * on the server after navigation or reload. Renders nothing when
 * there is none.
 */
export function TrackedOperations() {
  const ids = useTrackedOperations();
  if (ids.length === 0) return null;
  return (
    <section aria-label={m.op_region()} className="mx-auto w-full max-w-measure px-4 pt-4">
      <ul className="flex flex-col gap-3">
        {ids.map((id) => (
          <Tracked key={id} id={id} />
        ))}
      </ul>
    </section>
  );
}
