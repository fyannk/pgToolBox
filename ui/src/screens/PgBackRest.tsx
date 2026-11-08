import type {
  BackupRow,
  PgBackRestEvidence,
  PgBackRestMethod,
  PgBackRestRepoBackup,
  PgBackRestRepoBackupType,
  PgBackRestRetention,
} from "../api/generated/model";
import { formatBytes } from "../ds/format";
import { Reported } from "../ds/NotReported";
import { type Columns, columnHelper, DataTable } from "../ds/patterns/DataTable/DataTable";
import { StatusChip } from "../ds/patterns/StatusChip/StatusChip";
import { Time } from "../ds/time";
import { m } from "../paraglide/messages.js";
import { Fact, Facts, Panel, Titled } from "./Panel";

// pgBackRest: what the Cluster declares, and what the evidence
// agent read in the stanza. Neither plugin reports recoverability on the
// Cluster, so the agent's window and findings are the only source here;
// every value is Go's, rendered as is.

const when = (iso?: string) => (iso ? <Time iso={iso} /> : <Reported value={undefined} />);

function typeWord(t: PgBackRestRepoBackupType): string {
  switch (t) {
    case "full":
      return m.pgbr_type_full();
    case "diff":
      return m.pgbr_type_diff();
    default:
      return m.pgbr_type_incr();
  }
}

/** A backup's type as a badge: full, differential, incremental. */
function TypeBadge({ type }: { type: PgBackRestRepoBackupType }) {
  return (
    <span
      className="inline-block rounded-control border border-line px-1.5 text-caption font-semibold"
      data-pgbr-type={type}
    >
      {typeWord(type)}
    </span>
  );
}

/** Where a backup sits in its chain: the full that heads it, and the backup it was taken against. */
function Chain({ label, full, prior }: { label: string; full: string; prior?: string }) {
  if (label === full || !prior) {
    return <span className="text-caption text-muted">{m.pgbr_chain_head()}</span>;
  }
  return (
    <span className="flex flex-col text-caption" data-pgbr-chain={full}>
      <span>
        {m.pgbr_chain_on()} <code className="font-code">{prior}</code>
      </span>
      {prior !== full && (
        <span className="text-muted">
          {m.pgbr_chain_full()} <code className="font-code">{full}</code>
        </span>
      )}
    </span>
  );
}

/** The pgBackRest label a Backup object took, in the Backup objects table. */
export function BackupLabel({ b }: { b: BackupRow }) {
  const p = b.pgbackrest;
  if (!p) return null;
  return (
    <span className="flex flex-col items-start gap-0.5">
      <span className="flex flex-wrap items-center gap-1">
        <TypeBadge type={p.type} />
        <code className="font-code text-caption">{p.label}</code>
      </span>
      <Chain label={p.label} full={p.full} prior={p.prior} />
      {p.usable === false && <span className="text-caption font-semibold">{m.pgbr_unusable_short()}</span>}
    </span>
  );
}

/** The method facts of a pgBackRest plugin: which plugin, the object it names, the stanza and repository. */
export function PgBackRestMethodFacts({ p }: { p: PgBackRestMethod }) {
  return (
    <>
      <Fact label={m.method_plugin()}>
        {p.display} <code className="font-code text-caption text-muted">{p.plugin}</code>
      </Fact>
      <Fact label={p.objectKind === "Stanza" ? m.pgbr_object_stanza() : m.pgbr_object_archive()}>
        <Reported value={p.objectName} />
      </Fact>
      <Fact label={m.pgbr_stanza()}>
        {p.stanza ? <code className="font-code">{p.stanza}</code> : m.pgbr_stanza_from_agent()}
      </Fact>
      {p.archiveStanza && (
        <Fact label={m.pgbr_archive_stanza()}>
          <code className="font-code">{p.archiveStanza}</code>
        </Fact>
      )}
      <Fact label={m.pgbr_repository()}>
        {p.repository === undefined ? (
          <Reported value={undefined} />
        ) : (
          m.pgbr_repository_value({ n: String(p.repository) })
        )}
      </Fact>
      <Fact label={m.pgbr_encryption()}>
        {p.encrypted === undefined ? (
          <Reported value={undefined} />
        ) : p.encrypted ? (
          m.pgbr_encrypted()
        ) : (
          m.pgbr_not_encrypted()
        )}
      </Fact>
    </>
  );
}

