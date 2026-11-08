import { Link } from "@tanstack/react-router";
import { useGetFleetStatements } from "../../api/generated/endpoints/clusters/clusters";
import type {
  ClusterRef,
  FleetClusterStatement,
  FleetRegression,
  FleetStatement,
  FleetStatementsView,
} from "../../api/generated/model";
import { formatDuration, formatNumber } from "../../ds/format";
import { HEALTH_GLYPH } from "../../ds/health";
import { BlindSpots } from "../../ds/patterns/BlindSpots/BlindSpots";
import { type Columns, columnHelper, DataTable } from "../../ds/patterns/DataTable/DataTable";
import { Time } from "../../ds/time";
import { m } from "../../paraglide/messages.js";
import { Titled } from "../Panel";
import { QueryState } from "../QueryState";
import { packStateWord, YesNo } from "./common";
import { regressionColumns, statementColumns } from "./Statements";

const refId = (r: ClusterRef) => `${r.namespace}/${r.name}`;

/** A Cluster, linking to its statements screen (or its Insight landing). */
function ClusterCell({ cref, landing = false }: { cref: ClusterRef; landing?: boolean }) {
  const params = { namespace: cref.namespace, cluster: cref.name };
  return (
    <Link
      to={landing ? "/c/$namespace/$cluster/insight" : "/c/$namespace/$cluster/insight/statements"}
      params={params}
      className="font-code text-accent hover:underline"
    >
      <span className="text-muted">{cref.namespace}/</span>
      {cref.name}
    </Link>
  );
}

function withCluster<T extends { ref: ClusterRef }>(cols: Columns<T>): Columns<T> {
  const c = columnHelper<T>();
  return [
    c.display({
      id: "cluster",
      header: () => m.insight_col_cluster(),
      cell: (x) => <ClusterCell cref={x.row.original.ref} />,
    }),
    ...cols,
  ] as Columns<T>;
}

const regressions = withCluster(regressionColumns<FleetRegression>());
const top = withCluster(statementColumns<FleetStatement>());

const cc = columnHelper<FleetClusterStatement>();
const coverage: Columns<FleetClusterStatement> = cc.columns([
  cc.display({
    id: "cluster",
    header: () => m.insight_col_cluster(),
    cell: (x) => <ClusterCell cref={x.row.original.ref} landing />,
  }),
  cc.display({
    id: "state",
    header: () => m.insight_col_pack(),
    cell: (x) => <span data-pack-state={x.row.original.state}>{packStateWord(x.row.original.state)}</span>,
  }),
  cc.display({
    id: "statements",
    header: () => m.insight_col_pgss(),
    cell: (x) => <YesNo value={x.row.original.statements} />,
  }),
  cc.display({
    id: "comparable",
    header: () => m.insight_col_comparable(),
    cell: (x) => (
      <span className="flex flex-col" data-comparable={x.row.original.comparable}>
        <span className="font-semibold">{x.row.original.comparable ? m.yes() : m.no()}</span>
        <span className="text-caption text-muted">{x.row.original.comparisonText}</span>
      </span>
    ),
  }),
  cc.display({
    id: "covered",
    header: () => m.insight_col_covered(),
    cell: (x) => <span className="num">{formatDuration(x.row.original.coveredSeconds)}</span>,
  }),
]);

/** Team-wide query insight: every visible Cluster's regressions and costliest statements, and each one's coverage. */
export function FleetStatementsPage({ data }: { data: FleetStatementsView }) {
  const unchecked = data.unchecked ?? 0;
  return (
    <div className="flex flex-col gap-4">
      {unchecked > 0 && (
        <p role="status" className="health rounded-control px-3 py-1.5 text-label" data-health="unknown">
          <span aria-hidden="true" className="health-glyph mr-1.5">
            {HEALTH_GLYPH.unknown}
          </span>
          {m.clusters_unchecked({ count: formatNumber(unchecked) })}
        </p>
      )}
      <p className="text-caption text-muted">
        {m.generated_at()} <Time iso={data.generatedAt} past />
      </p>
      <Titled title={m.insight_regressions()}>
        <DataTable
          caption={m.insight_regressions()}
          data={data.regressions ?? []}
          columns={regressions}
          getRowId={(r) => `${refId(r.ref)}/${r.queryId}/${r.database}/${r.role}`}
          empty={m.insight_fleet_regressions_none()}
        />
      </Titled>
      <Titled title={m.insight_top_statements()}>
        <p className="text-caption text-muted">{m.insight_fleet_top_note()}</p>
        {data.topTruncated && (
          <p className="health rounded-control px-3 py-1.5 text-label" data-health="info" data-top-truncated="">
            <span aria-hidden="true" className="health-glyph mr-1.5">
              {HEALTH_GLYPH.info}
            </span>
            {m.insight_fleet_top_truncated({
              shown: formatNumber((data.top ?? []).length),
              total: formatNumber(data.topTotal),
            })}
          </p>
        )}
        <DataTable
          caption={m.insight_top_statements()}
          data={data.top ?? []}
          columns={top}
          getRowId={(r) => `${refId(r.ref)}/${r.queryId}/${r.database}/${r.role}`}
          empty={m.insight_top_none()}
        />
      </Titled>
      <Titled title={m.insight_coverage()}>
        <DataTable
          caption={m.insight_coverage()}
          data={data.clusters ?? []}
          columns={coverage}
          getRowId={(r) => refId(r.ref)}
          empty={m.insight_coverage_none()}
        />
      </Titled>
      <BlindSpots items={data.blindSpots} />
    </div>
  );
}

export function FleetStatementsScreen() {
  const query = useGetFleetStatements();
  return (
    <QueryState query={query} what={m.nav_fleet_insight()}>
      {(data) => <FleetStatementsPage data={data} />}
    </QueryState>
  );
}
