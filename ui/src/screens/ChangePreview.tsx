import type { ChangePreview, PreviewField, PreviewParameter } from "../api/generated/model";
import { m } from "../paraglide/messages.js";
import { Fact, Facts, Panel } from "./Panel";

type Parameter = Omit<PreviewParameter, "effect" | "context"> & { effect: string; context?: string };
type Field = Omit<PreviewField, "effect" | "reason"> & { effect: string; reason: string };

/**
 * The preview as the API sends it. Effects, contexts, reasons, the primary's
 * action and the floor are read as plain strings: a value this build does not
 * know is worded generically, never dropped.
 */
export type Preview = Omit<ChangePreview, "effect" | "primary" | "parameters" | "fields"> & {
  effect: string;
  primary: string;
  atLeast?: string;
  parameters: Parameter[] | null;
  fields: Field[] | null;
};

function effectWord(e: string): string {
  switch (e) {
    case "none":
      return m.preview_effect_none();
    case "reload":
      return m.preview_effect_reload();
    case "start":
      return m.preview_effect_start();
    case "restart":
      return m.preview_effect_restart();
    case "rollout":
      return m.preview_effect_rollout();
    case "shutdown":
      return m.preview_effect_shutdown();
    case "majorUpgrade":
      return m.preview_effect_major_upgrade();
    case "refused":
      return m.preview_effect_refused();
    default:
      return m.preview_effect_unknown();
  }
}

function effectSummary(e: string): string {
  switch (e) {
    case "none":
      return m.preview_summary_none();
    case "reload":
      return m.preview_summary_reload();
    case "start":
      return m.preview_summary_start();
    case "restart":
      return m.preview_summary_restart();
    case "rollout":
      return m.preview_summary_rollout();
    case "shutdown":
      return m.preview_summary_shutdown();
    case "majorUpgrade":
      return m.preview_summary_major_upgrade();
    default:
      return m.preview_summary_unknown();
  }
}

/** The floor of an unknown preview: what the classified changes do at least; undefined for none. */
function floorWord(e: string | undefined): string | undefined {
  switch (e) {
    case "reload":
      return m.preview_floor_reload();
    case "start":
      return m.preview_floor_start();
    case "restart":
      return m.preview_floor_restart();
    case "rollout":
      return m.preview_floor_rollout();
    case "shutdown":
      return m.preview_floor_shutdown();
    case "majorUpgrade":
      return m.preview_floor_major_upgrade();
    default:
      return undefined;
  }
}

function contextWord(c: string): string {
  switch (c) {
    case "postmaster":
      return m.preview_context_postmaster();
    case "reload":
      return m.preview_context_reload();
    case "fixed":
      return m.preview_context_fixed();
    case "blocked":
      return m.preview_context_blocked();
    case "absent":
      return m.preview_context_absent();
    default:
      return c;
  }
}

function reasonWord(r: string): string {
  switch (r) {
    case "image":
      return m.preview_reason_image();
    case "majorVersion":
      return m.preview_reason_major_version();
    case "podTemplate":
      return m.preview_reason_pod_template();
    case "newVolume":
      return m.preview_reason_new_volume();
    case "volumeResize":
      return m.preview_reason_volume_resize();
    case "scale":
      return m.preview_reason_scale();
    case "restartAnnotation":
      return m.preview_reason_restart_annotation();
    case "hibernation":
      return m.preview_reason_hibernation();
    case "fencing":
      return m.preview_reason_fencing();
    case "sharedPreloadLibraries":
      return m.preview_reason_shared_preload_libraries();
    case "managedExtension":
      return m.preview_reason_managed_extension();
    case "certificates":
      return m.preview_reason_certificates();
    case "hba":
      return m.preview_reason_hba();
    case "synchronous":
      return m.preview_reason_synchronous();
    case "replicaRole":
      return m.preview_reason_replica_role();
    case "operatorOnly":
      return m.preview_reason_operator_only();
    default:
      return m.preview_reason_unknown_field();
  }
}

