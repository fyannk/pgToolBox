import { Link } from "@tanstack/react-router";
import { useGetDigest, useListDigests } from "../api/generated/endpoints/digests/digests";
import type {
  ClusterRef,
  Digest,
  DigestCluster,
  DigestCounts,
  DigestList,
  DigestSchedule,
  DigestSummary,
} from "../api/generated/model";
import { type Columns, columnHelper, DataTable } from "../ds/patterns/DataTable/DataTable";
import { EmptyState } from "../ds/patterns/States/States";
import { StatusChip } from "../ds/patterns/StatusChip/StatusChip";
import { Timeline } from "../ds/patterns/Timeline/Timeline";
import { formatUtc, Time } from "../ds/time";
import { m } from "../paraglide/messages.js";
import { Fact, Facts, Panel } from "./Panel";
import { QueryState } from "./QueryState";
import { sourceStateWord } from "./sources";

/** The words of a partial digest's reason (Go: internal/console/digest Partial*). */
function partialReasonWord(reason: string): string {
  switch (reason) {
    case "notObserved":
      return m.digest_partial_not_observed();
    case "consoleRestarted":
      return m.digest_partial_console_restarted();
    case "sourceGap":
      return m.digest_partial_source_gap();
    case "clustersUnreadable":
      return m.digest_partial_clusters_unreadable();
    case "noTimeline":
      return m.digest_partial_no_timeline();
    default:
      return reason;
  }
}

/** A catch-up digest: made more than an hour after its window (Go: digest.Late). */
function isLate(d: { to: string; generatedAt: string }): boolean {
  return Date.parse(d.generatedAt) - Date.parse(d.to) > 60 * 60 * 1000;
}

/** The counts of a digest, in a fixed order, each with its word. */
function countItems(c: DigestCounts): Array<[keyof DigestCounts, number, string]> {
  return [
    ["revisions", c.revisions, m.digest_count_revisions()],
    ["newFindings", c.newFindings, m.digest_count_new_findings()],
    ["worseFindings", c.worseFindings, m.digest_count_worse_findings()],
    ["clearedFindings", c.clearedFindings, m.digest_count_cleared_findings()],
    ["failovers", c.failovers, m.digest_count_failovers()],
    ["switchovers", c.switchovers, m.digest_count_switchovers()],
    ["failedBackups", c.failedBackups, m.digest_count_failed_backups()],
    ["restarts", c.restarts, m.digest_count_restarts()],
  ];
}

/** The non-zero counts as one line ("Revisions 2 · Failovers 1"), or the words for none. */
function CountLine({ counts }: { counts: DigestCounts }) {
  const items = countItems(counts).filter(([, n]) => n > 0);
  if (items.length === 0) return <span className="text-muted">{m.digest_nothing_counted()}</span>;
  return (
    <span className="flex flex-wrap gap-x-3 gap-y-0.5">
      {items.map(([k, n, word]) => (
        <span key={k}>
          {word} <span className="num font-semibold">{n}</span>
        </span>
      ))}
    </span>
  );
}

function ClusterLink({ r }: { r: ClusterRef }) {
  return (
    <Link
      to="/c/$namespace/$cluster"
      params={{ namespace: r.namespace, cluster: r.name }}
      className="font-code text-accent hover:underline"
    >
      {r.namespace}/{r.name}
    </Link>
  );
}

function windowText(d: { from: string; to: string }): string {
  return m.digest_window({ from: formatUtc(d.from), to: formatUtc(d.to) });
}

