import { expect, test, vi } from "vitest";
import { clusterList, events, failingOverOverview, instances } from "../../mocks/fixtures";
import { renderInFrame } from "../testing/render";
import { ClusterListView } from "./ClusterList";
import { EventsView } from "./Events";
import { InstancesView } from "./Instances";
import { OverviewView } from "./Overview";

test("cluster list: a namespace that could not be listed is named, not hidden", async () => {
  const screen = await renderInFrame(
    <ClusterListView data={clusterList} q="" sort={undefined} onSearchChange={vi.fn()} />,
  );
  await expect.element(screen.getByText(/team-a-staging: clusters not permitted to the console/)).toBeVisible();
  expect(screen.getByRole("row").all().length).toBe(3);
});

test("cluster list: the filter is URL state, reported up", async () => {
  const onSearchChange = vi.fn();
  const screen = await renderInFrame(
    <ClusterListView data={clusterList} q="" sort={undefined} onSearchChange={onSearchChange} />,
  );
  await screen.getByLabelText("Filter").fill("ord");
  expect(onSearchChange).toHaveBeenLastCalledWith({ q: "ord" });
});

test("overview: the Go verdict is drawn as is, stale, with every contributor", async () => {
  const screen = await renderInFrame(<OverviewView data={failingOverOverview} />);
  const banner = screen.container.querySelector("section[data-health]");
  expect(banner?.getAttribute("data-health")).toBe("critical");
  expect(banner?.hasAttribute("data-stale")).toBe(true);
  expect(screen.getByRole("list", { name: "What the verdict was computed from" }).element().children.length).toBe(
    failingOverOverview.verdict.contributors.length,
  );
});

test("overview: an absent field reads 'not reported', never zero", async () => {
  const screen = await renderInFrame(<OverviewView data={failingOverOverview} />);
  // backups.archiving has no liveArchiving and no readyWalFiles in the golden.
  expect(screen.getByText("not reported").all().length).toBeGreaterThan(1);
});

test("instances: every unreadable instance is an error row with its reason", async () => {
  const screen = await renderInFrame(<InstancesView data={instances} />);
  expect(screen.container.querySelectorAll("tr[data-error-row]").length).toBe(3);
  await expect.element(screen.getByText(/status port without TLS \(Pod created before CNPG 1.24\)/)).toBeVisible();
});

test("events: an empty list from a watch that is not live does not claim 'no event'", async () => {
  const screen = await renderInFrame(
    <EventsView
      data={{ ...events, items: [], source: { source: "events", state: "unavailable", health: "warning" } }}
    />,
  );
  await expect.element(screen.getByText(/this is not “no event”/)).toBeVisible();
});

test("events: two Events with the same kind, name, reason and second are two rows", async () => {
  const first = events.items?.[0];
  if (!first) throw new Error("fixture has no event");
  const twin = { ...first, message: "the second one, same second" };
  const error = vi.spyOn(console, "error").mockImplementation(() => {});
  try {
    const screen = await renderInFrame(<EventsView data={{ ...events, items: [first, twin] }} />);
    await expect.element(screen.getByText("the second one, same second")).toBeVisible();
    expect(screen.container.querySelectorAll("tbody tr")).toHaveLength(2);
    expect(error.mock.calls.flat().join(" ")).not.toMatch(/same key/);
  } finally {
    error.mockRestore();
  }
});
