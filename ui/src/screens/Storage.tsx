import { useGetStorage } from "../api/generated/endpoints/clusters/clusters";
import type { Storage, VolumeRow, VolumeSpec } from "../api/generated/model";
import { formatBytes } from "../ds/format";
import { Reported } from "../ds/NotReported";
import { BlindSpots } from "../ds/patterns/BlindSpots/BlindSpots";
import { type Columns, columnHelper, DataTable } from "../ds/patterns/DataTable/DataTable";
import { StatusChip } from "../ds/patterns/StatusChip/StatusChip";
import { m } from "../paraglide/messages.js";
import { getLocale } from "../paraglide/runtime.js";
import { SourcesPanel } from "./Overview";
import { Titled } from "./Panel";
import { QueryState } from "./QueryState";
import { VolumeForecastsPanel } from "./VolumeForecasts";

const count = (n: number) => new Intl.NumberFormat(getLocale()).format(n);

const qc = columnHelper<VolumeSpec>();
const requestColumns: Columns<VolumeSpec> = qc.columns([
  qc.display({
    id: "role",
    header: () => m.col_role(),
    cell: (c) => (
      <span className="font-code">
        {c.row.original.role}
        {c.row.original.tablespace && ` (${c.row.original.tablespace})`}
        {c.row.original.temporary && <span className="text-muted"> · {m.volume_temporary()}</span>}
      </span>
    ),
  }),
  qc.display({ id: "size", header: () => m.col_size(), cell: (c) => <Reported value={c.row.original.size} /> }),
  qc.display({
    id: "class",
    header: () => m.col_storage_class(),
    cell: (c) => <Reported value={c.row.original.storageClass} />,
  }),
  qc.display({
    id: "autogrow",
    header: () => m.col_autogrow(),
    cell: (c) => {
      const a = c.row.original.autoGrow;
      if (!a) return <span className="text-muted">{m.autogrow_none()}</span>;
      return (
        <span className="font-code">
          {m.autogrow_declared({
            max: a.maxSize ?? m.not_reported(),
            step: a.step ?? m.not_reported(),
            threshold: a.threshold === undefined ? m.not_reported() : String(a.threshold),
          })}
        </span>
      );
    },
  }),
  qc.display({
    id: "resize",
    header: () => m.col_resize(),
    cell: (c) =>
      c.row.original.resizeInUseVolumes === undefined ? (
        <Reported value={undefined} />
      ) : c.row.original.resizeInUseVolumes ? (
        m.yes()
      ) : (
        m.no()
      ),
  }),
]);

const vc = columnHelper<VolumeRow>();
const volumeColumns: Columns<VolumeRow> = vc.columns([
  vc.accessor("name", {
    header: () => m.col_name(),
    sortFn: "text",
    cell: (c) => <span className="font-code">{c.getValue()}</span>,
  }),
  vc.display({
    id: "health",
    header: () => m.col_health(),
    cell: (c) => (
      <StatusChip
        health={c.row.original.health}
        size="sm"
        stale={c.row.original.provenance.stale}
        observedAt={c.row.original.provenance.observedAt}
      />
    ),
  }),
  vc.display({
    id: "group",
    header: () => m.col_group(),
    cell: (c) => (
      <span>
        <code className="font-code">{c.row.original.group}</code>
        {c.row.original.groupText && <span className="text-muted"> — {c.row.original.groupText}</span>}
        {c.row.original.resizing && <span className="font-semibold"> · {m.volume_resizing()}</span>}
      </span>
    ),
  }),
  vc.accessor((r) => r.instance ?? "", {
    id: "instance",
    header: () => m.col_instance(),
    sortFn: "text",
    cell: (c) => <Reported value={c.row.original.instance} />,
  }),
  vc.display({
    id: "role",
    header: () => m.col_role(),
    cell: (c) => (
      <span className="font-code">
        <Reported value={c.row.original.role} />
        {c.row.original.tablespace && ` (${c.row.original.tablespace})`}
      </span>
    ),
  }),
  vc.display({ id: "phase", header: () => m.col_phase(), cell: (c) => <Reported value={c.row.original.phase} /> }),
  vc.display({
    id: "cap",
    header: () => m.col_capacity(),
    cell: (c) => (
      <span className="num">
        <Reported value={c.row.original.capacity} /> / <Reported value={c.row.original.requested} />
      </span>
    ),
  }),
  vc.display({
    id: "class",
    header: () => m.col_storage_class(),
    cell: (c) => <Reported value={c.row.original.storageClass} />,
  }),
  vc.display({
    id: "usage",
    header: () => m.col_usage(),
    cell: (c) => {
      const u = c.row.original.usage;
      if (!u || u.usedBytes === undefined || u.totalBytes === undefined) return <Reported value={undefined} />;
      return (
        <span className="flex flex-col">
          <span className="num">
            {m.usage_of({ used: formatBytes(u.usedBytes), total: formatBytes(u.totalBytes) })}
          </span>
          {u.availableBytes !== undefined && (
            <span className="num text-caption text-muted">
              {m.usage_available({ available: formatBytes(u.availableBytes) })}
            </span>
          )}
          {u.inodes !== undefined && u.inodesFree !== undefined && (
            <span className="num text-caption text-muted">
              {m.volume_inodes_now({ free: count(u.inodesFree), total: count(u.inodes) })}
            </span>
          )}
        </span>
      );
    },
  }),
]);

/** The storage screen: what the Cluster asks for, the claims that exist, usage and forecasts only where an instance reports them. */
export function StorageView({ data }: { data: Storage }) {
  return (
    <div className="flex flex-col gap-4">
      <Titled title={m.caption_storage_requests()}>
        <DataTable
          caption={m.caption_storage_requests()}
          data={data.requests ?? []}
          columns={requestColumns}
          getRowId={(r) => `${r.role}/${r.tablespace ?? ""}`}
          empty={m.requests_none()}
        />
      </Titled>
      <Titled title={m.caption_volumes()}>
        <DataTable
          caption={m.caption_volumes()}
          data={data.volumes ?? []}
          columns={volumeColumns}
          getRowId={(r) => r.name}
          empty={m.volumes_none()}
        />
      </Titled>
      <VolumeForecastsPanel f={data.forecasts} />
      <SourcesPanel sources={data.sources} />
      <BlindSpots items={data.blindSpots} />
    </div>
  );
}

export function StorageScreen({ namespace, cluster }: { namespace: string; cluster: string }) {
  const query = useGetStorage(namespace, cluster);
  return (
    <QueryState query={query} what={m.nav_storage()}>
      {(data) => <StorageView data={data} />}
    </QueryState>
  );
}
