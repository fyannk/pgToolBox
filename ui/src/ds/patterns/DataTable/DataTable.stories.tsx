import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, userEvent } from "storybook/test";
import { events } from "../../../../mocks/fixtures";
import type { EventRow } from "../../../api/generated/model";
import { compact, dark, darkCompact } from "../../../testing/variants";
import { StatusChip } from "../StatusChip/StatusChip";
import { columnHelper, DataTable } from "./DataTable";

const col = columnHelper<EventRow>();
const columns = col.columns([
  col.accessor("reason", {
    header: () => "Reason",
    sortFn: "text",
    cell: (c) => <StatusChip health={c.row.original.health} label={c.getValue()} size="sm" />,
  }),
  col.accessor("count", { header: () => "Count", sortFn: "alphanumeric", cell: (c) => c.getValue() }),
  col.accessor("lastSeen", { header: () => "Last seen", sortFn: "datetime", cell: (c) => c.getValue() }),
  col.display({
    id: "object",
    header: () => "Object",
    cell: (c) => `${c.row.original.object.kind}/${c.row.original.object.name ?? ""}`,
  }),
]);

/**
 * Sticky header, sortable columns announced with aria-sort, row height from
 * the density token, and an explicit empty state. Sorting is controlled by the
 * route (URL state) on screens; uncontrolled here.
 */
const meta = {
  title: "Patterns/DataTable",
  component: DataTable<EventRow>,
  args: { caption: "Events", data: events.items, columns, getRowId: (r: EventRow) => `${r.reason}-${r.lastSeen}` },
} satisfies Meta<typeof DataTable<EventRow>>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Default: Story = {
  play: async ({ canvas }) => {
    const header = canvas.getByRole("columnheader", { name: /Count/ });
    await expect(header).toHaveAttribute("aria-sort", "none");
    await userEvent.click(canvas.getByRole("button", { name: /Count/ }));
    await expect(header).toHaveAttribute("aria-sort", "ascending");
    await userEvent.click(canvas.getByRole("button", { name: /Count/ }));
    await expect(header).toHaveAttribute("aria-sort", "descending");
  },
};
export const Dark = dark({ args: Default.args });
export const Compact = compact({ args: Default.args });
export const DarkCompact = darkCompact({ args: Default.args });

export const Empty: Story = {
  args: { data: [], empty: "No event in the last 1h0m0s." },
  play: async ({ canvas }) => {
    await expect(canvas.getByText("No event in the last 1h0m0s.")).toBeVisible();
  },
};

export const ErrorRows: Story = {
  args: { isErrorRow: (r: EventRow) => r.type === "Warning" },
};
