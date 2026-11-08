import { useGetInsight, useGetInsightBlocking } from "../../api/generated/endpoints/clusters/clusters";
import type {
  InsightBlockingView,
  InsightBlockNode,
  InsightInstanceBlocking,
  InsightView,
} from "../../api/generated/model";
import { formatDuration, formatNumber } from "../../ds/format";
import { Reported } from "../../ds/NotReported";
import { Provenance } from "../../ds/patterns/Provenance/Provenance";
import { Time } from "../../ds/time";
import { m } from "../../paraglide/messages.js";
import { Panel } from "../Panel";
import { QueryState } from "../QueryState";
import { InsightShell, PartialNotice, PgAdminLinks, Unavailable } from "./common";
import { blockingPollMs } from "./logic";

const age = (label: (a: { duration: string }) => string, v: number | undefined) =>
  v === undefined ? null : <span>{label({ duration: formatDuration(v) })}</span>;

/** One backend of the lock graph: who it is, what it waits on, how long, and how many wait behind it. */
function NodeLine({ n }: { n: InsightBlockNode }) {
  if (n.prepared)
    return (
      <div className="flex flex-col gap-0.5 text-label">
        <p className="flex flex-wrap items-baseline gap-x-2">
          <span className="font-semibold">{m.insight_prepared_node()}</span>
          {n.database && (
            <span className="text-muted">
              <Reported value={n.user} />@{n.database}
            </span>
          )}
          {n.blocked > 0 && (
            <span className="font-semibold" data-blocked={n.blocked}>
              {m.insight_blocks({ count: formatNumber(n.blocked) })}
            </span>
          )}
        </p>
        <p className="text-caption">{m.insight_prepared_node_note()}</p>
        {n.xactAgeSeconds !== undefined && (
          <p className="text-caption text-muted">
            {m.insight_xact_open_for({ duration: formatDuration(n.xactAgeSeconds) })}
          </p>
        )}
      </div>
    );
  if (n.missing)
    return (
      <p className="text-label">
        <span className="font-code font-semibold">{m.insight_pid({ pid: String(n.pid) })}</span>{" "}
        <span className="text-muted italic">{m.insight_node_missing()}</span>
      </p>
    );
  if (n.cycle)
    return (
      <p className="text-label">
        <span className="font-code font-semibold">{m.insight_pid({ pid: String(n.pid) })}</span>{" "}
        <span className="font-semibold">{m.insight_node_cycle()}</span>
      </p>
    );
  const wait = n.waitEventType || n.waitEvent ? [n.waitEventType, n.waitEvent].filter(Boolean).join(": ") : undefined;
  return (
    <div className="flex flex-col gap-0.5 text-label">
      <p className="flex flex-wrap items-baseline gap-x-2">
        <span className="font-code font-semibold">{m.insight_pid({ pid: String(n.pid) })}</span>
        <span>
          <Reported value={n.application} />
        </span>
        <span className="text-muted">
          <Reported value={n.user} />@<Reported value={n.database} />
        </span>
        {n.blocked > 0 && (
          <span className="font-semibold" data-blocked={n.blocked}>
            {m.insight_blocks({ count: formatNumber(n.blocked) })}
          </span>
        )}
      </p>
      <p className="flex flex-wrap gap-x-3 text-caption text-muted">
        <span>
          {m.col_state()}: <Reported value={n.state} />
        </span>
        <span>
          {m.insight_wait_event()}: <Reported value={wait} />
        </span>
        {age(m.insight_waiting_for, n.waitSeconds)}
        {age(m.insight_xact_open_for, n.xactAgeSeconds)}
        {age(m.insight_in_state_for, n.stateAgeSeconds)}
        {n.queryId && (
          <span>
            {m.insight_queryid()} <code className="font-code">{n.queryId}</code>
          </span>
        )}
      </p>
    </div>
  );
}

