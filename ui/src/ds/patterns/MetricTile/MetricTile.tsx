import type { Provenance as ProvenanceVM } from "../../../api/generated/model";
import { m } from "../../../paraglide/messages.js";
import { formatUnit } from "../../format";
import { Provenance } from "../Provenance/Provenance";

/**
 * MetricTile — one current reading ("now"), as Go served it: the value in its
 * unit, the instance it is from and the reading's provenance. A value the
 * instance did not report is said, never 0.
 */
export function MetricTile({
  label,
  value,
  unit,
  instance,
  provenance,
}: {
  label: string;
  value?: number | null;
  unit?: string;
  instance?: string;
  provenance: ProvenanceVM;
}) {
  const reported = value !== undefined && value !== null;
  return (
    <div
      className="flex min-w-40 flex-col gap-1 rounded-panel border border-line bg-surface p-3"
      data-stale={provenance.stale ? "" : undefined}
    >
      <span className="text-label text-muted">
        {label}
        {instance && <span className="font-code"> · {instance}</span>}
      </span>
      <span className={reported ? "num font-semibold text-heading" : "text-label text-muted"}>
        {reported ? formatUnit(value, unit) : m.not_reported()}
      </span>
      <Provenance provenance={provenance} />
    </div>
  );
}
