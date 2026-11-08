import { memo, useId, useLayoutEffect, useRef, useState } from "react";
import { m } from "../../../paraglide/messages.js";
import { formatLogTime, formatUtcOrRaw } from "../../time";

/** One log line as the console serves it: Go has already parsed and bounded it. */
export type LogLine = {
  /** Monotonic within one stream; the React key. */
  seq: number;
  /** When the container wrote it, when the source says. */
  time?: string;
  /** The text, already redacted by the server. */
  text: string;
  /** Set on a gap marker: lines were missed here, for this reason. Never joined across. */
  gap?: string;
};

export type LogStreamState = "connecting" | "live" | "ended" | "down";

/** Lines per chunk; must equal --log-chunk-lines in styles.css. */
const CHUNK = 200;

const Chunk = memo(
  function Chunk({ lines }: { lines: LogLine[] }) {
    return (
      <div className="log-chunk">
        {lines.map((l) =>
          l.gap !== undefined ? (
            <div
              key={l.seq}
              className="health my-1 border-x-0 px-3 py-0.5 font-sans text-caption"
              data-gap=""
              data-health="warning"
            >
              <span aria-hidden="true" className="health-glyph mr-1">
                ▲
              </span>
              {m.log_gap({ reason: l.gap })}
            </div>
          ) : (
            <div key={l.seq} className="flex gap-3 px-3 hover:bg-surface-2">
              {l.time && (
                <time dateTime={l.time} title={formatUtcOrRaw(l.time)} className="shrink-0 text-muted">
                  {formatLogTime(l.time)}
                </time>
              )}
              <span className="min-w-0 whitespace-pre-wrap break-all">{l.text}</span>
            </div>
          ),
        )}
      </div>
    );
  },
  // A full chunk never changes; only the last one grows.
  (a, b) => a.lines.length === b.lines.length && a.lines[0]?.seq === b.lines[0]?.seq,
);

function stateWord(s: LogStreamState): string {
  switch (s) {
    case "connecting":
      return m.log_state_connecting();
    case "live":
      return m.log_state_live();
    case "ended":
      return m.log_state_ended();
    default:
      return m.log_state_down();
  }
}

/**
 * LogViewer — a container log as it streams. Follows the end
 * until the reader scrolls up or presses Pause; new lines then wait behind an
 * "N new lines" button instead of moving the text being read. Search filters
 * the lines shown. The stream state is said in words, including a gap after a
 * reconnection. Lines are drawn in chunks the browser skips while off screen.
 */
