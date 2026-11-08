import type { DrillCheck, OperationOffer, OperationRequest, RestoreDrill, RestoreDrills } from "../api/generated/model";
import { formatBytes } from "../ds/format";
import { StatusChip } from "../ds/patterns/StatusChip/StatusChip";
import { formatSeconds, Time } from "../ds/time";
import { m } from "../paraglide/messages.js";
import { DrillBackupFacts, OfferCard } from "./Operations";
import { Fact, Facts, Panel } from "./Panel";

function drillResultWord(r: RestoreDrill["result"]): string {
  switch (r) {
    case "running":
      return m.drill_result_running();
    case "passed":
      return m.drill_result_passed();
    case "failed":
      return m.drill_result_failed();
    case "timedOut":
      return m.drill_result_timedOut();
    default:
      return m.drill_result_cancelled();
  }
}

function checkName(n: DrillCheck["name"]): string {
  switch (n) {
    case "healthy":
      return m.drill_check_healthy();
    case "promoted":
      return m.drill_check_promoted();
    default:
      return m.drill_check_size();
  }
}

/** A check's outcome as a status chip: a check not run is "not checked", never a pass. */
function checkHealth(o: DrillCheck["outcome"]): "ok" | "critical" | "progressing" | "unknown" {
  switch (o) {
    case "passed":
      return "ok";
    case "failed":
      return "critical";
    case "pending":
      return "progressing";
    default:
      return "unknown";
  }
}

function checkWord(o: DrillCheck["outcome"]): string {
  switch (o) {
    case "passed":
      return m.drill_outcome_passed();
    case "failed":
      return m.drill_outcome_failed();
    case "pending":
      return m.drill_outcome_pending();
    default:
      return m.drill_outcome_notChecked();
  }
}

function DrillItem({ d }: { d: RestoreDrill }) {
  return (
    <li className="flex flex-col gap-2 rounded-panel border border-line p-3" data-drill={d.result}>
      <div className="flex flex-wrap items-center gap-2">
        <StatusChip health={d.health} label={drillResultWord(d.result)} size="sm" />
        <code className="font-code">{d.drill.name}</code>
        <span className="text-muted">
          <Time iso={d.startedAt} past />
        </span>
      </div>
      {d.reason && <p className="text-label font-semibold">{d.reason}</p>}
      <Facts>
        <DrillBackupFacts backup={d.backup} backupStoppedAt={d.backupStoppedAt} backupLabel={d.backupLabel} />
        <Fact label={m.drill_duration()}>
          {d.durationSeconds !== undefined ? (
            formatSeconds(d.durationSeconds)
          ) : d.result === "running" ? (
            <>
              {m.drill_deadline()} <Time iso={d.deadline} />
            </>
          ) : (
            m.not_reported()
          )}
        </Fact>
        {(d.restoredBytes !== undefined || d.sourceBytes !== undefined) && (
          <Fact label={m.drill_size()}>
            {m.drill_size_value({
              restored: d.restoredBytes !== undefined ? formatBytes(d.restoredBytes) : m.not_reported(),
              source: d.sourceBytes !== undefined ? formatBytes(d.sourceBytes) : m.not_reported(),
            })}
          </Fact>
        )}
        {d.timeline !== undefined && <Fact label={m.drill_timeline()}>{String(d.timeline)}</Fact>}
        <Fact label={m.drill_teardown()}>
          {d.tornDownAt ? <Time iso={d.tornDownAt} past /> : m.drill_not_torn_down()}
        </Fact>
      </Facts>
      {d.teardownError && (
        <p className="health rounded-control px-3 py-1.5 text-label" data-health="warning">
          {m.drill_teardown_error({ error: d.teardownError })}
        </p>
      )}
      <ul className="flex flex-col gap-1 text-label" aria-label={m.drill_checks()}>
        {(d.checks ?? []).map((c) => (
          <li key={c.name} className="flex flex-wrap items-center gap-2" data-check={c.name}>
            <StatusChip health={checkHealth(c.outcome)} label={checkWord(c.outcome)} size="sm" />
            <span className="font-semibold">{checkName(c.name)}</span>
            <span>{c.text}</span>
            <code className="w-full pl-1 font-code text-caption text-muted">{c.source}</code>
          </li>
        ))}
      </ul>
    </li>
  );
}

type Props = {
  drills: RestoreDrills;
  /** The restoreDrill offer of the operations list, when the reader may see it. */
  offer?: OperationOffer;
  actor?: string;
  onRequest?: (body: OperationRequest) => Promise<unknown>;
};

/**
 * Restore drills: the latest result first, in a sentence, then each drill with
 * its checks. A drill proves the backup restores and the instance comes up;
 * what it does not check is stated with every result.
 */
export function RestoreDrillsPanel({ drills, offer, actor, onRequest }: Props) {
  const items = drills.items ?? [];
  const running = items.find((d) => d.result === "running");
  return (
    <Panel title={m.panel_restore_drills()}>
      {!drills.enabled ? (
        <p className="text-label text-muted">{m.drill_disabled()}</p>
      ) : (
        <>
          <p className="text-label" data-drill-last={drills.lastPassedAt ? "passed" : "none"}>
            {drills.lastPassedAt ? (
              <>
                {m.drill_last_passed()} <Time iso={drills.lastPassedAt} past />
              </>
            ) : (
              m.drill_never_passed()
            )}
          </p>
          {running && (
            <p className="health rounded-control px-3 py-1.5 text-label" data-health="progressing">
              {m.drill_running({ name: running.drill.name })}
            </p>
          )}
          {offer && actor && onRequest && (
            <ul>
              <OfferCard offer={offer} actor={actor} onRequest={onRequest} />
            </ul>
          )}
          {items.length === 0 ? (
            <p className="text-label text-muted">{m.drill_none()}</p>
          ) : (
            <ul className="flex flex-col gap-3">
              {items.map((d) => (
                <DrillItem key={`${d.drill.namespace}/${d.drill.name}`} d={d} />
              ))}
            </ul>
          )}
          {(drills.notChecked ?? []).length > 0 && (
            <div className="text-caption text-muted">
              <p>{m.drill_not_checked()}</p>
              <ul className="list-disc pl-5">
                {(drills.notChecked ?? []).map((n) => (
                  <li key={n}>{n}</li>
                ))}
              </ul>
            </div>
          )}
          <p className="text-caption text-muted">{drills.scope}</p>
        </>
      )}
    </Panel>
  );
}
