// The live log of one container: GET .../instances/{pod}/logs/stream (SSE).
//
//   line {at?, text}     → appended
//   gap  {at, reason}    → a marker; lines are never joined across it
//   end  {reason}        → the stream is over (container stopped, session
//                          ended or revoked, pods/log withdrawn, refusal);
//                          shown with its reason, never reopened
//   heartbeat {serverTime} → every 15 s: the stream is alive, the log quiet
//
// The stream opens with the last 100 lines. When it breaks, or delivers no
// event for twice the heartbeat (half-open, or a proxy that buffers), the
// hook marks a gap (lines around it may be missing or repeated) and reopens
// after a backoff (2 s doubling to 10 s), giving up after five failed
// attempts in a row; only an event, not an open alone, resets the count.
// Nothing is deduplicated by guess.

import { useEffect, useState } from "react";
import type { LogLine, LogStreamState } from "../ds/patterns/LogViewer/LogViewer";
import { m } from "../paraglide/messages.js";

/** Lines kept in the browser; older ones are dropped and the view says so. */
export const MAX_LOG_LINES = 10_000;

export const logStreamPath = (namespace: string, cluster: string, pod: string, container: string) =>
  `/api/v1/namespaces/${encodeURIComponent(namespace)}/clusters/${encodeURIComponent(cluster)}/instances/${encodeURIComponent(pod)}/logs/stream?container=${encodeURIComponent(container)}`;

type Stream = { lines: LogLine[]; state: LogStreamState; dropped: boolean; endReason?: string };

/** Consecutive failures to (re)open before the hook stops trying and says so. */
const MAX_ATTEMPTS = 5;

/** The server's log heartbeat (logHeartbeat in internal/console/api); twice it with nothing is a dead stream. */
const HEARTBEAT_MS = 15_000;

function parse<T>(ev: Event): T | null {
  try {
    return JSON.parse((ev as MessageEvent<string>).data) as T;
  } catch {
    return null;
  }
}

export function useLogStream(path: string | null): Stream {
  const [s, setS] = useState<Stream>({ lines: [], state: "connecting", dropped: false });
  useEffect(() => {
    if (!path) return;
    let seq = 0;
    let es: EventSource | null = null;
    let retry: ReturnType<typeof setTimeout> | undefined;
    let watchdog: ReturnType<typeof setTimeout> | undefined;
    let backoff = 2_000;
    let closed = false;
    let failures = 0;
    setS({ lines: [], state: "connecting", dropped: false });
    const append = (l: Omit<LogLine, "seq">) =>
      setS((prev) => {
        const lines = [...prev.lines, { ...l, seq: seq++ }];
        const over = lines.length - MAX_LOG_LINES;
        return { ...prev, lines: over > 0 ? lines.slice(over) : lines, dropped: prev.dropped || over > 0 };
      });
    // Silence for twice the heartbeat goes the way of an error.
    const arm = () => {
      clearTimeout(watchdog);
      watchdog = setTimeout(fail, 2 * HEARTBEAT_MS);
    };
    // An event proves the stream works: the watchdog restarts, the backoff resets.
    const alive = () => {
      arm();
      backoff = 2_000;
      failures = 0;
      setS((prev) => (prev.state === "live" ? prev : { ...prev, state: "live" }));
    };
    const fail = () => {
      clearTimeout(watchdog);
      es?.close();
      if (closed) return;
      failures++;
      if (failures >= MAX_ATTEMPTS) {
        closed = true;
        setS((prev) => ({ ...prev, state: "ended", endReason: m.log_gave_up() }));
        return;
      }
      setS((prev) => ({ ...prev, state: "down" }));
      // One marker per outage: a gap directly after a gap says nothing more.
      setS((prev) => {
        const last = prev.lines.at(-1);
        if (!last || last.gap !== undefined) return prev;
        return { ...prev, lines: [...prev.lines, { seq: seq++, text: "", gap: m.log_gap_reconnect() }] };
      });
      retry = setTimeout(open, backoff);
      backoff = Math.min(backoff * 2, 10_000);
    };
    const open = () => {
      es = new EventSource(path);
      arm();
      // Headers alone prove nothing (a proxy may hold the body back): the
      // stream is live from its first event, a heartbeat at the latest.
      es.onopen = arm;
      es.addEventListener("line", (ev) => {
        alive();
        const d = parse<{ at?: string; text: string }>(ev);
        if (d) append({ time: d.at, text: d.text });
      });
      es.addEventListener("gap", (ev) => {
        alive();
        const d = parse<{ at?: string; reason: string }>(ev);
        if (d) append({ time: d.at, text: "", gap: d.reason });
      });
      es.addEventListener("heartbeat", alive);
      // The server ends the stream (container stopped, session ended or
      // revoked, pods/log withdrawn, or a refusal): final, never reopened.
      es.addEventListener("end", (ev) => {
        closed = true;
        clearTimeout(watchdog);
        es?.close();
        const d = parse<{ reason?: string }>(ev);
        setS((prev) => ({ ...prev, state: "ended", endReason: d?.reason }));
      });
      es.onerror = fail;
    };
    open();
    return () => {
      closed = true;
      clearTimeout(retry);
      clearTimeout(watchdog);
      es?.close();
    };
  }, [path]);
  return s;
}