export function LogViewer({
  title,
  lines,
  state,
  dropped = false,
  maxLines,
  endReason,
}: {
  /** Accessible name of the log region, e.g. "Log of billing-1 (postgres)". */
  title: string;
  lines: LogLine[];
  state: LogStreamState;
  /** True when older lines were dropped to stay within maxLines. */
  dropped?: boolean;
  /** Why the stream ended, in the server's words. */
  endReason?: string;
  maxLines?: number;
}) {
  const [follow, setFollow] = useState(true);
  const [query, setQuery] = useState("");
  const [seenSeq, setSeenSeq] = useState(() => lines.at(-1)?.seq ?? -1);
  const scroller = useRef<HTMLDivElement>(null);
  const content = useRef<HTMLDivElement>(null);
  const lastTop = useRef(0);
  const following = useRef(follow);
  following.current = follow;
  const searchId = useId();

  const q = query.trim().toLowerCase();
  // A search keeps the gap markers: a filtered view must not hide that lines are missing.
  const shown = q ? lines.filter((l) => l.gap !== undefined || l.text.toLowerCase().includes(q)) : lines;
  const lastSeq = lines.at(-1)?.seq ?? -1;
  const unseen = follow ? 0 : lines.filter((l) => l.seq > seenSeq).length;

  // Following: keep the end in view and everything counted as seen.
  useLayoutEffect(() => {
    if (!follow) return;
    const el = scroller.current;
    if (el) {
      el.scrollTop = el.scrollHeight;
      lastTop.current = el.scrollTop;
    }
    setSeenSeq(lastSeq);
  }, [follow, lastSeq]);

  // Chunks take their real height when first laid out (wrapped lines are
  // taller than the estimate); while following, stay pinned to the end.
  useLayoutEffect(() => {
    const el = scroller.current;
    const inner = content.current;
    if (!el || !inner || typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(() => {
      if (!following.current) return;
      el.scrollTop = el.scrollHeight;
      // Our own scroll: its scroll event must not read as the reader's.
      lastTop.current = el.scrollTop;
    });
    ro.observe(inner);
    return () => ro.disconnect();
  }, []);

  // The reader scrolling up pauses; content growing or a layout shift never
  // does. Following resumes only on Follow or the "N new lines" button, so a
  // reader who scrolled to the end to read is never yanked along.
  const onScroll = () => {
    const el = scroller.current;
    if (!el) return;
    const delta = el.scrollTop - lastTop.current;
    lastTop.current = el.scrollTop;
    const atEnd = el.scrollHeight - el.scrollTop - el.clientHeight < 8;
    if (delta < -2 && !atEnd && follow) setFollow(false);
  };

  const chunks: LogLine[][] = [];
  for (let i = 0; i < shown.length; i += CHUNK) chunks.push(shown.slice(i, i + CHUNK));

  return (
    <section aria-label={title} className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex flex-wrap items-center gap-2 text-label">
          <button
            type="button"
            aria-pressed={follow}
            onClick={() => setFollow((f) => !f)}
            className="h-8 rounded-control border border-line-strong px-3 font-medium hover:bg-surface-2"
          >
            {follow ? m.log_pause() : m.log_follow()}
          </button>
          <span role="status" className="text-muted">
            {stateWord(state)} · {follow ? m.log_following() : m.log_paused()}
          </span>
        </div>
        <div className="flex items-center gap-2">
          <label htmlFor={searchId} className="text-label">
            {m.log_search()}
          </label>
          <input
            id={searchId}
            type="search"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            className="h-8 w-48 rounded-control border border-line-strong bg-canvas px-2 font-code text-label"
          />
        </div>
      </div>
      {state === "ended" && endReason && (
        <p className="health rounded-control px-3 py-1.5 text-label" data-health="inactive">
          {m.log_ended_reason({ reason: endReason })}
        </p>
      )}
      {state === "down" && (
        <p className="health rounded-control px-3 py-1.5 text-label" data-health="warning">
          {m.log_state_down()}
        </p>
      )}
      {dropped && maxLines !== undefined && (
        <p className="text-caption text-muted">{m.log_dropped({ max: String(maxLines) })}</p>
      )}
      {q && (
        <p aria-live="polite" className="text-caption text-muted">
          {m.log_matches({ shown: String(shown.length), total: String(lines.length) })}
        </p>
      )}
      <div className="relative">
        <div
          ref={scroller}
          role="log"
          aria-live="off"
          aria-label={title}
          // biome-ignore lint/a11y/noNoninteractiveTabindex: the log scrolls; keyboard readers need to reach it.
          tabIndex={0}
          onScroll={onScroll}
          className="h-[60dvh] overflow-y-auto overscroll-contain [overflow-anchor:none] rounded-panel border border-line bg-canvas py-2 font-code text-label"
        >
          <div ref={content}>
            {shown.length === 0 ? (
              <p className="px-3 text-muted">{q ? m.log_no_match({ query }) : m.log_empty()}</p>
            ) : (
              chunks.map((c) => <Chunk key={c[0]?.seq} lines={c} />)
            )}
          </div>
        </div>
        {unseen > 0 && (
          <button
            type="button"
            onClick={() => {
              setQuery("");
              setFollow(true);
            }}
            className="absolute right-4 bottom-3 rounded-control border border-accent bg-surface px-3 py-1 font-semibold text-accent text-label shadow-overlay"
          >
            {unseen === 1 ? m.log_new_one() : m.log_new_many({ count: String(unseen) })}
          </button>
        )}
      </div>
      <p className="sr-only" aria-live="polite">
        {unseen > 0 ? (unseen === 1 ? m.log_new_one() : m.log_new_many({ count: String(unseen) })) : ""}
      </p>
    </section>
  );
}
