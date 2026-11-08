import { Link } from "@tanstack/react-router";
import { useGetDiagnostics, useGetTriage } from "../api/generated/endpoints/clusters/clusters";
import type { Check, Diagnostics, Finding, Incident, Triage } from "../api/generated/model";
import { resolveClusterLink } from "../app/clusterLink";
import { type Columns, columnHelper, DataTable } from "../ds/patterns/DataTable/DataTable";
import { FindingSquares, SeverityChip } from "../ds/patterns/Severity/Severity";
import { EmptyState } from "../ds/patterns/States/States";
import { Tabs } from "../ds/patterns/Tabs/Tabs";
import { Time } from "../ds/time";
import { m } from "../paraglide/messages.js";
import { Panel } from "./Panel";
import { QueryState } from "./QueryState";
import { sourceStateWord } from "./sources";

function FindingBlock({
  f,
  params,
  root,
}: {
  f: Finding;
  params: { namespace: string; cluster: string };
  root: boolean;
}) {
  const target = f.link ? resolveClusterLink(f.link, params) : null;
  return (
    <div className={root ? "flex flex-col gap-1.5" : "flex flex-col gap-1.5 border-line border-l-2 pl-3"}>
      <div className="flex flex-wrap items-center gap-2">
        <SeverityChip severity={f.severity} />
        <span className="font-semibold">{f.summary}</span>
      </div>
      <p className="flex flex-wrap gap-x-3 text-caption text-muted">
        <code className="font-code">{f.check}</code>
        <span>
          {f.subject.kind}
          {f.subject.name ? `/${f.subject.name}` : ""}
        </span>
        {f.since && (
          <span>
            {m.since()} <Time iso={f.since} />
          </span>
        )}
        {f.relation && <span>{f.relation}</span>}
      </p>
      {f.detail && <p className="text-label">{f.detail}</p>}
      {(f.evidence ?? []).length > 0 && (
        <div>
          <p className="font-semibold text-caption">{m.evidence()}</p>
          <ul className="flex flex-col gap-0.5">
            {(f.evidence ?? []).map((e) => (
              <li key={`${e.origin}-${e.object ?? ""}-${e.detail}`} className="text-label">
                <span className="text-muted">
                  {e.origin}
                  {e.object ? ` · ${e.object}` : ""}:
                </span>{" "}
                <q className="font-code">{e.detail}</q>
              </li>
            ))}
          </ul>
        </div>
      )}
      {f.nextSteps && (
        <div>
          <p className="font-semibold text-caption">{m.look_yourself()}</p>
          <pre className="overflow-x-auto rounded-control bg-surface-2 p-2 font-code text-label">{f.nextSteps}</pre>
        </div>
      )}
      {target ? (
        <Link {...target} className="text-label text-accent hover:underline">
          {f.linkLabel ?? m.see_details()}
        </Link>
      ) : (
        f.link && <p className="text-label text-muted">{m.link_not_in_this_build({ label: f.linkLabel ?? f.link })}</p>
      )}
    </div>
  );
}

function IncidentCard({ i, params }: { i: Incident; params: { namespace: string; cluster: string } }) {
  return (
    <li className="flex flex-col gap-3 rounded-panel border border-line bg-surface p-4">
      <FindingBlock f={i.root} params={params} root />
      {(i.consequences ?? []).map((c) => (
        <FindingBlock key={c.id} f={c} params={params} root={false} />
      ))}
    </li>
  );
}

function outcomeWord(o: string): string {
  switch (o) {
    case "matched":
      return m.check_matched();
    case "clear":
      return m.check_clear();
    case "unavailable":
      return m.check_unavailable();
    case "notApplicable":
      return m.check_not_applicable();
    default:
      return o;
  }
}

const col = columnHelper<Check>();
const checkColumns: Columns<Check> = col.columns([
  col.accessor("name", {
    header: () => m.col_check(),
    sortFn: "text",
    cell: (c) => <code className="font-code">{c.getValue()}</code>,
  }),
  col.accessor("layer", { header: () => m.col_layer(), sortFn: "text", cell: (c) => c.getValue() }),
  col.accessor("describes", {
    header: () => m.col_describes(),
    cell: (c) => <span className="text-label">{c.getValue()}</span>,
  }),
  col.accessor("outcome", {
    header: () => m.col_outcome(),
    sortFn: "text",
    cell: (c) => (
      <span className="flex flex-col text-label">
        <span className={c.row.original.outcome === "clear" ? "" : "font-semibold"}>
          {outcomeWord(c.row.original.outcome)}
          {c.row.original.sourceOff && ` (${m.check_source_off()})`}
        </span>
        {c.row.original.because && <span className="text-muted">{c.row.original.because}</span>}
      </span>
    ),
  }),
]);

