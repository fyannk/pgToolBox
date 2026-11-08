import { useMemo } from "react";
import { useListEvents } from "../api/generated/endpoints/clusters/clusters";
import type { EventList, EventRow } from "../api/generated/model";
import { Reported } from "../ds/NotReported";
import { type Columns, columnHelper, DataTable, occurrenceRowIds } from "../ds/patterns/DataTable/DataTable";
import { originWord } from "../ds/patterns/Provenance/Provenance";
import { StatusChip } from "../ds/patterns/StatusChip/StatusChip";
import { Time } from "../ds/time";
import { m } from "../paraglide/messages.js";
import { QueryState } from "./QueryState";
import { sourceStateWord } from "./sources";

const col = columnHelper<EventRow>();
const columns: Columns<EventRow> = col.columns([
  col.accessor("lastSeen", {
    header: () => m.col_last_seen(),
    sortFn: "datetime",
    cell: (c) => <Time iso={c.row.original.lastSeen} past />,
  }),
  col.accessor("reason", {
    header: () => m.col_reason(),
    sortFn: "text",
    cell: (c) => <StatusChip health={c.row.original.health} label={c.row.original.reason} size="sm" />,
  }),
  col.display({
    id: "object",
    header: () => m.col_object(),
    cell: (c) => (
      <code className="font-code text-label">
        {c.row.original.object.kind}/{c.row.original.object.name ?? "—"}
      </code>
    ),
  }),
  col.display({
    id: "message",
    header: () => m.col_message(),
    cell: (c) => (
      <span className="text-label">
        <Reported value={c.row.original.message} />
      </span>
    ),
  }),
  col.accessor("count", {
    header: () => m.col_count(),
    sortFn: "alphanumeric",
    cell: (c) => <span className="num">{c.getValue()}</span>,
  }),
  col.display({
    id: "origin",
    header: () => m.col_origin(),
    cell: (c) => (
      <span className="text-label text-muted">
        {originWord(c.row.original.origin)}
        {c.row.original.reporter && ` (${c.row.original.reporter})`}
      </span>
    ),
  }),
]);

const eventKey = (r: EventRow) => `${r.type}/${r.object.kind}/${r.object.name ?? ""}/${r.reason}/${r.lastSeen}`;

export function EventsView({ data }: { data: EventList }) {
  const items = data.items ?? [];
  const rowId = useMemo(() => occurrenceRowIds(items, eventKey), [items]);
  return (
    <div className="flex flex-col gap-3">
      <p className="text-label">
        {m.events_source()}: <span className="font-semibold">{sourceStateWord(data.source.state)}</span>
        {data.source.reason && <span className="text-muted"> — {data.source.reason}</span>}
        <span className="text-muted"> · {m.events_window({ window: data.window })}</span>
      </p>
      {data.truncated && (
        <p role="status" className="health rounded-control px-3 py-1.5 text-label" data-health="info">
          {m.events_truncated()}
        </p>
      )}
      <DataTable
        caption={m.events_caption()}
        data={items}
        columns={columns}
        getRowId={rowId}
        empty={data.source.state === "live" ? m.events_none({ window: data.window }) : m.events_source_not_live()}
      />
    </div>
  );
}

export function EventsScreen({ namespace, cluster }: { namespace: string; cluster: string }) {
  const query = useListEvents(namespace, cluster);
  return (
    <QueryState query={query} what={m.nav_events()}>
      {(data) => <EventsView data={data} />}
    </QueryState>
  );
}
