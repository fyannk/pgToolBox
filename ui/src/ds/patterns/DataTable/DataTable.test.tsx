import { describe, expect, test } from "vitest";
import { userEvent } from "vitest/browser";
import { renderInFrame } from "../../../testing/render";
import { columnHelper, DataTable, occurrenceRowIds } from "./DataTable";

type Row = { name: string; n: number };
const col = columnHelper<Row>();
const columns = col.columns([
  col.accessor("name", { header: () => "Name", sortFn: "text", cell: (c) => c.getValue() }),
  col.accessor("n", { header: () => "N", sortFn: "alphanumeric", cell: (c) => c.getValue() }),
]);
const data: Row[] = [
  { name: "b", n: 2 },
  { name: "a", n: 10 },
  { name: "c", n: 1 },
];

function names(container: HTMLElement) {
  return [...container.querySelectorAll("tbody tr td:first-child")].map((td) => td.textContent);
}

describe("DataTable", () => {
  test("sorting is announced with aria-sort and cycles none → ascending → descending, by keyboard too", async () => {
    const screen = await renderInFrame(
      <DataTable caption="Rows" data={data} columns={columns} getRowId={(r) => r.name} />,
    );
    const header = screen.getByRole("columnheader", { name: "N", exact: true }).element();
    expect(header.getAttribute("aria-sort")).toBe("none");
    const button = screen.getByRole("button", { name: "N", exact: true });
    await button.click();
    expect(header.getAttribute("aria-sort")).toBe("ascending");
    expect(names(screen.container)).toEqual(["c", "b", "a"]);
    (button.element() as HTMLButtonElement).focus();
    await userEvent.keyboard("{Enter}");
    expect(header.getAttribute("aria-sort")).toBe("descending");
    expect(names(screen.container)).toEqual(["a", "b", "c"]);
  });

  test("a row that is not fine is tinted as a whole; ok rows stay plain", async () => {
    const health: Record<string, string> = { a: "critical", b: "ok", c: "warning" };
    const screen = await renderInFrame(
      <DataTable
        caption="Rows"
        data={data}
        columns={columns}
        getRowId={(r) => r.name}
        rowHealth={(r) => health[r.name]}
      />,
    );
    const states = [...screen.container.querySelectorAll("tbody tr")].map((tr) => tr.getAttribute("data-row-health"));
    expect(states).toEqual([null, "critical", "warning"]);
  });

  test("with rowLink, a click anywhere on the row follows its link, but not a click on another control", async () => {
    const followed: string[] = [];
    const linked = col.columns([
      col.accessor("name", {
        header: () => "Name",
        cell: (c) => (
          <a
            href={`#${c.getValue()}`}
            onClick={(e) => {
              e.preventDefault();
              followed.push(c.getValue());
            }}
          >
            {c.getValue()}
          </a>
        ),
      }),
      col.accessor("n", {
        header: () => "N",
        cell: (c) => (
          <>
            <span>{c.getValue()}</span> <button type="button">act {c.getValue()}</button>
          </>
        ),
      }),
    ]);
    const screen = await renderInFrame(
      <DataTable caption="Rows" data={data} columns={linked} getRowId={(r) => r.name} rowLink />,
    );
    await screen.getByText("10", { exact: true }).click();
    expect(followed).toEqual(["a"]);
    await screen.getByRole("button", { name: "act 2" }).click();
    expect(followed).toEqual(["a"]);
  });

  test("controlled sorting reports changes instead of applying them", async () => {
    const seen: unknown[] = [];
    const screen = await renderInFrame(
      <DataTable
        caption="Rows"
        data={data}
        columns={columns}
        getRowId={(r) => r.name}
        sorting={[]}
        onSortingChange={(s) => seen.push(s)}
      />,
    );
    await screen.getByRole("button", { name: /Name/ }).click();
    expect(seen).toEqual([[{ id: "name", desc: false }]]);
  });

  test("an empty list says so in words and the scroll region is keyboard reachable", async () => {
    const screen = await renderInFrame(
      <DataTable caption="Rows" data={[]} columns={columns} getRowId={(r) => r.name} empty="Nothing here." />,
    );
    await expect.element(screen.getByText("Nothing here.")).toBeVisible();
    expect(screen.getByRole("region", { name: "Rows" }).element().getAttribute("tabindex")).toBe("0");
  });

  test("error rows are marked", async () => {
    const screen = await renderInFrame(
      <DataTable caption="Rows" data={data} columns={columns} getRowId={(r) => r.name} isErrorRow={(r) => r.n > 5} />,
    );
    expect(screen.container.querySelectorAll("tr[data-error-row]").length).toBe(1);
  });
});

test("occurrenceRowIds: a repeated natural key gets a unique, order-stable id", () => {
  const a = { k: "Pod/x/Killing/12:00:00" };
  const b = { k: "Pod/x/Killing/12:00:00" };
  const c = { k: "Pod/x/Killing/12:00:00#2" };
  const d = { k: "Pod/y/Started/12:00:00" };
  const id = occurrenceRowIds([a, b, c, d], (r) => r.k);
  const ids = [a, b, c, d].map(id);
  expect(new Set(ids).size).toBe(4);
  expect(ids[0]).toBe(a.k);
  expect(ids[3]).toBe(d.k);
  // Same list, same ids.
  expect([a, b, c, d].map(occurrenceRowIds([a, b, c, d], (r) => r.k))).toEqual(ids);
});
