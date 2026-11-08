// Insight states the API fixtures do not cover, built from the Go goldens
// (getInsight.orders/billing, getInsightBlocking.orders, getFleetStatements)
// and typed against the generated model so they cannot drift from the OpenAPI.
import type {
  FleetStatementsView,
  InsightBlockingView,
  InsightForecast,
  InsightView,
} from "../../src/api/generated/model";
import { fleetStatements, insightBlocking, insightEnabled, insightNotEnabled } from "./index";

/** The lock graph of a Cluster without the pack: Go's panel text, no instance. */
export const insightBlockingNotEnabled = {
  available: false,
  text: insightNotEnabled.blocking.text,
  minRefreshSeconds: insightNotEnabled.blocking.minRefreshSeconds,
  instances: [],
  partial: false,
  pgAdmins: insightNotEnabled.pgAdmins,
} satisfies InsightBlockingView;

/** The lock chain, read by someone who may open no pgAdmin. */
export const insightBlockingNoPgAdmin = { ...insightBlocking, pgAdmins: [] } satisfies InsightBlockingView;

const blockingPrimary = (() => {
  const p = insightBlocking.instances.find((i) => i.primary);
  if (!p) throw new Error("no primary in getInsightBlocking.orders.json");
  return p;
})();

/** A root blocker outside the pack's 50 rows, and a lock cycle. */
export const insightBlockingMissingCycle = {
  ...insightBlocking,
  instances: [
    {
      ...blockingPrimary,
      waiting: 3,
      roots: [
        {
          pid: 900,
          missing: true,
          blocked: 1,
          waiters: [
            {
              pid: 901,
              database: "app",
              user: "orders",
              application: "orders-api",
              state: "active",
              waitEventType: "Lock",
              waitEvent: "transactionid",
              waitSeconds: 42,
              blocked: 1,
              waiters: [
                {
                  pid: 902,
                  database: "app",
                  user: "orders",
                  state: "active",
                  blocked: 1,
                  waiters: [{ pid: 901, cycle: true, blocked: 0, waiters: [] }],
                },
              ],
            },
          ],
        },
      ],
    },
  ],
} satisfies InsightBlockingView;

/** Pack outdated on one instance: the banner says so on every insight screen. */
export const insightOutdated = {
  ...insightEnabled,
  pack: {
    ...insightEnabled.pack,
    state: "outdated",
    health: "warning",
    text: "orders-2 reports the insight pack version 0, older than the version 1 this console reads.",
    instances: (insightEnabled.pack.instances ?? []).map((i) =>
      i.instance === "orders-2" ? { ...i, version: "0" } : i,
    ),
    queryErrors: [{ instance: "orders-2", query: "pgtoolbox_statements", count: 14 }],
  },
} satisfies InsightView;

const noRate = (f: InsightForecast | undefined): InsightForecast | undefined =>
  f && {
    ...f,
    autovacuum: { ...f.autovacuum, inSeconds: undefined },
    failsafe: { ...f.failsafe, inSeconds: undefined },
    stop: { ...f.stop, inSeconds: undefined },
  };

/** Just after a console restart: no rate measured, PostgreSQL defaults assumed, no comparison yet. */
export const insightFreshConsole = {
  ...insightEnabled,
  wraparound: {
    ...insightEnabled.wraparound,
    xidRate: undefined,
    mxidRate: undefined,
    limits: (insightEnabled.wraparound.limits ?? []).map((l) =>
      l.name.startsWith("vacuum_") ? { ...l, source: "default" as const } : l,
    ),
    assumed: ["vacuum_failsafe_age", "vacuum_multixact_failsafe_age"],
    databases: (insightEnabled.wraparound.databases ?? []).map((d) => ({
      ...d,
      xid: noRate(d.xid),
      mxid: noRate(d.mxid),
    })),
    tables: (insightEnabled.wraparound.tables ?? []).map((t) => ({ ...t, xid: noRate(t.xid) })),
  },
  statements: {
    ...insightEnabled.statements,
    comparable: false,
    comparisonText:
      "Not enough history yet: the console has 1 h of samples and a comparison needs the same hour a day earlier.",
    coveredSeconds: 3600,
    regressions: [],
  },
} satisfies InsightView;

