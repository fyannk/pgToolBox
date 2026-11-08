import { m } from "../../../paraglide/messages.js";
import { cn } from "../../cn";
import { asHealth, HEALTH_GLYPH, healthWord } from "../../health";
import { formatRelative, formatUtc, useNow } from "../../time";

export type StatusChipProps = {
  /** The state, from the Go view model. Unknown values are drawn as unknown. */
  health: string;
  /** Staleness modifier: the state is the last good reading, not a current one. */
  stale?: boolean;
  /** When the reading was taken; shown with the staleness modifier. */
  observedAt?: string;
  /** Why it is stale, in the source's words. */
  staleReason?: string;
  /** chip: tinted pill; text: the word in its colour on the surrounding surface. */
  variant?: "chip" | "text";
  size?: "sm" | "md" | "lg";
  /** Overrides the word, e.g. a phase text; the state word stays for screen readers. */
  label?: string;
  className?: string;
  /** Hover text, e.g. the full label when className caps the width. */
  title?: string;
};

/**
 * StatusChip — glyph + word + hue for one of the seven health states, with
 * the staleness modifier. Colour is never the only signal. Drawn as
 * fyannk/design's status chip: Plex Mono, a 1 px outline in the state's
 * colour, the one pill shape the system allows.
 */
export function StatusChip({
  health,
  stale = false,
  observedAt,
  staleReason,
  variant = "chip",
  size = "md",
  label,
  className,
  title: hover,
}: StatusChipProps) {
  const h = asHealth(health);
  const now = useNow();
  const word = healthWord(h);
  const age = stale && observedAt ? formatRelative(observedAt, now, undefined, true) : null;
  const staleText = stale ? (age ? m.stale_as_of({ age }) : m.stale()) : null;
  const title =
    [hover, stale && staleReason, observedAt && formatUtc(observedAt)].filter(Boolean).join(" — ") || undefined;
  return (
    <span
      className={cn(
        "health inline-flex items-center gap-1.5 whitespace-nowrap rounded-pill font-code font-medium",
        variant === "text" && "health-text",
        size === "sm" && "px-1.5 text-caption",
        size === "md" && "px-2 py-0.5 text-label",
        size === "lg" && "px-3 py-1 text-title",
        className,
      )}
      data-health={h}
      data-stale={stale ? "" : undefined}
      title={title}
    >
      <span className="health-glyph shrink-0" aria-hidden="true">
        {HEALTH_GLYPH[h]}
      </span>
      {label ? (
        <>
          <span className="sr-only">{word}: </span>
          <span className="truncate">{label}</span>
        </>
      ) : (
        <span>{word}</span>
      )}
      {staleText && <span className="font-normal">· {staleText}</span>}
    </span>
  );
}