/** What happens to an instance at one step, or to the primary (pkg/cnpgstatus/preview). */
function actionWord(a: string): string {
  switch (a) {
    case "none":
      return m.preview_action_none();
    case "reload":
      return m.preview_action_reload();
    case "restart":
      return m.preview_action_restart();
    case "recreate":
      return m.preview_action_recreate();
    case "restartInPlace":
      return m.preview_action_restart_in_place();
    case "switchover":
      return m.preview_action_switchover();
    case "waitForUser":
      return m.preview_action_wait_for_user();
    case "restartFirst":
      return m.preview_action_restart_first();
    case "majorUpgrade":
      return m.preview_action_major_upgrade();
    case "shutdown":
      return m.preview_action_shutdown();
    case "start":
      return m.preview_action_start();
    case "unknown":
      return m.preview_action_unknown();
    default:
      return a;
  }
}

function roleWord(r: string): string {
  if (r === "primary") return m.preview_role_primary();
  if (r === "replicas") return m.preview_role_replicas();
  return m.preview_role_all();
}

const th = "py-1 pr-4 font-semibold";
const td = "py-1 pr-4 align-top";

function Value({ v }: { v?: string }) {
  return v === undefined ? (
    <span className="text-muted italic">{m.preview_unset()}</span>
  ) : (
    <code className="font-code">{v}</code>
  );
}

function ParameterRow({ p }: { p: Parameter }) {
  return (
    <tr className="border-line border-t">
      <th scope="row" className={`${td} text-left font-code font-normal`}>
        {p.name}
      </th>
      <td className={td}>
        <Value v={p.before} />
        <span aria-hidden="true"> → </span>
        <span className="sr-only"> {m.tl_to()} </span>
        <Value v={p.after} />
      </td>
      <td className={td}>{effectWord(p.effect)}</td>
      <td className={td}>{p.context ? contextWord(p.context) : <span className="text-muted">—</span>}</td>
      <td className={`${td} text-caption`}>{p.decrease ? m.preview_decrease() : ""}</td>
    </tr>
  );
}

/**
 * "What this change sets in motion": Go's reading of the revision against
 * CloudNativePG's rules (pkg/cnpgstatus/preview) — the overall effect, each
 * parameter's reload/restart, the other fields, the primary update strategy
 * and method, and the order in which instances are touched. Shown as Go
 * classified it; an unverified preview (a PostgreSQL major or operator minor
 * the tables do not cover) says so first. When a change could not be
 * classified, the lead is "unknown, at least …" — never "No restart" — and the
 * order is labelled as a floor.
 */
