import {
  type ColumnDef,
  createColumnHelper,
  createSortedRowModel,
  flexRender,
  type RowData,
  rowSortingFeature,
  type SortingState,
  sortFn_alphanumeric,
  sortFn_datetime,
  sortFn_text,
  tableFeatures,
  useTable,
} from "@tanstack/react-table";
import { type ReactNode, useState } from "react";
import { m } from "../../../paraglide/messages.js";
import { cn } from "../../cn";
import { TINTED_HEALTH } from "../../health";
import { followRowLink } from "../../rowLink";

/** The features every console table has: client-side sorting, nothing else. */
const tableFeaturesSorted = tableFeatures({
  rowSortingFeature,
  sortedRowModel: createSortedRowModel(),
  sortFns: { alphanumeric: sortFn_alphanumeric, text: sortFn_text, datetime: sortFn_datetime },
});
export type Features = typeof tableFeaturesSorted;
export type Columns<T extends RowData> = Array<ColumnDef<Features, T>>;
export function columnHelper<T extends RowData>() {
  return createColumnHelper<Features, T>();
}
export type { SortingState };

/**
 * Row ids for rows whose natural key may repeat (Events carry no UID, and
 * two can share kind, name, reason and second): a repeat gets `#2`, `#3`,
 * … in list order, so every id is unique, and stable while the server's
 * order is. React keys and the table's row map then never collide.
 */
export function occurrenceRowIds<T extends object>(rows: readonly T[], base: (row: T) => string): (row: T) => string {
  const ids = new WeakMap<T, string>();
  const used = new Set<string>();
  for (const row of rows) {
    const b = base(row);
    let id = b;
    for (let n = 2; used.has(id); n++) id = `${b}#${n}`;
    used.add(id);
    ids.set(row, id);
  }
  return (row) => ids.get(row) ?? base(row);
}

export type DataTableProps<T extends RowData> = {
  /** Visible caption: what the table lists. */
  caption: string;
  data: readonly T[];
  columns: Columns<T>;
  getRowId: (row: T) => string;
  /** Controlled sorting (URL-synced by the route); uncontrolled when absent. */
  sorting?: SortingState;
  onSortingChange?: (next: SortingState) => void;
  /** Shown in place of the body when data is empty. */
  empty?: ReactNode;
  /** Marks a row as an error row (e.g. an unreadable instance). */
  isErrorRow?: (row: T) => boolean;
  /** Marks a row the reader should look at first (e.g. a difference), without calling it an error. */
  isHighlightedRow?: (row: T) => boolean;
  /**
   * The row's overall state. A row that is not fine is tinted as a whole, and
   * the "ok" chips inside it lose their green, so one cell does not contradict
   * the row. Defaults to the row's own `health` field when it has one.
   */
  rowHealth?: (row: T) => string | undefined;
  /**
   * Each row leads where its first link goes: a click anywhere on the row
   * that is not on another control follows that link (Ctrl, ⌘ or the middle
   * button open it in a new tab, as on the link). The link stays the one
   * keyboard and screen-reader target; the row adds no tab stop.
   */
  rowLink?: boolean;
};

function ownHealth(row: unknown): string | undefined {
  const h = (row as { health?: unknown } | null)?.health;
  return typeof h === "string" ? h : undefined;
}

function ariaSort(dir: false | "asc" | "desc"): "ascending" | "descending" | "none" {
  return dir === "asc" ? "ascending" : dir === "desc" ? "descending" : "none";
}

/**
 * DataTable — sticky header, sortable columns announced with aria-sort,
 * row height from the density token, an explicit empty state, and a focusable
 * scroll region on narrow screens.
 */
export function DataTable<T extends RowData>({
  caption,
  data,
  columns,
  getRowId,
  sorting: controlled,
  onSortingChange,
  empty,
  isErrorRow,
  isHighlightedRow,
  rowHealth = ownHealth,
  rowLink = false,
}: DataTableProps<T>) {
  const [local, setLocal] = useState<SortingState>([]);
  const sorting = controlled ?? local;
  const table = useTable({
    features: tableFeaturesSorted,
    columns,
    data: data as T[],
    getRowId,
    // First activation always sorts ascending, whatever the column type.
    sortDescFirst: false,
    state: { sorting },
    onSortingChange: (updater) => {
      const next = typeof updater === "function" ? updater(sorting) : updater;
      if (onSortingChange) onSortingChange(next);
      else setLocal(next);
    },
  });
  const rows = table.getRowModel().rows;
  return (
    // biome-ignore lint/a11y/noNoninteractiveTabindex: a scrollable region must be reachable by keyboard (WCAG 2.1.1).
    <section className="overflow-x-auto rounded-panel border border-line bg-surface" tabIndex={0} aria-label={caption}>
      <table className="w-full border-collapse text-label">
        <caption className="sr-only">{caption}</caption>
        <thead className="sticky top-0 bg-surface-2">
          {table.getHeaderGroups().map((group) => (
            <tr key={group.id}>
              {group.headers.map((header) => {
                const canSort = header.column.getCanSort();
                const dir = header.column.getIsSorted();
                const content = header.isPlaceholder
                  ? null
                  : flexRender(header.column.columnDef.header, header.getContext());
                return (
                  <th
                    key={header.id}
                    scope="col"
                    aria-sort={canSort ? ariaSort(dir) : undefined}
                    className="h-row whitespace-nowrap border-line border-b px-3 text-left font-code font-medium text-caption text-muted uppercase tracking-wider"
                  >
                    {canSort ? (
                      <button
                        type="button"
                        onClick={header.column.getToggleSortingHandler()}
                        className="inline-flex items-center gap-1 rounded-chip hover:text-fg"
                      >
                        {content}
                        <span aria-hidden="true" className="text-caption">
                          {dir === "asc" ? "▴" : dir === "desc" ? "▾" : "▵"}
                        </span>
                      </button>
                    ) : (
                      content
                    )}
                  </th>
                );
              })}
            </tr>
          ))}
        </thead>
        <tbody>
          {rows.length === 0 ? (
            <tr>
              <td colSpan={columns.length} className="px-3 py-6 text-center text-muted">
                {empty ?? m.table_empty()}
              </td>
            </tr>
          ) : (
            rows.map((row) => {
              const health = rowHealth(row.original);
              const tinted = health !== undefined && TINTED_HEALTH.has(health);
              const marked = isErrorRow?.(row.original) || isHighlightedRow?.(row.original);
              return (
                <tr
                  key={row.id}
                  data-error-row={isErrorRow?.(row.original) ? "" : undefined}
                  data-highlighted-row={isHighlightedRow?.(row.original) ? "" : undefined}
                  data-row-health={tinted ? health : undefined}
                  data-row-link={rowLink ? "" : undefined}
                  onClick={rowLink ? followRowLink : undefined}
                  onAuxClick={rowLink ? followRowLink : undefined}
                  className={cn(
                    "border-line border-b last:border-b-0",
                    marked && !tinted && "bg-surface-2",
                    tinted && "health-row",
                  )}
                >
                  {row.getAllCells().map((cell) => (
                    <td key={cell.id} className="h-row px-3 py-1 align-middle">
                      {flexRender(cell.column.columnDef.cell, cell.getContext())}
                    </td>
                  ))}
                </tr>
              );
            })
          )}
        </tbody>
      </table>
    </section>
  );
}
