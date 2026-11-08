import type { Operation } from "../../../api/generated/model";
import { m } from "../../../paraglide/messages.js";
import { formatUtc, Time, useNow } from "../../time";
import { StatusChip } from "../StatusChip/StatusChip";

type Phase = Operation["phase"];
const TERMINAL: readonly Phase[] = ["settled", "failed", "timedOut", "notObservable"];
const LADDER: readonly Phase[] = ["requested", "accepted", "acting"];

/**
 * How an operation is drawn: Go's Operation.health. Past the server's
 * deadline with no outcome the tracker's own clock says "timed out", drawn
 * as unknown like the server's timedOut.
 */
export function operationHealth(operation: Operation, overdue = false): string {
  return overdue ? "unknown" : (operation.health ?? "unknown");
}

export function phaseWord(phase: Phase): string {
  switch (phase) {
    case "requested":
      return m.op_requested();
    case "accepted":
      return m.op_accepted();
    case "acting":
      return m.op_acting();
    case "settled":
      return m.op_settled();
    case "notObservable":
      return m.op_not_observable();
    case "failed":
      return m.op_failed();
    default:
      return m.op_timed_out();
  }
}

/**
 * OperationTracker — follows one operation from Requested to an outcome.
 * Each step names the field that showed it. Past the server's
 * deadline with no outcome it says so honestly: no outcome reported does not
 * mean failed. The current phase is announced politely.
 */
export function OperationTracker({ operation, live }: { operation: Operation; live: boolean }) {
  const now = useNow(1_000);
  const terminal = TERMINAL.includes(operation.phase);
  const overdue = !terminal && Date.parse(operation.deadline) < now;
  const shown: Phase = overdue ? "timedOut" : operation.phase;
  const steps = operation.steps ?? [];
  const target = `${operation.target.namespace}/${operation.target.name}`;
  return (
    <section
      className="flex flex-col gap-3 rounded-panel border border-line bg-surface p-4"
      aria-label={m.op_tracker_label({ kind: operation.kind, target })}
    >
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="font-semibold">{m.op_title({ kind: operation.kind, target })}</h3>
        <div role="status" aria-live="polite" aria-atomic="true">
          <StatusChip health={operationHealth(operation, overdue)} label={phaseWord(shown)} />
        </div>
      </div>
      <ol className="flex flex-col gap-2">
        {LADDER.map((p) => {
          const step = steps.find((s) => s.phase === p);
          return (
            <li key={p} className="flex flex-wrap items-baseline gap-x-2 text-label">
              <span aria-hidden="true" className={step ? "text-health-ok-icon" : "text-faint"}>
                {step ? "●" : "○"}
              </span>
              <span className={step ? "font-medium" : "text-muted"}>{phaseWord(p)}</span>
              {step ? (
                <>
                  <Time iso={step.at} className="text-muted" />
                  <span>{step.text}</span>
                  <code className="font-code text-caption text-muted">{step.source}</code>
                </>
              ) : (
                <span className="text-muted">{m.op_not_yet()}</span>
              )}
            </li>
          );
        })}
        {terminal &&
          steps
            .filter((s) => s.phase === operation.phase)
            .map((s) => (
              <li key={`${s.phase}-${s.at}`} className="flex flex-wrap items-baseline gap-x-2 text-label">
                <span aria-hidden="true">■</span>
                <span className="font-medium">{phaseWord(s.phase)}</span>
                <Time iso={s.at} className="text-muted" />
                <span>{s.text}</span>
                <code className="font-code text-caption text-muted">{s.source}</code>
              </li>
            ))}
      </ol>
      {(overdue || operation.phase === "timedOut") && (
        <p className="health rounded-control p-2 text-label" data-health="unknown">
          {m.op_timed_out_note({ deadline: formatUtc(operation.deadline) })}
        </p>
      )}
      {operation.phase === "notObservable" && (
        <p className="health rounded-control p-2 text-label" data-health="info">
          {m.op_not_observable_note()}
        </p>
      )}
      {operation.blocked && !terminal && (
        <p className="health rounded-control p-2 text-label" data-health="warning">
          {m.op_blocked()}
        </p>
      )}
      {!live && !terminal && <p className="text-caption text-muted">{m.op_polling()}</p>}
    </section>
  );
}