const rc = columnHelper<PgBackRestRepoBackup>();
const repoColumns: Columns<PgBackRestRepoBackup> = rc.columns([
  rc.accessor("label", {
    header: () => m.col_label(),
    sortFn: "text",
    cell: (c) => <code className="font-code">{c.getValue()}</code>,
  }),
  rc.display({
    id: "type",
    header: () => m.col_type(),
    cell: (c) => <TypeBadge type={c.row.original.type} />,
  }),
  rc.display({
    id: "chain",
    header: () => m.col_chain(),
    cell: (c) => {
      const b = c.row.original;
      return (
        <span className="flex flex-col">
          <Chain label={b.label} full={b.full} prior={b.prior} />
          {b.references > 0 && (
            <span className="text-caption text-muted">{m.pgbr_references({ n: String(b.references) })}</span>
          )}
        </span>
      );
    },
  }),
  rc.display({
    id: "usable",
    header: () => m.col_usable(),
    cell: (c) => {
      const b = c.row.original;
      const reasons = b.unusableReasons ?? [];
      return (
        <span className="flex flex-col items-start gap-0.5">
          <StatusChip health={b.health} label={b.usable ? m.pgbr_usable() : m.pgbr_unusable_short()} size="sm" />
          {b.failed && <span className="text-caption font-semibold">{m.pgbr_failed()}</span>}
          {reasons.length > 0 && <code className="font-code text-caption">{reasons.join(", ")}</code>}
        </span>
      );
    },
  }),
  rc.accessor((r) => r.stoppedAt ?? r.startedAt ?? "", {
    id: "stopped",
    header: () => m.col_started_stopped(),
    sortFn: "datetime",
    cell: (c) => (
      <span>
        {when(c.row.original.startedAt)} → {when(c.row.original.stoppedAt)}
      </span>
    ),
  }),
  rc.display({
    id: "size",
    header: () => m.col_size(),
    cell: (c) =>
      m.pgbr_size_value({
        size: formatBytes(c.row.original.sizeBytes),
        stored: formatBytes(c.row.original.repositorySizeBytes),
      }),
  }),
  rc.display({
    id: "object",
    header: () => m.col_backup_object(),
    cell: (c) =>
      c.row.original.backupName ? (
        <code className="font-code">{c.row.original.backupName}</code>
      ) : (
        <span className="text-muted">{m.pgbr_no_backup_object()}</span>
      ),
  }),
]);

function archiveTypeWord(t: PgBackRestRetention["archiveType"]): string {
  return typeWord(t === "full" ? "full" : t === "diff" ? "diff" : "incr").toLowerCase();
}

/** The agent's comparison of the stanza with the repository's repo-retention-* options, as of the newest backup. */
function Retention({ r }: { r?: PgBackRestRetention }) {
  if (!r) {
    return <p className="text-label text-muted">{m.pgbr_retention_none()}</p>;
  }
  return (
    <Facts>
      <Fact label={m.pgbr_retention_result()}>
        <span className="flex flex-wrap items-center gap-2" data-pgbr-retention={r.result.state}>
          <StatusChip health={r.result.health} label={r.result.state} size="sm" />
          {r.result.code && <code className="font-code text-caption">{r.result.code}</code>}
          {r.result.message && <span>{r.result.message}</span>}
        </span>
      </Fact>
      <Fact label={m.pgbr_retention_full()}>
        {!r.configured || r.full === undefined
          ? m.pgbr_retention_unset()
          : r.fullType === "time"
            ? m.pgbr_retention_full_time({ n: String(r.full) })
            : m.pgbr_retention_full_count({ n: String(r.full) })}
      </Fact>
      {r.diff !== undefined && <Fact label={m.pgbr_retention_diff()}>{String(r.diff)}</Fact>}
      <Fact label={m.pgbr_retention_archive()}>
        {r.effectiveArchive === undefined
          ? m.pgbr_retention_archive_never()
          : m.pgbr_retention_archive_value({ n: String(r.effectiveArchive), type: archiveTypeWord(r.archiveType) })}
      </Fact>
      <Fact label={m.pgbr_retention_held()}>
        {m.pgbr_retention_held_value({
          full: String(r.fullBackups),
          diff: String(r.differentialBackups),
          incr: String(r.incrementalBackups),
        })}
      </Fact>
      <Fact label={m.pgbr_retention_evaluated()}>{when(r.evaluatedAt)}</Fact>
      {r.windowStartAt && (
        <Fact label={m.pgbr_retention_window_start()}>
          <Time iso={r.windowStartAt} />
        </Fact>
      )}
    </Facts>
  );
}

