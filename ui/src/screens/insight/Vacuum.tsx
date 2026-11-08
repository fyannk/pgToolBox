import { useGetInsight } from "../../api/generated/endpoints/clusters/clusters";
import type {
  InsightDatabase,
  InsightForecast,
  InsightHorizon,
  InsightHorizons,
  InsightPreparedXacts,
  InsightRate,
  InsightSetting,
  InsightTable,
  InsightVacuum,
  InsightView,
  InsightWraparound,
} from "../../api/generated/model";
import { formatCount, formatDuration, formatNumber, formatPercent } from "../../ds/format";
import { Reported } from "../../ds/NotReported";
import { type Columns, columnHelper, DataTable } from "../../ds/patterns/DataTable/DataTable";
import { StatusChip } from "../../ds/patterns/StatusChip/StatusChip";
import { Time } from "../../ds/time";
import { m } from "../../paraglide/messages.js";
import { Fact, Facts, Panel, Titled } from "../Panel";
import { QueryState } from "../QueryState";
import { Count, InsightShell, PanelProvenance, PartialNotice, Unavailable, YesNo } from "./common";
import { readHorizon } from "./logic";

/** One horizon: the threshold, and how far the age is from it in Go's terms. */
export function HorizonCell({ h, rate }: { h: InsightHorizon; rate: InsightRate | null | undefined }) {
  const r = readHorizon(h, rate);
  return (
    <span className="flex flex-col" data-horizon={r.kind}>
      <span className="text-caption text-muted">
        {m.insight_threshold()} <Count value={h.threshold} />
      </span>
      {r.kind === "past" ? (
        <span className="font-semibold">{m.insight_horizon_past({ over: formatCount(r.over) })}</span>
      ) : r.kind === "in" ? (
        <span>{m.insight_horizon_in({ duration: formatDuration(r.seconds) })}</span>
      ) : r.kind === "notApproaching" ? (
        <span className="text-muted">{m.insight_horizon_not_approaching()}</span>
      ) : (
        <span className="text-muted italic">{m.insight_horizon_no_rate()}</span>
      )}
    </span>
  );
}

type DbRow = { db: InsightDatabase; counter: "xid" | "mxid"; f: InsightForecast | undefined; rate?: InsightRate };

function dbRows(w: InsightWraparound): DbRow[] {
  return (w.databases ?? []).flatMap((db) => [
    { db, counter: "xid" as const, f: db.xid, rate: w.xidRate },
    { db, counter: "mxid" as const, f: db.mxid, rate: w.mxidRate },
  ]);
}

const horizon = (key: "autovacuum" | "failsafe" | "stop") => (r: DbRow) =>
  r.f ? <HorizonCell h={r.f[key]} rate={r.rate} /> : <Reported value={undefined} />;

const dc = columnHelper<DbRow>();
const dbColumns: Columns<DbRow> = dc.columns([
  dc.display({
    id: "db",
    header: () => m.col_database(),
    cell: (c) => (
      <span className="flex flex-col">
        <span className="font-code">{c.row.original.db.name}</span>
        {c.row.original.db.allowConnections === false && (
          <span className="text-caption text-muted">{m.insight_no_connections()}</span>
        )}
      </span>
    ),
  }),
  dc.display({
    id: "counter",
    header: () => m.insight_col_counter(),
    cell: (c) => (c.row.original.counter === "xid" ? m.insight_xid() : m.insight_mxid()),
  }),
  dc.display({
    id: "health",
    header: () => m.col_health(),
    cell: (c) =>
      c.row.original.f ? <StatusChip health={c.row.original.f.health} size="sm" /> : <Reported value={undefined} />,
  }),
  dc.display({ id: "age", header: () => m.insight_col_age(), cell: (c) => <Count value={c.row.original.f?.age} /> }),
  dc.display({
    id: "av",
    header: () => m.insight_col_autovacuum_horizon(),
    cell: (c) => horizon("autovacuum")(c.row.original),
  }),
  dc.display({
    id: "fs",
    header: () => m.insight_col_failsafe_horizon(),
    cell: (c) => horizon("failsafe")(c.row.original),
  }),
  dc.display({ id: "stop", header: () => m.insight_col_stop_horizon(), cell: (c) => horizon("stop")(c.row.original) }),
]);

