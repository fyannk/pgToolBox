import { Link } from "@tanstack/react-router";
import { useId } from "react";
import { useCompareClusters, useListClusters } from "../api/generated/endpoints/clusters/clusters";
import type { ComparedCluster, CompareRow, CompareSection, CompareValue, Comparison } from "../api/generated/model";
import { BlindSpots } from "../ds/patterns/BlindSpots/BlindSpots";
import { type Columns, columnHelper, DataTable } from "../ds/patterns/DataTable/DataTable";
import { originWord, Provenance } from "../ds/patterns/Provenance/Provenance";
import { StatusChip } from "../ds/patterns/StatusChip/StatusChip";
import { Time } from "../ds/time";
import { m } from "../paraglide/messages.js";
import { Panel, Titled } from "./Panel";
import { QueryState } from "./QueryState";

/** "namespace/name" as used in the compare route's search parameters. */
export type ClusterKey = string;
export function splitKey(key: ClusterKey | undefined): { namespace: string; name: string } | undefined {
  if (!key) return undefined;
  const i = key.indexOf("/");
  if (i <= 0 || i === key.length - 1) return undefined;
  return { namespace: key.slice(0, i), name: key.slice(i + 1) };
}

function sectionTitle(id: string): string {
  switch (id) {
    case "version":
      return m.compare_section_version();
    case "instances":
      return m.compare_section_instances();
    case "parameters":
      return m.compare_section_parameters();
    case "resources":
      return m.compare_section_resources();
    case "storage":
      return m.compare_section_storage();
    case "backup":
      return m.compare_section_backup();
    default:
      return m.compare_section_topology();
  }
}

/** A value as read: the text, "not set" (the default applies) or "cannot read" (never equal to anything). */
function Value({ v }: { v: CompareValue }) {
  if (v.state === "set") return <span className="break-all font-code">{v.value}</span>;
  if (v.state === "unset") return <span className="text-muted">{m.compare_unset()}</span>;
  return (
    <span className="health rounded-control px-1.5 text-caption" data-health="unknown">
      {m.compare_unknown()}
    </span>
  );
}

function rowState(r: CompareRow): "same" | "differs" | "unknown" {
  if (r.same) return "same";
  return r.left.state === "unknown" || r.right.state === "unknown" ? "unknown" : "differs";
}

function columnsFor(left: string, right: string): Columns<CompareRow> {
  const c = columnHelper<CompareRow>();
  return c.columns([
    c.display({
      id: "key",
      header: () => m.col_setting(),
      cell: (x) => <span className="font-code">{x.row.original.key}</span>,
    }),
    c.display({ id: "left", header: () => left, cell: (x) => <Value v={x.row.original.left} /> }),
    c.display({ id: "right", header: () => right, cell: (x) => <Value v={x.row.original.right} /> }),
    c.display({
      id: "state",
      header: () => m.col_comparison(),
      cell: (x) => {
        const r = x.row.original;
        const s = rowState(r);
        return (
          <span className="flex flex-col">
            <span className={s === "same" ? "text-muted" : "font-semibold"}>
              {s === "same" ? m.compare_same() : s === "unknown" ? m.compare_cannot_tell() : m.compare_differs()}
            </span>
            {r.note && <span className="text-caption text-muted">{r.note}</span>}
          </span>
        );
      },
    }),
  ]);
}

function Side({ c, label }: { c: ComparedCluster; label: string }) {
  return (
    <Panel title={label}>
      <p className="flex flex-wrap items-center gap-2">
        <Link
          to="/c/$namespace/$cluster"
          params={{ namespace: c.ref.namespace, cluster: c.ref.name }}
          className="font-semibold text-accent hover:underline"
        >
          <span className="text-muted">{c.ref.namespace}/</span>
          {c.ref.name}
        </Link>
        <StatusChip health={c.health} size="sm" stale={c.provenance.stale} />
      </p>
      {c.phase && <p className="text-label">{c.phase}</p>}
      <Provenance provenance={c.provenance} />
    </Panel>
  );
}

function Section({
  s,
  columns,
  onlyDifferences,
}: {
  s: CompareSection;
  columns: Columns<CompareRow>;
  onlyDifferences: boolean;
}) {
  const rows = (s.rows ?? []).filter((r) => !onlyDifferences || !r.same);
  const title = sectionTitle(s.id);
  return (
    <Titled title={`${title} · ${m.compare_differences({ count: s.differences })}`}>
      <p className="text-caption text-muted">
        {originWord(s.origin)} · {s.source}
      </p>
      <DataTable
        caption={title}
        data={rows}
        columns={columns}
        getRowId={(r) => r.key}
        rowHealth={(r) => (rowState(r) === "unknown" ? "unknown" : undefined)}
        isHighlightedRow={(r) => rowState(r) === "differs"}
        empty={onlyDifferences ? m.compare_section_no_differences() : m.compare_section_empty()}
      />
    </Titled>
  );
}