function triageWord(o: string): string {
  switch (o) {
    case "found":
      return m.triage_found();
    case "ruledOut":
      return m.triage_ruled_out();
    case "unknown":
      return m.triage_unknown();
    case "sourceOff":
      return m.triage_source_off();
    case "notApplicable":
      return m.triage_not_applicable();
    case "ask":
      return m.triage_ask();
    default:
      return o;
  }
}

export function TriageView({ data }: { data: Triage }) {
  const playbooks = data.playbooks ?? [];
  if (playbooks.length === 0) return <EmptyState title={m.triage_none()} />;
  return (
    <ul className="flex flex-col gap-4">
      {playbooks.map((p) => (
        <li key={p.id}>
          <Panel title={p.symptom}>
            {p.describes && <p className="text-label">{p.describes}</p>}
            <ol className="flex flex-col gap-2">
              {(p.steps ?? []).map((s, i) => (
                <li
                  key={s.question}
                  className="flex flex-col gap-0.5 rounded-control border border-line p-2 text-label"
                  aria-current={i === p.startAt ? "step" : undefined}
                >
                  <span className="flex flex-wrap items-center gap-2">
                    <span className="font-semibold">
                      {i + 1}. {s.question}
                    </span>
                    <span className="rounded-chip border border-line-strong px-1.5 text-caption">
                      {triageWord(s.outcome)}
                    </span>
                    {i === p.startAt && <span className="text-caption text-accent">{m.triage_start_here()}</span>}
                  </span>
                  {s.explain && <span>{s.explain}</span>}
                  {s.because && <span className="text-muted">{s.because}</span>}
                  {s.yourself && (
                    <pre className="overflow-x-auto rounded-control bg-surface-2 p-2 font-code">{s.yourself}</pre>
                  )}
                </li>
              ))}
            </ol>
            {p.upstream && (
              <a
                href={p.upstream}
                rel="noreferrer noopener"
                target="_blank"
                className="text-label text-accent hover:underline"
              >
                {m.triage_upstream()}
              </a>
            )}
          </Panel>
        </li>
      ))}
    </ul>
  );
}

export function DiagnosticsView({ data, triage }: { data: Diagnostics; triage: React.ReactNode }) {
  const params = { namespace: data.ref.namespace, cluster: data.ref.name };
  const incidents = data.incidents ?? [];
  const c = data.counts;
  return (
    <div className="flex flex-col gap-3">
      <p className="flex flex-wrap gap-x-3 text-label">
        <span>
          {m.diagnostics_runner()}: <span className="font-semibold">{sourceStateWord(data.runner.state)}</span>
          {data.runner.reason && <span className="text-muted"> — {data.runner.reason}</span>}
        </span>
        {data.runAt ? (
          <span>
            {m.diagnostics_run()} <Time iso={data.runAt} past />
          </span>
        ) : (
          <span className="font-semibold">{m.diagnostics_never_run()}</span>
        )}
        <FindingSquares counts={c} />
      </p>
      {data.applied ? (
        <p className="text-caption text-muted">
          {m.catalog_applied({ minor: data.applied, pins: (data.catalogPins ?? []).join(", ") })}
        </p>
      ) : (
        <p className="health rounded-control px-3 py-1.5 text-label" data-health="unknown">
          {m.catalog_not_applicable({ pins: (data.catalogPins ?? []).join(", ") })}
        </p>
      )}
      <Tabs
        label={m.nav_diagnostics()}
        items={[
          {
            value: "incidents",
            label: m.tab_incidents({ count: incidents.length }),
            panel:
              incidents.length === 0 ? (
                <EmptyState title={data.runAt ? m.incidents_none() : m.diagnostics_never_run()}>
                  {c.unavailable > 0 && m.incidents_none_but_unavailable({ count: c.unavailable })}
                </EmptyState>
              ) : (
                <ul className="flex flex-col gap-3">
                  {incidents.map((i) => (
                    <IncidentCard key={i.root.id} i={i} params={params} />
                  ))}
                </ul>
              ),
          },
          {
            value: "checks",
            label: m.tab_checks({ count: (data.checks ?? []).length }),
            panel: (
              <DataTable
                caption={m.checks_caption()}
                data={data.checks ?? []}
                columns={checkColumns}
                getRowId={(r) => r.name}
              />
            ),
          },
          { value: "triage", label: m.tab_triage(), panel: triage },
        ]}
      />
    </div>
  );
}

export function DiagnosticsScreen({ namespace, cluster }: { namespace: string; cluster: string }) {
  const diagnostics = useGetDiagnostics(namespace, cluster);
  const triage = useGetTriage(namespace, cluster);
  return (
    <QueryState query={diagnostics} what={m.nav_diagnostics()}>
      {(data) => (
        <DiagnosticsView
          data={data}
          triage={
            <QueryState query={triage} what={m.tab_triage()}>
              {(t) => <TriageView data={t} />}
            </QueryState>
          }
        />
      )}
    </QueryState>
  );
}
