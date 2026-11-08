import { Link } from "@tanstack/react-router";
import type { FailoverRehearsal, RehearsalRow } from "../api/generated/model";
import { formatBytes } from "../ds/format";
import { Reported } from "../ds/NotReported";
import { BlindSpots } from "../ds/patterns/BlindSpots/BlindSpots";
import { type Columns, columnHelper, DataTable } from "../ds/patterns/DataTable/DataTable";
import { Provenance } from "../ds/patterns/Provenance/Provenance";
import { StatusChip } from "../ds/patterns/StatusChip/StatusChip";
import { m } from "../paraglide/messages.js";
import { Fact, Facts, Panel } from "./Panel";

function outcomeWord(outcome: string): string {
  switch (outcome) {
    case "promote":
      return m.failover_outcome_promote();
    case "blocked":
      return m.failover_outcome_blocked();
    case "noCandidate":
      return m.failover_outcome_no_candidate();
    case "notApplicable":
      return m.failover_outcome_not_applicable();
    default:
      return m.failover_outcome_unknown();
  }
}

function lossWord(loss: string): string {
  return loss === "none"
    ? m.failover_loss_none()
    : loss === "upTo"
      ? m.failover_loss_up_to()
      : m.failover_loss_unknown();
}

function fencingWord(fencing: string): string {
  switch (fencing) {
    case "none":
      return m.failover_fencing_none();
    case "some":
      return m.failover_fencing_some();
    case "all":
      return m.failover_fencing_all();
    default:
      return m.failover_fencing_unreadable();
  }
}

function syncWord(mode: string): string {
  return mode === "none" ? m.sync_mode_none() : mode === "legacy" ? m.sync_mode_legacy() : m.sync_mode_explicit();
}

const bytes = (n: number | undefined) => (
  <Reported value={n}>{(v) => <span className="num">{formatBytes(v)}</span>}</Reported>
);

const rc = columnHelper<RehearsalRow>();
const rankingColumns: Columns<RehearsalRow> = rc.columns([
  rc.display({
    id: "rank",
    header: () => m.col_rank(),
    cell: (c) => <span className="num">{c.row.index + 1}</span>,
  }),
  rc.display({
    id: "name",
    header: () => m.col_instance(),
    cell: (c) => <span className="font-code">{c.row.original.name}</span>,
  }),
  rc.display({
    id: "eligible",
    header: () => m.col_eligible(),
    cell: (c) => {
      const r = c.row.original;
      return (
        <span className="flex flex-col">
          {r.eligible ? m.yes() : m.no()}
          {r.reason && <span className="text-caption text-muted">{r.reason}</span>}
        </span>
      );
    },
  }),
  rc.display({
    id: "lsn",
    header: () => m.col_received_replay(),
    cell: (c) => (
      <span className="font-code">
        <Reported value={c.row.original.receivedLsn} /> / <Reported value={c.row.original.replayLsn} />
      </span>
    ),
  }),
  rc.display({ id: "behind", header: () => m.col_behind(), cell: (c) => bytes(c.row.original.behindBytes) }),
  rc.display({
    id: "sync",
    header: () => m.col_sync_state(),
    cell: (c) => <Reported value={c.row.original.syncState} />,
  }),
  rc.display({
    id: "node",
    header: () => m.col_node(),
    cell: (c) => {
      const r = c.row.original;
      return (
        <span>
          <Reported value={r.node}>{(v) => <span className="font-code">{v}</span>}</Reported>
          {r.sameNodeAsPrimary && (
            <span className="health ml-2 rounded-control px-1.5 text-caption" data-health="warning">
              {m.failover_same_node_short()}
            </span>
          )}
        </span>
      );
    },
  }),
  rc.display({
    id: "ready",
    header: () => m.col_pod_ready(),
    cell: (c) => {
      const v = c.row.original.podReady;
      return v === undefined ? <Reported value={undefined} /> : v ? m.yes() : m.no();
    },
  }),
  rc.display({
    id: "read",
    header: () => m.col_observed(),
    cell: (c) => <Provenance provenance={c.row.original.provenance} />,
  }),
]);

/**
 * "If the primary failed now": the instance the operator would promote by
 * its own ordering, how far behind it was at the last reading, whether
 * commits could be lost, and what would stop or delay the promotion. Every
 * figure is the console's reading, not the operator's; when an input is not
 * readable the panel says it cannot tell.
 */
