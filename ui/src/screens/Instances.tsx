import { Link } from "@tanstack/react-router";
import { type ReactNode, useMemo, useRef, useState } from "react";
import { useGetCluster, useListInstances } from "../api/generated/endpoints/clusters/clusters";
import type { InstanceList, InstanceRow } from "../api/generated/model";
import { useFeatures } from "../app/sections";
import { formatBytes, healthRank } from "../ds/format";
import { Reported } from "../ds/NotReported";
import { BlindSpots } from "../ds/patterns/BlindSpots/BlindSpots";
import { type Columns, columnHelper, DataTable } from "../ds/patterns/DataTable/DataTable";
import { Drawer } from "../ds/patterns/Drawer/Drawer";
import { Provenance } from "../ds/patterns/Provenance/Provenance";
import { StatusChip } from "../ds/patterns/StatusChip/StatusChip";
import { Time } from "../ds/time";
import { m } from "../paraglide/messages.js";
import { Fact, Facts, Panel } from "./Panel";
import { QueryState } from "./QueryState";
import { sourceStateWord } from "./sources";
import { StatusReadHint, statusReadWord } from "./statusRead";

const col = columnHelper<InstanceRow>();

/** A field PostgreSQL fills, on a report whose manager did not reach PostgreSQL: unread, never false or zero. */
function NotRead() {
  return <span className="text-muted italic">{m.pg_not_read()}</span>;
}

/** The masked state of a report: the manager answered without reaching PostgreSQL (fenced, restarting or rewinding). */
function PostgresUnreadNote({ live }: { live: InstanceRow["live"] }) {
  if (!live?.postgresUnread) return null;
  return (
    <p className="health rounded-control px-3 py-1.5 text-label" data-health="warning" data-postgres-unread>
      {m.postgres_unread({ error: live.postgresUnread.error })}
    </p>
  );
}

function roleWord(r: string) {
  return r === "primary" ? m.role_primary() : r === "replica" ? m.role_replica() : m.role_unknown();
}

function columns(open: (name: string, opener: HTMLElement) => void): Columns<InstanceRow> {
  return col.columns([
    col.accessor("name", {
      header: () => m.col_instance(),
      sortFn: "alphanumeric",
      cell: (c) => (
        <button
          type="button"
          onClick={(e) => open(c.row.original.name, e.currentTarget)}
          className="font-code font-semibold text-accent hover:underline"
          aria-haspopup="dialog"
        >
          {c.row.original.name}
        </button>
      ),
    }),
    col.accessor((r) => healthRank(r.health), {
      id: "health",
      header: () => m.col_health(),
      sortFn: "alphanumeric",
      cell: (c) => (
        <StatusChip
          health={c.row.original.health}
          stale={c.row.original.live?.provenance.stale || c.row.original.pod?.provenance.stale}
          size="sm"
        />
      ),
    }),
    col.display({
      id: "roles",
      header: () => m.col_roles(),
      cell: (c) => {
        const r = c.row.original.role;
        return (
          <span className="text-label">
            {m.roles_triplet({ operator: roleWord(r.operator), label: roleWord(r.label), live: roleWord(r.live) })}
          </span>
        );
      },
    }),
    col.display({
      id: "pod",
      header: () => m.col_pod(),
      cell: (c) => {
        const p = c.row.original.pod;
        if (!p) return <span className="text-muted">{m.pod_absent()}</span>;
        return (
          <span className="text-label">
            {p.ready ? m.ready_yes() : m.ready_no()} · <Reported value={p.phase} />
            {p.waiting && <span className="font-semibold"> · {p.waiting}</span>}
            {p.restarts > 0 && <span> · {m.restarts({ count: p.restarts })}</span>}
          </span>
        );
      },
    }),
    col.display({
      id: "statusRead",
      header: () => m.col_status_read(),
      cell: (c) => {
        const s = c.row.original.statusRead;
        return (
          <span className="flex flex-col text-label">
            <span className={s.state === "ok" ? "" : "font-semibold"}>{statusReadWord(s.state)}</span>
            {s.reason && <span className="text-muted">{s.reason}</span>}
            {c.row.original.live?.postgresUnread && (
              <span className="font-semibold" title={c.row.original.live.postgresUnread.error}>
                {m.postgres_unread_short()}
              </span>
            )}
            <StatusReadHint state={s.state} />
          </span>
        );
      },
    }),
    col.display({
      id: "lag",
      header: () => m.col_lag(),
      cell: (c) => {
        const l = c.row.original.live;
        if (!l) return <Reported value={undefined} />;
        if (l.isPrimary) return <span className="text-muted">{m.lag_primary()}</span>;
        if (l.postgresUnread) return <NotRead />;
        return l.lagBytes === undefined ? (
          <Reported value={undefined} />
        ) : (
          <span className="num">{formatBytes(l.lagBytes)}</span>
        );
      },
    }),
  ]);
}