function keepUpWord(k: string): string {
  switch (k) {
    case "keepingUp":
      return m.insight_keepup_keeping_up();
    case "notStarted":
      return m.insight_keepup_not_started();
    case "fallingBehind":
      return m.insight_keepup_falling_behind();
    case "disabled":
      return m.insight_keepup_disabled();
    case "never":
      return m.insight_keepup_never();
    default:
      return m.insight_keepup_unknown();
  }
}

function kindWord(k: string): string {
  switch (k) {
    case "table":
      return m.insight_kind_table();
    case "toast":
      return m.insight_kind_toast();
    case "matview":
      return m.insight_kind_matview();
    case "temporary":
      return m.insight_kind_temporary();
    default:
      return k;
  }
}

/** A TOAST table is named as its parent's: VACUUM runs on the parent, never on pg_toast.* by hand. */
function ToastParent({ t }: { t: InsightTable }) {
  if (t.kind !== "toast" || !t.parentTable) return null;
  const parent = t.parentSchema ? `${t.parentSchema}.${t.parentTable}` : t.parentTable;
  return (
    <span className="font-semibold text-caption" data-toast-parent={parent}>
      {m.insight_toast_of({ table: parent })}
    </span>
  );
}

/**
 * The autovacuum threshold. Go omits it while the table was never vacuumed
 * nor analyzed (no reltuples to scale with): that is "unknown", said with its
 * reason, never 0.
 */
function Threshold({ value }: { value: number | undefined }) {
  if (value === undefined)
    return (
      <span className="text-muted italic" data-threshold="unknown">
        {m.insight_threshold_unknown()}
      </span>
    );
  return <Count value={value} />;
}

const ago = (v: number | undefined) => (
  <Reported value={v}>{(s) => <span>{m.insight_ago({ duration: formatDuration(s) })}</span>}</Reported>
);

