import { type FormEvent, useId, useState } from "react";
import { type AccessDecisionInput, useAccessMutations } from "../api/access";
import { ApiError } from "../api/fetcher";
import { useListAccessRequests } from "../api/generated/endpoints/access/access";
import { useListClusters } from "../api/generated/endpoints/clusters/clusters";
import type { AccessRequest, AccessRequestCreate, AccessRequestList, ClusterRef } from "../api/generated/model";
import { useSession } from "../api/session";
import { AccessRequestCard, capabilityWord } from "../ds/patterns/AccessRequestCard/AccessRequestCard";
import { m } from "../paraglide/messages.js";
import { Panel } from "./Panel";
import { QueryState } from "./QueryState";

export type AccessRequestsViewProps = {
  data: AccessRequestList;
  /** Clusters the reader may already see (suggestions only: a request may name one they cannot see). */
  clusters: ClusterRef[];
  /** The signed-in reader, as decisions are recorded. */
  actor: string;
  /** Pre-filled from the URL (the Overview's "request access" link). */
  initial?: { namespace?: string; cluster?: string };
  onCreate: (namespace: string, body: AccessRequestCreate) => Promise<unknown>;
  onDecide: (request: AccessRequest, decision: AccessDecisionInput) => Promise<unknown>;
};

const CAPABILITIES = ["viewer", "operator"] as const;

function capabilityHint(c: (typeof CAPABILITIES)[number]): string {
  return c === "viewer" ? m.access_cap_viewer_hint() : m.access_cap_operator_hint();
}

function RequestForm({
  namespaces,
  clusters,
  initial,
  onCreate,
}: {
  namespaces: string[];
  clusters: ClusterRef[];
  initial?: AccessRequestsViewProps["initial"];
  onCreate: AccessRequestsViewProps["onCreate"];
}) {
  const ids = { ns: useId(), cluster: useId(), list: useId(), cap: useId(), why: useId(), exp: useId() };
  const start = initial?.namespace && namespaces.includes(initial.namespace) ? initial.namespace : namespaces[0];
  const [namespace, setNamespace] = useState(start ?? "");
  const [cluster, setCluster] = useState(initial?.cluster ?? "");
  const [capability, setCapability] = useState<(typeof CAPABILITIES)[number]>("viewer");
  const [justification, setJustification] = useState("");
  const [expires, setExpires] = useState("");
  const [state, setState] = useState<"idle" | "sending" | "sent">("idle");
  const [error, setError] = useState<Error | null>(null);
  const ready = namespace !== "" && justification.trim() !== "";

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!ready || state === "sending") return;
    setState("sending");
    setError(null);
    try {
      const body: AccessRequestCreate = { capability, justification: justification.trim() };
      if (cluster.trim()) body.cluster = cluster.trim();
      if (expires) body.expiresAt = new Date(expires).toISOString();
      await onCreate(namespace, body);
      setState("sent");
      setJustification("");
    } catch (err) {
      setError(err instanceof Error ? err : new Error(String(err)));
      setState("idle");
    }
  };

  const field = "h-9 rounded-control border border-line-strong bg-canvas px-2";
  return (
    <form onSubmit={submit} className="flex flex-col gap-3">
      <p className="text-label text-muted">{m.access_for_you()}</p>
      <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
        <div className="flex flex-col gap-1">
          <label htmlFor={ids.ns} className="text-label">
            {m.access_namespace()}
          </label>
          <select id={ids.ns} value={namespace} onChange={(e) => setNamespace(e.target.value)} className={field}>
            {namespaces.map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
        </div>
        <div className="flex flex-col gap-1">
          <label htmlFor={ids.cluster} className="text-label">
            {m.access_cluster()} <span className="text-muted">({m.access_optional()})</span>
          </label>
          <input
            id={ids.cluster}
            list={ids.list}
            value={cluster}
            onChange={(e) => setCluster(e.target.value)}
            autoComplete="off"
            spellCheck={false}
            className={`${field} font-code`}
          />
          <datalist id={ids.list}>
            {clusters
              .filter((c) => c.namespace === namespace)
              .map((c) => (
                <option key={c.name} value={c.name} />
              ))}
          </datalist>
        </div>
      </div>
      <p className="health rounded-control p-2 text-label" data-health="info">
        {m.access_namespace_wide({ namespace: namespace || "…" })}
      </p>
      <fieldset className="flex flex-col gap-1">
        <legend className="mb-1 text-label">{m.access_capability()}</legend>
        {CAPABILITIES.map((c) => (
          <label key={c} className="flex min-h-6 flex-wrap items-center gap-2 text-label">
            <input
              type="radio"
              name={ids.cap}
              value={c}
              checked={capability === c}
              onChange={() => setCapability(c)}
              className="size-6"
            />
            <span className="font-semibold">{capabilityWord(c)}</span>
            <span className="text-muted">{capabilityHint(c)}</span>
          </label>
        ))}
      </fieldset>
      <div className="flex flex-col gap-1">
        <label htmlFor={ids.why} className="text-label">
          {m.access_justification()} <span className="text-muted">({m.access_required()})</span>
        </label>
        <textarea
          id={ids.why}
          value={justification}
          onChange={(e) => setJustification(e.target.value)}
          required
          maxLength={1024}
          rows={3}
          className="rounded-control border border-line-strong bg-canvas px-2 py-1"
        />
      </div>
      <div className="flex flex-col gap-1">
        <label htmlFor={ids.exp} className="text-label">
          {m.access_expiry()} <span className="text-muted">({m.access_optional()})</span>
        </label>
        <input
          id={ids.exp}
          type="datetime-local"
          value={expires}
          onChange={(e) => setExpires(e.target.value)}
          className={`${field} w-fit`}
        />
        <span className="text-caption text-muted">{m.access_expiry_hint()}</span>
      </div>
      {error && (
        <div role="alert" className="health rounded-control p-2 text-label" data-health="critical">
          <p>{m.confirm_refused({ reason: error.message })}</p>
          {error instanceof ApiError && error.problem?.detail && <p>{error.problem.detail}</p>}
          {error instanceof ApiError && error.requestId && (
            <p className="text-caption">{m.request_id({ id: error.requestId })}</p>
          )}
        </div>
      )}
      <div role="status" aria-live="polite" className="text-label">
        {state === "sent" && m.access_sent()}
      </div>
      <div>
        <button
          type="submit"
          disabled={!ready || state === "sending"}
          className="h-9 rounded-control border border-line-strong px-3 font-semibold hover:bg-surface-2 disabled:opacity-60"
        >
          {state === "sending" ? m.confirm_sending() : m.access_send()}
        </button>
      </div>
    </form>
  );
}

