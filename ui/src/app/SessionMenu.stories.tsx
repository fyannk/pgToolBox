import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect } from "storybook/test";
import { session, sessionMcpOff } from "../../mocks/fixtures";
import { dark } from "../testing/variants";
import { SessionMenuView } from "./Shell";

/** Who is signed in: the user, the assistants link where MCP is served, and the sign-out form. */
const meta = {
  title: "Patterns/SessionMenu",
  component: SessionMenuView,
  args: { session },
  decorators: [
    (Story) => (
      <div className="bg-topbar p-3 text-topbar-fg">
        <Story />
      </div>
    ),
  ],
} satisfies Meta<typeof SessionMenuView>;
export default meta;
type Story = StoryObj<typeof meta>;

export const SignedIn: Story = {
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("button", { name: "Sign out" })).toBeVisible();
    // The assistants page (connected assistants, connect one) is reached from here.
    await expect(canvas.getByRole("link", { name: "Assistants" })).toHaveAttribute("href", "/assistants");
  },
};
export const SignedInDark = dark(SignedIn);
/** A console without MCP (features.mcp false): no link to a page that would only say so. */
export const SignedInMcpOff: Story = {
  args: { session: sessionMcpOff },
  play: async ({ canvas }) => {
    await expect(canvas.getByRole("button", { name: "Sign out" })).toBeVisible();
    await expect(canvas.queryByRole("link", { name: "Assistants" })).toBeNull();
  },
};