function tableColumns(rate: InsightRate | undefined): Columns<InsightTable> {
  const tc = columnHelper<InsightTable>();
  return tc.columns([
    tc.display({
      id: "table",
      header: () => m.insight_col_table(),
      cell: (c) => (
        <span className="flex flex-col">
          <span className="font-code">
            {c.row.original.schema}.{c.row.original.name}
          </span>
          <span className="text-caption text-muted">
            {c.row.original.database} · {kindWord(c.row.original.kind)}
          </span>
          <ToastParent t={c.row.original} />
        </span>
      ),
    }),
    tc.display({
      id: "health",
      header: () => m.col_health(),
      cell: (c) => <StatusChip health={c.row.original.health} size="sm" />,
    }),
    tc.display({
      id: "xid",
      header: () => m.insight_col_xid_age(),
      cell: (c) => {
        const x = c.row.original.xid;
        return (
          <span className="flex min-w-48 flex-col gap-1">
            {x ? (
              <>
                <Count value={x.age} />
                <HorizonCell h={x.autovacuum} rate={rate} />
              </>
            ) : (
              <Reported value={undefined} />
            )}
            <span className="text-caption" data-mxid-age="">
              <span className="text-muted">{m.insight_mxid()}</span> <Count value={c.row.original.mxidAge} />
            </span>
          </span>
        );
      },
    }),
    tc.display({
      id: "tuples",
      header: () => m.insight_col_dead_live(),
      cell: (c) => (
        <span className="flex flex-col whitespace-nowrap">
          <span>
            <Count value={c.row.original.deadTuples} /> / <Count value={c.row.original.liveTuples} />
          </span>
          <dl className="grid grid-cols-[auto_1fr] gap-x-2 text-caption">
            <dt className="text-muted">{m.insight_rows_estimate()}</dt>
            <dd className="m-0">
              <Count value={c.row.original.reltuples} />
            </dd>
            <dt className="text-muted">{m.insight_rows_vacuum_at()}</dt>
            <dd className="m-0">
              <Threshold value={c.row.original.autovacuumThreshold} />
            </dd>
          </dl>
        </span>
      ),
    }),
    tc.display({
      id: "since",
      header: () => m.insight_col_since(),
      cell: (c) => (
        <dl className="grid grid-cols-[auto_1fr] gap-x-2 whitespace-nowrap text-caption">
          <dt className="text-muted">{m.insight_vacuum()}</dt>
          <dd className="m-0">{ago(c.row.original.sinceVacuumSeconds)}</dd>
          <dt className="text-muted">{m.insight_autovacuum()}</dt>
          <dd className="m-0">{ago(c.row.original.sinceAutovacuumSeconds)}</dd>
          <dt className="text-muted">{m.insight_analyze()}</dt>
          <dd className="m-0">{ago(c.row.original.sinceAnalyzeSeconds)}</dd>
        </dl>
      ),
    }),
    tc.display({
      id: "keepUp",
      header: () => m.insight_col_keep_up(),
      cell: (c) => (
        <span className="flex min-w-64 flex-col" data-keep-up={c.row.original.keepUp}>
          <span className="font-semibold">{keepUpWord(c.row.original.keepUp)}</span>
          {c.row.original.keepUpText && <span className="text-caption text-muted">{c.row.original.keepUpText}</span>}
          <span className="text-caption" data-av-enabled="">
            <span className="text-muted">{m.insight_col_av_enabled()}</span>{" "}
            <YesNo value={c.row.original.autovacuumEnabled} />
          </span>
        </span>
      ),
    }),
  ]);
}

const blocks = (done: number | undefined, total: number | undefined) =>
  done !== undefined && total !== undefined && total > 0 ? (
    <span>
      <Count value={done} /> / <Count value={total} /> ({formatPercent(done / total)})
    </span>
  ) : (
    <Reported value={done}>{(v) => <Count value={v} />}</Reported>
  );

const vc = columnHelper<InsightVacuum>();
const vacuumColumns: Columns<InsightVacuum> = vc.columns([
  vc.display({
    id: "table",
    header: () => m.insight_col_table(),
    cell: (c) => (
      <span className="flex flex-col">
        <span className="font-code">
          {c.row.original.schema}.{c.row.original.table}
        </span>
        <span className="text-caption text-muted">{c.row.original.database}</span>
      </span>
    ),
  }),
  vc.display({
    id: "kind",
    header: () => m.insight_col_vacuum_kind(),
    cell: (c) => (
      <span className="flex flex-wrap items-center gap-1">
        <Reported value={c.row.original.autovacuum}>
          {(a) => (a ? m.insight_autovacuum() : m.insight_vacuum_manual())}
        </Reported>
        {c.row.original.wraparound && (
          <span className="rounded-chip border border-line-strong px-1 font-semibold text-caption" data-wraparound="">
            {m.insight_to_prevent_wraparound()}
          </span>
        )}
      </span>
    ),
  }),
  vc.accessor("phase", { header: () => m.insight_col_phase(), cell: (c) => c.getValue() }),
  vc.display({
    id: "running",
    header: () => m.insight_col_running(),
    cell: (c) => <Reported value={c.row.original.runningSeconds}>{(s) => formatDuration(s)}</Reported>,
  }),
  vc.display({
    id: "scanned",
    header: () => m.insight_col_heap_scanned(),
    cell: (c) => blocks(c.row.original.heapBlksScanned, c.row.original.heapBlksTotal),
  }),
  vc.display({
    id: "vacuumed",
    header: () => m.insight_col_heap_vacuumed(),
    cell: (c) => blocks(c.row.original.heapBlksVacuumed, c.row.original.heapBlksTotal),
  }),
  vc.display({
    id: "index",
    header: () => m.insight_col_index_passes(),
    cell: (c) => <Reported value={c.row.original.indexVacuumCount}>{(v) => formatNumber(v)}</Reported>,
  }),
]);

