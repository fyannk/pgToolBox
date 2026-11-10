import { useGetBackups } from "../api/generated/endpoints/clusters/clusters";
import { useListOperations } from "../api/generated/endpoints/operations/operations";
import { useGetSession } from "../api/generated/endpoints/session/session";
import type {
  BackupRow,
  Backups,
  EvidenceCheck,
  EvidenceFact,
  OperationOffer,
  OperationRequest,
  ScheduleRow,
} from "../api/generated/model";
import { useRequestOperation } from "../api/operations";
import { Reported } from "../ds/NotReported";
import { BlindSpots } from "../ds/patterns/BlindSpots/BlindSpots";
import { type Columns, columnHelper, DataTable } from "../ds/patterns/DataTable/DataTable";
import { Provenance } from "../ds/patterns/Provenance/Provenance";
import { StatusChip } from "../ds/patterns/StatusChip/StatusChip";
import { Time } from "../ds/time";
import { m } from "../paraglide/messages.js";
import { BackupsPanel, SourcesPanel } from "./Overview";
import { Fact, Facts, Panel, Titled } from "./Panel";
import { BackupLabel, PgBackRestMethodFacts, PgBackRestRepository } from "./PgBackRest";
import { QueryState } from "./QueryState";
import { RecoveryWindowPanel } from "./RecoveryWindow";
import { RestoreDrillsPanel } from "./RestoreDrills";
import { phraseText } from "./recoveryPhrase";

const when = (iso?: string) => (iso ? <Time iso={iso} /> : <Reported value={undefined} />);

function repoWord(r: BackupRow["repository"]) {
  switch (r) {
    case "present":
      return m.repo_present();
    case "absent":
      return m.repo_absent();
    case "notChecked":
      return m.repo_notChecked();
    default:
      return m.repo_unknown();
  }
}

const bc = columnHelper<BackupRow>();
const backupColumns: Columns<BackupRow> = bc.columns([
  bc.accessor("name", {
    header: () => m.col_name(),
    sortFn: "text",
    cell: (c) => <span className="font-code">{c.getValue()}</span>,
  }),
  bc.display({
    id: "phase",
    header: () => m.col_phase(),
    cell: (c) => {
      const b = c.row.original;
      return (
        <span className="flex flex-col items-start gap-0.5">
          <StatusChip
            health={b.health}
            label={b.phase}
            size="sm"
            stale={b.provenance.stale}
            observedAt={b.provenance.observedAt}
          />
          {b.phase && !b.known && <span className="text-caption text-muted">{m.phase_not_in_table()}</span>}
          {b.predatesCluster && (
            <span className="text-caption font-semibold" title={m.backup_predates_cluster_hint()}>
              {m.backup_predates_cluster()}
            </span>
          )}
          {b.error && <span className="text-caption">{b.error}</span>}
          {b.notReportedWhileRunning && b.notReportedWhileRunning.length > 0 && (
            <span className="text-caption text-muted">
              {m.reported_at_completion({ fields: b.notReportedWhileRunning.join(", ") })}
            </span>
          )}
        </span>
      );
    },
  }),
  bc.accessor((r) => r.startedAt ?? r.createdAt ?? "", {
    id: "started",
    header: () => m.col_started_stopped(),
    sortFn: "datetime",
    cell: (c) => (
      <span>
        {when(c.row.original.startedAt)} → {when(c.row.original.stoppedAt)}
      </span>
    ),
  }),
  bc.display({
    id: "method",
    header: () => m.col_method(),
    cell: (c) => {
      const b = c.row.original;
      return (
        <span className="flex flex-col items-start gap-0.5">
          <span>
            <Reported value={b.pluginName ?? b.method} />
            {b.online !== undefined && (
              <span className="text-muted"> · {b.online ? m.online_backup() : m.offline_backup()}</span>
            )}
          </span>
          <BackupLabel b={b} />
        </span>
      );
    },
  }),
  bc.display({
    id: "instance",
    header: () => m.col_instance(),
    cell: (c) => <Reported value={c.row.original.instance} />,
  }),
  bc.display({
    id: "schedule",
    header: () => m.col_schedule(),
    cell: (c) => <Reported value={c.row.original.schedule} />,
  }),
  bc.display({
    id: "repo",
    header: () => m.col_repository(),
    cell: (c) => (
      <span className={c.row.original.repository === "absent" ? "font-semibold" : "text-muted"}>
        {repoWord(c.row.original.repository)}
      </span>
    ),
  }),
]);

