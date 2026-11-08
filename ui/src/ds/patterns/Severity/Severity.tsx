import type { FindingCounts } from "../../../api/generated/model";
import { m } from "../../../paraglide/messages.js";
import { cn } from "../../cn";

/** The four finding severities, worst first (pkg/diagnose.Severity). */
export const SEVERITIES = ["critical", "high", "warning", "note"] as const;
type Severity = (typeof SEVERITIES)[number];

const GLYPH: Record<Severity, string> = { critical: "◆", high: "■", warning: "▲", note: "i" };

function severityWord(s: string): string {
  switch (s) {
    case "critical":
      return m.severity_critical();
    case "high":
      return m.severity_high();
    case "warning":
      return m.severity_warning();
    default:
      return m.severity_note();
  }
}

/**
 * SeverityChip — a finding's severity: glyph, word and the severity's own
 * colour (purple critical, red high, orange warning, blue note), the same
 * colours as the count squares. Not a health state: a finding is evidence
 * about the cluster, not a verdict on it.
 */
export function SeverityChip({ severity, className }: { severity: string; className?: string }) {
  const s: Severity = (SEVERITIES as readonly string[]).includes(severity) ? (severity as Severity) : "note";
  return (
    <span
      className={cn(
        "health inline-flex items-center gap-1.5 whitespace-nowrap rounded-pill px-1.5 font-code font-medium text-caption",
        className,
      )}
      data-sev={s}
    >
      <span className="health-glyph" aria-hidden="true">
        {GLYPH[s]}
      </span>
      <span>{severityWord(s)}</span>
    </span>
  );
}

/**
 * FindingSquares — a diagnostics run as five fixed count slots: critical,
 * high, warning, note, and checks that could not run. An empty slot keeps its
 * place so a column of them lines up; a run that has not happened is one grey
 * dash.
 */
export function FindingSquares({ counts }: { counts: FindingCounts }) {
  if (!counts.runAt)
    return (
      <span className="sev-squares" title={m.findings_not_run()}>
        <span className="sev-sq" data-sev="unrun" aria-hidden="true">
          –
        </span>
        <span className="sr-only">{m.findings_not_run()}</span>
      </span>
    );
  const slots: Array<[string, number, string]> = [
    ["critical", counts.critical, m.findings_critical({ count: counts.critical })],
    ["high", counts.high, m.findings_high({ count: counts.high })],
    ["warning", counts.warning, m.findings_warning({ count: counts.warning })],
    ["note", counts.note, m.findings_note({ count: counts.note })],
    ["unrun", counts.unavailable, m.findings_unavailable({ count: counts.unavailable })],
  ];
  return (
    <span
      className="sev-squares"
      role="img"
      aria-label={
        slots
          .filter(([, n]) => n > 0)
          .map(([, , t]) => t)
          .join(", ") || m.findings_none()
      }
    >
      {slots.map(([sev, n, t]) =>
        n > 0 ? (
          <span key={sev} className="sev-sq num" data-sev={sev} title={t}>
            {n}
          </span>
        ) : (
          <span key={sev} className="sev-sq" data-sev="empty" />
        ),
      )}
    </span>
  );
}