/**
 * Two Clusters side by side: versions, instances, parameters, resources,
 * storage, backup configuration and topology. The server decides what is
 * the same; a value the console cannot read is never the same as anything.
 */
export function CompareView({
  data,
  onlyDifferences,
  onOnlyDifferencesChange,
}: {
  data: Comparison;
  onlyDifferences: boolean;
  onOnlyDifferencesChange: (next: boolean) => void;
}) {
  const toggle = useId();
  const left = `${data.left.ref.namespace}/${data.left.ref.name}`;
  const right = `${data.right.ref.namespace}/${data.right.ref.name}`;
  const columns = columnsFor(left, right);
  return (
    <div className="flex flex-col gap-4">
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <Side c={data.left} label={m.compare_left()} />
        <Side c={data.right} label={m.compare_right()} />
      </div>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <p role="status" className="font-semibold">
          {m.compare_differences({ count: data.differences })}
        </p>
        <label htmlFor={toggle} className="flex items-center gap-2 text-label">
          <input
            id={toggle}
            type="checkbox"
            checked={onlyDifferences}
            onChange={(e) => onOnlyDifferencesChange(e.target.checked)}
          />
          {m.compare_only_differences()}
        </label>
        <p className="text-caption text-muted">
          {m.generated_at()} <Time iso={data.generatedAt} />
        </p>
      </div>
      {(data.sections ?? []).map((s) => (
        <Section key={s.id} s={s} columns={columns} onlyDifferences={onlyDifferences} />
      ))}
      <BlindSpots items={data.blindSpots} />
    </div>
  );
}

/** The two pickers: every Cluster the caller may list, one per side. */
function ComparePicker({
  left,
  right,
  onChange,
}: {
  left: ClusterKey | undefined;
  right: ClusterKey | undefined;
  onChange: (next: { left?: ClusterKey; right?: ClusterKey }) => void;
}) {
  const query = useListClusters();
  const leftId = useId();
  const rightId = useId();
  const keys = (query.data?.items ?? []).map((r) => `${r.ref.namespace}/${r.ref.name}`);
  const select = (id: string, label: string, value: ClusterKey | undefined, side: "left" | "right") => (
    <div className="flex flex-col gap-1">
      <label htmlFor={id} className="text-label">
        {label}
      </label>
      <select
        id={id}
        value={value ?? ""}
        onChange={(e) => onChange({ [side]: e.target.value || undefined })}
        className="h-8 min-w-64 max-w-full rounded-control border border-line-strong bg-surface px-2 font-code"
      >
        <option value="">{m.compare_pick()}</option>
        {/* A Cluster named in the URL but not listed (another namespace, not listable) stays selectable. */}
        {value && !keys.includes(value) && <option value={value}>{value}</option>}
        {keys.map((k) => (
          <option key={k} value={k}>
            {k}
          </option>
        ))}
      </select>
    </div>
  );
  return (
    <div className="flex flex-wrap items-end gap-4">
      {select(leftId, m.compare_left(), left, "left")}
      {select(rightId, m.compare_right(), right, "right")}
    </div>
  );
}

function CompareResult({
  left,
  right,
  onlyDifferences,
  onOnlyDifferencesChange,
}: {
  left: { namespace: string; name: string };
  right: { namespace: string; name: string };
  onlyDifferences: boolean;
  onOnlyDifferencesChange: (next: boolean) => void;
}) {
  const query = useCompareClusters({
    leftNamespace: left.namespace,
    leftName: left.name,
    rightNamespace: right.namespace,
    rightName: right.name,
  });
  return (
    <QueryState query={query} what={m.compare_title()}>
      {(data) => (
        <CompareView data={data} onlyDifferences={onlyDifferences} onOnlyDifferencesChange={onOnlyDifferencesChange} />
      )}
    </QueryState>
  );
}

export type CompareScreenProps = {
  left: ClusterKey | undefined;
  right: ClusterKey | undefined;
  onlyDifferences: boolean;
  onSearchChange: (next: { left?: ClusterKey; right?: ClusterKey; diff?: boolean }) => void;
};

export function CompareScreen({ left, right, onlyDifferences, onSearchChange }: CompareScreenProps) {
  const l = splitKey(left);
  const r = splitKey(right);
  return (
    <>
      <h1 className="font-semibold text-display">{m.compare_title()}</h1>
      <ComparePicker left={left} right={right} onChange={onSearchChange} />
      {l && r ? (
        left === right ? (
          <p className="text-label text-muted">{m.compare_same_cluster()}</p>
        ) : (
          <CompareResult
            left={l}
            right={r}
            onlyDifferences={onlyDifferences}
            onOnlyDifferencesChange={(diff) => onSearchChange({ diff: diff || undefined })}
          />
        )
      ) : (
        <p className="text-label text-muted">{m.compare_pick_two()}</p>
      )}
    </>
  );
}