function InstanceDetail({ row }: { row: InstanceRow }) {
  const p = row.pod;
  const l = row.live;
  const r = row.statusRead;
  return (
    <div className="flex flex-col gap-4">
      <StatusChip health={row.health} />
      <Panel title={m.detail_status_read()}>
        <Facts>
          <Fact label={m.col_status_read()}>{statusReadWord(r.state)}</Fact>
          <Fact label={m.reason()}>
            <Reported value={r.reason} />
            <StatusReadHint state={r.state} />
          </Fact>
          <Fact label={m.read_at()}>{r.at ? <Time iso={r.at} /> : <Reported value={undefined} />}</Fact>
          <Fact label={m.since()}>{r.since ? <Time iso={r.since} /> : <Reported value={undefined} />}</Fact>
        </Facts>
      </Panel>
      <Panel title={m.detail_pod()}>
        {p ? (
          <>
            <Facts>
              <Fact label={m.col_phase()}>
                <Reported value={p.phase} />
              </Fact>
              <Fact label={m.ready()}>{p.ready ? m.yes() : m.no()}</Fact>
              <Fact label={m.node()}>
                <Reported value={p.node} />
              </Fact>
              <Fact label={m.restarts_label()}>{String(p.restarts)}</Fact>
              <Fact label={m.waiting()}>
                <Reported value={p.waiting} />
              </Fact>
              <Fact label={m.image()}>
                <Reported value={p.image} />
              </Fact>
            </Facts>
            <Provenance provenance={p.provenance} />
          </>
        ) : (
          <p className="text-label text-muted">{m.pod_absent()}</p>
        )}
      </Panel>
      <Panel title={m.detail_live()}>
        {l ? <LiveFacts l={l} /> : <p className="text-label text-muted">{m.live_never_read()}</p>}
      </Panel>
    </div>
  );
}

/** The instance's own report; on a masked one, PostgreSQL's fields are not read and say so. */
function LiveFacts({ l }: { l: NonNullable<InstanceRow["live"]> }) {
  const unread = l.postgresUnread !== undefined;
  const pg = (node: ReactNode) => (unread ? <NotRead /> : node);
  const flag = (v: boolean | undefined) => pg(v ? m.yes() : m.no_or_not_reported());
  return (
    <>
      <PostgresUnreadNote live={l} />
      <Facts>
        <Fact label={m.live_is_primary()}>{l.pgRewindRunning ? <NotRead /> : l.isPrimary ? m.yes() : m.no()}</Fact>
        <Fact label={m.might_be_unavailable_label()}>{l.mightBeUnavailable ? m.yes() : m.no()}</Fact>
        <Fact label={m.timeline()}>{pg(<Reported value={l.timelineId} />)}</Fact>
        <Fact label={m.current_lsn()}>{pg(<Reported value={l.currentLsn} />)}</Fact>
        <Fact label={m.received_lsn()}>{pg(<Reported value={l.receivedLsn} />)}</Fact>
        <Fact label={m.replay_lsn()}>{pg(<Reported value={l.replayLsn} />)}</Fact>
        <Fact label={m.col_lag()}>
          {pg(l.lagBytes === undefined ? <Reported value={undefined} /> : formatBytes(l.lagBytes))}
        </Fact>
        <Fact label={m.pending_restart()}>{flag(l.pendingRestart)}</Fact>
        {l.isPrimary ? (
          <>
            <Fact label={m.archiving_wal_label()}>{flag(l.archivingWal)}</Fact>
            <Fact label={m.ready_wal_files_label()}>{pg(<Reported value={l.readyWalFiles} />)}</Fact>
          </>
        ) : (
          <>
            <Fact label={m.wal_receiver_label()}>
              {pg(l.walReceiverActive ? m.receiver_active() : m.no_or_not_reported())}
            </Fact>
            <Fact label={m.replay_paused_label()}>{flag(l.replayPaused)}</Fact>
          </>
        )}
        <Fact label={m.manager_version()}>
          <Reported value={l.instanceManagerVersion} />
        </Fact>
      </Facts>
      <Provenance provenance={l.provenance} />
    </>
  );
}

