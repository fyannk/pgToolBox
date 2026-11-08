import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, fn } from "storybook/test";
import { comparison } from "../../mocks/fixtures";
import { handlersFor } from "../../mocks/handlers";
import { compact, dark, darkCompact, french } from "../testing/variants";
import { CompareScreen, CompareView } from "./Compare";

/**
 * Two Clusters side by side. The server decides each row: same, differs, or
 * cannot tell (a side the console could not read is never "the same").
 * Memory parameters spelled differently but equal in bytes are the same, with
 * a note.
 */
const meta = {
  title: "Screens/Compare",
  component: CompareView,
  args: { data: comparison, onlyDifferences: false, onOnlyDifferencesChange: fn() },
  parameters: { layout: "fullscreen" },
} satisfies Meta<typeof CompareView>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Default: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByText(`Differences: ${comparison.differences}`)).toBeVisible();
    await expect(canvas.getByText("the same value, spelled differently")).toBeVisible();
    await expect(canvas.getAllByText("cannot read").length).toBeGreaterThan(0);
  },
};
export const Dark = dark(Default);
export const Compact = compact(Default);
export const DarkCompact = darkCompact(Default);
export const French = french({ args: Default.args });
/** Only the rows that differ or cannot be told apart. */
export const OnlyDifferences: Story = {
  args: { onlyDifferences: true },
  play: async ({ canvas }) => {
    await expect(canvas.queryByText("the same value, spelled differently")).toBeNull();
  },
};
/** Two identical Clusters: every section says it has nothing different. */
export const NoDifference: Story = {
  args: {
    data: {
      ...comparison,
      differences: 0,
      sections: (comparison.sections ?? []).map((s) => ({ ...s, differences: 0, rows: [] })),
    },
    onlyDifferences: true,
  },
};

const screen = (left?: string, right?: string) => ({
  render: () => <CompareScreen left={left} right={right} onlyDifferences={false} onSearchChange={() => {}} />,
});
export const Live: Story = {
  ...screen("team-a/orders", "team-a/billing"),
  parameters: { msw: handlersFor("default") },
  play: async ({ canvas }) => {
    await expect(await canvas.findByText(`Differences: ${comparison.differences}`)).toBeVisible();
  },
};
/** Nothing chosen yet: the two pickers and what the comparison covers. */
export const Pick: Story = { ...screen(), parameters: { msw: handlersFor("default") } };
/** Reached from a Cluster's "Compare with…": the first side is filled in. */
export const PickSecond: Story = { ...screen("team-a/orders"), parameters: { msw: handlersFor("default") } };
export const Loading: Story = {
  ...screen("team-a/orders", "team-a/billing"),
  parameters: { msw: handlersFor("loading") },
};
/** One side is not readable by the caller: the whole comparison is refused, not half-shown. */
export const Forbidden: Story = {
  ...screen("team-a/orders", "team-a/billing"),
  parameters: { msw: handlersFor("forbidden") },
  play: async ({ canvas }) => {
    await expect(await canvas.findByText(/ask for the [\w-]*viewer role/)).toBeVisible();
  },
};
/** One side does not exist (deleted, or a typo in the URL): the API's 404, said as such. */
export const NotFound: Story = {
  ...screen("team-a/orders", "team-a/gone"),
  parameters: { msw: handlersFor("default") },
  play: async ({ canvas }) => {
    await expect(await canvas.findByRole("alert")).toBeVisible();
  },
};
export const ServerError: Story = {
  ...screen("team-a/orders", "team-a/billing"),
  parameters: { msw: handlersFor("unavailable") },
};