/** A lock tree: each backend, then (indented) the backends waiting on it. Missing and cycle nodes say so. */
export function BlockTree({ nodes, root = false }: { nodes: readonly InsightBlockNode[]; root?: boolean }) {
  return (
    <ul className={root ? "flex flex-col gap-2" : "mt-1 ml-3 flex flex-col gap-1 border-line border-l-2 pl-3"}>
      {nodes.map((n, i) => (
        <li
          // A pid appears once among its siblings; a cycle repeats it further down, under another parent.
          // Prepared transactions all have pid 0: their place among the siblings tells them apart.
          key={n.prepared ? `prepared-${i}` : `${n.pid}${n.cycle ? "-cycle" : ""}${n.missing ? "-missing" : ""}`}
          data-pid={n.prepared ? undefined : n.pid}
          data-prepared={n.prepared || undefined}
          data-missing={n.missing || undefined}
          data-cycle={n.cycle || undefined}
          className={root ? "rounded-control border border-line p-2" : undefined}
        >
          <NodeLine n={n} />
          {(n.waiters ?? []).length > 0 && <BlockTree nodes={n.waiters ?? []} />}
        </li>
      ))}
    </ul>
  );
}

function InstanceGraph({ i }: { i: InsightInstanceBlocking }) {
  const roots = i.roots ?? [];
  return (
    <Panel title={i.primary ? m.insight_instance_primary({ instance: i.instance }) : i.instance}>
      <p className="text-label">
        {m.insight_waiting_count()}: <Reported value={i.waiting}>{(v) => formatNumber(v)}</Reported>
      </p>
      <p className="text-caption text-muted" data-data-age={i.dataAgeSeconds}>
        {m.insight_data_age({ duration: formatDuration(i.dataAgeSeconds) })}
      </p>
      {roots.length > 0 ? (
        <BlockTree nodes={roots} root />
      ) : (
        <p className="text-label text-muted">
          {i.waiting !== undefined && i.waiting > 0 ? m.insight_waiting_no_blocker() : m.insight_no_lock_wait()}
        </p>
      )}
      <Provenance provenance={i.provenance} />
    </Panel>
  );
}

/** The lock graph of each instance, root blockers first. Nothing here ends a backend: pgAdmin does. */
export function BlockingView({
  insight,
  blocking,
  pollMs,
}: {
  insight: InsightView;
  blocking: InsightBlockingView;
  pollMs: number;
}) {
  return (
    <InsightShell data={insight}>
      <div className="flex flex-col gap-1 text-label" data-poll-ms={pollMs}>
        <p>
          {m.insight_graph_read()} <Reported value={blocking.refreshedAt}>{(v) => <Time iso={v} past />}</Reported>
        </p>
        <p className="text-caption text-muted">{m.insight_poll_note({ seconds: formatNumber(pollMs / 1000) })}</p>
      </div>
      <PartialNotice panel={blocking} />
      {!blocking.available ? (
        <Unavailable text={blocking.text} />
      ) : (
        (blocking.instances ?? []).map((i) => <InstanceGraph key={i.instance} i={i} />)
      )}
      <PgAdminLinks
        links={blocking.pgAdmins}
        unread={blocking.pgAdminsUnread}
        note={
          <>
            {m.insight_terminate_note()} <code className="font-code">pg_terminate_backend(pid)</code>
          </>
        }
      />
    </InsightShell>
  );
}

/**
 * Reads the lock graph again (refresh=true) while the screen is shown: at
 * Go's minRefreshSeconds but never faster than every 2 s, and not while the
 * tab is hidden (TanStack Query's focus manager follows document visibility).
 */
function useBlockingPoll(namespace: string, cluster: string) {
  return useGetInsightBlocking(
    namespace,
    cluster,
    { refresh: true },
    {
      query: {
        refetchInterval: (q) => blockingPollMs(q.state.data?.minRefreshSeconds),
        refetchIntervalInBackground: false,
      },
    },
  );
}

function BlockingGraph({ insight, namespace, cluster }: { insight: InsightView; namespace: string; cluster: string }) {
  const query = useBlockingPoll(namespace, cluster);
  return (
    <QueryState query={query} what={m.insight_nav_blocking()}>
      {(blocking) => (
        <BlockingView insight={insight} blocking={blocking} pollMs={blockingPollMs(blocking.minRefreshSeconds)} />
      )}
    </QueryState>
  );
}

export function BlockingScreen({ namespace, cluster }: { namespace: string; cluster: string }) {
  const query = useGetInsight(namespace, cluster);
  return (
    <QueryState query={query} what={m.insight_nav_blocking()}>
      {(insight) => <BlockingGraph insight={insight} namespace={namespace} cluster={cluster} />}
    </QueryState>
  );
}
