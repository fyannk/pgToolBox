import { Link } from "@tanstack/react-router";
import { type ReactNode, useId, useMemo, useState } from "react";
import { useListClusters } from "../api/generated/endpoints/clusters/clusters";
import type { ClusterListView as ClusterListData, ClusterRow, NamespaceState } from "../api/generated/model";
import { healthRank } from "../ds/format";
import { asHealth, HEALTH_GLYPH, healthWord } from "../ds/health";
import { Reported } from "../ds/NotReported";
import { type Columns, columnHelper, DataTable, type SortingState } from "../ds/patterns/DataTable/DataTable";
import { originWord } from "../ds/patterns/Provenance/Provenance";
import { FindingSquares } from "../ds/patterns/Severity/Severity";
import { EmptyState } from "../ds/patterns/States/States";
import { StatusChip } from "../ds/patterns/StatusChip/StatusChip";
import { formatUtc, Time } from "../ds/time";
import { m } from "../paraglide/messages.js";
import { QueryState } from "./QueryState";
import { phraseText } from "./recoveryPhrase";
import { sourceStateWord } from "./sources";

const col = columnHelper<ClusterRow>();

function primaryText(p: ClusterRow["primary"]): string {
  switch (p.state) {
    case "stable":
      return p.current ?? m.not_reported();
    case "switchover":
      return m.primary_switchover({ from: p.current ?? "?", to: p.target ?? "?" });
    case "failover":
      return m.primary_failover({ from: p.current ?? "?", to: p.target ?? "?" });
    case "failoverPending":
      return m.primary_failover_pending({ from: p.current ?? "?" });
    case "none":
      return m.primary_none();
    default:
      return m.health_unknown();
  }
}

/** A state as its word's colour alone: no glyph, no chip (the list is a summary). */
function StateText({
  health,
  stale = false,
  title,
  children,
}: {
  health: string;
  stale?: boolean;
  title?: string;
  children: ReactNode;
}) {
  const h = asHealth(health);
  return (
    <span
      className="state-text whitespace-nowrap font-code text-label"
      data-health={h}
      data-stale={stale ? "" : undefined}
      title={title}
    >
      <span className="sr-only">{healthWord(h)}: </span>
      {children}
      {stale && <span className="sr-only"> ({m.stale()})</span>}
    </span>
  );
}

const columns: Columns<ClusterRow> = col.columns([
  col.accessor((r) => `${r.ref.namespace}/${r.ref.name}`, {
    id: "cluster",
    header: () => m.col_cluster(),
    sortFn: "alphanumeric",
    cell: (c) => {
      const r = c.row.original;
      const p = r.provenance;
      return (
        <span className="whitespace-nowrap">
          <Link
            to="/c/$namespace/$cluster"
            params={{ namespace: r.ref.namespace, cluster: r.ref.name }}
            className="whitespace-nowrap font-semibold text-accent hover:underline"
          >
            <span className="text-muted">{r.ref.namespace}/</span>
            {r.ref.name}
          </Link>
          {/* Rows are as of the list's assembly time; a stale one says so, with its source and age on hover. */}
          {p.stale && (
            <span
              className="ml-2 text-caption text-health-unknown-fg"
              title={[originWord(p.origin), p.source, p.staleReason, p.observedAt && formatUtc(p.observedAt)]
                .filter(Boolean)
                .join(" · ")}
            >
              {m.stale()}
            </span>
          )}
        </span>
      );
    },
  }),
  col.accessor((r) => healthRank(r.verdict.health), {
    id: "verdict",
    header: () => m.col_verdict(),
    sortFn: "alphanumeric",
    cell: (c) => <StatusChip health={c.row.original.verdict.health} stale={c.row.original.verdict.stale} />,
  }),
  col.accessor((r) => r.phase.text ?? "", {
    id: "phase",
    header: () => m.col_phase(),
    sortFn: "text",
    cell: (c) => {
      const p = c.row.original.phase;
      return p.text ? (
        <StatusChip
          health={p.health}
          label={p.text}
          variant="text"
          size="sm"
          className="max-w-40"
          title={p.cap ? `${p.text} — ${phraseText(p.cap.code, p.cap.params, p.cap.text)}` : p.text}
        />
      ) : (
        <Reported value={p.text} />
      );
    },
  }),
  col.display({
    id: "primary",
    header: () => m.col_primary(),
    cell: (c) => <span className="whitespace-nowrap font-code text-label">{primaryText(c.row.original.primary)}</span>,
  }),
  col.display({
    id: "instances",
    header: () => m.col_instances(),
    cell: (c) => {
      const i = c.row.original.instances;
      return (
        <StateText health={i.health} title={m.instances_ready({ ready: i.ready ?? "?", desired: i.desired ?? "?" })}>
          {i.ready ?? "?"}/{i.desired ?? "?"}
        </StateText>
      );
    },
  }),
  col.display({
    id: "backup",
    header: () => m.col_backup(),
    cell: (c) => {
      const b = c.row.original.backup;
      if (!b.configured)
        return (
          <span className="whitespace-nowrap text-muted" title={m.backup_not_configured()}>
            <span aria-hidden="true">{HEALTH_GLYPH.warning} </span>
            {m.backup_off()}
          </span>
        );
      return (
        <StateText health={b.health} stale={b.provenance.stale}>
          {b.lastCompletedAt ? <Time iso={b.lastCompletedAt} past /> : m.backup_none_completed()}
        </StateText>
      );
    },
  }),
  col.display({
    id: "findings",
    header: () => m.col_findings(),
    cell: (c) => <FindingSquares counts={c.row.original.findings} />,
  }),
]);

