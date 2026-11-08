import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, within } from "storybook/test";
import { revision } from "../../mocks/fixtures";
import { compact, dark, darkCompact, french } from "../testing/variants";
import { RevisionScreen, RevisionView } from "./History";

const params = { namespace: "team-a", cluster: "orders" };
const preview = revision.preview;
if (!preview) throw new Error("getRevision.orders.json has no preview");

/**
 * One revision against its base: Go's structural diff, the line view, what
 * the change sets in motion (Go's preview against CloudNativePG's rules), the
 * definition.
 */
const meta = {
  title: "Screens/Revision",
  component: RevisionView,
  args: { data: revision, params },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof RevisionView>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Default: Story = {
  play: async ({ canvas }) => {
    const diff = within(canvas.getByRole("region", { name: /revision 42/ }));
    await expect(diff.getByText(".spec.instances")).toBeVisible();
    const motion = within(canvas.getByRole("region", { name: "What this change sets in motion" }));
    await expect(motion.getByRole("rowheader", { name: "max_connections" })).toBeVisible();
    await expect(motion.getAllByText(/switchover to the most advanced replica/).length).toBeGreaterThan(0);
    await expect(motion.queryByText(/Unverified/)).toBeNull();
  },
};
export const Dark = dark(Default);
export const Compact = compact(Default);
export const DarkCompact = darkCompact(Default);
export const French: Story = french({});
/** A first observation: nothing to compare, said. */
export const NoBase: Story = {
  args: { data: { ...revision, baseManifest: undefined, diff: { hasBase: false, entries: [] }, preview: undefined } },
};
/** A PostgreSQL major or operator minor the tables do not cover: the preview says it is unverified first. */
export const PreviewUnverified: Story = {
  args: {
    data: { ...revision, preview: { ...preview, verified: false, postgresMajor: 19, cnpgMinor: "1.31", unknown: 0 } },
  },
  play: async ({ canvas }) => {
    // Every change is classified; only the version is outside the tables.
    // (With an unknown change, Go also clears verified and the unknown lead
    // speaks instead: ChangePreview.test.tsx.)
    await expect(canvas.getByText(/Unverified: PostgreSQL 19 or CloudNativePG 1.31/)).toBeVisible();
    await expect(canvas.queryByText(/could not be classified/)).toBeNull();
  },
};
/**
 * A lower hot-standby-sensitive value: the primary restarts first. A stored
 * revision holds the spec only, so the order is given by role, without names.
 */
export const PreviewDecrease: Story = {
  args: {
    data: {
      ...revision,
      preview: {
        ...preview,
        primary: "restartFirst",
        parameters: [
          {
            name: "max_connections",
            before: "300",
            after: "200",
            effect: "restart",
            context: "postmaster",
            decrease: true,
          },
        ],
        steps: [
          { role: "primary", instances: [], action: "restartFirst" },
          { role: "replicas", instances: [], action: "restart" },
        ],
      },
    },
  },
  play: async ({ canvas }) => {
    await expect(canvas.getByText(/the primary restarts first/)).toBeVisible();
    await expect(canvas.getByText(/the order is given by role/)).toBeVisible();
  },
};
/** A revision the console does not hold: the API's 404 in words. */
export const NotFound: Story = {
  render: () => <RevisionScreen {...params} revision={9} />,
  play: async ({ canvas }) => {
    await expect(await canvas.findByRole("alert")).toBeVisible();
  },
};
