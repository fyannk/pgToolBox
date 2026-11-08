import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import { compact, dark, darkCompact } from "../../../testing/variants";
import { DiffViewer } from "./DiffViewer";

const before = JSON.stringify(
  { spec: { instances: 3, postgresql: { parameters: { max_connections: "200", work_mem: "4MB" } }, backup: {} } },
  null,
  2,
);
const after = JSON.stringify(
  {
    spec: {
      instances: 3,
      postgresql: { parameters: { max_connections: "300", work_mem: "4MB", shared_buffers: "2GB" } },
    },
  },
  null,
  2,
);
const entries = [
  {
    path: "spec.postgresql.parameters.max_connections",
    op: "changed",
    before: '"200"',
    after: '"300"',
    manager: "kubectl-edit",
  },
  { path: "spec.postgresql.parameters.shared_buffers", op: "added", after: '"2GB"', manager: "kubectl-edit" },
  { path: "spec.backup", op: "removed", before: "{}" },
];

/**
 * What changed between two revisions. Use it on the history screen for one
 * revision against its base; the structural list and its field managers are
 * Go's, the line view is drawn from the two definitions Go serves. Signs, not
 * colours, say added or removed; a change is not a verdict.
 */
const meta = {
  title: "Patterns/DiffViewer",
  component: DiffViewer,
  args: { hasBase: true, entries, before, after },
} satisfies Meta<typeof DiffViewer>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Changed: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByText("spec.postgresql.parameters.max_connections")).toBeVisible();
    await expect(canvas.getAllByText(/added line/).length).toBeGreaterThan(0);
  },
};
export const ChangedDark = dark(Changed);
export const ChangedCompact = compact(Changed);
export const ChangedDarkCompact = darkCompact(Changed);
/** First observation, a deletion, or an evicted base: said, not drawn as empty. */
export const NoBase: Story = {
  args: { hasBase: false, entries: [] },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/Nothing to compare with/)).toBeVisible();
  },
};
export const Truncated: Story = { args: { truncated: true } };
export const Identical: Story = { args: { entries: [], after: before } };
/** A deletion keeps its structural list; there is no line view of nothing. */
export const Deleted: Story = { args: { after: undefined } };
