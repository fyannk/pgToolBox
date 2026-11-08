// The seven health states and how each is drawn. Presentation
// only: the state itself always comes from the Go view model.
import { m } from "../paraglide/messages.js";

export const HEALTH_STATES = ["ok", "progressing", "warning", "critical", "inactive", "unknown", "info"] as const;
export type Health = (typeof HEALTH_STATES)[number];

/**
 * Shape cue, so state never rests on hue alone (dashed border adds one for
 * unknown). ● ▲ ■ ○ are fyannk/design's status glyphs (ok, degraded, failing,
 * unknown); ◐ and i are this design system's for the states fyannk has no glyph for, and
 * inactive takes ‖ (paused: hibernated, fenced, scaled to zero, suspended)
 * because ○ now means unknown.
 */
export const HEALTH_GLYPH: Record<Health, string> = {
  ok: "●",
  progressing: "◐",
  warning: "▲",
  critical: "■",
  inactive: "‖",
  unknown: "○",
  info: "i",
};

/** A state this build does not know is drawn as unknown, never as ok (vocabulary.go). */
export function asHealth(value: string | null | undefined): Health {
  return (HEALTH_STATES as readonly string[]).includes(value ?? "") ? (value as Health) : "unknown";
}

export function healthWord(h: Health): string {
  switch (h) {
    case "ok":
      return m.health_ok();
    case "progressing":
      return m.health_progressing();
    case "warning":
      return m.health_warning();
    case "critical":
      return m.health_critical();
    case "inactive":
      return m.health_inactive();
    case "info":
      return m.health_info();
    default:
      return m.health_unknown();
  }
}

/** States that tint a whole row (a table row, a verdict line); ok, inactive and info leave it plain. */
export const TINTED_HEALTH: ReadonlySet<string> = new Set(["warning", "critical", "progressing", "unknown"]);
