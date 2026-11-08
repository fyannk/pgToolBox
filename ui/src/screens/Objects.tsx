import { useGetObjects } from "../api/generated/endpoints/clusters/clusters";
import type { DeclaredObject, Objects, PoolerRow } from "../api/generated/model";
import { Reported } from "../ds/NotReported";
import { BlindSpots } from "../ds/patterns/BlindSpots/BlindSpots";
import { type Columns, columnHelper, DataTable } from "../ds/patterns/DataTable/DataTable";
import { Provenance } from "../ds/patterns/Provenance/Provenance";
import { StatusChip } from "../ds/patterns/StatusChip/StatusChip";
import { m } from "../paraglide/messages.js";
import { SourcesPanel } from "./Overview";
import { Fact, Facts, Panel, Titled } from "./Panel";
import { QueryState } from "./QueryState";

type Row = { health: string; provenance: { stale?: boolean; observedAt?: string; staleReason?: string } };
const state = (r: Row, label?: string) => (
  <StatusChip
    health={r.health}
    label={label}
    size="sm"
    stale={r.provenance.stale}
    observedAt={r.provenance.observedAt}
    staleReason={r.provenance.staleReason}
  />
);
const name = (v: string) => <span className="font-code">{v}</span>;

const dc = columnHelper<DeclaredObject>();
const declaredColumns: Columns<DeclaredObject> = dc.columns([
  dc.accessor("name", { header: () => m.col_object(), sortFn: "text", cell: (c) => name(c.getValue()) }),
  dc.display({
    id: "pg",
    header: () => m.col_pg_name(),
    cell: (c) => (
      <span className="font-code">
        <Reported value={c.row.original.pgName} />
        {c.row.original.database && <span className="text-muted"> @ {c.row.original.database}</span>}
      </span>
    ),
  }),
  dc.display({
    id: "state",
    header: () => m.col_state(),
    cell: (c) => {
      const o = c.row.original;
      return (
        <span className="flex flex-col items-start gap-0.5">
          {state(o, o.state)}
          {o.message && <span className="text-caption">{o.message}</span>}
          {o.failedObjects && o.failedObjects.length > 0 && (
            <span className="text-caption">{m.failed_objects({ objects: o.failedObjects.join(", ") })}</span>
          )}
        </span>
      );
    },
  }),
  dc.display({ id: "ensure", header: () => m.col_ensure(), cell: (c) => <Reported value={c.row.original.ensure} /> }),
  dc.display({
    id: "gen",
    header: () => m.col_generation(),
    cell: (c) => (
      <span className="num">
        <Reported value={c.row.original.observedGeneration} /> / <Reported value={c.row.original.generation} />
      </span>
    ),
  }),
]);

type ManagedRole = NonNullable<Objects["managedRoles"]>[number];
const mc = columnHelper<ManagedRole>();
const managedColumns: Columns<ManagedRole> = mc.columns([
  mc.accessor("name", { header: () => m.col_name(), sortFn: "text", cell: (c) => name(c.getValue()) }),
  mc.display({
    id: "state",
    header: () => m.col_state(),
    cell: (c) => (
      <span className="flex flex-col items-start gap-0.5">
        {state(c.row.original, c.row.original.status)}
        {(c.row.original.errors ?? []).map((e) => (
          <span key={e} className="text-caption">
            {e}
          </span>
        ))}
      </span>
    ),
  }),
  mc.display({
    id: "declared",
    header: () => m.col_declared(),
    cell: (c) =>
      c.row.original.declared ? m.yes() : <span className="font-semibold">{m.not_declared_anymore()}</span>,
  }),
]);

type Tablespace = NonNullable<Objects["tablespaces"]>[number];
const tc = columnHelper<Tablespace>();
const tablespaceColumns: Columns<Tablespace> = tc.columns([
  tc.accessor("name", { header: () => m.col_name(), sortFn: "text", cell: (c) => name(c.getValue()) }),
  tc.display({
    id: "state",
    header: () => m.col_state(),
    cell: (c) => (
      <span className="flex flex-col items-start gap-0.5">
        {state(c.row.original, c.row.original.state)}
        {c.row.original.error && <span className="text-caption">{c.row.original.error}</span>}
      </span>
    ),
  }),
  tc.display({ id: "owner", header: () => m.col_owner(), cell: (c) => <Reported value={c.row.original.owner} /> }),
  tc.display({
    id: "declared",
    header: () => m.col_declared(),
    cell: (c) => (
      <span>
        {c.row.original.declared ? m.yes() : <span className="font-semibold">{m.not_declared_anymore()}</span>}
        {c.row.original.temporary && <span className="text-muted"> · {m.volume_temporary()}</span>}
      </span>
    ),
  }),
]);