/** What the evidence agent read in a pgBackRest stanza: catalog integrity, its recovery window, its retention, its findings, and the stanza's backups with their chains. */
export function PgBackRestRepository({ p }: { p: PgBackRestEvidence }) {
  const w = p.recoveryWindow;
  const findings = p.findings ?? [];
  const assumptions = w?.assumptions ?? [];
  return (
    <div className="flex flex-col gap-4" data-pgbr-stanza={p.stanza}>
      <Panel title={m.panel_pgbackrest()}>
        <Facts>
          <Fact label={m.pgbr_stanza()}>
            <code className="font-code">{p.stanza}</code>
          </Fact>
          {p.archiveStanza !== p.stanza && (
            <Fact label={m.pgbr_archive_stanza()}>
              <code className="font-code">{p.archiveStanza}</code>
            </Fact>
          )}
          <Fact label={m.pgbr_repository()}>{m.pgbr_repository_value({ n: String(p.repository) })}</Fact>
          <Fact label={m.pgbr_encryption()}>
            {p.encrypted === undefined ? (
              <Reported value={undefined} />
            ) : p.encrypted ? (
              m.pgbr_encrypted()
            ) : (
              m.pgbr_not_encrypted()
            )}
          </Fact>
          <Fact label={m.pgbr_format()}>
            <Reported value={p.repositoryFormat} />
          </Fact>
          <Fact label={m.pgbr_version()}>
            <Reported value={p.pgbackrestVersion || undefined} />
          </Fact>
          <Fact label={m.pgbr_catalog()}>
            <span className="flex flex-wrap items-center gap-2">
              <StatusChip health={p.catalog.health} label={p.catalog.state} size="sm" />
              {p.catalog.code && <code className="font-code text-caption">{p.catalog.code}</code>}
              {p.catalog.message && <span>{p.catalog.message}</span>}
            </span>
          </Fact>
        </Facts>
        <h3 className="font-semibold text-label">{m.pgbr_window()}</h3>
        {w ? (
          <Facts>
            <Fact label={m.pgbr_window_from()}>
              <Time iso={w.startAt} /> · <code className="font-code">{w.oldestBackup}</code>
            </Fact>
            <Fact label={m.pgbr_window_to()}>
              {when(w.endReceiptAt)} · {m.pgbr_window_wal({ wal: w.endWal, timeline: String(w.endTimeline) })}
            </Fact>
            <Fact label={m.pgbr_window_newest()}>
              <code className="font-code">{w.newestBackup}</code> · <Time iso={w.newestStopAt} />
            </Fact>
            <Fact label={m.pgbr_window_usable()}>{String(w.usableBackups)}</Fact>
            {assumptions.length > 0 && (
              <Fact label={m.pgbr_window_assumptions()}>
                <code className="font-code text-caption">{assumptions.join(", ")}</code>
              </Fact>
            )}
          </Facts>
        ) : (
          <p className="health rounded-control px-3 py-1.5 text-label" data-health="warning">
            {m.pgbr_window_none()}
          </p>
        )}
        <h3 className="font-semibold text-label">{m.pgbr_retention()}</h3>
        <Retention r={p.retention} />
        <h3 className="font-semibold text-label">{m.pgbr_findings()}</h3>
        {findings.length === 0 ? (
          <p className="text-label text-muted">{m.pgbr_findings_none()}</p>
        ) : (
          <ul className="flex flex-col gap-1" aria-label={m.pgbr_findings()}>
            {findings.map((f) => (
              <li key={`${f.code}/${f.subject}`} className="flex flex-wrap items-center gap-2 text-label">
                <StatusChip health={f.health} label={f.severity} size="sm" />
                <code className="font-code">{f.code}</code>
                <code className="font-code text-caption text-muted">{f.subject}</code>
                {f.message && <span>{f.message}</span>}
              </li>
            ))}
          </ul>
        )}
        {p.findingsTruncated && <p className="text-caption text-muted">{m.pgbr_findings_truncated()}</p>}
      </Panel>
      <Titled title={m.caption_pgbr_backups()}>
        <DataTable
          caption={m.caption_pgbr_backups()}
          data={p.backups ?? []}
          columns={repoColumns}
          getRowId={(r) => r.label}
          empty={m.pgbr_backups_none()}
        />
        {p.backupsTruncated && <p className="text-caption text-muted">{m.pgbr_backups_truncated()}</p>}
      </Titled>
    </div>
  );
}
