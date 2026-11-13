import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";
import { useGetCluster } from "../api/generated/endpoints/clusters/clusters";
import type {
  ClusterOverviewView,
  ComponentVersion,
  EventRow,
  InstanceCard,
  SourceState,
  WaitReason,
} from "../api/generated/model";
import { useSession } from "../api/session";
import { Reported } from "../ds/NotReported";
import { BlindSpots } from "../ds/patterns/BlindSpots/BlindSpots";
import { Provenance } from "../ds/patterns/Provenance/Provenance";
import { StatusChip } from "../ds/patterns/StatusChip/StatusChip";
import { VerdictBanner } from "../ds/patterns/VerdictBanner/VerdictBanner";
import { Time } from "../ds/time";
import { m } from "../paraglide/messages.js";
import { AccessPanel } from "./Access";
import { FailoverSummary } from "./Failover";
import { Fact, Facts, Panel } from "./Panel";
import { PersonalServerFor } from "./PersonalServer";
import { QueryState } from "./QueryState";
import { phraseText } from "./recoveryPhrase";
import { sourceStateWord } from "./sources";
import { StatusReadHint, statusReadWord } from "./statusRead";
import { TopologyPanel } from "./Topology";

function primaryStateWord(state: string): string {
  switch (state) {
    case "stable":
      return m.primary_state_stable();
    case "switchover":
      return m.primary_state_switchover();
    case "failoverPending":
      return m.primary_state_failover_pending();
    case "failover":
      return m.primary_state_failover();
    case "none":
      return m.primary_none();
    default:
      return m.health_unknown();
  }
}

function list(items: string[] | null | undefined) {
  return items && items.length > 0 ? items.join(", ") : undefined;
}

export function PrimaryPanel({ p }: { p: ClusterOverviewView["primary"] }) {
  return (
    <Panel title={m.panel_primary()}>
      <Facts>
        <Fact label={m.primary_state()}>{primaryStateWord(p.state)}</Fact>
        <Fact label={m.primary_current()}>
          <Reported value={p.current} />
        </Fact>
        <Fact label={m.primary_target()}>
          <Reported value={p.target} />
        </Fact>
        <Fact label={m.primary_since()}>{p.since ? <Time iso={p.since} /> : <Reported value={p.since} />}</Fact>
        {p.failingSince && (
          <Fact label={m.primary_failing_since()}>
            <Time iso={p.failingSince} />
          </Fact>
        )}
        <Fact label={m.primary_live()}>
          <Reported value={list(p.livePrimaries)} />
        </Fact>
        <Fact label={m.primary_labels()}>
          <Reported value={list(p.labelPrimaries)} />
        </Fact>
        <Fact label={m.primary_lease()}>
          {p.lease ? (
            <>
              <Reported value={p.lease.holder} />
              {p.lease.renewedAt && (
                <>
                  {" · "}
                  {m.lease_renewed()} <Time iso={p.lease.renewedAt} past />
                </>
              )}
            </>
          ) : (
            <Reported value={undefined} />
          )}
        </Fact>
      </Facts>
      <Provenance provenance={p.provenance} />
    </Panel>
  );
}

function Cards({ cards, params }: { cards: InstanceCard[] | null; params: { namespace: string; cluster: string } }) {
  return (
    <Panel
      title={m.panel_instances()}
      actions={
        <Link to="/c/$namespace/$cluster/instances" params={params} className="text-label text-accent hover:underline">
          {m.see_all_instances()}
        </Link>
      }
    >
      {!cards || cards.length === 0 ? (
        <p className="text-label text-muted">{m.instances_none_reported()}</p>
      ) : (
        <ul className="grid grid-cols-1 gap-2 sm:grid-cols-2 lg:grid-cols-3">
          {cards.map((c) => (
            <li key={c.name} className="flex flex-col gap-1 rounded-control border border-line p-3">
              <div className="flex items-center justify-between gap-2">
                <span className="font-code font-semibold">{c.name}</span>
                <StatusChip health={c.health} size="sm" />
              </div>
              <p className="text-label text-muted">
                {c.role === "primary" ? m.role_primary() : c.role === "replica" ? m.role_replica() : m.role_unknown()}
                {" · "}
                {c.ready === undefined ? m.ready_not_reported() : c.ready ? m.ready_yes() : m.ready_no()}
                {c.fenced && ` · ${m.fenced()}`}
              </p>
              <p className="text-label">
                {m.status_read()}: {statusReadWord(c.statusRead)}
              </p>
              {c.detail && c.detail !== statusReadWord(c.statusRead) && <p className="text-label">{c.detail}</p>}
              <StatusReadHint state={c.statusRead} />
            </li>
          ))}
        </ul>
      )}
    </Panel>
  );
}

