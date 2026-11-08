import { useId, useState } from "react";
import type { AccessDecisionInput, AccessVerb } from "../../../api/access";
import type { AccessAction, AccessRequest } from "../../../api/generated/model";
import { m } from "../../../paraglide/messages.js";
import type { Health } from "../../health";
import { Time } from "../../time";
import { ConfirmDialog } from "../ConfirmDialog/ConfirmDialog";
import { StatusChip } from "../StatusChip/StatusChip";

export type AccessRequestCardProps = {
  request: AccessRequest;
  /** The signed-in reader, as the decision will be recorded. */
  actor: string;
  /** Sends the decision; resolves with the server's answer, throws ApiError when refused. Absent: read-only. */
  onDecide?: (decision: AccessDecisionInput) => Promise<unknown>;
};

function accessPhaseWord(p: AccessRequest["phase"]): string {
  switch (p) {
    case "Pending":
      return m.access_phase_pending();
    case "Approved":
      return m.access_phase_approved();
    case "Denied":
      return m.access_phase_denied();
    case "Revoked":
      return m.access_phase_revoked();
    default:
      return m.access_phase_expired();
  }
}

export function capabilityWord(c: string): string {
  if (c === "viewer") return m.access_cap_viewer();
  if (c === "operator") return m.access_cap_operator();
  return c;
}

function subjectKindWord(k: AccessRequest["subject"]["kind"]): string | null {
  if (k === "Group") return m.access_group();
  if (k === "ServiceAccount") return m.access_service_account();
  return null;
}

type Copy = { title: string; consequence: string; label: string };
function copyFor(verb: AccessVerb, r: AccessRequest): Copy {
  const args = { subject: r.subject.name, capability: capabilityWord(r.capability), namespace: r.namespace };
  if (verb === "approve")
    return {
      title: m.access_approve_title(args),
      consequence: m.access_approve_consequence(args),
      label: m.access_approve(),
    };
  if (verb === "deny" && r.mine)
    return {
      title: m.access_withdraw_title(args),
      consequence: m.access_withdraw_consequence(args),
      label: m.access_withdraw(),
    };
  if (verb === "deny")
    return { title: m.access_deny_title(args), consequence: m.access_deny_consequence(args), label: m.access_deny() };
  return {
    title: m.access_revoke_title(args),
    consequence: m.access_revoke_consequence(args),
    label: m.access_revoke(),
  };
}

function ActionButton({ action, label, onClick }: { action: AccessAction; label: string; onClick: () => void }) {
  const why = action.undecided ? m.cap_undecided() : action.why;
  return (
    <span className="flex flex-wrap items-center gap-2">
      <button
        type="button"
        disabled={!action.allowed}
        onClick={onClick}
        className="h-9 rounded-control border border-line-strong px-3 font-semibold hover:bg-surface-2 disabled:opacity-60"
      >
        {label}…
      </button>
      {!action.allowed && why && <span className="text-caption text-muted">{why}</span>}
    </span>
  );
}

/**
 * AccessRequestCard — one PgAccessRequest as Go describes it:
 * who asked what for whom and why, its phase (glyph + word + hue), the scope
 * the approval binds (a named Cluster still binds the whole namespace: Go's
 * words), the operator's condition, who decided or revoked and when. Every
 * decision is pessimistic: typed request name, a note (required to deny), the
 * result shown once the server answered. The "undo" of an approval is Revoke.
 */