export function ChangePreviewPanel({ preview: p }: { preview: Preview }) {
  const parameters = p.parameters ?? [];
  const fields = p.fields ?? [];
  const steps = p.steps ?? [];
  const unknown = p.unknown ?? 0;
  const isUnknown = p.effect === "unknown" || unknown > 0;
  const floor = floorWord(p.atLeast ?? (p.effect === "unknown" ? undefined : p.effect));
  return (
    <Panel title={m.preview_title()}>
      {!p.verified && !isUnknown && (
        <p className="health m-0 rounded-control px-3 py-1.5 text-label" data-health="unknown">
          {m.preview_unverified({
            major: p.postgresMajor === undefined ? m.not_reported() : String(p.postgresMajor),
            minor: p.cnpgMinor ?? m.not_reported(),
          })}
        </p>
      )}
      {isUnknown ? (
        <p className="health m-0 rounded-control px-3 py-1.5 text-label" data-health="unknown">
          <span className="font-semibold">
            {floor ? m.preview_effect_unknown_at_least({ floor }) : m.preview_effect_unknown()}
          </span>{" "}
          — {m.preview_unknown({ count: String(unknown) })}{" "}
          {floor ? m.preview_unknown_floor_note() : m.preview_unknown_no_floor()}
        </p>
      ) : (
        <p className="health m-0 rounded-control px-3 py-1.5 text-label" data-health="info">
          <span className="font-semibold">{effectWord(p.effect)}</span> — {effectSummary(p.effect)}
        </p>
      )}
      <Facts>
        <Fact label={m.preview_strategy()}>
          <code className="font-code">{p.strategy}</code>{" "}
          <span className="text-muted">
            ({p.strategy === "supervised" ? m.preview_strategy_supervised() : m.preview_strategy_unsupervised()})
          </span>
        </Fact>
        <Fact label={m.preview_method()}>
          <code className="font-code">{p.method}</code>
        </Fact>
        <Fact label={m.preview_primary()}>{actionWord(p.primary)}</Fact>
        {p.postgresMajor !== undefined && <Fact label={m.preview_postgres_major()}>{p.postgresMajor}</Fact>}
        {p.cnpgMinor && <Fact label={m.preview_cnpg_minor()}>{p.cnpgMinor}</Fact>}
      </Facts>
      {steps.length > 0 && (
        <section
          aria-label={isUnknown ? m.preview_steps_at_least() : m.preview_steps()}
          className="flex flex-col gap-1"
        >
          <h3 className="m-0 font-semibold text-label">{isUnknown ? m.preview_steps_at_least() : m.preview_steps()}</h3>
          <ol className="m-0 flex flex-col gap-1 pl-5 text-label">
            {steps.map((s, i) => (
              // biome-ignore lint/suspicious/noArrayIndexKey: the steps are an ordered list; their position is their identity.
              <li key={i}>
                <span className="font-semibold">{roleWord(s.role)}</span>
                {(s.instances ?? []).length > 0 && (
                  <span>
                    {" "}
                    (<code className="font-code">{(s.instances ?? []).join(", ")}</code>)
                  </span>
                )}
                : {actionWord(s.action)}
              </li>
            ))}
          </ol>
          {steps.some((s) => (s.instances ?? []).length === 0) && (
            <p className="m-0 text-caption text-muted">{m.preview_steps_by_role()}</p>
          )}
        </section>
      )}
      {parameters.length > 0 && (
        <div className="overflow-x-auto">
          <table className="w-full text-left text-label">
            <caption className="py-1 text-left font-semibold">{m.preview_parameters()}</caption>
            <thead>
              <tr>
                <th scope="col" className={th}>
                  {m.preview_col_parameter()}
                </th>
                <th scope="col" className={th}>
                  {m.preview_col_change()}
                </th>
                <th scope="col" className={th}>
                  {m.preview_col_effect()}
                </th>
                <th scope="col" className={th}>
                  {m.preview_col_context()}
                </th>
                <th scope="col" className={th}>
                  {m.preview_col_note()}
                </th>
              </tr>
            </thead>
            <tbody>
              {parameters.map((p) => (
                <ParameterRow key={p.name} p={p} />
              ))}
            </tbody>
          </table>
        </div>
      )}
      {fields.length > 0 && (
        <div className="overflow-x-auto">
          <table className="w-full text-left text-label">
            <caption className="py-1 text-left font-semibold">{m.preview_fields()}</caption>
            <thead>
              <tr>
                <th scope="col" className={th}>
                  {m.preview_col_field()}
                </th>
                <th scope="col" className={th}>
                  {m.preview_col_effect()}
                </th>
                <th scope="col" className={th}>
                  {m.preview_col_reason()}
                </th>
              </tr>
            </thead>
            <tbody>
              {fields.map((f) => (
                <tr key={f.path} className="border-line border-t">
                  <th scope="row" className={`${td} text-left font-code font-normal`}>
                    {f.path}
                  </th>
                  <td className={td}>{effectWord(f.effect)}</td>
                  <td className={td}>{reasonWord(f.reason)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <p className="m-0 text-caption text-muted">{m.preview_note()}</p>
    </Panel>
  );
}