function NamespaceNotices({ namespaces }: { namespaces: NamespaceState[] | null }) {
  const blocked = (namespaces ?? []).flatMap((ns) =>
    (ns.sources ?? []).filter((s) => s.state !== "live").map((s) => ({ ns: ns.name, s })),
  );
  if (blocked.length === 0) return null;
  return (
    <ul aria-label={m.namespaces_not_listed()} className="flex flex-col gap-1">
      {blocked.map(({ ns, s }) => (
        <li key={`${ns}/${s.source}`} className="health rounded-control px-3 py-1.5 text-label" data-health="info">
          <span aria-hidden="true" className="health-glyph mr-1.5">
            i
          </span>
          {m.namespace_source_state({ namespace: ns, source: s.source, state: sourceStateWord(s.state) })}
          {s.reason && <span className="text-muted"> — {s.reason}</span>}
        </li>
      ))}
    </ul>
  );
}

function parseSort(sort: string | undefined): SortingState {
  if (!sort) return [];
  const desc = sort.startsWith("-");
  return [{ id: desc ? sort.slice(1) : sort, desc }];
}
function formatSort(s: SortingState): string | undefined {
  const first = s[0];
  return first ? `${first.desc ? "-" : ""}${first.id}` : undefined;
}

export type ClusterListViewProps = {
  data: ClusterListData;
  q: string;
  sort: string | undefined;
  onSearchChange: (next: { q?: string; sort?: string }) => void;
};

const rowKey = (r: ClusterRow) => `${r.ref.namespace}/${r.ref.name}`;

/** The selection column: two Clusters, then the compare link. A third tick is refused rather than dropping one silently. */
function selectColumn(selected: string[], toggle: (key: string) => void) {
  return col.display({
    id: "select",
    header: () => <span className="sr-only">{m.compare_select_column()}</span>,
    cell: (c) => {
      const key = rowKey(c.row.original);
      const on = selected.includes(key);
      return (
        <input
          type="checkbox"
          checked={on}
          disabled={!on && selected.length >= 2}
          onChange={() => toggle(key)}
          aria-label={m.compare_select({ cluster: key })}
        />
      );
    },
  });
}

/** The "/" screen: one row per Cluster the caller may get, and the namespaces that could not be listed. */
export function ClusterListView({ data, q, sort, onSearchChange }: ClusterListViewProps) {
  const filterId = useId();
  const items = data.items ?? [];
  const [selected, setSelected] = useState<string[]>([]);
  const withSelect = useMemo(
    () =>
      col.columns([
        selectColumn(selected, (key) =>
          setSelected((prev) => (prev.includes(key) ? prev.filter((k) => k !== key) : [...prev, key].slice(0, 2))),
        ),
        ...columns,
      ]),
    [selected],
  );
  const [left, right] = selected;
  const shown = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return needle ? items.filter((r) => `${r.ref.namespace}/${r.ref.name}`.toLowerCase().includes(needle)) : items;
  }, [items, q]);
  return (
    <div className="flex flex-col gap-3">
      <NamespaceNotices namespaces={data.namespaces} />
      {(data.unchecked ?? 0) > 0 && (
        <p role="status" className="health rounded-control px-3 py-1.5 text-label" data-health="unknown">
          <span aria-hidden="true" className="health-glyph mr-1.5">
            ?
          </span>
          {m.clusters_unchecked({ count: data.unchecked ?? 0 })}
        </p>
      )}
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <label htmlFor={filterId} className="text-label">
            {m.filter_label()}
          </label>
          <input
            id={filterId}
            type="search"
            value={q}
            onChange={(e) => onSearchChange({ q: e.target.value || undefined })}
            className="h-8 w-64 max-w-full rounded-control border border-line-strong bg-surface px-2"
          />
        </div>
        <p className="text-caption text-muted">
          {m.generated_at()} <Time iso={data.generatedAt} />
        </p>
      </div>
      <p className="flex flex-wrap items-center gap-2 text-label">
        {left && right ? (
          <Link to="/compare" search={{ left, right }} className="font-semibold text-accent hover:underline">
            {m.compare_selected({ left, right })}
          </Link>
        ) : (
          <span className="text-muted">{m.compare_select_hint({ count: selected.length })}</span>
        )}
        {selected.length > 0 && (
          <button type="button" onClick={() => setSelected([])} className="text-accent hover:underline">
            {m.compare_clear()}
          </button>
        )}
      </p>
      <DataTable
        caption={m.clusters_caption()}
        data={shown}
        columns={withSelect}
        getRowId={rowKey}
        rowHealth={(r) => r.verdict.health}
        rowLink
        sorting={parseSort(sort)}
        onSortingChange={(s) => onSearchChange({ sort: formatSort(s) })}
        empty={
          items.length === 0 ? (
            <EmptyState title={m.clusters_none()}>{m.clusters_none_detail()}</EmptyState>
          ) : (
            m.clusters_no_match({ q })
          )
        }
      />
    </div>
  );
}

export function ClusterListScreen(props: Omit<ClusterListViewProps, "data">) {
  const query = useListClusters();
  return (
    <>
      <h1 className="font-semibold text-display">{m.nav_clusters()}</h1>
      <QueryState query={query} what={m.nav_clusters()}>
        {(data) => <ClusterListView data={data} {...props} />}
      </QueryState>
    </>
  );
}
