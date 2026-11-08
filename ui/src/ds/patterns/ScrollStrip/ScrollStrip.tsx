import { type ReactNode, useCallback, useEffect, useRef, useState } from "react";
import { m } from "../../../paraglide/messages.js";

/**
 * ScrollStrip — one row that never wraps (a tab bar). When it is wider than
 * the screen it scrolls sideways without a scrollbar: a ‹ or › button appears
 * at the end that has more, and the current item (aria-current) is brought
 * into view. The buttons are a pointer convenience only; keyboard focus moving
 * through the links scrolls the strip by itself, so they stay out of the tab
 * order and the accessibility tree.
 */
export function ScrollStrip({ children, className }: { children: ReactNode; className?: string }) {
  const ref = useRef<HTMLDivElement>(null);
  const [more, setMore] = useState({ start: false, end: false });

  const measure = useCallback(() => {
    const el = ref.current;
    if (!el) return;
    // 1 px of slack: fractional widths leave scrollLeft just short of the end.
    setMore({ start: el.scrollLeft > 1, end: el.scrollLeft + el.clientWidth < el.scrollWidth - 1 });
  }, []);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    el.querySelector<HTMLElement>("[aria-current='page']")?.scrollIntoView({ block: "nearest", inline: "nearest" });
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    for (const c of el.children) ro.observe(c);
    return () => ro.disconnect();
  }, [measure]);

  const step = (dir: 1 | -1) => {
    const el = ref.current;
    if (el) el.scrollBy({ left: dir * el.clientWidth * 0.7, behavior: "smooth" });
  };

  return (
    <div className={`scroll-strip relative ${className ?? ""}`}>
      <div ref={ref} onScroll={measure} className="scroll-strip-track overflow-x-auto overflow-y-hidden">
        {children}
      </div>
      {more.start && (
        <button
          type="button"
          tabIndex={-1}
          aria-hidden="true"
          title={m.scroll_back()}
          onClick={() => step(-1)}
          className="scroll-strip-btn left-0"
          data-end="start"
        >
          ‹
        </button>
      )}
      {more.end && (
        <button
          type="button"
          tabIndex={-1}
          aria-hidden="true"
          title={m.scroll_forward()}
          onClick={() => step(1)}
          className="scroll-strip-btn right-0"
          data-end="end"
        >
          ›
        </button>
      )}
    </div>
  );
}
