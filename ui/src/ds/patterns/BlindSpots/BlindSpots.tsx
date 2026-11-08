import type { BlindSpot } from "../../../api/generated/model";
import { m } from "../../../paraglide/messages.js";
import { HEALTH_GLYPH } from "../../health";

/**
 * BlindSpots — what the console structurally cannot see on this panel, stated
 * next to it so an absence reads as a limit of the observer.
 */
export function BlindSpots({
  items,
  panel,
}: {
  items: readonly BlindSpot[] | null | undefined;
  /** The panel these limits belong to, when a screen shows more than one list (unique landmark names). */
  panel?: string;
}) {
  if (!items || items.length === 0) return null;
  return (
    <aside
      aria-label={panel ? m.blind_spots_of({ panel }) : m.blind_spots()}
      className="health rounded-panel p-3 text-label"
      data-health="info"
    >
      <p className="font-semibold">
        <span aria-hidden="true" className="health-glyph mr-1">
          {HEALTH_GLYPH.info}
        </span>
        {m.blind_spots()}
      </p>
      <ul className="mt-1 list-disc pl-5">
        {items.map((b) => (
          <li key={b.id} data-blind-spot={b.id}>
            {b.text}
          </li>
        ))}
      </ul>
    </aside>
  );
}