function CardList({
  items,
  empty,
  actor,
  onDecide,
}: {
  items: AccessRequest[];
  empty: string;
  actor: string;
  onDecide?: AccessRequestsViewProps["onDecide"];
}) {
  if (items.length === 0) return <p className="text-label text-muted">{empty}</p>;
  return (
    <ul className="flex flex-col gap-2">
      {items.map((r) => (
        <li key={`${r.namespace}/${r.name}`}>
          <AccessRequestCard request={r} actor={actor} onDecide={onDecide && ((d) => onDecide(r, d))} />
        </li>
      ))}
    </ul>
  );
}

/**
 * Access requests: the UI over PgAccessRequest. A
 * request names a namespace, a capability, a justification and optionally a
 * Cluster and an expiry; whoever may create RoleBindings in that namespace
 * decides it (never the person it names). What the reader may do on each
 * row is the API server's answer (actions).
 */
export function AccessRequestsView({ data, clusters, actor, initial, onCreate, onDecide }: AccessRequestsViewProps) {
  const namespaces = data.namespaces ?? [];
  const requestable = namespaces.filter((n) => n.canRequest).map((n) => n.name);
  const reviewer = namespaces.some((n) => n.canReview);
  const items = data.items ?? [];
  const mine = items.filter((r) => r.mine);
  const others = items.filter((r) => !r.mine);
  const decide = data.writable ? onDecide : undefined;
  const partial = namespaces.filter((n) => !n.seesAll).map((n) => n.name);
  return (
    <div className="flex flex-col gap-4">
      <p className="text-label text-muted">{m.access_intro()}</p>
      {!data.writable && (
        <p role="status" className="health rounded-control p-2 text-label" data-health="info">
          {m.access_read_only()}
        </p>
      )}
      {(data.unchecked ?? 0) > 0 && (
        <p role="status" className="health rounded-control p-2 text-label" data-health="unknown" data-stale="">
          {m.access_unchecked({ count: String(data.unchecked) })}
        </p>
      )}
      {partial.length > 0 && (
        <p className="text-label text-muted">{m.access_partial({ namespaces: partial.join(", ") })}</p>
      )}
      {data.writable && (
        <Panel title={m.access_new()}>
          {requestable.length === 0 ? (
            <p className="text-label text-muted">{m.access_nothing_to_request()}</p>
          ) : (
            <RequestForm namespaces={requestable} clusters={clusters} initial={initial} onCreate={onCreate} />
          )}
        </Panel>
      )}
      <Panel title={m.access_mine()}>
        <CardList items={mine} empty={m.access_mine_empty()} actor={actor} onDecide={decide} />
      </Panel>
      {(reviewer || others.length > 0) && (
        <Panel title={reviewer ? m.access_to_review() : m.access_others()}>
          <CardList items={others} empty={m.access_to_review_empty()} actor={actor} onDecide={decide} />
        </Panel>
      )}
    </div>
  );
}

export function AccessRequestsScreen({ initial }: { initial?: { namespace?: string; cluster?: string } }) {
  const query = useListAccessRequests();
  const clusters = useListClusters();
  const session = useSession();
  const { create, decide } = useAccessMutations();
  return (
    <QueryState query={query} what={m.nav_access()}>
      {(data) => (
        <AccessRequestsView
          data={data}
          clusters={(clusters.data?.items ?? []).map((c) => c.ref)}
          actor={session.data?.username ?? ""}
          initial={initial}
          onCreate={(namespace, body) => create.mutateAsync({ namespace, body })}
          onDecide={(request, decision) => decide.mutateAsync({ request, decision })}
        />
      )}
    </QueryState>
  );
}
