import type { Provenance as ProvenanceVM } from "../../../api/generated/model";
import { m } from "../../../paraglide/messages.js";
import { Time } from "../../time";

export function originWord(origin: string): string {
  switch (origin) {
    case "operator":
      return m.origin_operator();
    case "kubernetes":
      return m.origin_kubernetes();
    case "instance":
      return m.origin_instance();
    case "console":
      return m.origin_console();
    case "evidence":
      return m.origin_evidence();
    default:
      return origin;
  }
}

/**
 * Provenance — who claims a fact, read from where, and when
 * ("every fact is shown with its origin and age").
 */
export function Provenance({ provenance }: { provenance: ProvenanceVM }) {
  return (
    <span className="inline-flex flex-wrap items-baseline gap-x-1.5 text-caption text-muted">
      <span className="rounded-chip border border-line px-1">{originWord(provenance.origin)}</span>
      {provenance.source && <code className="font-code">{provenance.source}</code>}
      {provenance.observedAt ? (
        <span>
          {m.observed()} <Time iso={provenance.observedAt} past />
        </span>
      ) : (
        <span className="italic">{m.never_read()}</span>
      )}
      {provenance.stale && (
        <span className="text-health-unknown-fg">
          {m.stale()}
          {provenance.staleReason ? `: ${provenance.staleReason}` : ""}
        </span>
      )}
    </span>
  );
}
