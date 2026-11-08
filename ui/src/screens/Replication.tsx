import { useGetReplication } from "../api/generated/endpoints/clusters/clusters";
import type { ReceiverRow, Replication, SlotRow, StandbyRow } from "../api/generated/model";
import { formatBytes } from "../ds/format";
import { Reported } from "../ds/NotReported";
import { BlindSpots } from "../ds/patterns/BlindSpots/BlindSpots";
import { type Columns, columnHelper, DataTable } from "../ds/patterns/DataTable/DataTable";
import { Provenance } from "../ds/patterns/Provenance/Provenance";
import { StatusChip } from "../ds/patterns/StatusChip/StatusChip";
import { m } from "../paraglide/messages.js";
import { FailoverPanel } from "./Failover";
import { PrimaryPanel, SourcesPanel } from "./Overview";
import { Fact, Facts, Panel, Titled } from "./Panel";
import { QueryState } from "./QueryState";
import { StatusReadHint, statusReadWord } from "./statusRead";

const chip = (r: { health: string; provenance: { stale?: boolean; observedAt?: string; staleReason?: string } }) => (
  <StatusChip
    health={r.health}
    size="sm"
    stale={r.provenance.stale}
    observedAt={r.provenance.observedAt}
    staleReason={r.provenance.staleReason}
  />
);
const bytes = (n: number | undefined) => (
  <Reported value={n}>{(v) => <span className="num">{formatBytes(v)}</span>}</Reported>
);
const pair = (a?: string, b?: string) => (
  <span className="font-code">
    <Reported value={a} /> / <Reported value={b} />
  </span>
);

const sc = columnHelper<StandbyRow>();
const standbyColumns: Columns<StandbyRow> = sc.columns([
  sc.accessor("name", {
    header: () => m.col_name(),
    sortFn: "text",
    cell: (c) => <span className="font-code">{c.getValue()}</span>,
  }),
  sc.display({ id: "health", header: () => m.col_health(), cell: (c) => chip(c.row.original) }),
  sc.display({ id: "state", header: () => m.col_state(), cell: (c) => <Reported value={c.row.original.state} /> }),
  sc.display({
    id: "sync",
    header: () => m.col_sync_state(),
    cell: (c) => (
      <span>
        <Reported value={c.row.original.syncState} />
        {c.row.original.syncPriority && <span className="text-muted"> ({c.row.original.syncPriority})</span>}
      </span>
    ),
  }),
  sc.display({
    id: "lsn",
    header: () => m.col_lsn_sent_replay(),
    cell: (c) => pair(c.row.original.sentLsn, c.row.original.replayLsn),
  }),
  sc.display({
    id: "lags",
    header: () => m.col_lags(),
    cell: (c) => (
      <span className="font-code">
        <Reported value={c.row.original.writeLag} /> / <Reported value={c.row.original.flushLag} /> /{" "}
        <Reported value={c.row.original.replayLag} />
      </span>
    ),
  }),
  sc.display({ id: "bytes", header: () => m.col_lag_bytes(), cell: (c) => bytes(c.row.original.lagBytes) }),
]);

const rc = columnHelper<ReceiverRow>();
const receiverColumns: Columns<ReceiverRow> = rc.columns([
  rc.accessor("name", {
    header: () => m.col_instance(),
    sortFn: "text",
    cell: (c) => <span className="font-code">{c.getValue()}</span>,
  }),
  rc.display({ id: "health", header: () => m.col_health(), cell: (c) => chip(c.row.original) }),
  rc.display({
    id: "read",
    header: () => m.col_status_read(),
    cell: (c) => (
      <span className="flex flex-col">
        {statusReadWord(c.row.original.statusRead.state)}
        <StatusReadHint state={c.row.original.statusRead.state} />
      </span>
    ),
  }),
  rc.display({
    id: "receiver",
    header: () => m.col_receiver(),
    cell: (c) => {
      const r = c.row.original;
      return (
        <span>
          {r.walReceiverActive === undefined ? (
            <Reported value={undefined} />
          ) : r.walReceiverActive ? (
            m.receiver_active()
          ) : (
            m.receiver_inactive()
          )}
          {r.replayPaused && <span className="font-semibold"> · {m.replay_paused()}</span>}
        </span>
      );
    },
  }),
  rc.display({
    id: "lsn",
    header: () => m.col_received_replay(),
    cell: (c) => pair(c.row.original.receivedLsn, c.row.original.replayLsn),
  }),
  rc.display({ id: "bytes", header: () => m.col_lag_bytes(), cell: (c) => bytes(c.row.original.lagBytes) }),
  rc.display({ id: "tl", header: () => m.col_timeline(), cell: (c) => <Reported value={c.row.original.timelineId} /> }),
]);