const shc = columnHelper<ScheduleRow>();
const scheduleColumns: Columns<ScheduleRow> = shc.columns([
  shc.accessor("name", {
    header: () => m.col_name(),
    sortFn: "text",
    cell: (c) => <span className="font-code">{c.getValue()}</span>,
  }),
  shc.display({
    id: "state",
    header: () => m.col_state(),
    cell: (c) => {
      const s = c.row.original;
      return (
        <span className="flex flex-col items-start gap-0.5">
          <StatusChip
            health={s.health}
            label={s.text ?? s.state}
            size="sm"
            stale={s.provenance.stale}
            observedAt={s.provenance.observedAt}
          />
          {s.error && <span className="text-caption">{s.error}</span>}
        </span>
      );
    },
  }),
  shc.display({
    id: "cron",
    header: () => m.col_schedule(),
    cell: (c) => (
      <span className="flex flex-col">
        <code className="font-code">
          <Reported value={c.row.original.schedule} />
        </code>
        {c.row.original.note && <span className="text-caption text-muted">{c.row.original.note}</span>}
      </span>
    ),
  }),
  shc.display({
    id: "lastnext",
    header: () => m.col_last_next(),
    cell: (c) => (
      <span>
        {when(c.row.original.lastScheduledAt)} /{" "}
        {c.row.original.suspended ? m.schedule_suspended() : when(c.row.original.nextScheduledAt)}
      </span>
    ),
  }),
  shc.display({ id: "method", header: () => m.col_method(), cell: (c) => <Reported value={c.row.original.method} /> }),
]);

function evidenceStateText(s: EvidenceCheck["state"]): string {
  switch (s) {
    case "notConfigured":
      return m.evidence_state_notConfigured();
    case "noAgent":
      return m.evidence_state_noAgent();
    case "pending":
      return m.evidence_state_pending();
    case "unavailable":
      return m.evidence_state_unavailable();
    case "live":
      return m.evidence_state_live();
    case "stale":
      return m.evidence_state_stale();
    default:
      return m.evidence_state_disabled();
  }
}

function FactRow({ label, fact }: { label: string; fact: EvidenceFact }) {
  return (
    <Fact label={label}>
      <span className="flex flex-wrap items-center gap-2">
        <StatusChip health={fact.health} label={fact.state} size="sm" />
        {fact.code && <code className="font-code text-caption">{fact.code}</code>}
        {fact.message && <span>{phraseText(fact.phrase?.code, fact.phrase?.params, fact.message)}</span>}
      </span>
    </Fact>
  );
}

/**
 * The evidence cross-check; its health is Go's. When the agent's answer was
 * not used (unavailable, stale) Go's reason is the headline, verbatim: it
 * says why ("not verifiable: audience mismatch (…)", "not verifiable: status
 * not written by the operator (…)"), which the generic word cannot.
 */
function EvidencePanel({
  e,
  restoreTested,
  windowDays,
}: {
  e: EvidenceCheck;
  restoreTested: boolean;
  windowDays: number;
}) {
  const r = e.repository;
  const reasonFirst = Boolean(e.reason) && (e.state === "unavailable" || e.state === "stale");
  return (
    <Panel title={m.panel_evidence()}>
      <div className="flex flex-wrap items-center gap-2">
        <StatusChip health={e.health} stale={e.state === "stale"} observedAt={e.provenance.observedAt} />
        <span className="text-label" data-evidence-text={reasonFirst ? "reason" : "state"}>
          {reasonFirst ? e.reason : evidenceStateText(e.state)}
        </span>
      </div>
      {e.reason && !reasonFirst && <p className="text-label text-muted">{e.reason}</p>}
      {e.agent && (
        <Facts>
          <Fact label={m.evidence_agent()}>
            <code className="font-code">{e.agent}</code>
          </Fact>
        </Facts>
      )}
      {r && (
        <>
          {r.truncated && (
            <p className="health rounded-control px-3 py-1.5 text-label" data-health="unknown">
              {m.evidence_truncated()}
            </p>
          )}
          <Facts>
            <FactRow label={m.evidence_overall()} fact={r.overall} />
            <FactRow label={m.evidence_wal()} fact={r.wal} />
            <FactRow label={m.evidence_timeline()} fact={r.timeline} />
            <FactRow label={m.evidence_coverage()} fact={r.coverage} />
            <FactRow label={m.evidence_retention()} fact={r.retention} />
            <Fact label={m.evidence_counts()}>
              {r.backups === undefined ? (
                <Reported value={undefined} />
              ) : (
                <>
                  {m.evidence_counts_value({
                    backups: String(r.backups),
                    usable: r.structurallyUsable === undefined ? m.not_reported() : String(r.structurallyUsable),
                  })}
                  {!restoreTested && (
                    <span className="block text-caption font-semibold">
                      {m.not_restore_tested({ days: String(windowDays) })}
                    </span>
                  )}
                </>
              )}
            </Fact>
            <Fact label={m.evidence_repo_only()}>
              <Reported value={r.repositoryOnly} />
            </Fact>
            <Fact label={m.evidence_window()}>
              {when(r.oldestCompletionAt)} → {when(r.newestCompletionAt)}
            </Fact>
            <Fact label={m.evidence_receipt()}>{when(r.latestArchiveReceiptAt)}</Fact>
            <Fact label={m.evidence_completed()}>{when(r.completedAt)}</Fact>
            <Fact label={m.evidence_completeness()}>
              <Reported value={r.completeness} />
            </Fact>
          </Facts>
          <p className="text-caption text-muted">{m.evidence_strongest()}</p>
        </>
      )}
      <Provenance provenance={e.provenance} />
    </Panel>
  );
}

