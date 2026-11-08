import "@xyflow/react/dist/style.css";
import {
  Background,
  BackgroundVariant,
  Controls,
  MiniMap,
  type Node,
  type NodeProps,
  ReactFlow,
  ReactFlowProvider,
  useReactFlow,
  ViewportPortal,
} from "@xyflow/react";
import { type ReactNode, useEffect, useId, useMemo, useState } from "react";
import type { TopoFrame, Topology, TopoNode } from "../../../api/generated/model";
import { m } from "../../../paraglide/messages.js";
import { asHealth } from "../../health";
import { boxName, FoldButtons, TopoBox, TopoCaption, TopoFrameShape, TopoMarkers, TopoWires } from "./parts";

/**
 * TopologyView — the wiring drawing: endpoints, servers, backup
 * path and storage, as Go laid them out, on React Flow (MIT). Every
 * coordinate, path and line of text comes from the view model; this draws,
 * it never lays out. Each box and frame is a React Flow node at Go's
 * coordinates and the wires are one SVG layer in the viewport, so the boxes
 * do not move: Go routed the wires for where it placed them. React Flow
 * brings the viewport (wheel and pinch zoom, drag to pan, fit), the controls
 * and the minimap (off until asked for).
 *
 * A server's health is its border, its glyph and, under the drawing, its
 * word. A click on a box with a link follows it (`onOpen`); the list under
 * the drawing offers the same links to the keyboard and to a reader who
 * never sees the drawing, with the view model's sentence. With
 * `onToggleFold` the foldable frames fold, from the toolbar or a click on
 * their heading: Go lays the drawing out again with each folded frame as one
 * summary box. Loaded on demand, so the overview does not pay for it.
 */

type FrameData = { frame: TopoFrame; onToggle?: (kind: string) => void };
type BoxData = { node: TopoNode; stale: boolean };
type FrameNode = Node<FrameData, "frame">;
type BoxNode = Node<BoxData, "box">;

/** A piece of the drawing cut to its own extent, drawn with the shared parts. */
function Cut({ x, y, w, h, children }: { x: number; y: number; w: number; h: number; children: ReactNode }) {
  return (
    <svg
      viewBox={`${x} ${y} ${w} ${h}`}
      width={w}
      height={h}
      className="topo block overflow-visible"
      aria-hidden="true"
    >
      {children}
    </svg>
  );
}

function FrameView({ data }: NodeProps<FrameNode>) {
  const f = data.frame;
  return (
    <Cut x={f.x} y={f.y} w={f.w} h={f.h}>
      <TopoFrameShape frame={f} onToggle={data.onToggle} />
    </Cut>
  );
}

function BoxView({ data }: NodeProps<BoxNode>) {
  const n = data.node;
  return (
    <div title={boxName(n)} data-link={n.link ? "" : undefined} className="topo-flow-box">
      <Cut x={n.x} y={n.y} w={n.w} h={n.h}>
        <TopoBox node={n} stale={data.stale} />
      </Cut>
    </div>
  );
}

const nodeTypes = { frame: FrameView, box: BoxView };

/** Fits the view once mounted: keyed by the layout's size, a new layout (a frame folded or unfolded) is fitted again. */
function FitOnLayout() {
  const { fitView } = useReactFlow();
  useEffect(() => {
    const id = requestAnimationFrame(() => fitView({ duration: 250, padding: 0.04 }));
    return () => cancelAnimationFrame(id);
  }, [fitView]);
  return null;
}

export function TopologyView({
  topology,
  stale = false,
  onOpen,
  renderLink,
  onToggleFold,
  toolbar,
}: {
  topology: Topology;
  stale?: boolean;
  /** Follows a box's cluster-relative link. */
  onOpen?: (link: string) => void;
  /** Turns a box's cluster-relative link into a link element for the list under the drawing; null when unresolved. */
  renderLink?: (link: string, label: string) => ReactNode | null;
  /** Folds or unfolds a frame (a foldable frame's kind); without it the drawing offers no folding. */
  onToggleFold?: (kind: string) => void;
  /** Extra controls at the end of the toolbar. */
  toolbar?: ReactNode;
}) {
  const markerId = useId().replace(/:/g, "");
  const descId = useId();
  const [minimap, setMinimap] = useState(false);
  const edges = topology.edges ?? [];
  const kinds = [...new Set(edges.map((e) => e.kind))];
  const nodes = useMemo<(FrameNode | BoxNode)[]>(
    () => [
      ...(topology.frames ?? []).map(
        (f): FrameNode => ({
          id: `frame-${f.kind}-${f.x}-${f.y}`,
          type: "frame",
          position: { x: f.x, y: f.y },
          width: f.w,
          height: f.h,
          data: { frame: f, onToggle: onToggleFold },
          draggable: false,
          selectable: false,
          focusable: false,
          zIndex: 0,
        }),
      ),
      ...(topology.nodes ?? []).map(
        (n): BoxNode => ({
          id: n.id,
          type: "box",
          position: { x: n.x, y: n.y },
          width: n.w,
          height: n.h,
          data: { node: n, stale },
          draggable: false,
          zIndex: 2,
          ariaLabel: boxName(n),
        }),
      ),
    ],
    [topology, stale, onToggleFold],
  );

  return (
    <figure className="flex flex-col gap-2">
      <div className="topo-zoom" role="toolbar" aria-label={m.topo_zoom()}>
        <span className="text-caption text-muted">{m.topo_zoom_hint()}</span>
        <FoldButtons topology={topology} onToggle={onToggleFold} />
        <button type="button" aria-pressed={minimap} onClick={() => setMinimap((v) => !v)}>
          {m.topo_minimap()}
        </button>
        {toolbar}
      </div>
      <section className="topo-region topo-flow" aria-label={topology.title} aria-describedby={descId}>
        <p id={descId} className="sr-only">
          {topology.aria}
        </p>
        <ReactFlowProvider>
          <ReactFlow
            nodes={nodes}
            edges={[]}
            nodeTypes={nodeTypes}
            nodesDraggable={false}
            nodesConnectable={false}
            elementsSelectable={false}
            minZoom={0.2}
            maxZoom={4}
            fitView
            fitViewOptions={{ padding: 0.04 }}
            // React Flow is MIT: the corner link is a request, not a condition.
            proOptions={{ hideAttribution: true }}
            onNodeClick={(_, node) => {
              const link = node.type === "box" ? (node.data as BoxData).node.link : undefined;
              if (link) onOpen?.(link);
            }}
          >
            <ViewportPortal>
              <svg
                width={topology.width}
                height={topology.height}
                viewBox={`0 0 ${topology.width} ${topology.height}`}
                className="topo topo-flow-wires"
                aria-hidden="true"
              >
                <TopoMarkers kinds={kinds} id={markerId} />
                <TopoWires topology={topology} markerId={markerId} />
              </svg>
            </ViewportPortal>
            <Background variant={BackgroundVariant.Dots} gap={24} size={1} />
            {minimap && (
              <MiniMap
                pannable
                zoomable
                nodeClassName={(n) =>
                  n.type === "box" ? `topo-mini health-${asHealth((n.data as BoxData).node.state)}` : "topo-mini-frame"
                }
              />
            )}
            <Controls showInteractive={false} />
            <FitOnLayout key={`${topology.width}x${topology.height}`} />
          </ReactFlow>
        </ReactFlowProvider>
      </section>
      <TopoCaption topology={topology} stale={stale} renderLink={renderLink} />
    </figure>
  );
}
