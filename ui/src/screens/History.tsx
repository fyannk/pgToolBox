import { keepPreviousData } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { type FormEvent, useState } from "react";
import { useGetRevision, useGetTimeline, useListHistory } from "../api/generated/endpoints/clusters/clusters";
import type { History, HistoryEntry, Revision, Timeline as TimelineVM } from "../api/generated/model";
import { cn } from "../ds/cn";
import { Reported } from "../ds/NotReported";
import { type Columns, columnHelper, DataTable } from "../ds/patterns/DataTable/DataTable";
import { DiffViewer } from "../ds/patterns/DiffViewer/DiffViewer";
import { EmptyState } from "../ds/patterns/States/States";
import { Tabs } from "../ds/patterns/Tabs/Tabs";
import { Timeline, timelineKindWord } from "../ds/patterns/Timeline/Timeline";
import { formatUtc, Time, useNow } from "../ds/time";
import { m } from "../paraglide/messages.js";
import { ChangePreviewPanel } from "./ChangePreview";
import {
  activePreset,
  formatKinds,
  type HistorySearch,
  localInputToUtc,
  parseKinds,
  presetAnchor,
  RANGE_PRESETS,
  type RangePreset,
  REANCHOR_MS,
  timelineParams,
  utcToLocalInput,
} from "./historySearch";
import { Panel } from "./Panel";
import { QueryState } from "./QueryState";
import { sourceStateWord } from "./sources";

type Params = { namespace: string; cluster: string };

function changeWord(c: string): string {
  switch (c) {
    case "created":
      return m.change_created();
    case "first-observed":
      return m.change_first_observed();
    case "spec-changed":
      return m.change_spec_changed();
    case "status-transition":
      return m.change_status_transition();
    case "deleted":
      return m.change_deleted();
    case "deleted-unobserved":
      return m.change_deleted_unobserved();
    default:
      return c;
  }
}

function columns(params: Params): Columns<HistoryEntry> {
  const hc = columnHelper<HistoryEntry>();
  return hc.columns([
    hc.accessor("observedAt", {
      header: () => m.col_observed(),
      sortFn: "datetime",
      cell: (c) => (
        <Link
          to="/c/$namespace/$cluster/history/$revision"
          params={{ ...params, revision: c.row.original.seq }}
          className="text-accent hover:underline"
        >
          <span className="font-code">#{c.row.original.seq}</span> <Time iso={c.getValue()} past />
        </Link>
      ),
    }),
    hc.accessor((r) => `${r.kind}/${r.name}`, {
      id: "object",
      header: () => m.col_object(),
      sortFn: "text",
      cell: (c) => <code className="font-code">{c.getValue()}</code>,
    }),
    hc.display({
      id: "change",
      header: () => m.col_change(),
      cell: (c) => {
        const e = c.row.original;
        return (
          <span className="flex flex-col">
            {changeWord(e.change)}
            {e.afterGap && e.prevObservedAt && (
              <span className="text-caption text-muted">{m.after_gap({ since: formatUtc(e.prevObservedAt) })}</span>
            )}
            {e.coalesced ? (
              <span className="text-caption text-muted">{m.coalesced({ count: String(e.coalesced) })}</span>
            ) : null}
          </span>
        );
      },
    }),
    hc.display({
      id: "manager",
      header: () => m.col_manager(),
      cell: (c) => (
        <span className="font-code">
          <Reported value={c.row.original.manager} />
          {c.row.original.operation && <span className="text-muted"> ({c.row.original.operation})</span>}
        </span>
      ),
    }),
  ]);
}

