import { type UseQueryResult, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";
import { ApiError } from "../api/fetcher";
import {
  useGetAssistantAgents,
  useGetAssistantSetup,
  useGetAssistantSkill,
} from "../api/generated/endpoints/assistant/assistant";
import {
  getListGrantsQueryKey,
  useListGrants,
  useRevokeAllGrants,
  useRevokeGrant,
} from "../api/generated/endpoints/session/session";
import type {
  AssistantClientSetup,
  AssistantSetupView,
  ConfigFile,
  GrantView,
  PromptPackSetup,
} from "../api/generated/model";
import { useSession } from "../api/session";
import { ConfirmDialog } from "../ds/patterns/ConfirmDialog/ConfirmDialog";
import { CopyCode } from "../ds/patterns/CopyCode/CopyCode";
import { EmptyState } from "../ds/patterns/States/States";
import { Tabs } from "../ds/patterns/Tabs/Tabs";
import { Time } from "../ds/time";
import { m } from "../paraglide/messages.js";
import { Fact, Facts, Panel } from "./Panel";
import { QueryState } from "./QueryState";

/**
 * The console answers 404 on the assistant endpoints when it does not serve
 * MCP (MCP_ENABLED off, internal/console/api/assistant.go): the routes exist
 * whenever the API does, so a 404 there is that answer, in Go's words.
 */
const notServed = (error: unknown): error is ApiError => error instanceof ApiError && error.status === 404;

/** MCP is off on this console: said once, with Go's reason, and nothing to click. */
export function AssistantsNotServed({ detail }: { detail?: string }) {
  return (
    <div className="flex flex-col gap-2" data-assistants="not-served">
      <EmptyState title={m.assistant_not_served_title()}>
        <p>{detail ?? m.assistant_not_served_detail()}</p>
        <p>{m.assistant_not_served_admin()}</p>
      </EmptyState>
    </div>
  );
}

/** The consented scopes in words; an unknown scope is shown as Go sent it. */
function ScopeList({ scopes }: { scopes: string[] }) {
  if (scopes.length === 0) return <span className="text-muted">{m.assistant_scope_none()}</span>;
  return (
    <ul className="flex flex-wrap gap-1">
      {scopes.map((s) => (
        <li key={s} className="rounded-control border border-line px-1.5 text-caption">
          {s === "mcp:read" ? m.assistant_scope_read() : s === "mcp:operate" ? m.assistant_scope_operate() : null}{" "}
          <code className="font-code text-muted">{s}</code>
        </li>
      ))}
    </ul>
  );
}

function GrantCard({ grant, onDisconnect }: { grant: GrantView; onDisconnect: () => void }) {
  return (
    <li className="flex flex-col gap-2 rounded-control border border-line p-3" data-grant={grant.id}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="flex flex-wrap items-baseline gap-2 font-semibold">
          <span className="break-all">{grant.client}</span>
          <span className="health rounded-control px-1.5 text-caption font-normal" data-health="unknown">
            {m.assistant_unverified()}
          </span>
        </h3>
        <button
          type="button"
          onClick={onDisconnect}
          aria-label={m.assistant_disconnect_named({ client: grant.client })}
          className="h-8 rounded-control border border-line-strong px-3 font-semibold hover:bg-surface-2"
        >
          {m.assistant_disconnect()}
        </button>
      </div>
      <Facts>
        <Fact label={m.assistant_access()}>
          <ScopeList scopes={grant.scopes ?? []} />
        </Fact>
        <Fact label={m.assistant_approved()}>
          <Time iso={grant.createdAt} past />
        </Fact>
        <Fact label={m.assistant_last_used()}>
          {grant.lastUsedAt ? (
            <>
              <Time iso={grant.lastUsedAt} past />{" "}
              <span className="text-muted">{m.assistant_last_used_precision()}</span>
            </>
          ) : (
            <span className="text-muted">{m.assistant_never_used()}</span>
          )}
        </Fact>
        <Fact label={m.assistant_ends()}>
          <Time iso={grant.expiresAt} />
        </Fact>
        <Fact label={m.assistant_reference()}>
          <code className="break-all font-code">{grant.id}</code>
        </Fact>
      </Facts>
    </li>
  );
}

export type ConnectedAssistantsViewProps = {
  grants: GrantView[];
  /** The signed-in user: whose grants these are. */
  actor: string;
  /** DELETEs the grant; resolves on 204, throws ApiError otherwise (404: not yours, or already gone). */
  onRevoke: (grant: GrantView) => Promise<void>;
  /** DELETEs every grant of the user (revokeAllGrants); resolves on 204. */
  onRevokeAll: () => Promise<void>;
};

/**
 * Connected assistants: the MCP clients the reader approved (console
 * grants), each acting as them. Disconnecting is confirmed by typing the
 * client's name; the 204 is the revocation itself (every replica refuses the
 * grant within about a second), which the page then says. With more than
 * one, "Disconnect all" revokes them together, confirmed by typing a fixed
 * phrase; the browser session stays.
 */
export function ConnectedAssistantsView({ grants, actor, onRevoke, onRevokeAll }: ConnectedAssistantsViewProps) {
  const [target, setTarget] = useState<GrantView | null>(null);
  const [all, setAll] = useState(false);
  const [revoked, setRevoked] = useState<string | null>(null);
  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-start justify-between gap-2">
        <p className="min-w-0 flex-1 text-label text-muted">{m.assistant_connected_intro()}</p>
        {grants.length > 1 && (
          <button
            type="button"
            onClick={() => {
              setRevoked(null);
              setAll(true);
            }}
            className="h-8 rounded-control border border-line-strong px-3 font-semibold hover:bg-surface-2"
          >
            {m.assistant_disconnect_all()}
          </button>
        )}
      </div>
      <div role="status" aria-live="polite">
        {revoked && (
          <p className="health rounded-control p-2 text-label" data-health="ok">
            {revoked}
          </p>
        )}
      </div>
      {grants.length === 0 ? (
        <EmptyState title={m.assistant_connected_empty()}>{m.assistant_connected_empty_hint()}</EmptyState>
      ) : (
        <ul className="flex flex-col gap-2">
          {grants.map((g) => (
            <GrantCard
              key={g.id}
              grant={g}
              onDisconnect={() => {
                setRevoked(null);
                setTarget(g);
              }}
            />
          ))}
        </ul>
      )}
      <ConfirmDialog
        open={target !== null}
        onOpenChange={(open) => {
          if (!open) setTarget(null);
        }}
        title={m.assistant_disconnect_title({ client: target?.client ?? "" })}
        consequence={m.assistant_disconnect_consequence()}
        confirmWord={target?.client || target?.id || ""}
        actor={actor}
        recordedAs={m.assistant_disconnect_recorded({ actor })}
        submitLabel={m.assistant_disconnect()}
        onConfirm={async () => {
          if (!target) return;
          await onRevoke(target);
          setRevoked(m.assistant_revoked({ client: target.client }));
        }}
      />
      <ConfirmDialog
        open={all}
        onOpenChange={setAll}
        title={m.assistant_disconnect_all_title({ count: grants.length })}
        consequence={m.assistant_disconnect_all_consequence()}
        confirmWord={m.assistant_disconnect_all_word()}
        actor={actor}
        recordedAs={m.assistant_disconnect_all_recorded({ actor })}
        submitLabel={m.assistant_disconnect_all()}
        onConfirm={async () => {
          await onRevokeAll();
          setRevoked(m.assistant_revoked_all());
        }}
      />
    </div>
  );
}

