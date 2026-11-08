import { Link } from "@tanstack/react-router";
import { useGetAdmin } from "../api/generated/endpoints/admin/admin";
import type { Admin, DeliveryState, DigestStatus } from "../api/generated/model";
import { Reported } from "../ds/NotReported";
import { type Columns, columnHelper, DataTable } from "../ds/patterns/DataTable/DataTable";
import { Provenance } from "../ds/patterns/Provenance/Provenance";
import { StatusChip } from "../ds/patterns/StatusChip/StatusChip";
import { formatUtc } from "../ds/time";
import { m } from "../paraglide/messages.js";
import { SourcesPanel } from "./Overview";
import { Fact, Facts, Panel } from "./Panel";
import { QueryState } from "./QueryState";

type Ns = NonNullable<Admin["namespaces"]>[number];
type PgAdminRow = NonNullable<Ns["pgAdmins"]>[number];
type AgentRow = NonNullable<Ns["evidenceAgents"]>[number];
type Runner = NonNullable<NonNullable<Admin["console"]>["runners"]>[number];

const httpsOnly = (u?: string) => (u && /^https:\/\//i.test(u) ? u : undefined);
const state = (r: { health: string; stale?: boolean; provenance: { observedAt?: string; stale?: boolean } }) => (
  <span className="flex flex-col items-start gap-0.5">
    <StatusChip
      health={r.health}
      size="sm"
      stale={r.stale || r.provenance.stale}
      observedAt={r.provenance.observedAt}
    />
    {r.stale && <span className="text-caption text-muted">{m.stale_generation()}</span>}
  </span>
);

const pc = columnHelper<PgAdminRow>();
const pgAdminColumns: Columns<PgAdminRow> = pc.columns([
  pc.accessor("name", {
    header: () => m.col_name(),
    sortFn: "text",
    cell: (c) => <span className="font-code">{c.getValue()}</span>,
  }),
  pc.display({ id: "health", header: () => m.col_health(), cell: (c) => state(c.row.original) }),
  pc.display({
    id: "url",
    header: () => m.col_url(),
    cell: (c) => {
      const url = httpsOnly(c.row.original.url);
      return url ? (
        <a href={url} target="_blank" rel="noreferrer noopener" className="text-accent underline">
          {m.open_pgadmin()} <span className="sr-only">{c.row.original.name}</span>
        </a>
      ) : (
        <Reported value={c.row.original.url} />
      );
    },
  }),
  pc.display({
    id: "version",
    header: () => m.col_version(),
    cell: (c) => <Reported value={c.row.original.version} />,
  }),
  pc.display({
    id: "servers",
    header: () => m.col_servers(),
    cell: (c) => {
      const servers = c.row.original.servers ?? [];
      if (servers.length === 0) return <Reported value={undefined} />;
      return (
        <ul className="flex flex-col gap-0.5">
          {servers.map((s) => (
            <li key={`${s.cluster}/${s.endpoint}`}>
              <code className="font-code">{s.cluster}</code> <span className="text-muted">({s.endpoint})</span>{" "}
              {s.state}
              {s.message && <span className="text-caption text-muted"> — {s.message}</span>}
            </li>
          ))}
        </ul>
      );
    },
  }),
]);

const ac = columnHelper<AgentRow>();
const agentColumns: Columns<AgentRow> = ac.columns([
  ac.accessor("name", {
    header: () => m.col_name(),
    sortFn: "text",
    cell: (c) => <span className="font-code">{c.getValue()}</span>,
  }),
  ac.display({ id: "health", header: () => m.col_health(), cell: (c) => state(c.row.original) }),
  ac.display({
    id: "endpoint",
    header: () => m.col_endpoint(),
    cell: (c) => (
      <code className="font-code">
        <Reported value={c.row.original.endpoint} />
      </code>
    ),
  }),
  ac.display({
    id: "fp",
    header: () => m.col_fingerprint(),
    cell: (c) => (
      <code className="font-code break-all text-caption">
        <Reported value={c.row.original.fingerprint} />
      </code>
    ),
  }),
]);

const rc = columnHelper<Runner>();
const runnerColumns: Columns<Runner> = rc.columns([
  rc.accessor("name", {
    header: () => m.col_name(),
    sortFn: "text",
    cell: (c) => <code className="font-code">{c.getValue()}</code>,
  }),
  rc.display({
    id: "running",
    header: () => m.col_running(),
    cell: (c) => (c.row.original.running ? m.yes() : <span className="font-semibold">{m.no()}</span>),
  }),
  rc.display({ id: "core", header: () => m.col_core(), cell: (c) => (c.row.original.core ? m.yes() : m.no()) }),
  rc.accessor("restarts", {
    header: () => m.col_restarts(),
    sortFn: "alphanumeric",
    cell: (c) => <span className="num">{c.getValue()}</span>,
  }),
  rc.display({
    id: "err",
    header: () => m.col_last_error(),
    cell: (c) => <Reported value={c.row.original.lastError} />,
  }),
]);

function channelWord(c: string): string {
  if (c === "chat") return m.digest_channel_chat();
  if (c === "webhook") return m.digest_channel_webhook();
  if (c === "mail") return m.digest_channel_mail();
  return c;
}

const when = (iso?: string) => (iso ? formatUtc(iso) : undefined);

const dc = columnHelper<DeliveryState>();
const deliveryColumns: Columns<DeliveryState> = dc.columns([
  dc.accessor("channel", {
    header: () => m.digest_col_channel(),
    sortFn: "text",
    cell: (c) => channelWord(c.getValue()),
  }),
  dc.display({
    id: "configured",
    header: () => m.digest_col_configured(),
    cell: (c) =>
      c.row.original.configured ? (
        m.digest_configured()
      ) : (
        <span className="text-muted">{m.digest_not_configured()}</span>
      ),
  }),
  dc.display({
    id: "lastAttempt",
    header: () => m.digest_col_last_attempt(),
    cell: (c) =>
      c.row.original.configured ? (
        <span className="flex flex-col">
          <Reported value={when(c.row.original.lastAttempt)} />
          {c.row.original.digestId && (
            <Link
              to="/digests/$id"
              params={{ id: c.row.original.digestId }}
              className="font-code text-caption text-accent hover:underline"
            >
              {c.row.original.digestId}
            </Link>
          )}
        </span>
      ) : null,
  }),
  dc.display({
    id: "lastSuccess",
    header: () => m.digest_col_last_success(),
    cell: (c) => (c.row.original.configured ? <Reported value={when(c.row.original.lastSuccess)} /> : null),
  }),
  dc.display({
    id: "attempts",
    header: () => m.digest_col_attempts(),
    cell: (c) =>
      c.row.original.configured ? (
        <span className="flex flex-col">
          <span className="num">
            <Reported value={c.row.original.attempts} />
          </span>
          {c.row.original.pending && <span className="font-semibold text-caption">{m.digest_pending()}</span>}
        </span>
      ) : null,
  }),
  dc.display({
    id: "lastError",
    header: () => m.col_last_error(),
    cell: (c) =>
      c.row.original.lastError ? (
        <span className="health inline-block rounded-control px-1.5 text-caption" data-health="warning">
          {c.row.original.lastError}
        </span>
      ) : c.row.original.configured ? (
        <span className="text-muted">—</span>
      ) : null,
  }),
]);

/** The daily digest: its schedule and, per channel, the delivery state (Go's DigestStatus). */
export function DigestCard({ digest: d }: { digest: DigestStatus }) {
  return (
    <Panel title={m.digest_admin_title()}>
      {!d.enabled ? (
        <p className="m-0 text-label text-muted">{m.digest_disabled_detail()}</p>
      ) : (
        <>
          <Facts>
            <Fact label={m.digest_schedule_hour()}>
              {m.digest_hour_utc({ hour: String(d.schedule.hour).padStart(2, "0") })}
            </Fact>
            <Fact label={m.digest_schedule_next()}>
              <Reported value={when(d.schedule.next)} />
            </Fact>
            <Fact label={m.digest_schedule_retention()}>
              <code className="font-code">{d.schedule.retention}</code>{" "}
              <span className="text-muted">
                · {d.schedule.persistent ? m.digest_persistent() : m.digest_not_persistent()}
              </span>
            </Fact>
            <Fact label={m.digest_last()}>
              {d.last ? (
                <Link to="/digests/$id" params={{ id: d.last }} className="font-code text-accent hover:underline">
                  {d.last}
                </Link>
              ) : (
                <Reported value={undefined} />
              )}
            </Fact>
          </Facts>
          <DataTable
            caption={m.digest_caption_delivery()}
            data={d.channels ?? []}
            columns={deliveryColumns}
            getRowId={(r) => r.channel}
            empty={m.digest_no_channels()}
          />
        </>
      )}
    </Panel>
  );
}

/** The empty text of a namespace's table: "none" only when its list was read. */
function emptyFor(ns: Ns, source: string): string {
  const state = ns.sources?.find((s) => s.source === source)?.state;
  return state === undefined || state === "live" || state === "stale" ? m.admin_none() : m.admin_not_read();
}

function Section({ ns }: { ns: Ns }) {
  return (
    <Panel title={m.admin_namespace({ name: ns.name })}>
      {ns.pgAdminsVisible ? (
        <DataTable
          caption={m.caption_pgadmins()}
          data={ns.pgAdmins ?? []}
          columns={pgAdminColumns}
          getRowId={(r) => r.name}
          empty={emptyFor(ns, "pgAdmins")}
        />
      ) : (
        <p className="text-label text-muted">{m.admin_not_visible({ kind: "PgAdmin" })}</p>
      )}
      {ns.evidenceAgentsVisible ? (
        <DataTable
          caption={m.caption_agents()}
          data={ns.evidenceAgents ?? []}
          columns={agentColumns}
          getRowId={(r) => r.name}
          empty={emptyFor(ns, "evidenceAgents")}
        />
      ) : (
        <p className="text-label text-muted">{m.admin_not_visible({ kind: "EvidenceAgent" })}</p>
      )}
      <SourcesPanel sources={ns.sources} title={m.sources_of({ what: ns.name })} />
    </Panel>
  );
}

/** The admin screen (namespace admin persona): PgAdmin and EvidenceAgent objects, and the console's own health. */
export function AdminView({ data }: { data: Admin }) {
  const namespaces = data.namespaces ?? [];
  const c = data.console;
  return (
    <div className="flex flex-col gap-4">
      <p className="text-label text-muted">{m.admin_intro()}</p>
      {namespaces.length === 0 ? (
        <p className="text-label">{m.admin_no_namespaces()}</p>
      ) : (
        namespaces.map((ns) => <Section key={ns.name} ns={ns} />)
      )}
      <Panel title={m.panel_console()}>
        {!c ? (
          <p className="text-label text-muted">{m.console_hidden()}</p>
        ) : (
          <>
            <Facts>
              <Fact label={m.console_version()}>
                <Reported value={c.version} />
              </Fact>
              <Fact label={m.console_namespace()}>
                <Reported value={c.namespace} />
              </Fact>
              <Fact label={m.console_watches()}>
                <Reported value={(c.watchNamespaces ?? []).join(", ") || undefined} />
              </Fact>
            </Facts>
            <DataTable
              caption={m.caption_runners()}
              data={c.runners ?? []}
              columns={runnerColumns}
              getRowId={(r) => r.name}
            />
            <SourcesPanel sources={c.sources} title={m.sources_of({ what: m.panel_console() })} />
            <Provenance provenance={c.provenance} />
          </>
        )}
      </Panel>
      {c?.digest && <DigestCard digest={c.digest} />}
    </div>
  );
}

export function AdminScreen() {
  const query = useGetAdmin();
  return (
    <QueryState query={query} what={m.nav_admin()}>
      {(data) => <AdminView data={data} />}
    </QueryState>
  );
}
