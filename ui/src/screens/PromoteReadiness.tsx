import { useId } from "react";
import type { PromoteTarget, ReadinessFactKey } from "../api/generated/model";
import { Provenance } from "../ds/patterns/Provenance/Provenance";
import { StatusChip } from "../ds/patterns/StatusChip/StatusChip";
import { m } from "../paraglide/messages.js";

function factLabel(key: ReadinessFactKey | string): string {
  switch (key) {
    case "pod":
      return m.ops_readiness_pod();
    case "streaming":
      return m.ops_readiness_streaming();
    case "replayLag":
      return m.ops_readiness_replay_lag();
    case "syncState":
      return m.ops_readiness_sync_state();
    case "fencing":
      return m.ops_readiness_fencing();
    default:
      return key;
  }
}

/**
 * The facts a person weighs before confirming a switchover to `target`,
 * each as Go read and judged it, with its source and age. No score is
 * derived here; what CloudNativePG would refuse is the target's blockers.
 */
export function PromoteReadiness({ target }: { target: PromoteTarget }) {
  const id = useId();
  const facts = target.readiness ?? [];
  const blockers = target.blockers ?? [];
  return (
    <section
      aria-labelledby={id}
      className="flex flex-col gap-2 rounded-control border border-line p-2"
      data-readiness={target.name}
    >
      <h3 id={id} className="font-semibold text-label">
        {m.ops_readiness_title({ instance: target.name })}
      </h3>
      {blockers.length > 0 && (
        <div className="health rounded-control px-2 py-1 text-label" data-health="critical">
          <p className="font-semibold">{m.ops_readiness_refused()}</p>
          <ul className="list-disc pl-5">
            {blockers.map((b) => (
              <li key={b.reason}>
                {b.text} <code className="font-code text-caption">{b.source}</code>
              </li>
            ))}
          </ul>
        </div>
      )}
      {facts.length === 0 ? (
        <p className="flex flex-wrap items-center gap-2 text-label">
          <StatusChip health="unknown" size="sm" />
          {m.ops_readiness_none()}
        </p>
      ) : (
        <dl className="grid grid-cols-[minmax(7rem,auto)_1fr] gap-x-3 gap-y-2 text-label">
          {facts.map((f) => (
            <div key={f.key} className="contents" data-fact={f.key} data-health={f.health}>
              <dt className="text-muted">{factLabel(f.key)}</dt>
              <dd className="m-0 flex min-w-0 flex-col gap-0.5">
                <span className="flex flex-wrap items-center gap-2">
                  <StatusChip
                    health={f.health}
                    size="sm"
                    stale={f.provenance.stale}
                    observedAt={f.provenance.observedAt}
                    staleReason={f.provenance.staleReason}
                  />
                  <span className="break-words">{f.text}</span>
                </span>
                <Provenance provenance={f.provenance} />
              </dd>
            </div>
          ))}
        </dl>
      )}
      <p className="text-caption text-muted">{m.ops_readiness_note()}</p>
    </section>
  );
}