/** The pack enabled but the primary's panels reported nothing yet: each panel says why instead of an empty table. */
export const insightPanelsUnavailable = {
  ...insightEnabled,
  wraparound: { ...insightNotEnabled.wraparound, text: "The primary's database query has not reported yet." },
  statements: {
    ...insightNotEnabled.statements,
    text: "pg_stat_statements is not loaded on the primary (shared_preload_libraries).",
  },
  indexes: { ...insightNotEnabled.indexes, text: "The primary's index query has not reported yet." },
  connections: { ...insightNotEnabled.connections, text: "No instance's connection query has reported yet." },
  blocking: { ...insightNotEnabled.blocking, text: "No instance's lock query has reported yet." },
} satisfies InsightView;

/** Clusters left out of the team-wide view because the API server could not be asked. */
export const fleetStatementsUnchecked = { ...fleetStatements, unchecked: 2 } satisfies FleetStatementsView;

/**
 * A pack state Go reaches without pack data: every panel says the pack's text
 * (internal/console/insightview), as for "not enabled".
 */
function packOnly(pack: Partial<InsightView["pack"]> & Pick<InsightView["pack"], "state" | "health" | "text">) {
  const text = pack.text;
  return {
    ...insightNotEnabled,
    pack: { ...insightNotEnabled.pack, ...pack },
    wraparound: { ...insightNotEnabled.wraparound, text },
    statements: { ...insightNotEnabled.statements, text },
    indexes: { ...insightNotEnabled.indexes, text },
    connections: { ...insightNotEnabled.connections, text },
    blocking: { ...insightNotEnabled.blocking, text },
  } satisfies InsightView;
}

/** The console cut every exporter's response before the pack's series: whether the pack runs is unknown, never "not enabled". */
export const insightPackTruncated = packOnly({
  state: "truncated",
  health: "warning",
  referenced: true,
  text: "The console cut an exporter's response at its size bound before the pack's series, so whether the pack runs is not known: the instance exports more than the console reads (many databases or custom queries).",
  instances: (insightNotEnabled.pack.instances ?? []).map((i) => ({ ...i, truncated: ["exposition"] })),
});

/** PostgreSQL 13: the pack reports only its version. */
export const insightPackUnsupported = packOnly({
  state: "unsupported",
  health: "info",
  referenced: true,
  text: "The instances run PostgreSQL 13 or earlier: the insight pack supports PostgreSQL 14 and later, and reports only its version here.",
  instances: (insightEnabled.pack.instances ?? []).map((i) => ({ ...i, serverVersionNum: 130016 })),
});

/** Pack series without their version: what they report is shown, read as the console's version. */
export const insightPackUnknown = {
  ...insightEnabled,
  pack: {
    ...insightEnabled.pack,
    state: "unknown",
    health: "warning",
    text: "An instance reports the pack's series without its version (cnpg_pgtoolbox_pack_info): what it reports is shown, read as version 1.",
    instances: (insightEnabled.pack.instances ?? []).map((i) =>
      i.instance === "orders-2" ? { ...i, version: undefined } : { ...i, serverVersionNum: 170006 },
    ),
  },
} satisfies InsightView;

const failedOnBilling =
  "pgtoolbox_tables failed on database billing on orders-1: its rows are missing (see the exporter's log).";

/** A per-database query failed on one database: the panels it feeds are partial, their rows still shown. */
export const insightPartial = {
  ...insightEnabled,
  pack: {
    ...insightEnabled.pack,
    health: "warning",
    text: "The insight pack version 1 is enabled. Some of its queries fail on the exporter: see the query errors and the instance's log.",
    queryErrors: [
      { instance: "orders-1", query: "pgtoolbox_tables", count: 1, databases: ["billing"] },
      { instance: "orders-2", query: "pgtoolbox_backends", count: 3 },
    ],
  },
  wraparound: { ...insightEnabled.wraparound, partial: true, partialText: failedOnBilling },
  statements: {
    ...insightEnabled.statements,
    partial: true,
    partialText: "pgtoolbox_statements on orders-1 was cut at the console's row bound (see the exporter's log).",
  },
  indexes: {
    ...insightEnabled.indexes,
    partial: true,
    partialText:
      "pgtoolbox_indexes failed on database billing on orders-1: its rows are missing (see the exporter's log).",
  },
  connections: {
    ...insightEnabled.connections,
    partial: true,
    partialText: "pgtoolbox_backends failed on orders-2: its rows are missing (see the exporter's log).",
  },
  blocking: {
    ...insightEnabled.blocking,
    partial: true,
    partialText: "pgtoolbox_backends failed on orders-2: its rows are missing (see the exporter's log).",
  },
} satisfies InsightView;

