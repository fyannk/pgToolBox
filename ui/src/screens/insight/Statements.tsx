import { useGetInsight } from "../../api/generated/endpoints/clusters/clusters";
import type { InsightRegression, InsightStatement, InsightStatements, InsightView } from "../../api/generated/model";
import { formatDuration, formatMillis, formatNumber, formatPercent } from "../../ds/format";
import { Reported } from "../../ds/NotReported";
import { CopyCode } from "../../ds/patterns/CopyCode/CopyCode";
import { type Columns, columnHelper, DataTable } from "../../ds/patterns/DataTable/DataTable";
import { StatusChip } from "../../ds/patterns/StatusChip/StatusChip";
import { Time } from "../../ds/time";
import { m } from "../../paraglide/messages.js";
import { Fact, Facts, Panel, Titled } from "../Panel";
import { QueryState } from "../QueryState";
import { Count, InsightShell, PanelProvenance, PartialNotice, PgAdminLinks, Unavailable } from "./common";
import { statementSql } from "./logic";

/** The SQL that shows a statement's text in pgAdmin, or why there is none. */
function TextQuery({ textQuery, queryId }: { textQuery: string; queryId: string }) {
  const sql = statementSql(textQuery, queryId);
  if (!sql) return <span className="text-muted italic">{m.insight_text_query_none()}</span>;
  return (
    <details className="text-label">
      <summary className="cursor-pointer text-accent">{m.insight_text_query()}</summary>
      <div className="mt-1 min-w-64">
        <CopyCode code={sql} label={m.insight_text_query_of({ queryId })} />
      </div>
    </details>
  );
}

const ms = (v: number | undefined) => (
  <Reported value={v}>{(x) => <span className="num">{formatMillis(x)}</span>}</Reported>
);

type StatementLike = Pick<
  InsightStatement,
  "queryId" | "database" | "role" | "calls" | "totalMillis" | "meanMillis" | "rows" | "hitRatio"
>;

/** The statement columns, shared by one Cluster's screen and the team-wide page. */
export function statementColumns<T extends StatementLike>(): Columns<T> {
  const c = columnHelper<T>();
  return c.columns([
    c.display({
      id: "queryId",
      header: () => m.insight_queryid(),
      cell: (x) => <code className="font-code">{x.row.original.queryId}</code>,
    }),
    c.display({
      id: "database",
      header: () => m.col_database(),
      cell: (x) => <span className="font-code">{x.row.original.database}</span>,
    }),
    c.display({
      id: "role",
      header: () => m.insight_col_role(),
      cell: (x) => <span className="font-code">{x.row.original.role}</span>,
    }),
    c.display({
      id: "calls",
      header: () => m.insight_col_calls(),
      cell: (x) => <Count value={x.row.original.calls} />,
    }),
    c.display({ id: "total", header: () => m.insight_col_total(), cell: (x) => ms(x.row.original.totalMillis) }),
    c.display({ id: "mean", header: () => m.insight_col_mean(), cell: (x) => ms(x.row.original.meanMillis) }),
    c.display({ id: "rows", header: () => m.insight_col_rows(), cell: (x) => <Count value={x.row.original.rows} /> }),
    c.display({
      id: "hit",
      header: () => m.insight_col_hit_ratio(),
      cell: (x) => (
        <Reported value={x.row.original.hitRatio}>{(v) => <span className="num">{formatPercent(v)}</span>}</Reported>
      ),
    }),
  ]) as Columns<T>;
}

type RegressionLike = Pick<
  InsightRegression,
  | "queryId"
  | "database"
  | "role"
  | "ratio"
  | "recentMeanMillis"
  | "baselineMeanMillis"
  | "recentCalls"
  | "baselineCalls"
  | "recentFrom"
  | "recentTo"
  | "baselineFrom"
  | "baselineTo"
  | "uncertain"
>;

const timeWindow = (from: string, to: string) => (
  <span className="text-caption text-muted">
    <Time iso={from} past /> → <Time iso={to} past />
  </span>
);

/** The regression columns, shared by one Cluster's screen and the team-wide page. */
export function regressionColumns<T extends RegressionLike>(): Columns<T> {
  const c = columnHelper<T>();
  return c.columns([
    c.display({
      id: "queryId",
      header: () => m.insight_queryid(),
      cell: (x) => <code className="font-code">{x.row.original.queryId}</code>,
    }),
    c.display({
      id: "who",
      header: () => m.insight_col_db_role(),
      cell: (x) => (
        <span className="font-code">
          {x.row.original.database} / {x.row.original.role}
        </span>
      ),
    }),
    c.display({
      id: "ratio",
      header: () => m.insight_col_ratio(),
      cell: (x) => (
        <span className="flex flex-col items-start gap-1">
          <span className="num font-semibold">{m.insight_times({ ratio: formatNumber(x.row.original.ratio) })}</span>
          {x.row.original.uncertain && (
            <span className="flex flex-col items-start gap-0.5" data-uncertain="">
              <StatusChip health="warning" label={m.insight_uncertain()} size="sm" />
              <span className="max-w-64 text-caption text-muted">{m.insight_uncertain_note()}</span>
            </span>
          )}
        </span>
      ),
    }),
    c.display({
      id: "recent",
      header: () => m.insight_col_recent(),
      cell: (x) => (
        <span className="flex flex-col">
          <span>
            {m.insight_mean_over_calls({
              mean: formatMillis(x.row.original.recentMeanMillis),
              calls: formatNumber(x.row.original.recentCalls),
            })}
          </span>
          {timeWindow(x.row.original.recentFrom, x.row.original.recentTo)}
        </span>
      ),
    }),
    c.display({
      id: "baseline",
      header: () => m.insight_col_baseline(),
      cell: (x) => (
        <span className="flex flex-col">
          <span>
            {m.insight_mean_over_calls({
              mean: formatMillis(x.row.original.baselineMeanMillis),
              calls: formatNumber(x.row.original.baselineCalls),
            })}
          </span>
          {timeWindow(x.row.original.baselineFrom, x.row.original.baselineTo)}
        </span>
      ),
    }),
  ]) as Columns<T>;
}