const lc = columnHelper<SlotRow>();
const slotColumns: Columns<SlotRow> = lc.columns([
  lc.accessor("name", {
    header: () => m.col_name(),
    sortFn: "text",
    cell: (c) => <span className="font-code">{c.getValue()}</span>,
  }),
  lc.display({ id: "health", header: () => m.col_health(), cell: (c) => chip(c.row.original) }),
  lc.display({ id: "type", header: () => m.col_type(), cell: (c) => <Reported value={c.row.original.type} /> }),
  lc.display({
    id: "db",
    header: () => m.col_database(),
    cell: (c) => (
      <span>
        <Reported value={c.row.original.database} />
        {c.row.original.plugin && <span className="text-muted"> ({c.row.original.plugin})</span>}
      </span>
    ),
  }),
  lc.display({
    id: "wal",
    header: () => m.col_wal_status(),
    cell: (c) => <Reported value={c.row.original.walStatus} />,
  }),
  lc.display({ id: "active", header: () => m.col_active(), cell: (c) => (c.row.original.active ? m.yes() : m.no()) }),
  lc.accessor("instance", {
    header: () => m.col_instance(),
    sortFn: "text",
    cell: (c) => <span className="font-code">{c.getValue()}</span>,
  }),
]);

function modeWord(mode: string) {
  return mode === "none" ? m.sync_mode_none() : mode === "legacy" ? m.sync_mode_legacy() : m.sync_mode_explicit();
}
const names = (l: string[] | null | undefined) => (l && l.length > 0 ? l.join(", ") : undefined);

/** The replication screen: who is primary, what is declared, what PostgreSQL applied, every sender, receiver and slot. */
export function ReplicationView({ data }: { data: Replication }) {
  const s = data.synchronous;
  const q = data.quorum;
  return (
    <div className="flex flex-col gap-4">
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <PrimaryPanel p={data.primary} />
        <Panel title={m.panel_sync()}>
          <Facts>
            <Fact label={m.sync_mode()}>{modeWord(s.mode)}</Fact>
            {s.mode !== "none" && (
              <>
                <Fact label={m.sync_method()}>
                  <Reported value={s.method} />
                </Fact>
                {s.mode === "explicit" ? (
                  <Fact label={m.sync_number()}>
                    <Reported value={s.number} />
                  </Fact>
                ) : (
                  <Fact label={m.sync_min_max()}>
                    <Reported value={s.minSync} /> / <Reported value={s.maxSync} />
                  </Fact>
                )}
                <Fact label={m.sync_durability()}>
                  <Reported value={s.dataDurability} />
                </Fact>
                <Fact label={m.sync_names_pre()}>
                  <Reported value={names(s.standbyNamesPre)} />
                </Fact>
                <Fact label={m.sync_names_post()}>
                  <Reported value={names(s.standbyNamesPost)} />
                </Fact>
              </>
            )}
            <Fact label={m.sync_failover_quorum()}>{s.failoverQuorum ? m.yes() : m.no()}</Fact>
          </Facts>
          {s.unreadable && s.unreadable.length > 0 && (
            <p className="health rounded-control px-3 py-1.5 text-label" data-health="unknown">
              {m.sync_unreadable({ fields: s.unreadable.join(", ") })}
            </p>
          )}
          <p className="text-caption text-muted">{m.sync_intent_note()}</p>
          <Provenance provenance={s.provenance} />
        </Panel>
      </div>
      <Panel title={m.panel_quorum()}>
        {q ? (
          <>
            <div className="flex flex-wrap items-center gap-2">
              {chip(q)}
              {q.text && <span className="text-label">{q.text}</span>}
            </div>
            <Facts>
              <Fact label={m.sync_method()}>
                <Reported value={q.method} />
              </Fact>
              <Fact label={m.sync_number()}>
                <Reported value={q.number} />
              </Fact>
              <Fact label={m.quorum_names()}>
                <Reported value={names(q.standbyNames)} />
              </Fact>
              <Fact label={m.quorum_primary()}>
                <Reported value={q.primary} />
              </Fact>
            </Facts>
            <Provenance provenance={q.provenance} />
          </>
        ) : (
          <p className="text-label text-muted">{m.quorum_absent()}</p>
        )}
      </Panel>
      <FailoverPanel f={data.failover} />
      <p className="text-caption text-muted">{m.lag_not_judged()}</p>
      <Titled title={m.caption_standbys()}>
        <DataTable
          caption={m.caption_standbys()}
          data={data.standbys ?? []}
          columns={standbyColumns}
          getRowId={(r) => r.name}
          empty={m.standbys_none()}
        />
      </Titled>
      <Titled title={m.caption_receivers()}>
        <DataTable
          caption={m.caption_receivers()}
          data={data.receivers ?? []}
          columns={receiverColumns}
          getRowId={(r) => r.name}
          empty={m.receivers_none()}
          isErrorRow={(r) => r.statusRead.state !== "ok"}
        />
      </Titled>
      <Titled title={m.caption_slots()}>
        <DataTable
          caption={m.caption_slots()}
          data={data.slots ?? []}
          columns={slotColumns}
          getRowId={(r) => `${r.instance}/${r.name}`}
          empty={m.slots_none()}
        />
      </Titled>
      <SourcesPanel sources={data.sources} />
      <BlindSpots items={data.blindSpots} />
    </div>
  );
}

export function ReplicationScreen({ namespace, cluster }: { namespace: string; cluster: string }) {
  // The wiring drawing lives on the Overview, near the top (one copy, one query).
  const query = useGetReplication(namespace, cluster);
  return (
    <QueryState query={query} what={m.nav_replication()}>
      {(data) => <ReplicationView data={data} />}
    </QueryState>
  );
}