/** The history screen: what changed on the Cluster and its children, as this console observed it. */
export function HistoryView({ data, params }: { data: History; params: Params }) {
  return (
    <div className="flex flex-col gap-3">
      <p className="text-label">
        {m.history_source()}: <span className="font-semibold">{sourceStateWord(data.source.state)}</span>
        {data.source.reason && <span className="text-muted"> — {data.source.reason}</span>}
        <span className="text-muted"> · {data.persistent ? m.history_persistent() : m.history_not_persistent()}</span>
      </p>
      {data.evicted && (
        <p className="health rounded-control px-3 py-1.5 text-label" data-health="info">
          {m.history_evicted()}
        </p>
      )}
      <DataTable
        caption={m.caption_history()}
        data={data.items ?? []}
        columns={columns(params)}
        getRowId={(r) => String(r.seq)}
        empty={m.history_none()}
      />
      <p className="text-caption text-muted">{m.history_manager_note()}</p>
    </div>
  );
}

/** One revision: what changed against its base, and the definition itself. */
export function RevisionView({ data, params }: { data: Revision; params: Params }) {
  const e = data.entry;
  const d = data.diff;
  return (
    <div className="flex flex-col gap-4">
      <Link
        to="/c/$namespace/$cluster/history"
        params={params}
        search={{ view: "revisions" }}
        className="text-label text-accent hover:underline"
      >
        ← {m.back_to_history()}
      </Link>
      <Panel title={m.revision_title({ kind: e.kind, name: e.name, seq: String(e.seq) })}>
        <p className="text-label">
          {changeWord(e.change)} · {m.col_observed()} <Time iso={e.observedAt} past />
          {e.manager && (
            <span className="text-muted">
              {" "}
              · {m.col_manager()}: <code className="font-code">{e.manager}</code>
            </span>
          )}
        </p>
        {d.hasBase && d.baseSeq !== undefined && (
          <p className="text-label text-muted">
            {m.revision_base({ seq: String(d.baseSeq) })} {d.baseObservedAt && <Time iso={d.baseObservedAt} />}
          </p>
        )}
        <DiffViewer
          hasBase={d.hasBase}
          entries={d.entries ?? []}
          before={data.baseManifest}
          after={data.manifest}
          truncated={d.truncated}
        />
      </Panel>
      {data.preview && <ChangePreviewPanel preview={data.preview} />}
      <Panel title={m.revision_manifest()}>
        {data.manifest ? (
          <section
            // biome-ignore lint/a11y/noNoninteractiveTabindex: the definition scrolls; keyboard readers need to reach it.
            tabIndex={0}
            aria-label={m.revision_manifest_scroll()}
            className="max-h-[60dvh] overflow-auto rounded-control bg-surface-2"
          >
            <pre className="m-0 p-3 font-code text-label">{data.manifest}</pre>
          </section>
        ) : (
          <p className="text-label text-muted">{m.revision_deleted()}</p>
        )}
      </Panel>
    </div>
  );
}

/** The revision list (the History screen's second tab). */
function RevisionsScreen({ namespace, cluster }: Params) {
  const query = useListHistory(namespace, cluster);
  return (
    <QueryState query={query} what={m.history_tab_revisions()}>
      {(data) => <HistoryView data={data} params={{ namespace, cluster }} />}
    </QueryState>
  );
}

type SearchProps = { search: HistorySearch; onSearch: (next: HistorySearch) => void };

function presetWord(p: RangePreset): string {
  switch (p) {
    case "1h":
      return m.tl_range_1h();
    case "6h":
      return m.tl_range_6h();
    case "24h":
      return m.tl_range_24h();
    default:
      return m.tl_range_7d();
  }
}

const toggle =
  "rounded-control border border-line px-2 py-1 font-code text-label hover:bg-surface-2 aria-pressed:border-accent aria-pressed:bg-surface-2 aria-pressed:font-semibold";

