import type { ReactNode } from "react";
import type { TopoFrame, Topology, TopoNode } from "../../../api/generated/model";
import { m } from "../../../paraglide/messages.js";
import { asHealth, HEALTH_GLYPH, healthWord } from "../../health";
import { StatusChip } from "../StatusChip/StatusChip";

/**
 * The drawing's pieces, shared by the SVG drawing and the React Flow one:
 * both draw the same view model, the same way. Coordinates are Go's, in
 * drawing units.
 */

const KEY_W = 28;

/** One arrowhead per flow kind, coloured like its wire. */
export function TopoMarkers({ kinds, id }: { kinds: string[]; id: string }) {
  return (
    <defs>
      {kinds.map((k) => (
        <marker
          key={k}
          id={`${id}-${k}`}
          viewBox="0 0 10 10"
          refX="9"
          refY="5"
          markerWidth="7"
          markerHeight="7"
          orient="auto-start-reverse"
        >
          <path d="M0 0 L10 5 L0 10 z" className="topo-arrow" data-kind={k} />
        </marker>
      ))}
    </defs>
  );
}

/** The wires, their tee dots and their labels. */
export function TopoWires({ topology, markerId }: { topology: Topology; markerId: string }) {
  const edges = topology.edges ?? [];
  return (
    <>
      {edges.map((e) => (
        <path
          key={`${e.kind}-${e.path}`}
          d={e.path}
          className="topo-edge"
          data-kind={e.kind}
          markerEnd={`url(#${markerId}-${e.kind})`}
        />
      ))}
      {(topology.dots ?? []).map((d) => (
        <circle key={`${d.kind}-${d.x}-${d.y}`} cx={d.x} cy={d.y} r={3.5} className="topo-dot" data-kind={d.kind} />
      ))}
      {edges
        .filter((e) => e.label && e.labelX !== undefined && e.labelY !== undefined)
        .map((e) => (
          <text
            key={`label-${e.kind}-${e.path}`}
            x={e.labelX}
            y={e.labelY}
            textAnchor="middle"
            className="topo-elabel"
            transform={e.labelVertical ? `rotate(-90 ${e.labelX} ${e.labelY})` : undefined}
          >
            {e.label}
          </text>
        ))}
    </>
  );
}

/**
 * A dotted frame and its heading. With `onToggle`, a foldable frame's heading
 * carries ▾ (open) or ▸ (folded) and a click on it folds or unfolds the
 * frame; that is a pointer shortcut, the toolbar's buttons are the keyboard's
 * and the screen reader's.
 */
export function TopoFrameShape({ frame: f, onToggle }: { frame: TopoFrame; onToggle?: (kind: string) => void }) {
  const toggle = f.foldable && onToggle ? () => onToggle(f.kind) : undefined;
  return (
    <g className="topo-frame" data-kind={f.kind} data-folded={f.folded ? "" : undefined}>
      <rect x={f.x} y={f.y} width={f.w} height={f.h} rx={4} />
      {/* biome-ignore lint/a11y/noStaticElementInteractions: a pointer shortcut; the toolbar's fold buttons are the keyboard path. */}
      <text x={f.x + 10} y={f.y + 16} onClick={toggle} className={toggle ? "topo-fold-toggle" : undefined}>
        {toggle && <tspan>{f.folded ? "▸ " : "▾ "}</tspan>}
        {f.label}
        {f.note && <tspan className="topo-disk"> {f.note}</tspan>}
      </text>
    </g>
  );
}

/** A box's visible text and its state word: the name of its link (WCAG 2.5.3). */
export function boxName(n: TopoNode): string {
  const h = asHealth(n.state);
  const shown = n.lines && n.lines.length > 0 ? n.lines.map((l) => l.text) : [n.label, n.sub, n.disk].filter(Boolean);
  return [shown.join(" "), h !== "info" && healthWord(h)].filter(Boolean).join(" — ");
}

