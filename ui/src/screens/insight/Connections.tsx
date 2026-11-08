import { useGetInsight } from "../../api/generated/endpoints/clusters/clusters";
import type { InsightConnectionGroup, InsightInstanceConnections, InsightView } from "../../api/generated/model";
import { formatDuration, formatNumber, formatPercent } from "../../ds/format";
import { Reported } from "../../ds/NotReported";
import { type Columns, columnHelper, DataTable } from "../../ds/patterns/DataTable/DataTable";
import { Provenance } from "../../ds/patterns/Provenance/Provenance";
import { StatusChip } from "../../ds/patterns/StatusChip/StatusChip";
import { m } from "../../paraglide/messages.js";
import { Fact, Facts, Panel } from "../Panel";
import { QueryState } from "../QueryState";
import { InsightShell, PartialNotice, Unavailable } from "./common";

const count = (v: number | undefined) => (
  <Reported value={v}>{(x) => <span className="num">{formatNumber(x)}</span>}</Reported>
);
const secs = (v: number | undefined) => (
  <Reported value={v}>{(x) => <span className="num">{formatDuration(x)}</span>}</Reported>
);

const gc = columnHelper<InsightConnectionGroup>();
const groupColumns: Columns<InsightConnectionGroup> = gc.columns([
  gc.accessor("database", {
    header: () => m.col_database(),
    sortFn: "text",
    cell: (c) => <span className="font-code">{c.getValue()}</span>,
  }),
  gc.accessor("application", {
    header: () => m.insight_col_application(),
    sortFn: "text",
    cell: (c) => (c.getValue() ? <span className="font-code">{c.getValue()}</span> : <Reported value={undefined} />),
  }),
  gc.accessor("state", { header: () => m.col_state(), sortFn: "text", cell: (c) => <Reported value={c.getValue()} /> }),
  gc.accessor((r) => r.sessions ?? -1, {
    id: "sessions",
    header: () => m.insight_col_sessions(),
    cell: (c) => count(c.row.original.sessions),
  }),
  gc.accessor((r) => r.maxStateAgeSeconds ?? -1, {
    id: "age",
    header: () => m.insight_col_max_state_age(),
    cell: (c) => secs(c.row.original.maxStateAgeSeconds),
  }),
]);

/** idle_in_transaction_session_timeout: 0 is PostgreSQL's "no timeout", said in words. */
function Timeout({ v }: { v: number | undefined }) {
  if (v === undefined) return <Reported value={undefined} />;
  return v === 0 ? <span>{m.insight_timeout_off()}</span> : <span className="num">{formatDuration(v)}</span>;
}

function InstanceConnections({ i }: { i: InsightInstanceConnections }) {
  return (
    <Panel title={i.primary ? m.insight_instance_primary({ instance: i.instance }) : i.instance}>
      <p className="flex flex-wrap items-center gap-2 text-label">
        <StatusChip
          health={i.health}
          size="sm"
          stale={i.provenance.stale}
          observedAt={i.provenance.observedAt}
          staleReason={i.provenance.staleReason}
        />
        <span data-share={i.share}>
          {i.client === undefined
            ? m.insight_clients_not_reported()
            : i.usable !== undefined
              ? m.insight_clients_of_usable({
                  client: formatNumber(i.client),
                  usable: formatNumber(i.usable),
                  share: i.share !== undefined ? formatPercent(i.share) : m.not_reported(),
                })
              : m.insight_clients_no_usable({ client: formatNumber(i.client) })}
        </span>
      </p>
      <Facts>
        <Fact label={m.insight_max_connections()}>{count(i.maxConnections)}</Fact>
        <Fact label={m.insight_reserved()}>{count(i.reserved)}</Fact>
        <Fact label={m.insight_usable()}>{count(i.usable)}</Fact>
        <Fact label={m.insight_active()}>{count(i.active)}</Fact>
        <Fact label={m.insight_idle_in_xact()}>
          {count(i.idleInTransaction)}
          {i.idleInTransactionMaxSeconds !== undefined && (
            <span className="text-muted">
              {" "}
              ({m.insight_oldest({ duration: formatDuration(i.idleInTransactionMaxSeconds) })})
            </span>
          )}
        </Fact>
        <Fact label={m.insight_lock_waiting()}>{count(i.lockWaiting)}</Fact>
        <Fact label={m.insight_max_xact_age()}>{secs(i.maxXactAgeSeconds)}</Fact>
        <Fact label={m.insight_idle_timeout()}>
          <Timeout v={i.idleInTransactionSessionTimeoutSeconds} />
        </Fact>
      </Facts>
      <DataTable
        caption={m.insight_groups_of({ instance: i.instance })}
        data={i.groups ?? []}
        columns={groupColumns}
        getRowId={(r) => `${r.database}/${r.application}/${r.state}`}
        empty={m.insight_groups_none()}
      />
      <Provenance provenance={i.provenance} />
    </Panel>
  );
}

/** Connection saturation per instance: client connections against the usable slots (max_connections less the reserved ones), who holds them and in which state. */
export function ConnectionsView({ data }: { data: InsightView }) {
  const c = data.connections;
  return (
    <InsightShell data={data}>
      <PartialNotice panel={c} />
      {!c.available ? (
        <Unavailable text={c.text} />
      ) : (
        <>
          <p className="text-caption text-muted">{m.insight_connections_note()}</p>
          {(c.instances ?? []).map((i) => (
            <InstanceConnections key={i.instance} i={i} />
          ))}
        </>
      )}
    </InsightShell>
  );
}

export function ConnectionsScreen({ namespace, cluster }: { namespace: string; cluster: string }) {
  const query = useGetInsight(namespace, cluster);
  return (
    <QueryState query={query} what={m.insight_nav_connections()}>
      {(data) => <ConnectionsView data={data} />}
    </QueryState>
  );
}