type ViewProps = {
  data: Backups;
  /** The restoreDrill offer, from the operations list (absent: not offered to this reader). */
  drillOffer?: OperationOffer;
  actor?: string;
  onRequest?: (body: OperationRequest) => Promise<unknown>;
};

/** The backups screen: method, recency and archiving, the recovery window, restore drills, every Backup and schedule, and the repository cross-check. */
export function BackupsView({ data, drillOffer, actor, onRequest }: ViewProps) {
  const meth = data.method;
  return (
    <div className="flex flex-col gap-4">
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <BackupsPanel b={data.summary} />
        <div className="flex flex-col gap-4">
          <Panel title={m.panel_backup_method()}>
            {!meth.configured ? (
              <p className="text-label">{m.method_none()}</p>
            ) : meth.pgbackrest ? (
              <Facts>
                <PgBackRestMethodFacts p={meth.pgbackrest} />
              </Facts>
            ) : (
              <Facts>
                {meth.pluginBased && (
                  <Fact label={m.method_plugin()}>
                    <Reported value={meth.pluginName} />
                  </Fact>
                )}
                {meth.objectStore && <Fact label={m.method_object_store()}>{meth.objectStore}</Fact>}
                {meth.inTreeObjectStore && <Fact label={m.method_in_tree()}>{m.yes()}</Fact>}
                {meth.volumeSnapshot && <Fact label={m.method_snapshot()}>{m.yes()}</Fact>}
              </Facts>
            )}
            <Provenance provenance={meth.provenance} />
          </Panel>
          <EvidencePanel
            e={data.evidence}
            restoreTested={data.drills.restoreTested ?? false}
            windowDays={data.drills.windowDays ?? 90}
          />
        </div>
      </div>
      {data.evidence.repository?.pgbackrest && <PgBackRestRepository p={data.evidence.repository.pgbackrest} />}
      <RecoveryWindowPanel t={data.recoveryTimeline} cluster={data.ref} windowDays={data.drills.windowDays ?? 90} />
      {data.drills && (
        <RestoreDrillsPanel drills={data.drills} offer={drillOffer} actor={actor} onRequest={onRequest} />
      )}
      <Titled title={m.caption_backup_objects()}>
        <DataTable
          caption={m.caption_backup_objects()}
          data={data.items ?? []}
          columns={backupColumns}
          getRowId={(r) => r.name}
          empty={m.backups_none()}
        />
      </Titled>
      <Titled title={m.caption_schedules()}>
        <DataTable
          caption={m.caption_schedules()}
          data={data.schedules ?? []}
          columns={scheduleColumns}
          getRowId={(r) => r.name}
          empty={m.schedules_none()}
        />
      </Titled>
      <SourcesPanel sources={data.sources} />
      <BlindSpots items={data.blindSpots} />
    </div>
  );
}

export function BackupsScreen({ namespace, cluster }: { namespace: string; cluster: string }) {
  const query = useGetBackups(namespace, cluster);
  const drillsOn = Boolean(query.data?.drills?.enabled);
  // The drill is requested from here like any operation: the offer (and why
  // not) comes from the operations list, asked only when drills are on.
  const ops = useListOperations(namespace, cluster, { query: { enabled: drillsOn, retry: false } });
  const session = useGetSession({ query: { enabled: drillsOn, retry: false, refetchInterval: false } });
  const request = useRequestOperation(namespace, cluster);
  const drillOffer = ops.data?.offers?.find((o) => o.kind === "restoreDrill");
  return (
    <QueryState query={query} what={m.nav_backups()}>
      {(data) => (
        <BackupsView
          data={data}
          drillOffer={drillOffer}
          actor={session.data?.username ?? m.ops_you()}
          onRequest={request}
        />
      )}
    </QueryState>
  );
}