function FileForm({ file, label }: { file: ConfigFile; label: string }) {
  return (
    <div className="flex flex-col gap-1">
      <p className="text-label">
        {label}: <code className="font-code font-semibold">{file.path}</code>{" "}
        <span className="text-muted">({file.format.toUpperCase()})</span>
      </p>
      <CopyCode code={file.content.trimEnd()} label={file.path} />
    </div>
  );
}

function Step({ title, children }: { title: string; children: ReactNode }) {
  return (
    <li className="flex flex-col gap-2">
      <h3 className="font-semibold">{title}</h3>
      {children}
    </li>
  );
}

type TextQuery = Pick<UseQueryResult<string, unknown>, "data" | "error" | "isPending" | "refetch">;

/** The prompt pack of one client, where Go says it reads it: download it, or open it and copy it. */
function PackStep({ pack, query }: { pack: PromptPackSetup; query: TextQuery }) {
  return (
    <Step title={m.assistant_step_pack()}>
      <p className="text-label text-muted">{m.assistant_pack_intro()}</p>
      <div className="flex flex-wrap items-center gap-2">
        <a
          href={pack.download}
          download={pack.fileName}
          className="inline-flex h-8 items-center rounded-control border border-line-strong px-3 text-label hover:bg-surface-2"
        >
          {m.assistant_download({ file: pack.fileName })}
        </a>
      </div>
      <Facts>
        {pack.projectPath && (
          <Fact label={m.assistant_pack_project()}>
            <code className="break-all font-code">{pack.projectPath}</code>
          </Fact>
        )}
        {pack.userPath && (
          <Fact label={m.assistant_pack_user()}>
            <code className="break-all font-code">{pack.userPath}</code>
          </Fact>
        )}
      </Facts>
      {pack.document === "agents" && <p className="text-label text-muted">{m.assistant_pack_merge()}</p>}
      <details>
        <summary className="cursor-pointer text-label">{m.assistant_show({ file: pack.fileName })}</summary>
        <div className="pt-2">
          <QueryState query={query} what={pack.fileName}>
            {(text) => <CopyCode code={text.trimEnd()} label={pack.fileName} />}
          </QueryState>
        </div>
      </details>
    </Step>
  );
}

