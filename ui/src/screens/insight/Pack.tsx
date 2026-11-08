import { Link } from "@tanstack/react-router";
import { useGetInsight } from "../../api/generated/endpoints/clusters/clusters";
import type { InsightPackInstance, InsightQueryError, InsightView } from "../../api/generated/model";
import { formatNumber } from "../../ds/format";
import { Reported } from "../../ds/NotReported";
import { type Columns, columnHelper, DataTable } from "../../ds/patterns/DataTable/DataTable";
import { Provenance } from "../../ds/patterns/Provenance/Provenance";
import { StatusChip } from "../../ds/patterns/StatusChip/StatusChip";
import { Time } from "../../ds/time";
import { m } from "../../paraglide/messages.js";
import { Fact, Facts, Panel, Titled } from "../Panel";
import { QueryState } from "../QueryState";
import { InsightShell, PartialNotice, packStateWord, SpecChange, Unavailable, YesNo } from "./common";

const ic = columnHelper<InsightPackInstance>();
const instanceColumns: Columns<InsightPackInstance> = ic.columns([
  ic.accessor("instance", {
    header: () => m.col_instance(),
    sortFn: "text",
    cell: (c) => (
      <span className="font-code">
        {c.getValue()}
        {c.row.original.primary && <span className="text-muted"> ({m.insight_primary()})</span>}
      </span>
    ),
  }),
  ic.display({
    id: "version",
    header: () => m.insight_col_pack_version(),
    cell: (c) =>
      c.row.original.version ? (
        <span className="font-code">{c.row.original.version}</span>
      ) : (
        <span className="text-muted italic">{m.insight_no_pack_reported()}</span>
      ),
  }),
  ic.display({
    id: "serverVersion",
    header: () => m.insight_col_server_version(),
    cell: (c) => (
      <Reported value={c.row.original.serverVersionNum}>
        {(v) => <span className="font-code">{String(v)}</span>}
      </Reported>
    ),
  }),
  ic.display({
    id: "updated",
    header: () => m.insight_col_exporter_updated(),
    cell: (c) => <Reported value={c.row.original.exporterUpdatedAt}>{(v) => <Time iso={v} past />}</Reported>,
  }),
  ic.display({
    id: "truncated",
    header: () => m.insight_col_truncated(),
    cell: (c) => {
      const t = c.row.original.truncated ?? [];
      if (t.length === 0) return <span className="text-muted">—</span>;
      return (
        <ul className="flex flex-col" data-truncated={t.join(" ")}>
          {t.map((f) =>
            f === "exposition" ? (
              <li key={f} className="font-semibold">
                {m.insight_truncated_exposition()}
              </li>
            ) : (
              <li key={f} className="font-code">
                {f}
              </li>
            ),
          )}
        </ul>
      );
    },
  }),
  ic.display({
    id: "read",
    header: () => m.insight_col_read(),
    cell: (c) => <Provenance provenance={c.row.original.provenance} />,
  }),
]);

const ec = columnHelper<InsightQueryError>();
const errorColumns: Columns<InsightQueryError> = ec.columns([
  ec.accessor("instance", {
    header: () => m.col_instance(),
    sortFn: "text",
    cell: (c) => <span className="font-code">{c.getValue()}</span>,
  }),
  ec.accessor("query", {
    header: () => m.insight_col_query(),
    sortFn: "text",
    cell: (c) => <span className="font-code">{c.getValue()}</span>,
  }),
  ec.accessor("count", {
    header: () => m.insight_col_errors(),
    cell: (c) => <span className="num">{formatNumber(c.getValue())}</span>,
  }),
  ec.display({
    id: "databases",
    header: () => m.insight_col_failed_databases(),
    cell: (c) => {
      const dbs = c.row.original.databases ?? [];
      return (
        <Reported value={dbs.length > 0 ? dbs : undefined}>
          {(v) => <span className="font-code">{v.join(", ")}</span>}
        </Reported>
      );
    },
  }),
]);

/** Each insight panel, linked, with whether it has data and Go's reason when it has none. */
function PanelsIndex({ data }: { data: InsightView }) {
  const params = { namespace: data.ref.namespace, cluster: data.ref.name };
  const panels = [
    { to: "/c/$namespace/$cluster/insight/vacuum", label: m.insight_nav_vacuum(), p: data.wraparound },
    { to: "/c/$namespace/$cluster/insight/blocking", label: m.insight_nav_blocking(), p: data.blocking },
    { to: "/c/$namespace/$cluster/insight/statements", label: m.insight_nav_statements(), p: data.statements },
    { to: "/c/$namespace/$cluster/insight/indexes", label: m.insight_nav_indexes(), p: data.indexes },
    { to: "/c/$namespace/$cluster/insight/connections", label: m.insight_nav_connections(), p: data.connections },
  ] as const;
  return (
    <Panel title={m.insight_panels()}>
      <ul className="flex flex-col gap-2 text-label">
        {panels.map((x) => (
          <li
            key={x.to}
            className="flex flex-col gap-1"
            data-panel-available={x.p.available}
            data-panel-partial={x.p.partial || undefined}
          >
            <Link to={x.to} params={params} className="font-semibold text-accent hover:underline">
              {x.label}
            </Link>
            {!x.p.available ? (
              <Unavailable text={x.p.text} />
            ) : x.p.partial ? null : (
              <span className="text-muted">{m.insight_panel_has_data()}</span>
            )}
            <PartialNotice panel={x.p} />
          </li>
        ))}
      </ul>
    </Panel>
  );
}

/** The Insight landing: whether the Cluster runs the pack, which version each instance reports, and what failed. */
export function InsightPackView({ data }: { data: InsightView }) {
  const pack = data.pack;
  const errors = pack.queryErrors ?? [];
  return (
    <InsightShell data={data} banner={false}>
      <Panel title={m.insight_pack_title()}>
        <p className="flex flex-wrap items-center gap-2 text-label">
          <StatusChip health={pack.health} label={packStateWord(pack.state)} />
          <span>{pack.text}</span>
        </p>
        <Facts>
          <Fact label={m.insight_pack_expected()}>
            <span className="font-code">{pack.expected}</span>
          </Fact>
          <Fact label={m.insight_pack_referenced()}>
            <YesNo value={pack.referenced} />
          </Fact>
          <Fact label={m.insight_pack_configmap()}>
            <code className="font-code">
              {pack.configMap} / {pack.key}
            </code>
          </Fact>
        </Facts>
        {(pack.state === "notEnabled" || pack.referenced === false) && <SpecChange pack={pack} />}
        <p className="text-caption text-muted">{m.insight_pack_runs_no_sql()}</p>
      </Panel>
      <Titled title={m.insight_instances()}>
        <DataTable
          caption={m.insight_instances()}
          data={pack.instances ?? []}
          columns={instanceColumns}
          getRowId={(r) => r.instance}
          empty={m.insight_instances_none()}
        />
      </Titled>
      <Titled title={m.insight_query_errors()}>
        <DataTable
          caption={m.insight_query_errors()}
          data={errors}
          columns={errorColumns}
          getRowId={(r) => `${r.instance}/${r.query}`}
          empty={m.insight_query_errors_none()}
        />
      </Titled>
      <p className="text-caption text-muted">{m.insight_query_errors_note()}</p>
      <PanelsIndex data={data} />
    </InsightShell>
  );
}

export function InsightPackScreen({ namespace, cluster }: { namespace: string; cluster: string }) {
  const query = useGetInsight(namespace, cluster);
  return (
    <QueryState query={query} what={m.nav_insight()}>
      {(data) => <InsightPackView data={data} />}
    </QueryState>
  );
}
