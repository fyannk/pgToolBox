import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, spyOn, userEvent } from "storybook/test";
import { compact, dark, darkCompact } from "../../../testing/variants";
import { CopyCode } from "./CopyCode";

/**
 * A line the reader runs elsewhere — a Cluster spec change, SQL for pgAdmin —
 * shown in full with a Copy button. Use it for text the console will not run
 * itself; never for secrets. The copy result is announced (role=status); when
 * the clipboard is refused the text is selected for a manual copy.
 */
const meta = {
  title: "Patterns/CopyCode",
  component: CopyCode,
  args: {
    code: "spec.monitoring.customQueriesConfigMap: [{name: pgtoolbox-insight-pack, key: queries}]",
    label: "the Cluster spec change",
  },
} satisfies Meta<typeof CopyCode>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Default: Story = {
  play: async ({ canvas, args }) => {
    // The headless browser may leave a clipboard permission pending: the story stands in for it.
    const write = spyOn(navigator.clipboard, "writeText").mockResolvedValue();
    await userEvent.click(canvas.getByRole("button", { name: "Copy the Cluster spec change" }));
    await expect(write).toHaveBeenCalledWith(args.code);
    await expect(await canvas.findByText("Copied.")).toBeVisible();
    write.mockRestore();
  },
};
export const Dark = dark(Default);
export const Compact = compact(Default);
export const DarkCompact = darkCompact(Default);
export const LongSql: Story = {
  args: {
    code: "SELECT query FROM pg_stat_statements WHERE queryid = -1840029318271940210 LIMIT 1;",
    label: "the text query of -1840029318271940210",
  },
};