/** One client's setup, as Go rendered it for this console. */
function ClientSetup({ client, pack }: { client: AssistantClientSetup; pack: (p: PromptPackSetup) => TextQuery }) {
  return (
    <div className="flex flex-col gap-4" data-client={client.id}>
      <ol className="flex list-decimal flex-col gap-4 ps-5">
        <Step title={m.assistant_step_add()}>
          {client.add ? (
            <>
              <p className="text-label text-muted">{m.assistant_step_add_command()}</p>
              <CopyCode code={client.add} label={m.assistant_add_command({ client: client.name })} />
              <p className="text-label text-muted">{m.assistant_step_add_or_file()}</p>
            </>
          ) : (
            <p className="text-label text-muted">{m.assistant_step_add_file_only()}</p>
          )}
          <FileForm file={client.userConfig} label={m.assistant_user_config()} />
          {client.projectConfig && <FileForm file={client.projectConfig} label={m.assistant_project_config()} />}
        </Step>
        <Step title={m.assistant_step_sign_in()}>
          {client.login ? (
            <>
              <p className="text-label text-muted">{m.assistant_step_sign_in_command()}</p>
              <CopyCode code={client.login} label={m.assistant_login_command({ client: client.name })} />
            </>
          ) : (
            <p className="text-label text-muted">{m.assistant_step_sign_in_automatic()}</p>
          )}
          <p className="text-label text-muted">{m.assistant_step_consent()}</p>
        </Step>
        {(client.check || client.checkNote) && (
          <Step title={m.assistant_step_check()}>
            {client.check && (
              <CopyCode code={client.check} label={m.assistant_check_command({ client: client.name })} />
            )}
            {client.checkNote && <p className="text-label">{client.checkNote}</p>}
          </Step>
        )}
        {client.pack && <PackStep pack={client.pack} query={pack(client.pack)} />}
      </ol>
      {client.staticToken && (
        <section className="flex flex-col gap-2 rounded-control border border-line border-dashed p-3">
          <h3 className="font-semibold">{m.assistant_static_token()}</h3>
          <p className="text-label">{client.staticToken.note}</p>
          {client.staticToken.env && <CopyCode code={client.staticToken.env} label={m.assistant_static_token_env()} />}
          <FileForm file={client.staticToken.config} label={m.assistant_static_token_config()} />
        </section>
      )}
      <p className="text-label">
        <a href={client.docs} target="_blank" rel="noreferrer noopener" className="text-accent underline">
          {m.assistant_client_docs({ client: client.name })}
        </a>
      </p>
    </div>
  );
}