export function AccessRequestCard({ request: r, actor, onDecide }: AccessRequestCardProps) {
  const [verb, setVerb] = useState<AccessVerb | null>(null);
  const [note, setNote] = useState("");
  const noteId = useId();
  const pending = r.phase === "Pending";
  const approved = r.phase === "Approved";
  const kind = subjectKindWord(r.subject.kind);
  const copy = verb ? copyFor(verb, r) : null;
  // Go's health is warning exactly when the operator's Ready condition is False.
  const conditionHealth: Health = r.health === "warning" ? "warning" : "info";
  return (
    <article
      className="flex flex-col gap-2 rounded-panel border border-line bg-surface p-3"
      aria-label={m.access_card_label({ subject: r.subject.name, namespace: r.namespace })}
      data-phase={r.phase}
    >
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="font-semibold">
          {m.access_card_title({ capability: capabilityWord(r.capability), namespace: r.namespace })}
        </h3>
        <StatusChip health={r.health} label={accessPhaseWord(r.phase)} size="sm" />
      </div>
      <dl className="grid grid-cols-[minmax(7rem,auto)_1fr] gap-x-4 gap-y-1 text-label">
        <dt className="text-muted">{m.access_name()}</dt>
        <dd className="m-0 min-w-0 break-words font-code">{r.name}</dd>
        <dt className="text-muted">{m.access_subject()}</dt>
        <dd className="m-0 min-w-0 break-words">
          <code className="font-code">{r.subject.name}</code>
          {kind && <span className="text-muted"> ({kind})</span>}
          {r.mine && <span className="text-muted"> — {m.access_names_you()}</span>}
        </dd>
        {r.cluster && (
          <>
            <dt className="text-muted">{m.access_cluster()}</dt>
            <dd className="m-0 font-code">{r.cluster}</dd>
          </>
        )}
        <dt className="text-muted">{m.access_role()}</dt>
        <dd className="m-0 font-code">{r.clusterRole}</dd>
        <dt className="text-muted">{m.access_justification()}</dt>
        <dd className="m-0 min-w-0 break-words">{r.justification}</dd>
        <dt className="text-muted">{m.access_requested()}</dt>
        <dd className="m-0">
          <Time iso={r.createdAt} />
        </dd>
        <dt className="text-muted">{m.access_expiry()}</dt>
        <dd className="m-0">{r.expiresAt ? <Time iso={r.expiresAt} /> : m.access_no_expiry()}</dd>
        {r.decidedBy && (
          <>
            <dt className="text-muted">{m.access_decided()}</dt>
            <dd className="m-0">
              {m.access_by({ who: r.decidedBy })} {r.decidedAt && <Time iso={r.decidedAt} className="text-muted" />}
            </dd>
          </>
        )}
        {r.revokedBy && (
          <>
            <dt className="text-muted">{m.access_revoked()}</dt>
            <dd className="m-0">
              {m.access_by({ who: r.revokedBy })} {r.revokedAt && <Time iso={r.revokedAt} className="text-muted" />}
            </dd>
          </>
        )}
        {r.note && (
          <>
            <dt className="text-muted">{m.access_note()}</dt>
            <dd className="m-0 min-w-0 break-words">{r.note}</dd>
          </>
        )}
        {r.roleBinding && (
          <>
            <dt className="text-muted">{m.access_binding()}</dt>
            <dd className="m-0 min-w-0 break-words font-code">{r.roleBinding}</dd>
          </>
        )}
      </dl>
      {(pending || approved) && (
        <p className="health rounded-control p-2 text-label" data-health="info">
          {m.access_scope()}: {r.scope}
        </p>
      )}
      <p
        className="health rounded-control p-2 text-label"
        data-health={conditionHealth}
        data-condition={r.condition.reason}
      >
        {r.condition.observed ? r.condition.message : m.access_not_observed()}{" "}
        <code className="font-code text-caption">{r.condition.reason}</code>
      </p>
      {onDecide && (pending || approved) && (
        <div className="flex flex-wrap items-center gap-2">
          {pending && (
            <>
              {!r.mine && (
                <ActionButton
                  action={r.actions.approve}
                  label={m.access_approve()}
                  onClick={() => setVerb("approve")}
                />
              )}
              <ActionButton
                action={r.actions.deny}
                label={r.mine ? m.access_withdraw() : m.access_deny()}
                onClick={() => setVerb("deny")}
              />
              {r.mine && !r.actions.approve.allowed && r.actions.approve.why && (
                <p className="w-full text-caption text-muted">{r.actions.approve.why}</p>
              )}
            </>
          )}
          {approved && (
            <ActionButton action={r.actions.revoke} label={m.access_revoke()} onClick={() => setVerb("revoke")} />
          )}
        </div>
      )}
      {verb && copy && onDecide && (
        <ConfirmDialog
          open
          onOpenChange={(open) => {
            if (!open) {
              setVerb(null);
              setNote("");
            }
          }}
          title={copy.title}
          consequence={copy.consequence}
          confirmWord={r.name}
          actor={actor}
          canSubmit={verb !== "deny" || note.trim() !== ""}
          submitLabel={copy.label}
          onConfirm={async () => {
            await onDecide({ verb, note: note.trim() || undefined });
          }}
        >
          <div className="flex flex-col gap-1">
            <label htmlFor={noteId} className="text-label">
              {m.access_note()}{" "}
              <span className="text-muted">({verb === "deny" ? m.access_required() : m.access_optional()})</span>
            </label>
            <textarea
              id={noteId}
              value={note}
              onChange={(e) => setNote(e.target.value)}
              required={verb === "deny"}
              maxLength={1024}
              rows={2}
              className="rounded-control border border-line-strong bg-canvas px-2 py-1"
            />
            <span className="text-caption text-muted">{m.access_note_hint()}</span>
          </div>
        </ConfirmDialog>
      )}
    </article>
  );
}