const col = columnHelper<DigestSummary>();
const columns: Columns<DigestSummary> = col.columns([
  col.accessor("to", {
    header: () => m.digest_col_window(),
    sortFn: "datetime",
    cell: (c) => (
      <span className="flex flex-col items-start gap-0.5">
        <Link to="/digests/$id" params={{ id: c.row.original.id }} className="text-accent hover:underline">
          {windowText(c.row.original)}
        </Link>
        {c.row.original.partial && (
          <span className="health rounded-chip px-1 text-caption" data-health="unknown">
            {m.digest_partial_badge()}
          </span>
        )}
      </span>
    ),
  }),
  col.display({
    id: "clusters",
    header: () => m.digest_col_clusters(),
    cell: (c) => <span className="num">{c.row.original.counts.clusters}</span>,
  }),
  col.display({
    id: "attention",
    header: () => m.digest_col_attention(),
    cell: (c) => {
      const a = c.row.original.attention ?? [];
      if (a.length === 0) return <span className="text-muted">{m.digest_no_attention()}</span>;
      return (
        <ul className="m-0 flex list-none flex-col gap-0.5 p-0">
          {a.map((r) => (
            <li key={`${r.namespace}/${r.name}`}>
              <ClusterLink r={r} />
            </li>
          ))}
        </ul>
      );
    },
  }),
  col.display({
    id: "changes",
    header: () => m.digest_col_changes(),
    cell: (c) => (
      <span className="text-label">
        <CountLine counts={c.row.original.counts} />
      </span>
    ),
  }),
]);

function ScheduleFacts({ schedule: s }: { schedule: DigestSchedule }) {
  return (
    <Facts>
      <Fact label={m.digest_schedule_hour()}>{m.digest_hour_utc({ hour: String(s.hour).padStart(2, "0") })}</Fact>
      <Fact label={m.digest_schedule_next()}>
        {s.next ? (
          <>
            {formatUtc(s.next)} (<Time iso={s.next} />)
          </>
        ) : (
          <span className="text-muted italic">{m.not_reported()}</span>
        )}
      </Fact>
      <Fact label={m.digest_schedule_retention()}>
        <code className="font-code">{s.retention}</code>{" "}
        <span className="text-muted">· {s.persistent ? m.digest_persistent() : m.digest_not_persistent()}</span>
      </Fact>
    </Facts>
  );
}

/** The digests of this console: one per day, closing at a fixed UTC hour; each is a shareable page. */
export function DigestsView({ data }: { data: DigestList }) {
  if (data.source.state === "disabled") {
    return <EmptyState title={m.digest_disabled()}>{data.source.reason ?? m.digest_disabled_detail()}</EmptyState>;
  }
  return (
    <div className="flex flex-col gap-4">
      <p className="m-0 text-label text-muted">{m.digest_intro()}</p>
      <p className="m-0 text-label">
        {m.digest_source()}: <span className="font-semibold">{sourceStateWord(data.source.state)}</span>
        {data.source.reason && <span className="text-muted"> — {data.source.reason}</span>}
      </p>
      <Panel title={m.digest_schedule()}>
        <ScheduleFacts schedule={data.schedule} />
      </Panel>
      <DataTable
        caption={m.digest_caption()}
        data={data.items ?? []}
        columns={columns}
        getRowId={(r) => r.id}
        empty={m.digest_none()}
      />
      <p className="m-0 text-caption text-muted">{m.digest_visibility_note()}</p>
    </div>
  );
}

function ClusterSection({ c }: { c: DigestCluster }) {
  const params = { namespace: c.ref.namespace, cluster: c.ref.name };
  const entries = c.entries ?? [];
  const reasons = c.reasons ?? [];
  return (
    <Panel
      title={`${c.ref.namespace}/${c.ref.name}`}
      actions={
        <Link to="/c/$namespace/$cluster" params={params} className="text-label text-accent hover:underline">
          {m.digest_open_cluster()} <span className="sr-only">{`${c.ref.namespace}/${c.ref.name}`}</span>
        </Link>
      }
    >
      <div className="flex flex-wrap items-center gap-2 text-label">
        <StatusChip health={c.health} size="sm" />
        <span className="text-muted">{m.digest_verdict_at_close()}</span>
        {c.attention && <span className="font-semibold">{m.digest_needs_attention()}</span>}
      </div>
      {reasons.length > 0 && (
        <p className="m-0 text-label">
          {m.digest_reasons()}: {reasons.join(", ")}
        </p>
      )}
      <p className="m-0 text-label">
        <CountLine counts={c.counts} />
      </p>
      {c.truncated && (
        <p role="status" className="health m-0 rounded-control px-3 py-1.5 text-label" data-health="info">
          {m.digest_cluster_truncated()}
        </p>
      )}
      {entries.length === 0 ? (
        <p className="m-0 text-label text-muted">{m.digest_cluster_quiet()}</p>
      ) : (
        <Timeline
          items={entries}
          params={params}
          label={m.digest_entries_of({ cluster: `${c.ref.namespace}/${c.ref.name}` })}
        />
      )}
    </Panel>
  );
}