/** One box: its border and glyph carry its health, its lines are Go's. */
export function TopoBox({ node: n, stale = false }: { node: TopoNode; stale?: boolean }) {
  const h = asHealth(n.state);
  const health = h === "info" ? undefined : h;
  return (
    <g className="topo-node" data-kind={n.kind} data-health={health} data-stale={stale && health ? "" : undefined}>
      <rect x={n.x} y={n.y} width={n.w} height={n.h} rx={4} />
      {n.lines && n.lines.length > 0 ? (
        n.lines.map((l, i) => (
          // biome-ignore lint/suspicious/noArrayIndexKey: a node's lines are positional and fixed.
          <text key={i} x={l.x} y={l.y} className={`topo-${l.class === "note" ? "sub" : l.class}`}>
            {l.text}
          </text>
        ))
      ) : (
        // The fixed triple of label offsets.
        <>
          <text x={n.x + 14} y={n.y + 23} className="topo-label">
            {n.label}
          </text>
          {n.sub && (
            <text x={n.x + 14} y={n.y + 40} className="topo-sub">
              {n.sub}
            </text>
          )}
          {n.disk && (
            <text x={n.x + 14} y={n.y + 54} className="topo-disk">
              {n.disk}
            </text>
          )}
        </>
      )}
      {health && (
        <text x={n.x + n.w - 10} y={n.y + 18} textAnchor="end" className="topo-glyph">
          {HEALTH_GLYPH[health]}
        </text>
      )}
    </g>
  );
}

/**
 * The fold switches: one toggle button per foldable frame, pressed when the
 * frame is drawn folded.
 */
export function FoldButtons({ topology, onToggle }: { topology: Topology; onToggle?: (kind: string) => void }) {
  const foldable = (topology.frames ?? []).filter((f) => f.foldable);
  if (!onToggle || foldable.length === 0) return null;
  return (
    <fieldset className="topo-fold">
      <legend className="sr-only">{m.topo_fold()}</legend>
      <span aria-hidden="true" className="text-caption text-muted">
        {m.topo_fold()}
      </span>
      {foldable.map((f) => (
        <button key={f.kind} type="button" aria-pressed={!!f.folded} onClick={() => onToggle(f.kind)}>
          {f.label}
        </button>
      ))}
    </fieldset>
  );
}

/** The caption, the flow legend and the servers' states: the drawing's text alternative. */
export function TopoCaption({
  topology,
  stale,
  renderLink,
}: {
  topology: Topology;
  stale: boolean;
  renderLink?: (link: string, label: string) => ReactNode | null;
}) {
  // Boxes whose state is a verdict (infrastructure boxes are info).
  const servers = (topology.nodes ?? []).filter((n) => asHealth(n.state) !== "info");
  return (
    <figcaption className="flex flex-col gap-2 text-label">
      {topology.caption && <p className="text-muted">{topology.caption}</p>}
      {topology.legend && topology.legend.length > 0 && (
        <div className="flex flex-wrap items-center gap-x-4 gap-y-1">
          <span className="font-semibold">{m.topo_legend()}</span>
          {topology.legend.map((l) => (
            <span key={l.kind} className="flex items-center gap-1.5">
              <svg viewBox={`0 0 ${KEY_W} 8`} width={KEY_W} height={8} aria-hidden="true" className="topo topo-key">
                <line x1={0} y1={4} x2={KEY_W} y2={4} className="topo-edge" data-kind={l.kind} />
              </svg>
              {l.label}
            </span>
          ))}
        </div>
      )}
      {servers.length > 0 && (
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
          <span className="font-semibold">{m.topo_states()}</span>
          <ul className="flex flex-wrap gap-2">
            {servers.map((n) => (
              <li key={n.id} className="flex items-center gap-1">
                <span className="font-code">{(n.link && renderLink?.(n.link, n.label)) ?? n.label}</span>
                <StatusChip health={n.state} stale={stale} size="sm" />
              </li>
            ))}
          </ul>
        </div>
      )}
    </figcaption>
  );
}