/** The window: presets ending now, or a fixed from/to in UTC. Outside the query so a refused window can be corrected. */
function RangeControls({ search, onSearch, onChoose }: SearchProps & { onChoose: (at: number) => void }) {
  const preset = activePreset(search);
  const [open, setOpen] = useState(preset === null);
  const submit = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const f = new FormData(e.currentTarget);
    onSearch({
      ...search,
      range: undefined,
      from: localInputToUtc(String(f.get("from") ?? "")),
      to: localInputToUtc(String(f.get("to") ?? "")),
    });
  };
  return (
    <div className="flex flex-col gap-2">
      {/* biome-ignore lint/a11y/useSemanticElements: a group of toggle buttons, not a form fieldset. */}
      <div role="group" aria-label={m.tl_range_label()} className="flex flex-wrap items-center gap-1.5">
        <span className="text-label text-muted">{m.tl_range_label()}</span>
        {RANGE_PRESETS.map((p) => (
          <button
            key={p}
            type="button"
            aria-pressed={preset === p}
            className={toggle}
            onClick={() => {
              setOpen(false);
              // Choosing a preset, even the one shown, re-anchors it at now.
              onChoose(Date.now());
              onSearch({ ...search, from: undefined, to: undefined, range: p === "24h" ? undefined : p });
            }}
          >
            {presetWord(p)}
          </button>
        ))}
        <button
          type="button"
          aria-pressed={preset === null}
          aria-expanded={open}
          className={toggle}
          onClick={() => setOpen((o) => !o)}
        >
          {m.tl_range_custom()}
        </button>
      </div>
      {open && (
        <form
          key={`${search.from ?? ""}|${search.to ?? ""}`}
          onSubmit={submit}
          className="flex flex-wrap items-end gap-3 rounded-control border border-line p-2"
        >
          <label className="flex flex-col gap-0.5 text-label">
            {m.tl_from_utc()}
            <input
              type="datetime-local"
              name="from"
              defaultValue={utcToLocalInput(search.from)}
              className="h-8 rounded-control border border-line bg-surface px-1 font-code text-label"
            />
          </label>
          <label className="flex flex-col gap-0.5 text-label">
            {m.tl_to_utc()}
            <input
              type="datetime-local"
              name="to"
              defaultValue={utcToLocalInput(search.to)}
              className="h-8 rounded-control border border-line bg-surface px-1 font-code text-label"
            />
          </label>
          <button type="submit" className="h-8 rounded-control border border-line-strong bg-surface px-3 text-label">
            {m.tl_apply()}
          </button>
          <p className="m-0 basis-full text-caption text-muted">{m.tl_custom_hint()}</p>
        </form>
      )}
    </div>
  );
}

/** The kind filter: one toggle per kind with its count over the whole window (before the filter). */
function KindFilter({ data, search, onSearch }: SearchProps & { data: TimelineVM }) {
  const selected = parseKinds(search.kind);
  const counts = new Map((data.counts ?? []).map((c) => [c.kind as string, c.count]));
  const shown = [...(data.counts ?? []).map((c) => c.kind as string), ...selected.filter((k) => !counts.has(k))];
  const set = (kinds: string[]) => onSearch({ ...search, kind: formatKinds(kinds) });
  if (shown.length === 0) return null;
  return (
    // biome-ignore lint/a11y/useSemanticElements: a group of toggle buttons, not a form fieldset.
    <div role="group" aria-label={m.tl_kinds_label()} className="flex flex-wrap items-center gap-1.5">
      <span className="text-label text-muted">{m.tl_kinds_label()}</span>
      <button type="button" aria-pressed={selected.length === 0} className={toggle} onClick={() => set([])}>
        {m.tl_kinds_all()}
      </button>
      {shown.map((k) => {
        const on = selected.includes(k as (typeof selected)[number]);
        return (
          <button
            key={k}
            type="button"
            aria-pressed={on}
            className={cn(toggle, "inline-flex items-center gap-1.5")}
            onClick={() => set(on ? selected.filter((s) => s !== k) : [...selected, k])}
          >
            {timelineKindWord(k)} <span className="num text-caption text-muted">{counts.get(k) ?? 0}</span>
          </button>
        );
      })}
    </div>
  );
}

