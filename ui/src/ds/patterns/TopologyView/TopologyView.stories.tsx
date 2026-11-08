import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import { topology } from "../../../../mocks/fixtures";
import type { Topology, TopoNode } from "../../../api/generated/model";
import { compact, dark, darkCompact } from "../../../testing/variants";
import { HEALTH_STATES } from "../../health";
import { TopologyView } from "./TopologyView";

const nodes: TopoNode[] = topology.nodes ?? [];
const servers = nodes.filter((n) => n.state !== "info");
/** The API fixture with its servers put in the given states, in order. */
const withStates = (...states: TopoNode["state"][]): Topology => ({
  ...topology,
  nodes: nodes.map((n) => {
    const i = servers.indexOf(n);
    return i < 0 ? n : { ...n, state: states[i % states.length] ?? "unknown" };
  }),
});

/**
 * The cluster's wiring as Go laid it out, in fyannk/design (Plex Mono, 4 px
 * boxes, 1 px dotted frames). Use it full width near the top of the overview;
 * it draws the view model's geometry and nothing else, so a box is never
 * moved or invented here. Health is border + glyph in the drawing and the
 * word under it. React Flow draws it: wheel or pinch to zoom, drag to pan,
 * the controls to fit; a click on a box with a link follows it, and the
 * list under the drawing offers the same links to the keyboard. Kinds
 * are styled by family (`*backup*`, `*storage*`, `*volume*`…), and a kind no
 * rule knows is drawn neutral, never dropped. Do not use it as the only
 * account of the cluster: the list under it and the view model's sentence
 * are the text alternative.
 */
const meta = {
  title: "Patterns/TopologyView",
  component: TopologyView,
  args: { topology },
} satisfies Meta<typeof TopologyView>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Served: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("region", { name: "How orders is wired" })).toBeVisible();
    await expect(canvas.getByText("States in the drawing")).toBeVisible();
    // One word per server in the list under the drawing, as many as the fixture has.
    for (const state of ["ok", "unknown"] as const)
      await expect(canvas.getAllByText(state, { exact: true })).toHaveLength(
        servers.filter((n) => n.state === state).length,
      );
  },
};
export const ServedDark = dark(Served);
export const ServedCompact = compact(Served);
export const ServedDarkCompact = darkCompact(Served);

/** Every health state on a server, glyph and word included. */
export const EveryState: Story = { args: { topology: withStates(...HEALTH_STATES.filter((h) => h !== "info")) } };
export const EveryStateDark = dark(EveryState);
export const FailingOver: Story = { args: { topology: withStates("critical", "progressing", "warning") } };
/** The last good drawing, shown as such. */
export const Stale: Story = { args: { stale: true } };
/** An empty drawing (no instance observed yet) still says what it is. */
export const Empty: Story = {
  args: { topology: { ...topology, nodes: [], edges: [], dots: [], frames: [], legend: [] } },
};

/** Boxes with a link open it on click; the list under the drawing carries the same links (here plain hrefs; the screen resolves them onto routes). */
export const Linked: Story = {
  args: {
    renderLink: (link, label) => <a href={`#${link}`}>{label}</a>,
  },
  play: async ({ canvasElement }) => {
    const link = canvasElement.querySelector('figcaption a[href$="instances/orders-1"]');
    await expect(link).toBeVisible();
    await expect(canvasElement.querySelector(".topo-flow-box[data-link]")).toBeVisible();
  },
};
export const LinkedDark = dark(Linked);

/** On a phone the drawing is fitted to the panel; the reader zooms in. */
export const Narrow: Story = {
  decorators: [
    (Story) => (
      <div className="w-80">
        <Story />
      </div>
    ),
  ],
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("region", { name: "How orders is wired" })).toBeVisible();
  },
};

/**
 * The grouped layout's newer kinds (Kubernetes storage and object storage
 * frames, a scheduled-backup box, base-backup and volume wires) and one kind
 * no rule knows: each is drawn by family or neutral.
 */
const Y = topology.height + 12;
export const FutureKinds: Story = {
  args: {
    topology: {
      ...topology,
      height: topology.height + 96,
      frames: [
        ...(topology.frames ?? []),
        { kind: "kubernetesStorage" as never, label: "Kubernetes storage", x: 8, y: Y, w: 420, h: 80 },
      ],
      nodes: [
        ...nodes,
        {
          id: "sched",
          kind: "scheduledBackup" as never,
          label: "daily",
          sub: "0 2 * * *",
          state: "info",
          x: 24,
          y: Y + 26,
          w: 140,
          h: 44,
        },
      ],
      edges: [
        ...(topology.edges ?? []),
        {
          kind: "baseBackup" as never,
          path: `M170 ${Y + 34} L400 ${Y + 34}`,
          label: "base backup",
          labelX: 285,
          labelY: Y + 28,
        },
        { kind: "volume" as never, path: `M170 ${Y + 48} L400 ${Y + 48}` },
        { kind: "notYetKnown" as never, path: `M170 ${Y + 62} L400 ${Y + 62}` },
      ],
      legend: [
        ...(topology.legend ?? []),
        { kind: "baseBackup" as never, label: "base backup" },
        { kind: "volume" as never, label: "volume" },
      ],
    },
  },
};
export const FutureKindsDark = dark(FutureKinds);