function EventList({ events }: { events: EventRow[] | null | undefined }) {
  if (!events || events.length === 0) return <p className="text-label text-muted">{m.warnings_none()}</p>;
  return (
    <ul className="flex flex-col gap-2">
      {events.map((e) => (
        <li
          key={`${e.reason}-${e.lastSeen}-${e.object.kind}-${e.object.name ?? ""}`}
          className="flex flex-col gap-0.5 text-label"
        >
          <span className="flex flex-wrap items-center gap-2">
            <StatusChip health={e.health} label={e.reason} size="sm" />
            <span className="font-code text-muted">
              {e.object.kind}/{e.object.name ?? "—"}
            </span>
            <Time iso={e.lastSeen} className="text-muted" past />
            {e.count > 1 && <span className="text-muted">×{e.count}</span>}
          </span>
          <span>{e.message ?? m.not_reported()}</span>
        </li>
      ))}
    </ul>
  );
}

export function BackupsPanel({ b }: { b: ClusterOverviewView["backups"] }) {
  const notReported = b.notReported ?? [];
  const arch = b.archiving;
  return (
    <Panel title={m.panel_backups()}>
      <div className="flex flex-wrap items-center gap-2">
        <StatusChip health={b.health} stale={b.provenance.stale} observedAt={b.provenance.observedAt} />
        {!b.configured && <span className="text-label">{m.backup_not_configured()}</span>}
        {b.pluginBased && <span className="text-label text-muted">{m.backup_plugin_based()}</span>}
      </div>
      <Facts>
        <Fact label={m.backup_last_completed()}>
          {b.lastCompletedAt ? (
            <Time iso={b.lastCompletedAt} />
          ) : (
            <span className="text-muted">{m.backup_none_completed()}</span>
          )}
          {b.cadence?.overdue && (
            <span className="block text-caption font-semibold">
              {b.cadence.basis === "schedule" ? m.backup_overdue_schedule() : m.backup_overdue_default()}
            </span>
          )}
          {b.cadence && !b.cadence.overdue && b.cadence.basis === "default" && (
            <span className="block text-caption text-muted">{m.backup_no_cadence_known()}</span>
          )}
        </Fact>
        {b.lastFailedAt && (
          <Fact label={m.backup_last_failed()}>
            <Time iso={b.lastFailedAt} />
          </Fact>
        )}
        <Fact label={m.backup_next()}>
          {b.nextScheduledAt ? <Time iso={b.nextScheduledAt} /> : <Reported value={undefined} />}
        </Fact>
        <Fact label={m.backup_window()}>
          {b.recoveryWindow ? (
            <>
              {b.recoveryWindow.first ? <Time iso={b.recoveryWindow.first} /> : m.not_reported()}
              {" → "}
              {b.recoveryWindow.last && b.recoveryWindow.provenance.origin === "operator" && (
                // serverRecoveryWindow's lastSuccessfulBackupTime is the last
                // base backup, not the end of the archived WAL.
                <span className="text-muted">{m.backup_window_last_base_backup()} </span>
              )}
              {b.recoveryWindow.last ? <Time iso={b.recoveryWindow.last} /> : m.not_reported()}
            </>
          ) : notReported.length > 0 ? (
            <span className="text-muted">{m.backup_not_reported_by_plugin()}</span>
          ) : (
            <Reported value={undefined} />
          )}
        </Fact>
      </Facts>
      {notReported.length > 0 && (
        <p className="text-caption text-muted">{m.backup_not_reported_list({ fields: notReported.join(", ") })}</p>
      )}
      <h3 className="mt-2 font-semibold text-label">{m.archiving()}</h3>
      <div className="flex flex-wrap items-center gap-2">
        <StatusChip
          health={arch.health}
          stale={arch.provenance.stale}
          observedAt={arch.provenance.observedAt}
          staleReason={arch.provenance.staleReason}
        />
        {arch.reason && <span className="text-label">{arch.reason}</span>}
      </div>
      <Facts>
        <Fact label={m.archiving_condition()}>
          <Reported value={arch.conditionStatus} />
        </Fact>
        <Fact label={m.archiving_live()}>
          {arch.liveArchiving === undefined ? <Reported value={undefined} /> : arch.liveArchiving ? m.yes() : m.no()}
        </Fact>
        <Fact label={m.archiving_backlog()}>
          <Reported value={arch.readyWalFiles} />
        </Fact>
        <Fact label={m.archiving_last_archived()}>
          {arch.lastArchivedAt ? (
            // pg_stat_archiver.last_archived_time, the archiving instance's
            // own report as of the provenance below.
            <span className="flex flex-col" data-last-archived="">
              <span>
                <Time iso={arch.lastArchivedAt} past />
                {arch.lastArchivedWal && <code className="ml-2 font-code text-caption">{arch.lastArchivedWal}</code>}
              </span>
              <span className="text-caption text-muted">{m.archiving_last_archived_source()}</span>
            </span>
          ) : (
            <Reported value={undefined} />
          )}
        </Fact>
        <Fact label={m.archiving_last_failed()}>
          <Reported value={arch.lastFailedWal} />
        </Fact>
      </Facts>
      <Provenance provenance={arch.provenance} />
    </Panel>
  );
}

