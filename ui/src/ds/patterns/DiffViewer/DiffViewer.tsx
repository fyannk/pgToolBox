import { structuredPatch } from "diff";
import { m } from "../../../paraglide/messages.js";

/** One changed path of Go's structural diff. */
export type DiffEntry = {
  path: string;
  op: "added" | "removed" | "changed" | string;
  before?: string;
  after?: string;
  manager?: string;
};

function opWord(op: string): string {
  return op === "added" ? m.diff_added() : op === "removed" ? m.diff_removed() : m.diff_changed();
}
const OP_GLYPH: Record<string, string> = { added: "+", removed: "−", changed: "~" };

/**
 * DiffViewer — what changed between two revisions of an object.
 * The structural changes come from Go with their field managers; the line
 * view below is jsdiff over the two definitions Go serves, rendered here
 * with a sign on every line so additions and removals never rest on colour.
 * Without a base it says why there is nothing to compare.
 */
export function DiffViewer({
  hasBase,
  entries,
  before,
  after,
  truncated = false,
}: {
  hasBase: boolean;
  entries: DiffEntry[];
  /** The base definition (indented JSON), when there is one. */
  before?: string;
  /** This revision's definition; absent for a deletion. */
  after?: string;
  truncated?: boolean;
}) {
  if (!hasBase) {
    return (
      <p className="rounded-control border border-line border-dashed p-3 text-label text-muted">{m.diff_no_base()}</p>
    );
  }
  const patch =
    before !== undefined ? structuredPatch("before", "after", before, after ?? "", "", "", { context: 3 }) : null;
  return (
    <div className="flex flex-col gap-4">
      <section className="flex flex-col gap-2">
        <h3 className="font-semibold text-label">{m.diff_changes()}</h3>
        {truncated && (
          <p className="health rounded-control px-3 py-1.5 text-label" data-health="warning">
            {m.diff_truncated()}
          </p>
        )}
        {entries.length === 0 ? (
          <p className="text-label text-muted">{m.diff_identical()}</p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-left text-label">
              <thead>
                <tr className="border-line border-b">
                  <th scope="col" className="py-1 pr-4 font-semibold">
                    {m.diff_col_path()}
                  </th>
                  <th scope="col" className="py-1 pr-4 font-semibold">
                    {m.diff_col_change()}
                  </th>
                  <th scope="col" className="py-1 pr-4 font-semibold">
                    {m.diff_col_before()}
                  </th>
                  <th scope="col" className="py-1 pr-4 font-semibold">
                    {m.diff_col_after()}
                  </th>
                  <th scope="col" className="py-1 font-semibold">
                    {m.diff_col_manager()}
                  </th>
                </tr>
              </thead>
              <tbody>
                {entries.map((e) => (
                  <tr key={`${e.op}:${e.path}`} className="border-line border-b align-top">
                    <td className="py-1 pr-4 font-code">{e.path}</td>
                    <td className="py-1 pr-4 whitespace-nowrap">
                      <span aria-hidden="true" className="font-code">
                        {OP_GLYPH[e.op] ?? "~"}{" "}
                      </span>
                      {opWord(e.op)}
                    </td>
                    <td className="py-1 pr-4 font-code break-all">{e.before ?? "—"}</td>
                    <td className="py-1 pr-4 font-code break-all">{e.after ?? "—"}</td>
                    <td className="py-1 font-code">{e.manager ?? m.not_reported()}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        <p className="text-caption text-muted">{m.diff_manager_note()}</p>
      </section>
      {patch && (
        <section className="flex flex-col gap-2">
          <h3 className="font-semibold text-label">{m.diff_lines()}</h3>
          {patch.hunks.length === 0 ? (
            <p className="text-label text-muted">{m.diff_identical()}</p>
          ) : (
            patch.hunks.map((h) => (
              <figure key={`${h.oldStart}-${h.newStart}`} className="overflow-x-auto rounded-panel border border-line">
                <figcaption className="border-line border-b bg-surface-2 px-3 py-1 text-caption text-muted">
                  {m.diff_hunk({ from: String(h.newStart), to: String(h.newStart + Math.max(h.newLines - 1, 0)) })}
                </figcaption>
                <pre className="m-0 py-1 font-code text-label">
                  {h.lines
                    // jsdiff's "\ No newline at end of file" is about the text file, not the definition.
                    .filter((line) => !line.startsWith("\\"))
                    .map((line, i) => {
                      const sign = line[0];
                      const kind = sign === "+" ? "added" : sign === "-" ? "removed" : "context";
                      return (
                        // biome-ignore lint/suspicious/noArrayIndexKey: hunk lines are positional.
                        <div key={i} className="diff-line flex gap-2 px-3" data-kind={kind}>
                          <span aria-hidden="true" className="w-3 shrink-0 select-none">
                            {kind === "added" ? "+" : kind === "removed" ? "−" : " "}
                          </span>
                          {kind !== "context" && (
                            <span className="sr-only">
                              {kind === "added" ? m.diff_line_added() : m.diff_line_removed()}:{" "}
                            </span>
                          )}
                          <span className="whitespace-pre-wrap break-all">{line.slice(1)}</span>
                        </div>
                      );
                    })}
                </pre>
              </figure>
            ))
          )}
        </section>
      )}
    </div>
  );
}
