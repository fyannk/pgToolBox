import { expect, test } from "vitest";
import { page } from "vitest/browser";
import { topology } from "../../../../mocks/fixtures";
import { renderInFrame } from "../../../testing/render";
import { TopologyView } from "./TopologyView";

const nodes = topology.nodes ?? [];
const edges = topology.edges ?? [];

/** React Flow lays its nodes out after mounting: wait until every box is drawn. */
async function drawn(count = nodes.length) {
  await expect.poll(() => document.querySelectorAll(".topo-node").length).toBe(count);
}

test("draws exactly the served geometry", async () => {
  await renderInFrame(<TopologyView topology={topology} />);
  await drawn();
  expect([...document.querySelectorAll("path.topo-edge")].map((p) => p.getAttribute("d"))).toEqual(
    edges.map((e) => e.path),
  );
  // Each box is a React Flow node placed at Go's coordinates, never moved.
  const first = nodes[0];
  const node = document.querySelector<HTMLElement>(`.react-flow__node[data-id="${first?.id}"]`);
  expect(node?.style.transform).toContain(`translate(${first?.x}px, ${first?.y}px)`);
  const rect = node?.querySelector(".topo-node rect");
  expect(rect?.getAttribute("x")).toBe(String(first?.x));
  expect(rect?.getAttribute("width")).toBe(String(first?.w));
});

test("a state this build does not know is drawn as unknown, with its word", async () => {
  const odd = {
    ...topology,
    nodes: nodes.map((n) => (n.state !== "info" ? { ...n, state: "degraded" as unknown as typeof n.state } : n)),
  };
  await renderInFrame(<TopologyView topology={odd} />);
  await drawn();
  for (const g of document.querySelectorAll(".topo-node[data-health]"))
    expect(g.getAttribute("data-health")).toBe("unknown");
  await expect.element(page.getByText("unknown", { exact: true }).first()).toBeVisible();
});

test("the description is the view model's sentence", async () => {
  await renderInFrame(<TopologyView topology={topology} />);
  await expect.element(page.getByRole("region", { name: topology.title })).toHaveAccessibleDescription(topology.aria);
});

test("a box's link is offered in the list under the drawing", async () => {
  await renderInFrame(
    <TopologyView topology={topology} renderLink={(link, label) => <a href={`/c/team-a/orders/${link}`}>{label}</a>} />,
  );
  // The first box labelled orders-1 is the instance (its volume carries the same name).
  await expect
    .element(page.getByRole("link", { name: "orders-1", exact: true }).first())
    .toHaveAttribute("href", "/c/team-a/orders/instances/orders-1");
});

test("a click on a box with a link opens it; the others go nowhere", async () => {
  const opened: string[] = [];
  await renderInFrame(<TopologyView topology={topology} onOpen={(link) => opened.push(link)} />);
  await drawn();
  const instance = nodes.find((n) => n.link === "instances/orders-1");
  const plain = nodes.find((n) => !n.link);
  expect(instance).toBeDefined();
  document.querySelector<HTMLElement>(`.react-flow__node[data-id="${instance?.id}"]`)?.click();
  if (plain) document.querySelector<HTMLElement>(`.react-flow__node[data-id="${plain.id}"]`)?.click();
  expect(opened).toEqual(["instances/orders-1"]);
});

test("the controls are there, the minimap only when asked for, and no corner link", async () => {
  await renderInFrame(<TopologyView topology={topology} />);
  await drawn();
  expect(document.querySelector(".react-flow__controls")).not.toBeNull();
  expect(document.querySelector(".react-flow__minimap")).toBeNull();
  expect(document.querySelector(".react-flow__attribution")).toBeNull();
  const minimap = page.getByRole("button", { name: "Minimap" });
  await expect.element(minimap).toHaveAttribute("aria-pressed", "false");
  await minimap.click();
  await expect.poll(() => document.querySelector(".react-flow__minimap")).not.toBeNull();
});

test("kinds this build does not know are drawn by family, never dropped", async () => {
  const future = {
    ...topology,
    frames: [...(topology.frames ?? []), { kind: "kubernetesStorage", label: "k8s", x: 0, y: 0, w: 10, h: 10 }],
    edges: [...edges, { kind: "baseBackup", path: "M0 0 L10 10" }, { kind: "somethingNew", path: "M0 0 L5 5" }],
  } as unknown as typeof topology;
  await renderInFrame(<TopologyView topology={future} />);
  await drawn();
  const stroke = (sel: string) => {
    const el = document.querySelector(sel);
    return el ? getComputedStyle(el).stroke : "";
  };
  const archive = document.querySelector('path.topo-edge[data-kind="archive"]');
  if (archive) expect(stroke('path.topo-edge[data-kind="baseBackup"]')).toBe(getComputedStyle(archive).stroke);
  const read = document.querySelector('path.topo-edge[data-kind="read"]');
  if (read) expect(stroke('path.topo-edge[data-kind="somethingNew"]')).toBe(getComputedStyle(read).stroke);
  await expect.poll(() => stroke('.topo-frame[data-kind="kubernetesStorage"] rect')).not.toBe("none");
});

test("each foldable frame has a fold toggle, from the toolbar and from its heading", async () => {
  const asked: string[] = [];
  const folded = {
    ...topology,
    frames: (topology.frames ?? []).map((f) => (f.kind === "cluster" ? { ...f, folded: true } : f)),
  };
  await renderInFrame(<TopologyView topology={folded} onToggleFold={(k) => asked.push(k)} />);
  await drawn();
  const foldable = (topology.frames ?? []).filter((f) => f.foldable);
  for (const f of foldable) {
    const button = page.getByRole("button", { name: f.label, exact: true });
    await expect.element(button).toHaveAttribute("aria-pressed", f.kind === "cluster" ? "true" : "false");
  }
  await page.getByRole("button", { name: "Poolers", exact: true }).click();
  const heading = document.querySelector<SVGTextElement>(".topo-frame[data-kind=cluster] text");
  heading?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
  expect(asked).toEqual(["pool", "cluster"]);
  expect(heading?.textContent).toMatch(/^▸ /);
});

test("without onToggleFold the drawing offers no folding", async () => {
  await renderInFrame(<TopologyView topology={topology} />);
  await drawn();
  expect(document.querySelector(".topo-fold")).toBeNull();
  expect(document.querySelector(".topo-fold-toggle")).toBeNull();
});