export function SourcesPanel({ sources, title }: { sources: SourceState[] | null; title?: string }) {
  if (!sources || sources.length === 0) return null;
  return (
    <Panel title={title ?? m.panel_sources()}>
      <ul className="flex flex-col gap-1 text-label">
        {sources.map((s) => (
          <li key={s.source} className="flex flex-wrap gap-x-2">
            <code className="font-code">{s.source}</code>
            <StatusChip health={s.health} label={sourceStateWord(s.state)} size="sm" />
            {s.since && (
              <span className="text-muted">
                {m.since()} <Time iso={s.since} />
              </span>
            )}
            {s.reason && <span className="text-muted">— {s.reason}</span>}
          </li>
        ))}
      </ul>
    </Panel>
  );
}

/**
 * Why the operator waits, keyed on Go's reason codes. A reason the console
 * derived is labelled a heuristic, one the operator reported is labelled as
 * such; each lists the facts it rests on, naming their source.
 */
function WaitReasons({ reasons }: { reasons: WaitReason[] | null | undefined }) {
  if (!reasons || reasons.length === 0) return null;
  return (
    <ul className="flex flex-col gap-2">
      {reasons.map((r) => (
        <li key={r.id} className="flex flex-col gap-1 rounded-control border border-line p-2" data-reason={r.id}>
          <p className="flex flex-wrap items-center gap-2">
            <span
              className="rounded-control border border-line px-1.5 text-caption text-muted"
              data-heuristic={r.heuristic}
            >
              {r.heuristic ? m.wait_heuristic() : m.wait_reported()}
            </span>
            <span>{r.text}</span>
            <code className="font-code text-caption text-muted">{r.id}</code>
          </p>
          {(r.evidence ?? []).length > 0 && (
            <ul className="flex list-disc flex-col gap-0.5 pl-6 text-label text-muted">
              {(r.evidence ?? []).map((e) => (
                <li key={e}>
                  <code className="font-code break-all">{e}</code>
                </li>
              ))}
            </ul>
          )}
        </li>
      ))}
    </ul>
  );
}