/** The lock graph while a query behind it failed on a replica. */
export const insightBlockingPartial = {
  ...insightBlocking,
  partial: true,
  partialText: "pgtoolbox_backends failed on orders-2: its rows are missing (see the exporter's log).",
} satisfies InsightBlockingView;

/** Two prepared transactions (pid 0, no backend) holding locks others wait on. */
export const insightBlockingPrepared = {
  ...insightBlocking,
  instances: [
    {
      ...blockingPrimary,
      waiting: 3,
      roots: [
        {
          pid: 0,
          prepared: true,
          database: "app",
          user: "orders",
          xactAgeSeconds: 259_200,
          blocked: 2,
          waiters: [
            {
              pid: 6101,
              database: "app",
              user: "orders",
              application: "orders-api",
              state: "active",
              waitEventType: "Lock",
              waitEvent: "transactionid",
              waitSeconds: 75,
              blocked: 0,
              waiters: [],
            },
            {
              pid: 6102,
              database: "app",
              user: "orders",
              application: "orders-api",
              state: "active",
              waitEventType: "Lock",
              waitEvent: "transactionid",
              waitSeconds: 31,
              blocked: 0,
              waiters: [],
            },
          ],
        },
        {
          pid: 0,
          prepared: true,
          database: "app",
          user: "billing",
          xactAgeSeconds: 7_200,
          blocked: 1,
          waiters: [
            {
              pid: 6103,
              database: "app",
              user: "billing",
              application: "billing-api",
              state: "active",
              waitEventType: "Lock",
              waitEvent: "tuple",
              waitSeconds: 12,
              blocked: 0,
              waiters: [],
            },
          ],
        },
      ],
    },
  ],
} satisfies InsightBlockingView;

const tables = insightEnabled.wraparound.tables ?? [];

/**
 * What holds the xmin horizon (a 3-day-old prepared transaction, the oldest),
 * the prepared transactions per database, the TOAST table named with its
 * parent, and a table never vacuumed nor analyzed (threshold unknown).
 */
export const insightHorizonHolders = {
  ...insightEnabled,
  wraparound: {
    ...insightEnabled.wraparound,
    preparedXacts: [
      { database: "app", count: 2, oldestSeconds: 259_200, oldestXidAge: 210_000_000 },
      { database: "reporting", count: 0 },
    ],
    horizons: {
      slotXminAge: 1_200_000,
      backendXminAge: 400_000,
      backendXidAge: 300_000,
      preparedXidAge: 210_000_000,
      holder: "prepared",
    },
    tables: [
      ...tables.map((t) => (t.kind === "toast" ? { ...t, parentSchema: "public", parentTable: "documents" } : t)),
      {
        database: "app",
        schema: "public",
        name: "imports_staging",
        kind: "table",
        liveTuples: 0,
        deadTuples: 0,
        keepUp: "never",
        keepUpText: "Never vacuumed nor analyzed on this instance.",
        health: "info",
      },
    ],
  },
} satisfies InsightView;

const regressions = insightEnabled.statements.regressions ?? [];

/** pg_stat_statements evicted entries across the windows: a regression is flagged uncertain; some statements were not compared. */
export const insightStatementsUncertain = {
  ...insightEnabled,
  statements: {
    ...insightEnabled.statements,
    noBaseline: 4,
    restarted: 2,
    evicted: 130,
    regressions: regressions.map((r, i) => (i === 0 ? { ...r, uncertain: true } : r)),
  },
} satisfies InsightView;

const duplicate = insightEnabled.indexes.duplicate ?? [];
const unused = insightEnabled.indexes.unused ?? [];

/** A duplicate pair where both back a constraint (not droppable), and an unused index that serves a foreign key. */
export const insightIndexesConstraints = {
  ...insightEnabled,
  indexes: {
    ...insightEnabled.indexes,
    unused: unused.map((x, i) => (i === 1 ? { ...x, fkSupport: true, backsConstraint: false } : x)),
    duplicate: [
      ...duplicate.map((x) => ({ ...x, backsConstraint: false, droppable: true })),
      {
        database: "app",
        schema: "public",
        table: "customers",
        name: "customers_email_key",
        duplicateOf: "customers_email_uniq",
        sizeBytes: 64_000_000,
        scans: 9_120,
        unique: true,
        backsConstraint: true,
        droppable: false,
        statsResetAt: "2026-09-19T12:00:00Z",
      },
    ],
  },
} satisfies InsightView;

/** The team-wide top cut at 100 of 240 statements. */
export const fleetStatementsTruncated = {
  ...fleetStatements,
  topTotal: 240,
  topTruncated: true,
} satisfies FleetStatementsView;
