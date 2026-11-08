import type { MouseEvent as ReactMouseEvent } from "react";

/**
 * A row that leads where its first link goes: a click anywhere on it that is
 * not on another control, nor the end of a text selection, follows that link.
 * The link stays the one keyboard and screen-reader target; the row adds no
 * tab stop. Wire `onClick={followRowLink}` and `onAuxClick={followRowLink}`.
 */
function rowLinkOf(e: ReactMouseEvent<HTMLElement>): HTMLAnchorElement | null {
  const target = e.target as Element;
  if (target.closest("a, button, input, select, textarea, label, summary, [role='button']")) return null;
  if (window.getSelection()?.toString()) return null;
  return e.currentTarget.querySelector<HTMLAnchorElement>("a[href]");
}

export function followRowLink(e: ReactMouseEvent<HTMLElement>) {
  if (e.button !== 0 && e.button !== 1) return;
  const a = rowLinkOf(e);
  if (!a) return;
  // The middle button opens a new tab, as on the link.
  if (e.button === 1) {
    window.open(a.href, "_blank", "noopener");
    return;
  }
  // Replayed on the link with the same modifiers, so the router (or the
  // browser, for Ctrl/⌘/Shift) decides exactly as for a click on the link.
  a.dispatchEvent(
    new MouseEvent("click", {
      bubbles: true,
      cancelable: true,
      ctrlKey: e.ctrlKey,
      metaKey: e.metaKey,
      shiftKey: e.shiftKey,
      altKey: e.altKey,
    }),
  );
}