const pc = columnHelper<PoolerRow>();
const poolerColumns: Columns<PoolerRow> = pc.columns([
  pc.accessor("name", { header: () => m.col_name(), sortFn: "text", cell: (c) => name(c.getValue()) }),
  pc.display({
    id: "state",
    header: () => m.col_state(),
    cell: (c) => {
      const p = c.row.original;
      return (
        <span className="flex flex-col items-start gap-0.5">
          {state(p, p.phase)}
          {p.paused && <span className="font-semibold text-caption">{m.pooler_paused()}</span>}
          {p.text && <span className="text-caption">{p.text}</span>}
          {p.error && <span className="text-caption">{p.error}</span>}
        </span>
      );
    },
  }),
  pc.display({
    id: "type",
    header: () => m.col_pool(),
    cell: (c) => (
      <span>
        <Reported value={c.row.original.type} /> / <Reported value={c.row.original.poolMode} />
      </span>
    ),
  }),
  pc.display({
    id: "pods",
    header: () => m.col_pods(),
    cell: (c) => (
      <span className="num">
        <Reported value={c.row.original.podsReady} /> / <Reported value={c.row.original.desiredInstances} />
      </span>
    ),
  }),
]);

function Declared({ caption, rows }: { caption: string; rows: DeclaredObject[] | null }) {
  return (
    <Titled title={caption}>
      <DataTable
        caption={caption}
        data={rows ?? []}
        columns={declaredColumns}
        getRowId={(r) => r.name}
        empty={m.objects_none()}
      />
    </Titled>
  );
}

/** The objects screen: every declarative object aimed at the Cluster, its roles, tablespaces, poolers and image catalog. */
export function ObjectsView({ data }: { data: Objects }) {
  const cat = data.imageCatalog;
  return (
    <div className="flex flex-col gap-4">
      <Declared caption={m.caption_databases()} rows={data.databases} />
      <Declared caption={m.caption_db_roles()} rows={data.roles} />
      <Titled title={m.caption_managed_roles()}>
        <DataTable
          caption={m.caption_managed_roles()}
          data={data.managedRoles ?? []}
          columns={managedColumns}
          getRowId={(r) => r.name}
          empty={m.objects_none()}
        />
      </Titled>
      <Titled title={m.caption_tablespaces()}>
        <DataTable
          caption={m.caption_tablespaces()}
          data={data.tablespaces ?? []}
          columns={tablespaceColumns}
          getRowId={(r) => r.name}
          empty={m.objects_none()}
        />
      </Titled>
      <Declared caption={m.caption_publications()} rows={data.publications} />
      <Declared caption={m.caption_subscriptions()} rows={data.subscriptions} />
      <Titled title={m.caption_poolers()}>
        <DataTable
          caption={m.caption_poolers()}
          data={data.poolers ?? []}
          columns={poolerColumns}
          getRowId={(r) => r.name}
          empty={m.objects_none()}
        />
      </Titled>
      <Panel title={m.panel_catalog()}>
        {!cat ? (
          <p className="text-label text-muted">{m.catalog_none()}</p>
        ) : (
          <>
            <div className="flex flex-wrap items-center gap-2">
              {state(cat)}
              <code className="font-code">
                {cat.kind}/{cat.name}
              </code>
            </div>
            {cat.state === "notFound" && <p className="text-label">{m.catalog_state_notFound()}</p>}
            {cat.state === "notReadable" && <p className="text-label">{m.catalog_state_notReadable()}</p>}
            {cat.reason && <p className="text-label text-muted">{cat.reason}</p>}
            <Facts>
              <Fact label={m.catalog_major()}>
                <Reported value={cat.major} />
              </Fact>
              <Fact label={m.catalog_image()}>
                <code className="font-code break-all">
                  <Reported value={cat.image} />
                </code>
              </Fact>
            </Facts>
            <Provenance provenance={cat.provenance} />
          </>
        )}
      </Panel>
      <SourcesPanel sources={data.sources} />
      <BlindSpots items={data.blindSpots} />
    </div>
  );
}

export function ObjectsScreen({ namespace, cluster }: { namespace: string; cluster: string }) {
  const query = useGetObjects(namespace, cluster);
  return (
    <QueryState query={query} what={m.nav_objects()}>
      {(data) => <ObjectsView data={data} />}
    </QueryState>
  );
}
