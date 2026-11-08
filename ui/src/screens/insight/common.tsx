// What every insight screen shares: the pack banner, the panel that has no
// data, the pgAdmin links, and the frame (banner, content, sources, limits).
import type { ReactNode } from "react";
import type { InsightPack, InsightView, PgAdminLink, Provenance as ProvenanceVM } from "../../api/generated/model";
import { formatCount, formatNumber } from "../../ds/format";
import { HEALTH_GLYPH } from "../../ds/health";
import { Reported } from "../../ds/NotReported";
import { BlindSpots } from "../../ds/patterns/BlindSpots/BlindSpots";
import { CopyCode } from "../../ds/patterns/CopyCode/CopyCode";
import { Provenance } from "../../ds/patterns/Provenance/Provenance";
import { StatusChip } from "../../ds/patterns/StatusChip/StatusChip";
import { m } from "../../paraglide/messages.js";
import { SourcesPanel } from "../Overview";
import { Panel } from "../Panel";
import { isHttps } from "./logic";

export function packStateWord(state: string): string {
  switch (state) {
    case "enabled":
      return m.insight_pack_enabled();
    case "notEnabled":
      return m.insight_pack_not_enabled();
    case "outdated":
      return m.insight_pack_outdated();
    case "newer":
      return m.insight_pack_newer();
    case "noData":
      return m.insight_pack_no_data();
    case "metricsOff":
      return m.insight_pack_metrics_off();
    case "truncated":
      return m.insight_pack_truncated();
    case "unknown":
      return m.insight_pack_unknown();
    case "unsupported":
      return m.insight_pack_unsupported();
    default:
      return state;
  }
}

/** The spec change that opts a Cluster into the pack, copyable. */
export function SpecChange({ pack }: { pack: InsightPack }) {
  return (
    <div className="flex flex-col gap-1">
      <p className="text-label">{m.insight_spec_change_intro()}</p>
      <CopyCode code={pack.specChange} label={m.insight_spec_change()} />
    </div>
  );
}

/**
 * Said on every insight screen while the pack is not "enabled", or enabled
 * with a fault Go raised its health for (failing queries, a cut response):
 * what the panels below lack and why, in Go's words. Nothing is shown when the
 * pack is enabled and healthy.
 */
function PackBanner({ pack }: { pack: InsightPack }) {
  if (pack.state === "enabled" && pack.health === "ok") return null;
  return (
    <div
      role="status"
      className="health flex flex-col gap-2 rounded-panel p-3 text-label"
      data-health={pack.health}
      data-pack-state={pack.state}
    >
      <p className="flex flex-wrap items-center gap-2">
        <StatusChip health={pack.health} label={packStateWord(pack.state)} size="sm" />
        <span>{pack.text}</span>
      </p>
      {pack.state === "notEnabled" && <SpecChange pack={pack} />}
    </div>
  );
}

/**
 * A panel without data: Go's reason, never an empty table, a zero or "healthy"
 * (the honesty rule: missing data is never shown as nothing to report).
 */
export function Unavailable({ text }: { text?: string }) {
  return (
    <p className="health rounded-control px-3 py-2 text-label" data-health="unknown" data-unavailable="">
      <span aria-hidden="true" className="health-glyph mr-1.5">
        {HEALTH_GLYPH.unknown}
      </span>
      <span className="font-semibold">{m.insight_unavailable()}</span>
      {text ? `: ${text}` : ""}
    </p>
  );
}

/**
 * A panel showing less than the pack reports: the warning glyph and word, then
 * Go's reason, above the rows (which are real and still shown). Nothing when
 * the panel is complete.
 */
export function PartialNotice({ panel }: { panel: { partial: boolean; partialText?: string } }) {
  if (!panel.partial) return null;
  return (
    <p role="status" className="health rounded-control px-3 py-2 text-label" data-health="warning" data-partial="">
      <span aria-hidden="true" className="health-glyph mr-1.5">
        {HEALTH_GLYPH.warning}
      </span>
      <span className="font-semibold">{m.insight_partial()}</span>
      {": "}
      {panel.partialText ? `${panel.partialText} ` : ""}
      {m.insight_partial_rows()}
    </p>
  );
}

/** A big count, short, with the exact figure on hover; "not reported" when absent. */
export function Count({ value }: { value: number | null | undefined }) {
  return (
    <Reported value={value}>
      {(v) => (
        <span className="num" title={formatNumber(v)}>
          {formatCount(v)}
        </span>
      )}
    </Reported>
  );
}

/** Yes, no, or not reported (an absent boolean is never false). */
export function YesNo({ value }: { value: boolean | null | undefined }) {
  return <Reported value={value}>{(v) => (v ? m.yes() : m.no())}</Reported>;
}

/** The pgAdmins the reader may open for this Cluster, with what is done there instead of here. */
export function PgAdminLinks({
  links,
  unread,
  note,
}: {
  links: readonly PgAdminLink[] | null | undefined;
  /** The PgAdmin list could not be read: an empty list says nothing. */
  unread?: boolean | null;
  note: ReactNode;
}) {
  return (
    <Panel title={m.panel_pgadmin()}>
      <p className="text-label">{note}</p>
      {unread && (
        <p className="text-label text-muted" data-pgadmin="unread">
          {m.pgadmin_unread()}
        </p>
      )}
      {links && links.length > 0 ? (
        <ul className="flex flex-col gap-1 text-label">
          {links.map((l) => (
            <li key={l.name} className="flex flex-wrap items-center gap-2">
              <StatusChip health={l.health} size="sm" />
              {isHttps(l.url) ? (
                <a href={l.url} target="_blank" rel="noreferrer noopener" className="text-accent underline">
                  {m.pgadmin_open({ name: l.name })}
                </a>
              ) : (
                <span>
                  <code className="font-code">{l.name}</code> <span className="text-muted">({m.pgadmin_no_url()})</span>
                </span>
              )}
              {l.serverState && <span className="text-muted">{m.pgadmin_server({ state: l.serverState })}</span>}
            </li>
          ))}
        </ul>
      ) : (
        !unread && (
          <p className="text-label text-muted" data-pgadmin="none">
            {m.insight_no_pgadmin()}
          </p>
        )
      )}
    </Panel>
  );
}

/** Where a panel's numbers were read, when Go read them at all (an unavailable panel may carry none). */
export function PanelProvenance({ provenance }: { provenance: ProvenanceVM | null | undefined }) {
  return provenance?.origin ? <Provenance provenance={provenance} /> : null;
}

/** The frame of an insight screen: pack banner first, then the screen, then what it rests on and cannot see. */
export function InsightShell({
  data,
  children,
  banner = true,
}: {
  data: InsightView;
  children: ReactNode;
  /** The landing states the pack itself and leaves the banner out. */
  banner?: boolean;
}) {
  return (
    <div className="flex flex-col gap-4">
      {banner && <PackBanner pack={data.pack} />}
      {children}
      <SourcesPanel sources={data.sources} />
      <BlindSpots items={data.blindSpots} />
    </div>
  );
}