/** The pgAdmins this Cluster is registered in that the reader may open (dba-only: absent for everyone else). */
function PgAdminPanel({
  links,
  unread,
  namespace,
  cluster,
}: {
  links: ClusterOverviewView["pgAdmins"];
  unread: ClusterOverviewView["pgAdminsUnread"];
  namespace: string;
  cluster: string;
}) {
  const actor = useSession().data?.username;
  if (!unread && (!links || links.length === 0)) return null;
  return (
    <Panel title={m.panel_pgadmin()}>
      {unread && (
        <p className="text-label text-muted" data-pgadmin="unread">
          {m.pgadmin_unread()}
        </p>
      )}
      <ul className="flex flex-col gap-1 text-label">
        {(links ?? []).map((l) => (
          <li key={l.name} className="flex flex-col gap-1">
            <div className="flex flex-wrap items-center gap-2">
              <StatusChip health={l.health} size="sm" />
              {l.url && /^https:\/\//i.test(l.url) ? (
                <a href={l.url} target="_blank" rel="noreferrer noopener" className="text-accent underline">
                  {m.pgadmin_open({ name: l.name })}
                </a>
              ) : (
                <span>
                  <code className="font-code">{l.name}</code> <span className="text-muted">({m.pgadmin_no_url()})</span>
                </span>
              )}
              {l.serverState && <span className="text-muted">{m.pgadmin_server({ state: l.serverState })}</span>}
            </div>
          </li>
        ))}
      </ul>
      {actor && <PersonalServerFor namespace={namespace} cluster={cluster} actor={actor} />}
    </Panel>
  );
}

/**
 * The overview of one Cluster: the real roll-up first, then the wiring drawing
 * full width, then what it all rests on. `wiring` is the drawing's panel (its
 * own query); a story may leave it out.
 */