export type InstancesViewProps = {
  data: InstanceList;
  /** The instance whose drawer is open (URL state on the route); local state when absent. */
  pod?: string | undefined;
  onPodChange?: (pod: string | null) => void;
  /** The logs capability as the API server decided it, or "off" when the console serves no logs; the link is offered only when true. */
  logs?: boolean | "off";
};

export function InstancesView({ data, pod, onPodChange, logs }: InstancesViewProps) {
  const [localName, setLocalName] = useState<string | null>(null);
  const openName = onPodChange ? (pod ?? null) : localName;
  const setOpenName = onPodChange ?? setLocalName;
  const setOpen = useRef(setOpenName);
  setOpen.current = setOpenName;
  const opener = useRef<HTMLElement | null>(null);
  // Stable column definitions: TanStack renders cells as components, so a new
  // definition per render would remount every cell and lose focus.
  const cols = useMemo(
    () =>
      columns((name, el) => {
        opener.current = el;
        setOpen.current(name);
      }),
    [],
  );
  const items = data.items ?? [];
  const row = items.find((r) => r.name === openName);
  return (
    <div className="flex flex-col gap-3">
      <p className="text-label">
        {m.sweep()}: <span className="font-semibold">{sourceStateWord(data.sweep.state)}</span>
        {data.sweep.reason && <span className="text-muted"> — {data.sweep.reason}</span>}
        <span className="text-muted">
          {" · "}
          {m.generated_at()} <Time iso={data.generatedAt} />
        </span>
      </p>
      <DataTable
        caption={m.instances_caption()}
        data={items}
        columns={cols}
        getRowId={(r) => r.name}
        isErrorRow={(r) => r.statusRead.state !== "ok"}
        empty={m.instances_none_reported()}
      />
      {openName && !row && (
        <p role="status" className="health rounded-control px-3 py-1.5 text-label" data-health="info">
          {m.instance_not_listed({ name: openName })}
        </p>
      )}
      <BlindSpots items={data.blindSpots} />
      <Drawer
        open={row !== undefined}
        onOpenChange={(o) => !o && setOpenName(null)}
        title={row?.name ?? ""}
        returnFocus={opener}
      >
        {row && (
          <>
            {logs === true ? (
              <Link
                to="/c/$namespace/$cluster/logs/$pod"
                params={{ namespace: data.ref.namespace, cluster: data.ref.name, pod: row.name }}
                className="self-start rounded-control border border-line-strong px-3 py-1 text-label hover:bg-surface-2"
              >
                {m.open_logs({ pod: row.name })}
              </Link>
            ) : (
              (logs === false || logs === "off") && (
                <p className="text-label text-muted">{logs === "off" ? m.logs_feature_off() : m.logs_not_allowed()}</p>
              )
            )}
            <InstanceDetail row={row} />
          </>
        )}
      </Drawer>
    </div>
  );
}

export function InstancesScreen({
  namespace,
  cluster,
  pod,
  onPodChange,
}: { namespace: string; cluster: string } & Omit<InstancesViewProps, "data">) {
  const query = useListInstances(namespace, cluster);
  // The capabilities ride the overview; usually already in the cache.
  const overview = useGetCluster(namespace, cluster);
  const features = useFeatures();
  // Offered only when this console serves logs at all and the API server allows it for you.
  const logs = features?.logs === false ? "off" : overview.data?.capabilities.logs;
  return (
    <QueryState query={query} what={m.nav_instances()}>
      {(data) => <InstancesView data={data} pod={pod} onPodChange={onPodChange} logs={logs} />}
    </QueryState>
  );
}
