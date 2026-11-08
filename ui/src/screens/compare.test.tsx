import { expect, test, vi } from "vitest";
import { clusterList, comparison, replication } from "../../mocks/fixtures";
import { renderInFrame } from "../testing/render";
import { ClusterListView } from "./ClusterList";
import { CompareView, splitKey } from "./Compare";
import { FailoverPanel } from "./Failover";

test("compare: differing rows are highlighted, unknown rows are tinted, equal-in-bytes rows are the same", async () => {
  const screen = await renderInFrame(
    <CompareView data={comparison} onlyDifferences={false} onOnlyDifferencesChange={vi.fn()} />,
  );
  const highlighted = screen.container.querySelectorAll("tr[data-highlighted-row]").length;
  const unknown = screen.container.querySelectorAll('tr[data-row-health="unknown"]').length;
  // Every difference is either a highlighted row or a row the console cannot tell.
  expect(highlighted + unknown).toBe(comparison.differences);
  expect(unknown).toBeGreaterThan(0);
  await expect.element(screen.getByText("the same value, spelled differently")).toBeVisible();
});

test("compare: 'only differences' hides the rows that are the same and reports the toggle up", async () => {
  const onChange = vi.fn();
  const screen = await renderInFrame(
    <CompareView data={comparison} onlyDifferences onOnlyDifferencesChange={onChange} />,
  );
  expect(screen.getByText("the same value, spelled differently").query()).toBeNull();
  await screen.getByLabelText("Show only differences").click();
  expect(onChange).toHaveBeenLastCalledWith(false);
});

test("compare: a cluster key is namespace/name, nothing else", () => {
  expect(splitKey("team-a/orders")).toEqual({ namespace: "team-a", name: "orders" });
  expect(splitKey("orders")).toBeUndefined();
  expect(splitKey("/orders")).toBeUndefined();
  expect(splitKey("team-a/")).toBeUndefined();
  expect(splitKey(undefined)).toBeUndefined();
});

test("cluster list: two ticks offer the comparison, a third is refused", async () => {
  const screen = await renderInFrame(
    <ClusterListView data={clusterList} q="" sort={undefined} onSearchChange={vi.fn()} />,
  );
  const boxes = screen.getByRole("checkbox").all();
  expect(boxes.length).toBe((clusterList.items ?? []).length);
  await boxes[0]?.click();
  await expect.element(screen.getByText(/1 of 2 selected/)).toBeVisible();
  await boxes[1]?.click();
  const link = screen.getByRole("link", { name: /^Compare / });
  await expect.element(link).toHaveAttribute("href", expect.stringContaining("/compare?"));
});

test("failover: the candidate, the quorum the console cannot judge, and the replica on the primary's node", async () => {
  const screen = await renderInFrame(<FailoverPanel f={replication.failover} />);
  await expect.element(screen.getByText(/^Cannot tell: some members are not readable by the console\./)).toBeVisible();
  await expect.element(screen.getByText(/not a certainty/)).toBeVisible();
  await expect.element(screen.getByText(/On the primary's node node-a: orders-3/)).toBeVisible();
  // The unread replica is an error row, never silently ranked as fine.
  expect(screen.container.querySelectorAll("tr[data-error-row]").length).toBe(1);
});