function withText<T extends { queryId: string }>(cols: Columns<T>, textQuery: string): Columns<T> {
  const c = columnHelper<T>();
  return [
    ...cols,
    c.display({
      id: "text",
      header: () => m.insight_col_text(),
      cell: (x) => <TextQuery textQuery={textQuery} queryId={x.row.original.queryId} />,
    }),
  ] as Columns<T>;
}

/** Whether the regression comparison could be made, in Go's words, and how much history it has. */
function Comparison({
  s,
}: {
  s: Pick<InsightStatements, "comparable" | "comparisonText" | "coveredSeconds" | "needsSeconds">;
}) {
  return (
    <div
      className="health flex flex-col gap-1 rounded-control px-3 py-2 text-label"
      data-health={s.comparable ? "info" : "unknown"}
      data-comparable={s.comparable}
    >
      <p>{s.comparisonText}</p>
      {!s.comparable && (
        <p className="text-caption">
          {m.insight_history_covered({
            covered: formatDuration(s.coveredSeconds),
            needs: formatDuration(s.needsSeconds),
          })}
        </p>
      )}
    </div>
  );
}

/** Query insight of one Cluster's primary: the costliest statements, the regressions, never the statement text. */
export function StatementsView({ data }: { data: InsightView }) {
  const s = data.statements;
  return (
    <InsightShell data={data}>
      <PartialNotice panel={s} />
      {!s.available ? (
        <Unavailable text={s.text} />
      ) : (
        <>
          <Panel title={m.insight_statements_title()}>
            <Facts>
              <Fact label={m.insight_read_on()}>
                <Reported value={s.instance}>{(v) => <span className="font-code">{v}</span>}</Reported>
              </Fact>
              <Fact label={m.insight_stats_reset()}>
                <Reported value={s.statsResetAt}>{(v) => <Time iso={v} past />}</Reported>
              </Fact>
              <Fact label={m.insight_deallocations()}>
                <Reported value={s.deallocations}>{(v) => formatNumber(v)}</Reported>
              </Fact>
              <Fact label={m.insight_max_statements()}>
                <Reported value={s.maxStatements}>{(v) => formatNumber(v)}</Reported>
              </Fact>
              <Fact label={m.insight_regression_rule()}>
                {m.insight_regression_rule_value({ ratio: formatNumber(s.regressionRatio) })}
              </Fact>
              <Fact label={m.insight_no_baseline()}>
                <span className="num" data-no-baseline={s.noBaseline}>
                  {formatNumber(s.noBaseline)}
                </span>
              </Fact>
              <Fact label={m.insight_restarted()}>
                <span className="num" data-restarted={s.restarted}>
                  {formatNumber(s.restarted)}
                </span>
              </Fact>
              <Fact label={m.insight_evicted()}>
                <Reported value={s.evicted}>{(v) => <span className="num">{formatNumber(v)}</span>}</Reported>
              </Fact>
            </Facts>
            <Comparison s={s} />
            <PanelProvenance provenance={s.provenance} />
          </Panel>
          <Titled title={m.insight_regressions()}>
            <DataTable
              caption={m.insight_regressions()}
              data={s.regressions ?? []}
              columns={withText(regressionColumns<InsightRegression>(), s.textQuery)}
              getRowId={(r) => `${r.queryId}/${r.database}/${r.role}`}
              empty={s.comparable ? m.insight_regressions_none() : m.insight_regressions_not_compared()}
            />
          </Titled>
          <Titled title={m.insight_top_statements()}>
            <p className="text-caption text-muted">{m.insight_top_note()}</p>
            <DataTable
              caption={m.insight_top_statements()}
              data={s.top ?? []}
              columns={withText(statementColumns<InsightStatement>(), s.textQuery)}
              getRowId={(r) => `${r.queryId}/${r.database}/${r.role}`}
              empty={m.insight_top_none()}
            />
          </Titled>
        </>
      )}
      <PgAdminLinks links={data.pgAdmins} unread={data.pgAdminsUnread} note={m.insight_text_in_pgadmin()} />
    </InsightShell>
  );
}

export function StatementsScreen({ namespace, cluster }: { namespace: string; cluster: string }) {
  const query = useGetInsight(namespace, cluster);
  return (
    <QueryState query={query} what={m.insight_nav_statements()}>
      {(data) => <StatementsView data={data} />}
    </QueryState>
  );
}