export type ConnectAssistantViewProps = {
  setup: AssistantSetupView;
  skill: TextQuery;
  agents: TextQuery;
};

/**
 * Connect an assistant: this console's MCP endpoint, the ready-to-paste
 * setup per client and its prompt pack, all rendered by Go for this
 * console's public URL. What a connection may do (read-only, or read and
 * operate) is Go's answer too.
 */
export function ConnectAssistantView({ setup, skill, agents }: ConnectAssistantViewProps) {
  const clients = setup.clients ?? [];
  const operate = setup.access === "readOperate";
  const pack = (p: PromptPackSetup) => (p.document === "skill" ? skill : agents);
  return (
    <div className="flex flex-col gap-4">
      <div className="health flex flex-col gap-1 rounded-control p-3 text-label" data-health="info">
        <p className="font-semibold">{operate ? m.assistant_access_operate() : m.assistant_access_read()}</p>
        <p>{operate ? m.assistant_access_operate_detail() : m.assistant_access_read_detail()}</p>
        <p>{m.assistant_access_as_you()}</p>
      </div>
      <Facts>
        <Fact label={m.assistant_mcp_url()}>
          <CopyCode code={setup.mcpURL} label={m.assistant_mcp_url()} />
        </Fact>
        <Fact label={m.assistant_server_name()}>
          <code className="font-code">{setup.serverName}</code>
        </Fact>
        <Fact label={m.assistant_token_login()}>
          {setup.tokenLogin ? m.assistant_token_login_on() : m.assistant_token_login_off()}
        </Fact>
      </Facts>
      {clients.length === 0 ? (
        <EmptyState title={m.assistant_no_clients()} />
      ) : (
        <Tabs
          label={m.assistant_client_picker()}
          items={clients.map((c) => ({ value: c.id, label: c.name, panel: <ClientSetup client={c} pack={pack} /> }))}
        />
      )}
    </div>
  );
}

/** The assistants page: connected assistants (grants) and how to connect one. */
export function AssistantsScreen() {
  const client = useQueryClient();
  const session = useSession();
  const grants = useListGrants();
  const setup = useGetAssistantSetup();
  const served = setup.data !== undefined;
  const skill = useGetAssistantSkill({ query: { enabled: served } });
  const agents = useGetAssistantAgents({ query: { enabled: served } });
  const refresh = { mutation: { onSettled: () => client.invalidateQueries({ queryKey: getListGrantsQueryKey() }) } };
  const revoke = useRevokeGrant(refresh);
  const revokeAll = useRevokeAllGrants(refresh);
  if (notServed(setup.error) && (notServed(grants.error) || grants.isPending)) {
    return <AssistantsNotServed detail={setup.error.problem?.detail} />;
  }
  return (
    <div className="flex flex-col gap-4">
      <Panel title={m.assistant_connected()}>
        {notServed(grants.error) ? (
          <AssistantsNotServed detail={grants.error.problem?.detail} />
        ) : (
          <QueryState query={grants} what={m.assistant_connected()}>
            {(data) => (
              <ConnectedAssistantsView
                grants={data.items ?? []}
                actor={session.data?.username ?? ""}
                onRevoke={(g) => revoke.mutateAsync({ id: g.id })}
                onRevokeAll={() => revokeAll.mutateAsync()}
              />
            )}
          </QueryState>
        )}
      </Panel>
      <Panel title={m.assistant_connect()}>
        {notServed(setup.error) ? (
          <AssistantsNotServed detail={setup.error.problem?.detail} />
        ) : (
          <QueryState query={setup} what={m.assistant_connect()}>
            {(data) => <ConnectAssistantView setup={data} skill={skill} agents={agents} />}
          </QueryState>
        )}
      </Panel>
    </div>
  );
}