export function OverviewView({ data, wiring }: { data: ClusterOverviewView; wiring?: ReactNode }) {
  const params = { namespace: data.ref.namespace, cluster: data.ref.name };
  const hatches = data.escapeHatches ?? [];
  return (
    <div className="flex flex-col gap-4">
      {!data.present && (
        <p role="status" className="health rounded-panel p-3" data-health="inactive" data-stale="">
          {m.cluster_absent()}
        </p>
      )}
      <VerdictBanner verdict={data.verdict} title={m.verdict_title()} params={params} />
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-label">
        <span>
          {m.col_phase()}:{" "}
          {data.phase.text ? (
            <StatusChip health={data.phase.health} label={data.phase.text} size="sm" />
          ) : (
            <Reported value={undefined} />
          )}
        </span>
        {data.phase.text && !data.phase.known && <span className="text-muted">{m.phase_unknown_to_table()}</span>}
        {data.phase.cap && (
          <span className="text-muted" data-phase-cap={data.phase.cap.code}>
            {phraseText(data.phase.cap.code, data.phase.cap.params, data.phase.cap.text)}
          </span>
        )}
        {data.phase.since && (
          <span className="text-muted">
            {m.since()} <Time iso={data.phase.since} />
          </span>
        )}
        {data.version.components && data.version.components.length > 0 ? (
          <Versions components={data.version.components} />
        ) : (
          <span className="text-muted">
            CNPG <Reported value={data.version.cnpg} /> ({data.version.confidence})
          </span>
        )}
        <span className="text-muted">
          {m.generated_at()} <Time iso={data.generatedAt} />
        </span>
      </div>
      {data.waiting && (
        <Panel title={m.panel_waiting()}>
          {data.waiting.phaseReason && <p>{data.waiting.phaseReason}</p>}
          <WaitReasons reasons={data.waiting.reasons} />
          {data.waiting.filteredCacheTrap && (
            <p className="health rounded-control p-2 text-label" data-health="warning">
              {m.filtered_cache_trap()}
            </p>
          )}
          <EventList events={data.waiting.causes} />
        </Panel>
      )}
      {hatches.length > 0 && (
        <Panel title={m.panel_escape_hatches()}>
          <ul className="flex flex-col gap-2 text-label">
            {hatches.map((h) => (
              <li key={h.annotation} className="flex flex-col gap-0.5">
                <span className="flex flex-wrap items-center gap-2">
                  <StatusChip health={h.health} size="sm" />
                  <code className="font-code">
                    {h.annotation}={h.value}
                  </code>
                </span>
                <span>{h.effect}</span>
              </li>
            ))}
          </ul>
        </Panel>
      )}
      {wiring}
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <PrimaryPanel p={data.primary} />
        <BackupsPanel b={data.backups} />
      </div>
      <FailoverSummary f={data.failover} params={params} />
      <Cards cards={data.cards} params={params} />
      <Panel title={m.panel_conditions()}>
        {!data.conditions || data.conditions.length === 0 ? (
          <p className="text-label text-muted">{m.conditions_none()}</p>
        ) : (
          <ul className="flex flex-col gap-2 text-label">
            {data.conditions.map((c) => (
              <li key={c.type} className="flex flex-col gap-0.5">
                <span className="flex flex-wrap items-center gap-2">
                  <StatusChip health={c.health} label={`${c.type}: ${c.status}`} size="sm" />
                  {c.reason && <code className="font-code">{c.reason}</code>}
                  {c.lastTransition && <Time iso={c.lastTransition} className="text-muted" />}
                  {!c.known && <span className="text-muted">{m.condition_unknown_type()}</span>}
                  {c.cap && (
                    <span className="text-muted" data-condition-cap={c.cap.code}>
                      {phraseText(c.cap.code, c.cap.params, c.cap.text)}
                    </span>
                  )}
                </span>
                {c.message && <span>{c.message}</span>}
              </li>
            ))}
          </ul>
        )}
      </Panel>
      <Panel title={m.panel_recent_warnings()}>
        <EventList events={data.recentWarnings} />
      </Panel>
      <SourcesPanel sources={data.sources} />
      <PgAdminPanel
        links={data.pgAdmins}
        unread={data.pgAdminsUnread}
        namespace={params.namespace}
        cluster={params.cluster}
      />
      <AccessPanel capabilities={data.capabilities} namespace={params.namespace} cluster={params.cluster} />
      <BlindSpots items={data.blindSpots} />
    </div>
  );
}

export function OverviewScreen({ namespace, cluster }: { namespace: string; cluster: string }) {
  const query = useGetCluster(namespace, cluster);
  return (
    <QueryState query={query} what={m.nav_overview()}>
      {(data) => <OverviewView data={data} wiring={<TopologyPanel namespace={namespace} cluster={cluster} />} />}
    </QueryState>
  );
}

/**
 * The versions in play, each placed in the lifecycle table: a line past its
 * end of life, or within six months of it, says so with its date; a
 * supported line's date is in its tooltip with where the version was read.
 */
function Versions({ components }: { components: ComponentVersion[] }) {
  return (
    <span className="flex flex-wrap items-center gap-x-3 gap-y-1">
      <span className="text-muted">{m.versions_label()}</span>
      {components.map((c) => {
        const date = c.endsOn ?? "";
        const title = [
          m.version_source({ source: c.source }),
          c.support === "supported" && date ? m.support_until({ date }) : "",
        ]
          .filter(Boolean)
          .join(" · ");
        return (
          <span key={c.component} title={title} className="flex items-center gap-1">
            <span className="text-muted">{c.component}</span>
            <span className="font-code">{c.version}</span>
            {c.support === "ended" && <StatusChip health="warning" label={m.support_ended({ date })} size="sm" />}
            {c.support === "endingSoon" && (
              <StatusChip health="progressing" label={m.support_ending({ date })} size="sm" />
            )}
          </span>
        );
      })}
    </span>
  );
}
