import { useEffect, useRef, useState } from "react";
import { ApiError } from "../api/fetcher";
import { getClusterSummary } from "../api/generated/endpoints/clusters/clusters";
import { m } from "../paraglide/messages.js";

type State =
  | { kind: "idle" }
  | { kind: "loading" }
  | { kind: "copied" }
  | { kind: "manual"; text: string }
  | { kind: "failed"; error: unknown };

/**
 * CopySummary — "Copy as Markdown" on a Cluster's overview: the verdict, each
 * finding with its source and age, the sources that are not live, the console
 * page and the time, rendered by the console (GET .../summary) so the words are
 * the screen's and every value from the cluster is escaped there. The summary is
 * read on the click, never ahead of it. When the clipboard is refused (an
 * insecure origin, a denied permission, a browser that wants the copy inside
 * the click) the text is shown selected so it can be copied by hand.
 */
export function CopySummary({
  namespace,
  cluster,
  load = getClusterSummary,
}: {
  namespace: string;
  cluster: string;
  /** Reads the summary; the generated client by default, a stand-in in tests. */
  load?: (namespace: string, cluster: string) => Promise<string>;
}) {
  const [state, setState] = useState<State>({ kind: "idle" });
  const area = useRef<HTMLTextAreaElement>(null);
  useEffect(() => {
    if (state.kind === "manual") area.current?.select();
    if (state.kind !== "copied") return;
    const id = setTimeout(() => setState({ kind: "idle" }), 4_000);
    return () => clearTimeout(id);
  }, [state]);

  const copy = async () => {
    setState({ kind: "loading" });
    let text: string;
    try {
      text = await load(namespace, cluster);
    } catch (error) {
      setState({ kind: "failed", error });
      return;
    }
    try {
      await navigator.clipboard.writeText(text);
      setState({ kind: "copied" });
    } catch {
      setState({ kind: "manual", text });
    }
  };

  const api = state.kind === "failed" && state.error instanceof ApiError ? state.error : null;
  return (
    <div className="flex min-w-0 flex-col gap-1">
      <div className="flex flex-wrap items-center gap-2">
        <button
          type="button"
          onClick={() => void copy()}
          disabled={state.kind === "loading"}
          aria-busy={state.kind === "loading"}
          aria-label={m.incident_summary_copy_label({ cluster })}
          className="h-8 shrink-0 rounded-control border border-line-strong px-2 text-label hover:bg-canvas disabled:opacity-60"
        >
          {m.incident_summary_copy()}
        </button>
        <span role="status" className="text-caption text-muted">
          {state.kind === "loading"
            ? m.incident_summary_copying()
            : state.kind === "copied"
              ? m.incident_summary_copied()
              : state.kind === "manual"
                ? m.incident_summary_manual()
                : ""}
        </span>
      </div>
      {state.kind === "failed" && (
        <p role="alert" className="health rounded-control px-2 py-1 text-label" data-health="critical">
          {m.incident_summary_failed()}{" "}
          {api?.problem?.detail ?? (state.error instanceof Error ? state.error.message : String(state.error))}
          {api?.requestId && <> {m.request_id({ id: api.requestId })}</>}
        </p>
      )}
      {state.kind === "manual" && (
        <textarea
          ref={area}
          readOnly
          rows={8}
          value={state.text}
          aria-label={m.incident_summary_text({ cluster })}
          className="w-full rounded-control border border-line bg-canvas px-2 py-1 font-code text-label"
        />
      )}
    </div>
  );
}