const pc = columnHelper<InsightPreparedXacts>();
const preparedColumns: Columns<InsightPreparedXacts> = pc.columns([
  pc.accessor("database", {
    header: () => m.col_database(),
    sortFn: "text",
    cell: (c) => <span className="font-code">{c.getValue()}</span>,
  }),
  pc.display({ id: "count", header: () => m.insight_col_count(), cell: (c) => <Count value={c.row.original.count} /> }),
  pc.display({
    id: "oldest",
    header: () => m.insight_col_oldest(),
    cell: (c) => ago(c.row.original.oldestSeconds),
  }),
  pc.display({
    id: "xidAge",
    header: () => m.insight_col_oldest_xid_age(),
    cell: (c) => <Count value={c.row.original.oldestXidAge} />,
  }),
]);

const HOLDERS = [
  { holder: "slot", field: "slotXminAge", label: () => m.insight_horizon_slot() },
  { holder: "catalogSlot", field: "slotCatalogXminAge", label: () => m.insight_horizon_catalog_slot() },
  { holder: "backend", field: "backendXminAge", label: () => m.insight_horizon_backend() },
  { holder: "transaction", field: "backendXidAge", label: () => m.insight_horizon_transaction() },
  { holder: "prepared", field: "preparedXidAge", label: () => m.insight_horizon_prepared() },
] as const satisfies readonly { holder: string; field: keyof InsightHorizons; label: () => string }[];

/**
 * What holds the xmin horizon back, each age as Go reports it; Go's holder (the
 * oldest) is emphasised and named in words. A missing age is "nothing holds
 * one" (viewmodel.InsightHorizons), a missing block "not reported".
 */
export function HorizonHolders({ horizons }: { horizons: InsightHorizons | undefined }) {
  return (
    <Panel title={m.insight_horizons()}>
      <p className="text-caption text-muted">{m.insight_horizons_intro()}</p>
      {horizons ? (
        <dl
          className="grid grid-cols-1 gap-x-4 text-label sm:grid-cols-[minmax(8rem,auto)_1fr] sm:gap-y-1"
          data-holder={horizons.holder}
        >
          {HOLDERS.map((h) => {
            const age = horizons[h.field];
            const holds = horizons.holder === h.holder;
            return (
              <div key={h.holder} className="contents" data-horizon-holder={holds ? "" : undefined}>
                <dt className={holds ? "font-semibold" : "text-muted"}>{h.label()}</dt>
                <dd className="m-0 mb-1 sm:mb-0">
                  {age === undefined ? (
                    <span className="text-muted">{m.insight_horizon_none_holds()}</span>
                  ) : (
                    <span className={holds ? "font-semibold" : undefined}>
                      <Count value={age} />
                      {holds && <> · {m.insight_horizon_oldest()}</>}
                    </span>
                  )}
                </dd>
              </div>
            );
          })}
        </dl>
      ) : (
        <Reported value={undefined} />
      )}
    </Panel>
  );
}

function sourceWord(s: string): string {
  return s === "instance"
    ? m.insight_source_instance()
    : s === "spec"
      ? m.insight_source_spec()
      : m.insight_source_default();
}

const sc = columnHelper<InsightSetting>();
const limitColumns: Columns<InsightSetting> = sc.columns([
  sc.accessor("name", {
    header: () => m.col_name(),
    sortFn: "text",
    cell: (c) => <span className="font-code">{c.getValue()}</span>,
  }),
  sc.accessor("value", { header: () => m.insight_col_value(), cell: (c) => <Count value={c.getValue()} /> }),
  sc.accessor("source", {
    header: () => m.insight_col_source(),
    cell: (c) => <span data-source={c.getValue()}>{sourceWord(c.getValue())}</span>,
  }),
]);

