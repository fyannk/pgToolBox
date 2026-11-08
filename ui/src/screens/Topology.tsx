import { keepPreviousData } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { lazy, type ReactNode, Suspense, useCallback, useState } from "react";
import { useGetTopology } from "../api/generated/endpoints/clusters/clusters";
import type { GetTopologyFoldItem, Topology as TopologyVM } from "../api/generated/model";
import { resolveClusterLink } from "../app/clusterLink";
import { getFolded, setFolded } from "../app/prefs";
import { BlindSpots } from "../ds/patterns/BlindSpots/BlindSpots";
import { m } from "../paraglide/messages.js";
import { Panel } from "./Panel";
import { QueryState } from "./QueryState";

// Loaded on demand, so the overview never pays for React Flow.
const TopologyView = lazy(() =>
  import("../ds/patterns/TopologyView/TopologyView").then((mod) => ({ default: mod.TopologyView })),
);

/** The wiring drawing of one Cluster in its panel, box links resolved onto this build's routes. */
export function Topology({
  data,
  onToggleFold,
  toolbar,
}: {
  data: TopologyVM;
  onToggleFold?: (kind: string) => void;
  toolbar?: ReactNode;
}) {
  const params = { namespace: data.ref.namespace, cluster: data.ref.name };
  const navigate = useNavigate();
  const renderLink = (link: string, label: string) => {
    const target = resolveClusterLink(link, params);
    return target ? (
      <Link {...target} className="text-accent hover:underline">
        {label}
      </Link>
    ) : null;
  };
  return (
    <Panel title={m.panel_topology()}>
      <Suspense fallback={<p className="text-muted">{m.panel_topology()}…</p>}>
        <TopologyView
          topology={data}
          renderLink={renderLink}
          onToggleFold={onToggleFold}
          toolbar={toolbar}
          onOpen={(link) => {
            const target = resolveClusterLink(link, params);
            if (target) void navigate(target);
          }}
        />
      </Suspense>
      <BlindSpots items={data.blindSpots} panel={m.panel_topology()} />
    </Panel>
  );
}

/**
 * The wiring panel with its own query: loading, error and retry stay inside
 * the panel. The folded frames are the reader's, kept in the browser; a
 * fold asks Go for the drawing again, the previous one staying up meanwhile.
 */
export function TopologyPanel({ namespace, cluster }: { namespace: string; cluster: string }) {
  const [folded, setFoldedState] = useState(getFolded);
  const query = useGetTopology(
    namespace,
    cluster,
    folded.length > 0 ? { fold: folded as GetTopologyFoldItem[] } : undefined,
    {
      query: { placeholderData: keepPreviousData },
    },
  );
  const toggle = useCallback((kind: string) => {
    setFoldedState((now) => {
      const next = now.includes(kind) ? now.filter((k) => k !== kind) : [...now, kind].sort();
      setFolded(next);
      return next;
    });
  }, []);
  return (
    <QueryState query={query} what={m.panel_topology()}>
      {(data) => <Topology data={data} onToggleFold={toggle} />}
    </QueryState>
  );
}