export function FailoverPanel({ f }: { f: FailoverRehearsal }) {
  const q = f.quorum;
  return (
    <Panel title={m.panel_failover()}>
      <div className="flex flex-wrap items-center gap-2">
        <StatusChip health={f.health} label={outcomeWord(f.outcome)} size="sm" />
        <span className="text-label">{f.text}</span>
      </div>
      {!f.certain && (
        <p className="health rounded-control px-3 py-1.5 text-label" data-health="unknown">
          {m.failover_not_certain()}
        </p>
      )}
      <Facts>
        <Fact label={m.failover_candidate()}>
          <Reported value={f.candidate}>{(v) => <span className="font-code">{v}</span>}</Reported>
        </Fact>
        <Fact label={m.failover_behind()}>{bytes(f.behindBytes)}</Fact>
        <Fact label={m.failover_data_loss()}>
          <span className="flex flex-col">
            <span>{lossWord(f.dataLoss)}</span>
            {f.dataLossText && <span className="text-caption text-muted">{f.dataLossText}</span>}
          </span>
        </Fact>
        <Fact label={m.sync_mode()}>{syncWord(f.synchronous)}</Fact>
        <Fact label={m.failover_fencing()}>{fencingWord(f.fencing)}</Fact>
        <Fact label={m.failover_delay()}>
          {f.failoverDelaySeconds
            ? m.failover_delay_seconds({ seconds: f.failoverDelaySeconds })
            : m.failover_delay_none()}
        </Fact>
        <Fact label={m.failover_primary_node()}>
          <Reported value={f.primaryNode}>{(v) => <span className="font-code">{v}</span>}</Reported>
        </Fact>
      </Facts>
      {f.sameNodeAsPrimary && f.sameNodeAsPrimary.length > 0 && (
        <p className="health rounded-control px-3 py-1.5 text-label" data-health="warning">
          {m.failover_same_node({ instances: f.sameNodeAsPrimary.join(", "), node: f.primaryNode ?? "?" })}
        </p>
      )}
      {q && (
        <section aria-label={m.failover_quorum()} className="flex flex-col gap-1">
          <h3 className="font-semibold text-label">{m.failover_quorum()}</h3>
          <p className="text-label">
            {q.met === undefined
              ? m.failover_quorum_unknown()
              : q.met
                ? m.failover_quorum_met()
                : m.failover_quorum_not_met()}{" "}
            <span className="text-muted">{q.text}</span>
          </p>
          <Facts>
            <Fact label={m.failover_quorum_set()}>
              <Reported value={q.set && q.set.length > 0 ? q.set.join(", ") : undefined} />
            </Fact>
            <Fact label={m.failover_quorum_required()}>
              <span className="num">{q.required}</span>
            </Fact>
            <Fact label={m.failover_quorum_promotable()}>
              <Reported value={q.promotable && q.promotable.length > 0 ? q.promotable.join(", ") : undefined} />
            </Fact>
          </Facts>
          <Provenance provenance={q.provenance} />
        </section>
      )}
      {f.notes && f.notes.length > 0 && (
        <ul className="list-disc pl-5 text-caption text-muted">
          {f.notes.map((n) => (
            <li key={n}>{n}</li>
          ))}
        </ul>
      )}
      <DataTable
        caption={m.caption_failover_ranking()}
        data={f.ranking ?? []}
        columns={rankingColumns}
        getRowId={(r) => r.name}
        empty={m.failover_ranking_none()}
        isErrorRow={(r) => r.provenance.stale === true}
      />
      <p className="text-caption text-muted">{m.failover_rule_note()}</p>
      <Provenance provenance={f.provenance} />
      <BlindSpots items={f.blindSpots} panel={m.panel_failover()} />
    </Panel>
  );
}

/** One line on the Overview: the rehearsal's outcome and a link to the full reasoning on Replication. */
export function FailoverSummary({
  f,
  params,
}: {
  f: FailoverRehearsal;
  params: { namespace: string; cluster: string };
}) {
  if (f.outcome === "notApplicable") return null;
  return (
    <p className="flex flex-wrap items-center gap-2 text-label">
      <span className="font-semibold">{m.panel_failover()}:</span>
      <StatusChip health={f.health} label={outcomeWord(f.outcome)} size="sm" />
      <span>{f.text}</span>
      <Link to="/c/$namespace/$cluster/replication" params={params} className="text-accent hover:underline">
        {m.failover_see_reasoning()}
      </Link>
    </p>
  );
}