/** The incident timeline of one Cluster: what the console observed and what Kubernetes reported, on one axis. */
export function TimelineView({ data, params, search, onSearch }: SearchProps & { data: TimelineVM; params: Params }) {
  if (data.source.state === "disabled") {
    return <EmptyState title={m.tl_disabled()}>{data.source.reason ?? m.tl_disabled_detail()}</EmptyState>;
  }
  const items = data.items ?? [];
  const filtered = parseKinds(search.kind).length > 0;
  return (
    <div className="flex flex-col gap-3">
      <p className="m-0 text-label">
        {m.tl_source()}: <span className="font-semibold">{sourceStateWord(data.source.state)}</span>
        {data.source.reason && <span className="text-muted"> — {data.source.reason}</span>}
        <span className="text-muted"> · {data.persistent ? m.tl_persistent() : m.tl_not_persistent()}</span>
      </p>
      <p className="m-0 text-label text-muted">{m.tl_window({ from: formatUtc(data.from), to: formatUtc(data.to) })}</p>
      <KindFilter data={data} search={search} onSearch={onSearch} />
      <ul className="m-0 flex list-disc flex-col gap-0.5 pl-5 text-caption text-muted">
        {data.observedSince && <li>{m.tl_observed_since({ since: formatUtc(data.observedSince) })}</li>}
        <li>{m.tl_events_window({ window: data.eventsWindow })}</li>
        <li>{m.tl_timing_note()}</li>
      </ul>
      {data.evicted && (
        <p className="health m-0 rounded-control px-3 py-1.5 text-label" data-health="info">
          {m.tl_evicted()}
        </p>
      )}
      {data.truncated && (
        <p role="status" className="health m-0 rounded-control px-3 py-1.5 text-label" data-health="info">
          {m.tl_truncated({ count: String(items.length) })}
        </p>
      )}
      {items.length === 0 ? (
        <EmptyState title={filtered ? m.tl_none_filtered() : m.tl_none()} />
      ) : (
        <Timeline items={items} params={params} label={m.tl_list_label()} />
      )}
    </div>
  );
}

function TimelineQuery({ namespace, cluster, search, onSearch, anchor }: Params & SearchProps & { anchor: number }) {
  // The previous answer stays on screen while a new filter is read: the controls do not jump.
  const query = useGetTimeline(namespace, cluster, timelineParams(search, anchor), {
    query: { placeholderData: keepPreviousData },
  });
  return (
    <QueryState query={query} what={m.history_tab_timeline()}>
      {(data) => <TimelineView data={data} params={{ namespace, cluster }} search={search} onSearch={onSearch} />}
    </QueryState>
  );
}

/**
 * The History screen: the incident timeline (default) and the revision list,
 * two tabs whose state is in the URL (?view=revisions, the window, the kinds).
 */
export function HistoryScreen({ namespace, cluster, search, onSearch }: Params & SearchProps) {
  const view = search.view ?? "timeline";
  // A relative preset's start: when it was chosen, then moved on once a
  // minute so it never drifts from "the last hour" (see presetAnchor).
  const now = useNow(REANCHOR_MS);
  const [chosenAt, setChosenAt] = useState(now);
  const anchor = presetAnchor(chosenAt, now);
  return (
    <Tabs
      label={m.history_tabs()}
      value={view}
      onValueChange={(v) => onSearch({ ...search, view: v === "revisions" ? "revisions" : undefined })}
      items={[
        {
          value: "timeline",
          label: m.history_tab_timeline(),
          panel: (
            <div className="flex flex-col gap-3">
              <RangeControls search={search} onSearch={onSearch} onChoose={setChosenAt} />
              <TimelineQuery
                anchor={anchor}
                namespace={namespace}
                cluster={cluster}
                search={search}
                onSearch={onSearch}
              />
            </div>
          ),
        },
        {
          value: "revisions",
          label: m.history_tab_revisions(),
          panel: <RevisionsScreen namespace={namespace} cluster={cluster} />,
        },
      ]}
    />
  );
}

export function RevisionScreen({ namespace, cluster, revision }: Params & { revision: number }) {
  const query = useGetRevision(namespace, cluster, revision);
  return (
    <QueryState query={query} what={m.history_open({ seq: String(revision) })}>
      {(data) => <RevisionView data={data} params={{ namespace, cluster }} />}
    </QueryState>
  );
}