function RateFact({ rate, unit }: { rate: InsightRate | undefined; unit: string }) {
  if (!rate) return <span className="text-muted italic">{m.insight_no_rate()}</span>;
  return (
    <span>
      {m.insight_rate({ rate: formatNumber(rate.perSecond), unit, window: formatDuration(rate.windowSeconds) })}{" "}
      <span className="text-caption text-muted">
        (<Time iso={rate.from} past /> → <Time iso={rate.to} past />)
      </span>
    </span>
  );
}

/** Wraparound and vacuum: every database's XID and MXID age against PostgreSQL's three thresholds, the tables the pack singled out, and the vacuums running now. */
export function VacuumView({ data }: { data: InsightView }) {
  const w = data.wraparound;
  const assumed = w.assumed ?? [];
  return (
    <InsightShell data={data}>
      <PartialNotice panel={w} />
      {!w.available ? (
        <Unavailable text={w.text} />
      ) : (
        <>
          <Panel title={m.insight_limits()}>
            <Facts>
              <Fact label={m.insight_read_on()}>
                <Reported value={w.instance}>{(v) => <span className="font-code">{v}</span>}</Reported>
              </Fact>
              <Fact label={m.insight_xid_rate()}>
                <RateFact rate={w.xidRate} unit={m.insight_xid()} />
              </Fact>
              <Fact label={m.insight_mxid_rate()}>
                <RateFact rate={w.mxidRate} unit={m.insight_mxid()} />
              </Fact>
            </Facts>
            <DataTable
              caption={m.insight_limits_table()}
              data={w.limits ?? []}
              columns={limitColumns}
              getRowId={(r) => r.name}
              empty={m.insight_limits_none()}
            />
            {assumed.length > 0 && (
              <p className="health rounded-control px-3 py-1.5 text-label" data-health="info" data-assumed="">
                {m.insight_assumed({ settings: assumed.join(", ") })}
              </p>
            )}
            <p className="text-caption text-muted">{m.insight_horizons_note()}</p>
            <PanelProvenance provenance={w.provenance} />
          </Panel>
          <Titled title={m.insight_databases()}>
            <DataTable
              caption={m.insight_databases()}
              data={dbRows(w)}
              columns={dbColumns}
              getRowId={(r) => `${r.db.name}/${r.counter}`}
              rowHealth={(r) => r.f?.health}
              empty={m.insight_databases_none()}
            />
          </Titled>
          <HorizonHolders horizons={w.horizons} />
          <Titled title={m.insight_prepared_xacts()}>
            <p className="text-caption text-muted">{m.insight_prepared_xacts_note()}</p>
            <DataTable
              caption={m.insight_prepared_xacts()}
              data={w.preparedXacts ?? []}
              columns={preparedColumns}
              getRowId={(r) => r.database}
              empty={m.insight_prepared_xacts_none()}
            />
          </Titled>
          <Titled title={m.insight_tables()}>
            <p className="text-caption text-muted">{m.insight_tables_note()}</p>
            <DataTable
              caption={m.insight_tables()}
              data={w.tables ?? []}
              columns={tableColumns(w.xidRate)}
              getRowId={(r) => `${r.database}/${r.schema}/${r.name}`}
              empty={m.insight_tables_none()}
            />
          </Titled>
          <Titled title={m.insight_vacuums()}>
            <DataTable
              caption={m.insight_vacuums()}
              data={w.vacuums ?? []}
              columns={vacuumColumns}
              getRowId={(r) => `${r.database}/${r.schema}/${r.table}`}
              empty={m.insight_vacuums_none()}
            />
          </Titled>
        </>
      )}
    </InsightShell>
  );
}

export function VacuumScreen({ namespace, cluster }: { namespace: string; cluster: string }) {
  const query = useGetInsight(namespace, cluster);
  return (
    <QueryState query={query} what={m.insight_nav_vacuum()}>
      {(data) => <VacuumView data={data} />}
    </QueryState>
  );
}
