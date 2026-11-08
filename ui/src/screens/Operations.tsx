import { useId, useState } from "react";
import { useListOperations } from "../api/generated/endpoints/operations/operations";
import { useGetSession } from "../api/generated/endpoints/session/session";
import type { OperationOffer, OperationRequest, Operations } from "../api/generated/model";
import { trackedOperations, useRequestOperation } from "../api/operations";
import { formatBytes } from "../ds/format";
import { ConfirmDialog } from "../ds/patterns/ConfirmDialog/ConfirmDialog";
import { operationHealth, phaseWord } from "../ds/patterns/OperationTracker/OperationTracker";
import { StatusChip } from "../ds/patterns/StatusChip/StatusChip";
import { formatSeconds, Time } from "../ds/time";
import { m } from "../paraglide/messages.js";
import { Fact, Facts, Panel } from "./Panel";
import { QueryState } from "./QueryState";

type Props = {
  data: Operations;
  /** The signed-in user, as the operation will be recorded. */
  actor: string;
  /** POSTs the request; resolves on 202 (the tracker takes over), throws ApiError otherwise. */
  onRequest: (body: OperationRequest) => Promise<unknown>;
};

const verbs = (o: OperationOffer) => (o.requires ?? []).join(", ");

function TargetPicker({
  offer,
  value,
  onChange,
}: {
  offer: OperationOffer;
  value: string;
  onChange: (v: string) => void;
}) {
  const name = useId();
  return (
    <fieldset className="flex flex-col gap-1">
      <legend className="mb-1 font-semibold text-label">{m.ops_choose_target()}</legend>
      {(offer.targets ?? []).map((t) => (
        <label key={t.name} className="flex flex-wrap items-center gap-2 text-label" data-eligible={t.eligible}>
          <input
            type="radio"
            name={name}
            value={t.name}
            checked={value === t.name}
            disabled={!t.eligible}
            onChange={() => onChange(t.name)}
          />
          <code className="font-code">{t.name}</code>
          <StatusChip health={t.health} size="sm" />
          {t.lagBytes !== undefined && (
            <span className="text-muted">{m.ops_target_lag({ lag: formatBytes(t.lagBytes) })}</span>
          )}
          {!t.eligible && <span className="font-semibold">{m.ops_target_ineligible()}</span>}
          {(t.blockers ?? []).map((b) => (
            <span key={b.reason} className="w-full pl-6 text-caption text-muted">
              {b.text} <code className="font-code">{b.source}</code>
            </span>
          ))}
        </label>
      ))}
    </fieldset>
  );
}

/** The backup a drill restores: its name, when it stopped, and the repository's label. */
export function DrillBackupFacts({
  backup,
  backupStoppedAt,
  backupLabel,
}: {
  backup?: string;
  backupStoppedAt?: string;
  backupLabel?: string;
}) {
  return (
    <>
      <Fact label={m.drill_backup()}>
        {backup ? <code className="font-code">{backup}</code> : m.not_reported()}
        {backupStoppedAt && (
          <span className="text-muted">
            {" "}
            · <Time iso={backupStoppedAt} past />
          </span>
        )}
      </Fact>
      {backupLabel && (
        <Fact label={m.drill_backup_label()}>
          <code className="font-code">{backupLabel}</code>
        </Fact>
      )}
    </>
  );
}

/** What a restore drill will do, stated before it is requested and again in the dialog. */
function DrillOfferDetails({ offer }: { offer: OperationOffer }) {
  const d = offer.drill;
  if (!d) return null;
  return (
    <Facts>
      <Fact label={m.drill_namespace()}>
        <code className="font-code">{d.namespace}</code>
      </Fact>
      <DrillBackupFacts backup={d.backup} backupStoppedAt={d.backupStoppedAt} backupLabel={d.backupLabel} />
      {d.recoverySource && (
        <Fact label={m.drill_recovery_source()}>
          <code className="font-code">{d.recoverySource}</code>
        </Fact>
      )}
      <Fact label={m.drill_time_limit()}>{formatSeconds(d.timeLimitSeconds)}</Fact>
    </Facts>
  );
}

