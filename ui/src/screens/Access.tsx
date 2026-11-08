import { Link } from "@tanstack/react-router";
import type { Capabilities } from "../api/generated/model";
import { m } from "../paraglide/messages.js";
import { Panel } from "./Panel";

type Capability = Exclude<keyof Capabilities, "undecided">;

const ROWS: ReadonlyArray<{ key: Capability; label: () => string }> = [
  { key: "view", label: () => m.cap_view() },
  { key: "logs", label: () => m.cap_logs() },
  { key: "backup", label: () => m.cap_backup() },
  { key: "reload", label: () => m.cap_reload() },
  { key: "restart", label: () => m.cap_restart() },
  { key: "promote", label: () => m.cap_promote() },
];

/**
 * What the signed-in reader may do on this Cluster, as the API server decided
 * it (Capabilities). An undecided capability is said as such: it is false
 * until the API server answers, never a silent "no".
 */
export function AccessPanel({
  capabilities,
  namespace,
  cluster,
}: {
  capabilities: Capabilities;
  namespace?: string;
  cluster?: string;
}) {
  const undecided = new Set(capabilities.undecided ?? []);
  return (
    <Panel title={m.panel_access()}>
      <ul className="flex flex-col gap-1 text-label">
        {ROWS.map(({ key, label }) => {
          const state = undecided.has(key) ? "undecided" : capabilities[key] ? "allowed" : "denied";
          return (
            <li key={key} className="flex flex-wrap items-baseline gap-x-2" data-capability={key} data-state={state}>
              <span aria-hidden="true" className={state === "allowed" ? "text-fg" : "text-muted"}>
                {state === "allowed" ? "✓" : state === "undecided" ? "?" : "–"}
              </span>
              <span>{label()}:</span>
              <span className={state === "allowed" ? "font-semibold" : "text-muted"}>
                {state === "allowed" ? m.cap_allowed() : state === "undecided" ? m.cap_undecided() : m.cap_denied()}
              </span>
            </li>
          );
        })}
      </ul>
      <p className="text-caption text-muted">{m.cap_note()}</p>
      {namespace && cluster && (
        <p className="text-label">
          <Link to="/access" search={{ namespace, cluster }} className="text-accent underline">
            {m.access_request_link()}
          </Link>
        </p>
      )}
    </Panel>
  );
}
