import { useId, useState } from "react";
import { createDatabaseRole } from "../api/generated/endpoints/pgadmin/pgadmin";
import type { CreateDatabaseRoleBody, PersonalDatabaseRole } from "../api/generated/model";
import { ConfirmDialog } from "../ds/patterns/ConfirmDialog/ConfirmDialog";
import { m } from "../paraglide/messages.js";

export type PersonalServerActionProps = {
  cluster: string;
  /** The signed-in reader: who the role is created as, and the default pgAdmin user. */
  actor: string;
  /** POSTs the request; resolves with the server's 201, throws ApiError otherwise (403, 409, 422, 501, 503). */
  onCreate: (body: CreateDatabaseRoleBody) => Promise<PersonalDatabaseRole>;
};

/**
 * "My personal server" under the pgAdmin link-out: a DatabaseRole of the
 * reader's own (POST …/database-roles), registered as their personal,
 * non-shared server in every PgAdmin that registers the Cluster. Pessimistic
 * and honest: a 201 means the role was declared, not that it is visible; it
 * shows up in pgAdmin under "PgToolBox (personal)" only after the
 * registration has run, which Go's note says. Refusals (403, 409, 422, 501 on
 * a CloudNativePG minor without generated passwords, 503) stay in the dialog
 * in Go's words. No password is ever shown: CloudNativePG generates it into
 * a Secret the console never reads.
 */
export function PersonalServerAction({ cluster, actor, onCreate }: PersonalServerActionProps) {
  const [open, setOpen] = useState(false);
  const [role, setRole] = useState("");
  const [pgAdminUser, setPgAdminUser] = useState("");
  const [inRoles, setInRoles] = useState("");
  const [created, setCreated] = useState<PersonalDatabaseRole | null>(null);
  const ids = { role: useId(), user: useId(), roles: useId() };
  const field = "h-9 rounded-control border border-line-strong bg-canvas px-2 font-code";
  const reset = () => {
    setRole("");
    setPgAdminUser("");
    setInRoles("");
  };
  return (
    <div className="flex flex-col gap-1" data-personal-server={created ? "declared" : "none"}>
      <div className="flex flex-wrap items-center gap-2">
        <button
          type="button"
          onClick={() => setOpen(true)}
          className="h-8 rounded-control border border-line-strong px-3 font-semibold hover:bg-surface-2"
        >
          {m.personal_create()}
        </button>
        <span className="text-caption text-muted">{m.personal_hint()}</span>
      </div>
      <div role="status" aria-live="polite">
        {created && (
          <div className="health flex flex-col gap-1 rounded-control p-2 text-label" data-health="progressing">
            <p>{m.personal_declared({ role: created.role, user: created.pgAdminUser })}</p>
            <p>{created.note}</p>
            <p className="text-caption">
              {m.personal_objects()} <code className="font-code">databaserole/{created.name}</code>,{" "}
              <code className="font-code">secret/{created.passwordSecret}</code>
            </p>
          </div>
        )}
      </div>
      <ConfirmDialog
        open={open}
        onOpenChange={(next) => {
          setOpen(next);
          if (!next) reset();
        }}
        title={m.personal_confirm_title({ cluster })}
        consequence={m.personal_consequence({ cluster })}
        confirmWord={cluster}
        actor={actor}
        canSubmit={role.trim() !== ""}
        onConfirm={async () => {
          const body: CreateDatabaseRoleBody = { role: role.trim() };
          if (pgAdminUser.trim()) body.pgAdminUser = pgAdminUser.trim();
          const roles = inRoles
            .split(",")
            .map((r) => r.trim())
            .filter((r) => r !== "");
          if (roles.length > 0) body.inRoles = roles;
          setCreated(await onCreate(body));
          reset();
        }}
      >
        <div className="flex flex-col gap-1">
          <label htmlFor={ids.role} className="text-label">
            {m.personal_role()} <span className="text-muted">({m.access_required()})</span>
          </label>
          <input
            id={ids.role}
            value={role}
            onChange={(e) => setRole(e.target.value)}
            autoComplete="off"
            spellCheck={false}
            maxLength={63}
            className={field}
          />
          <span className="text-caption text-muted">{m.personal_role_hint()}</span>
        </div>
        <div className="flex flex-col gap-1">
          <label htmlFor={ids.user} className="text-label">
            {m.personal_pgadmin_user()} <span className="text-muted">({m.access_optional()})</span>
          </label>
          <input
            id={ids.user}
            value={pgAdminUser}
            onChange={(e) => setPgAdminUser(e.target.value)}
            placeholder={actor}
            autoComplete="off"
            spellCheck={false}
            maxLength={254}
            className={field}
          />
          <span className="text-caption text-muted">{m.personal_pgadmin_user_hint()}</span>
        </div>
        <div className="flex flex-col gap-1">
          <label htmlFor={ids.roles} className="text-label">
            {m.personal_in_roles()} <span className="text-muted">({m.access_optional()})</span>
          </label>
          <input
            id={ids.roles}
            value={inRoles}
            onChange={(e) => setInRoles(e.target.value)}
            autoComplete="off"
            spellCheck={false}
            className={field}
          />
          <span className="text-caption text-muted">{m.personal_in_roles_hint()}</span>
        </div>
      </ConfirmDialog>
    </div>
  );
}

/** The action under the pgAdmin panel of one Cluster. */
export function PersonalServerFor({
  namespace,
  cluster,
  actor,
}: {
  namespace: string;
  cluster: string;
  actor: string;
}) {
  return (
    <PersonalServerAction
      cluster={cluster}
      actor={actor}
      onCreate={(body) => createDatabaseRole(namespace, cluster, body)}
    />
  );
}
