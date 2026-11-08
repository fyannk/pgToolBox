import { useGetInsight } from "../../api/generated/endpoints/clusters/clusters";
import type { InsightIndex, InsightView } from "../../api/generated/model";
import { formatBytes, formatNumber } from "../../ds/format";
import { Reported } from "../../ds/NotReported";
import { type Columns, columnHelper, DataTable } from "../../ds/patterns/DataTable/DataTable";
import { Time } from "../../ds/time";
import { m } from "../../paraglide/messages.js";
import { Fact, Facts, Panel, Titled } from "../Panel";
import { QueryState } from "../QueryState";
import { InsightShell, PanelProvenance, PartialNotice, Unavailable, YesNo } from "./common";

/**
 * Scans, and since when they are counted: the index's own database's reset
 * ("no scan since …" for zero). A failover restarts the counters without
 * moving that date; the blind spots say so.
 */
function Scans({ index }: { index: InsightIndex }) {
  return (
    <span className="flex flex-col">
      <Reported value={index.scans}>{(v) => <span className="num">{formatNumber(v)}</span>}</Reported>
      {index.statsResetAt && (
        <span className="text-caption text-muted" data-stats-reset={index.statsResetAt}>
          {index.scans === 0 ? m.insight_no_scan_since() : m.insight_scans_since()}{" "}
          <Time iso={index.statsResetAt} past />
        </span>
      )}
    </span>
  );
}

/**
 * What the index serves beyond scans: a constraint (or the replica identity), a
 * foreign key's checks, and for a duplicate pair Go's "not droppable" when both
 * back a constraint. Nothing here suggests dropping an index.
 */
function ConstraintCell({ index }: { index: InsightIndex }) {
  return (
    <span className="flex flex-col gap-0.5">
      <YesNo value={index.backsConstraint} />
      {index.backsConstraint && <span className="text-caption text-muted">{m.insight_backs_constraint()}</span>}
      {index.fkSupport && (
        <span className="text-caption" data-fk-support="">
          {m.insight_fk_support()}
        </span>
      )}
      {index.droppable === false && (
        <span className="font-semibold text-caption" data-droppable="false">
          {m.insight_not_droppable()}
        </span>
      )}
    </span>
  );
}

function indexColumns(duplicate: boolean): Columns<InsightIndex> {
  const c = columnHelper<InsightIndex>();
  return c.columns([
    c.accessor("name", {
      header: () => m.insight_col_index(),
      sortFn: "text",
      cell: (x) => (
        <span className="flex flex-col">
          <span className="font-code">{x.getValue()}</span>
          {duplicate && x.row.original.duplicateOf && (
            <span className="text-caption text-muted">
              {m.insight_duplicate_of()} <code className="font-code">{x.row.original.duplicateOf}</code>
            </span>
          )}
        </span>
      ),
    }),
    c.display({
      id: "table",
      header: () => m.insight_col_table(),
      cell: (x) => (
        <span className="flex flex-col">
          <span className="font-code">
            {x.row.original.schema}.{x.row.original.table}
          </span>
          <span className="text-caption text-muted">{x.row.original.database}</span>
        </span>
      ),
    }),
    c.accessor((r) => r.sizeBytes ?? -1, {
      id: "size",
      header: () => m.insight_col_size(),
      cell: (x) => (
        <Reported value={x.row.original.sizeBytes}>{(v) => <span className="num">{formatBytes(v)}</span>}</Reported>
      ),
    }),
    c.display({
      id: "scans",
      header: () => m.insight_col_scans(),
      cell: (x) => <Scans index={x.row.original} />,
    }),
    c.display({
      id: "unique",
      header: () => m.insight_col_unique(),
      cell: (x) => <YesNo value={x.row.original.unique} />,
    }),
    c.display({
      id: "constraint",
      header: () => m.insight_col_constraint(),
      cell: (x) => <ConstraintCell index={x.row.original} />,
    }),
  ]);
}
const plain = indexColumns(false);
const withDuplicate = indexColumns(true);
const rowId = (r: InsightIndex) => `${r.database}/${r.schema}/${r.name}`;

/** Index hygiene of the primary: unused, invalid and duplicate indexes, with their size. */
export function IndexesView({ data }: { data: InsightView }) {
  const x = data.indexes;
  return (
    <InsightShell data={data}>
      <PartialNotice panel={x} />
      {!x.available ? (
        <Unavailable text={x.text} />
      ) : (
        <>
          <Panel title={m.insight_indexes_title()}>
            <Facts>
              <Fact label={m.insight_read_on()}>
                <Reported value={x.instance}>{(v) => <span className="font-code">{v}</span>}</Reported>
              </Fact>
              <Fact label={m.insight_stats_reset()}>
                <Reported value={x.statsResetAt}>{(v) => <Time iso={v} past />}</Reported>
              </Fact>
            </Facts>
            <p className="health rounded-control px-3 py-1.5 text-label" data-health="info" data-primary-only="">
              {m.insight_primary_only_caveat()}
            </p>
            <PanelProvenance provenance={x.provenance} />
          </Panel>
          <Titled title={m.insight_unused()}>
            <p className="text-caption text-muted">
              {x.statsResetAt ? (
                <>
                  {m.insight_unused_since()} <Time iso={x.statsResetAt} past />
                </>
              ) : (
                m.insight_unused_since_unknown()
              )}
            </p>
            <DataTable
              caption={m.insight_unused()}
              data={x.unused ?? []}
              columns={plain}
              getRowId={rowId}
              empty={m.insight_unused_none()}
            />
          </Titled>
          <Titled title={m.insight_invalid()}>
            <DataTable
              caption={m.insight_invalid()}
              data={x.invalid ?? []}
              columns={plain}
              getRowId={rowId}
              empty={m.insight_invalid_none()}
            />
          </Titled>
          <Titled title={m.insight_duplicate()}>
            <DataTable
              caption={m.insight_duplicate()}
              data={x.duplicate ?? []}
              columns={withDuplicate}
              getRowId={rowId}
              empty={m.insight_duplicate_none()}
            />
          </Titled>
        </>
      )}
    </InsightShell>
  );
}

export function IndexesScreen({ namespace, cluster }: { namespace: string; cluster: string }) {
  const query = useGetInsight(namespace, cluster);
  return (
    <QueryState query={query} what={m.insight_nav_indexes()}>
      {(data) => <IndexesView data={data} />}
    </QueryState>
  );
}
