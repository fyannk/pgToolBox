import { useEffect, useRef, useState } from "react";
import { m } from "../../../paraglide/messages.js";

type CopyState = "idle" | "copied" | "failed";

/**
 * CopyCode — a line of code the reader runs elsewhere (a Cluster spec change,
 * SQL for pgAdmin), shown in full and copyable. The console never runs it.
 * The result of the copy is announced; when the clipboard is refused (an
 * insecure origin, a denied permission) the text is selected instead so it
 * can be copied by hand.
 */
export function CopyCode({ code, label }: { code: string; label: string }) {
  const [state, setState] = useState<CopyState>("idle");
  const ref = useRef<HTMLElement>(null);
  useEffect(() => {
    if (state === "idle") return;
    const id = setTimeout(() => setState("idle"), 4_000);
    return () => clearTimeout(id);
  }, [state]);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(code);
      setState("copied");
    } catch {
      setState("failed");
      const el = ref.current;
      const sel = window.getSelection();
      if (el && sel) {
        const range = document.createRange();
        range.selectNodeContents(el);
        sel.removeAllRanges();
        sel.addRange(range);
      }
    }
  };
  return (
    <div className="flex min-w-0 flex-wrap items-start gap-2">
      <code
        ref={ref}
        className="min-w-0 flex-1 whitespace-pre-wrap break-all rounded-control border border-line bg-canvas px-2 py-1 font-code text-label"
      >
        {code}
      </code>
      <button
        type="button"
        onClick={() => void copy()}
        aria-label={m.copy_named({ what: label })}
        className="h-8 shrink-0 rounded-control border border-line-strong px-2 text-label hover:bg-canvas"
      >
        {m.copy()}
      </button>
      <span role="status" className="text-caption text-muted">
        {state === "copied" ? m.copy_done() : state === "failed" ? m.copy_failed() : ""}
      </span>
    </div>
  );
}