/** One offer: what it does, whether the reader may, and the dialog that requests it. */
export function OfferCard({
  offer,
  actor,
  onRequest,
}: {
  offer: OperationOffer;
  actor: string;
  onRequest: Props["onRequest"];
}) {
  const [open, setOpen] = useState(false);
  const [target, setTarget] = useState("");
  const blockers = offer.blockers ?? [];
  const usable = offer.allowed && !offer.undecided && blockers.length === 0;
  const promote = offer.kind === "promote";
  const confirmWord = promote ? target : offer.confirmation;
  return (
    <li className="flex flex-col gap-2 rounded-panel border border-line p-3" data-offer={offer.kind}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="font-semibold">{offer.title}</h3>
        <button
          type="button"
          disabled={!usable}
          onClick={() => setOpen(true)}
          className="h-9 rounded-control border border-line-strong px-3 font-semibold hover:bg-surface-2 disabled:opacity-60"
        >
          {m.ops_request()}
          <span className="sr-only">: {offer.title}</span>
        </button>
      </div>
      <p className="text-label">{offer.summary}</p>
      <DrillOfferDetails offer={offer} />
      {offer.undecided ? (
        <p className="health rounded-control px-3 py-1.5 text-label" data-health="unknown">
          {m.ops_undecided()}
        </p>
      ) : !offer.allowed ? (
        <p className="health rounded-control px-3 py-1.5 text-label" data-health="inactive">
          {m.ops_denied({ verbs: verbs(offer) })}
        </p>
      ) : (
        <p className="text-caption text-muted">{m.ops_requires({ verbs: verbs(offer) })}</p>
      )}
      {blockers.length > 0 && (
        <div className="health rounded-control px-3 py-1.5 text-label" data-health="warning">
          <p className="font-semibold">{m.ops_blocked_by()}</p>
          <ul className="list-disc pl-5">
            {blockers.map((b) => (
              <li key={b.reason}>
                {b.text} <code className="font-code text-caption">{b.source}</code>
              </li>
            ))}
          </ul>
        </div>
      )}
      {!offer.observable && <p className="text-caption text-muted">{m.ops_not_observable()}</p>}
      {usable && (
        <ConfirmDialog
          open={open}
          onOpenChange={(o) => {
            setOpen(o);
            if (!o) setTarget("");
          }}
          title={offer.title}
          consequence={offer.consequence}
          expectedDuration={offer.expectedDuration || undefined}
          confirmWord={confirmWord}
          actor={actor}
          onConfirm={async () => {
            await onRequest({
              kind: offer.kind,
              target: promote ? target : undefined,
              confirmation: confirmWord,
            });
          }}
        >
          {offer.drill && <DrillOfferDetails offer={offer} />}
          {promote && (
            <>
              <TargetPicker offer={offer} value={target} onChange={setTarget} />
              {!target && <p className="text-caption text-muted">{m.ops_choose_first()}</p>}
            </>
          )}
        </ConfirmDialog>
      )}
    </li>
  );
}

/** The operations screen: what the reader may do here and why not, and the operations this console replica issued. */
export function OperationsView({ data, actor, onRequest }: Props) {
  const offers = data.offers ?? [];
  const recent = data.recent ?? [];
  return (
    <div className="flex flex-col gap-4">
      <p className="text-label text-muted">{m.ops_intro()}</p>
      <Panel title={m.ops_offers()}>
        {offers.length === 0 ? (
          <p className="text-label text-muted">{m.ops_none()}</p>
        ) : (
          <ul className="grid grid-cols-1 gap-3 lg:grid-cols-2">
            {offers.map((o) => (
              <OfferCard key={o.kind} offer={o} actor={actor} onRequest={onRequest} />
            ))}
          </ul>
        )}
      </Panel>
      <Panel title={m.ops_recent()}>
        <p className="text-caption text-muted">{data.recentScope}</p>
        {recent.length === 0 ? (
          <p className="text-label text-muted">{m.ops_recent_none()}</p>
        ) : (
          <ul className="flex flex-col gap-2 text-label">
            {recent.map((op) => (
              <li key={op.operationId} className="flex flex-wrap items-center gap-2">
                <span className="font-semibold">{op.kind}</span>
                {op.instance && <code className="font-code">{op.instance}</code>}
                <StatusChip health={operationHealth(op)} label={phaseWord(op.phase)} size="sm" />
                {op.acceptedAt && <Time iso={op.acceptedAt} className="text-muted" />}
                <button
                  type="button"
                  onClick={() => trackedOperations.track(op.operationId)}
                  className="rounded-control border border-line px-2 py-0.5 text-caption hover:bg-surface-2"
                >
                  {m.ops_track()}
                </button>
              </li>
            ))}
          </ul>
        )}
      </Panel>
    </div>
  );
}

export function OperationsScreen({ namespace, cluster }: { namespace: string; cluster: string }) {
  const query = useListOperations(namespace, cluster);
  const session = useGetSession({ query: { retry: false, refetchInterval: false } });
  const request = useRequestOperation(namespace, cluster);
  return (
    <QueryState query={query} what={m.nav_operations()}>
      {(data) => <OperationsView data={data} actor={session.data?.username ?? m.ops_you()} onRequest={request} />}
    </QueryState>
  );
}