/**
 * One digest: what happened in 24 hours on each Cluster the reader may get,
 * those that need attention first (Go's order), the console's own gaps, and
 * how many Clusters were left out because the reader may not get them.
 */
export function DigestView({ data }: { data: Digest }) {
  const clusters = data.clusters ?? [];
  const gaps = data.consoleGaps ?? [];
  return (
    <div className="flex flex-col gap-4">
      <Link to="/digests" className="text-label text-accent hover:underline">
        ← {m.digest_back()}
      </Link>
      <p className="m-0 text-label">
        {windowText(data)}{" "}
        <span className="text-muted">· {m.digest_generated({ at: formatUtc(data.generatedAt) })}</span>
      </p>
      {data.partial && (
        <div className="health m-0 rounded-control px-3 py-1.5 text-label" data-health="unknown">
          <p className="m-0">{m.digest_partial()}</p>
          {(data.partialReasons ?? []).length > 0 && (
            <ul className="m-0 list-disc pl-5">
              {(data.partialReasons ?? []).map((r) => (
                <li key={r}>{partialReasonWord(r)}</li>
              ))}
            </ul>
          )}
        </div>
      )}
      {isLate(data) && (
        <p className="m-0 text-caption text-muted">{m.digest_late({ at: formatUtc(data.generatedAt) })}</p>
      )}
      {data.hidden !== undefined && data.hidden > 0 && (
        <p className="health m-0 rounded-control px-3 py-1.5 text-label" data-health="info">
          {m.digest_hidden({ count: String(data.hidden) })}
        </p>
      )}
      {data.observedSince && (
        <p className="m-0 text-caption text-muted">{m.tl_observed_since({ since: formatUtc(data.observedSince) })}</p>
      )}
      <Panel title={m.digest_totals()}>
        <Facts>
          <Fact label={m.digest_col_clusters()}>
            <span className="num">{data.counts.clusters}</span>
          </Fact>
          <Fact label={m.digest_col_attention()}>
            <span className="num">{data.counts.attention}</span>
          </Fact>
          {countItems(data.counts).map(([k, n, word]) => (
            <Fact key={k} label={word}>
              <span className="num">{n}</span>
            </Fact>
          ))}
        </Facts>
        <p className="m-0 text-caption text-muted">{m.tl_restart_note()}</p>
      </Panel>
      {gaps.length > 0 && (
        <Panel title={m.digest_console_gaps()}>
          <p className="m-0 text-caption text-muted">{m.digest_console_gaps_note()}</p>
          <Timeline items={gaps} label={m.digest_console_gaps()} />
        </Panel>
      )}
      {clusters.length === 0 ? (
        <EmptyState title={m.digest_no_clusters()} />
      ) : (
        clusters.map((c) => <ClusterSection key={`${c.ref.namespace}/${c.ref.name}`} c={c} />)
      )}
    </div>
  );
}

export function DigestsScreen() {
  const query = useListDigests();
  return (
    <QueryState query={query} what={m.nav_digests()}>
      {(data) => <DigestsView data={data} />}
    </QueryState>
  );
}

export function DigestScreen({ id }: { id: string }) {
  const query = useGetDigest(id);
  return (
    <QueryState query={query} what={m.digest_heading({ id })}>
      {(data) => <DigestView data={data} />}
    </QueryState>
  );
}
