import { m } from "../../../paraglide/messages.js";

/**
 * LiveBanner — says when live updates are paused and how often the page is
 * re-read meanwhile (the polling backstop). Silent when live.
 */
export function LiveBanner({ connected, intervalMs }: { connected: boolean; intervalMs: number | false }) {
  if (connected) return null;
  return (
    <div role="status" className="health border-x-0 border-t-0 px-4 py-1.5 text-label" data-health="warning">
      <span aria-hidden="true" className="health-glyph mr-1.5">
        ▲
      </span>
      {intervalMs ? m.live_paused({ seconds: String(Math.round(intervalMs / 1000)) }) : m.live_paused_no_poll()}
    </div>
  );
}
