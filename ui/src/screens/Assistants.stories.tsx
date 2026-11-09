import type { Meta, StoryObj } from "@storybook/react-vite";
import { HttpResponse, http } from "msw";
import { expect, fn, userEvent, waitFor, within } from "storybook/test";
import {
  assistantSetupOperate,
  assistantSetupTokenLogin,
  grantNotFound,
  grants,
  grantsEmpty,
} from "../../mocks/fixtures";
import { assistantHandlers, handlersFor } from "../../mocks/handlers";
import { ApiError } from "../api/fetcher";
import {
  getGetAssistantSetupMockHandler,
  getGetAssistantSkillMockHandler,
} from "../api/generated/endpoints/assistant/assistant.msw";
import { getListGrantsMockHandler } from "../api/generated/endpoints/session/session.msw";
import { compact, dark, darkCompact, french } from "../testing/variants";
import { AssistantsScreen, ConnectedAssistantsView } from "./Assistants";

/**
 * Assistants: the MCP clients the reader approved
 * (console grants) with a Disconnect each, and "Connect an assistant", the
 * ready-to-paste setup per client and the prompt pack, as Go renders them for
 * this console. What a connection may do (read-only, or read and operate) is
 * Go's `access`; with MCP off the console answers 404 and the page says so,
 * once, with nothing to click.
 *
 * Use it for: everything an MCP client needs from this console. Not for: the
 * consent page (served by Go at /oauth/authorize).
 *
 * Feedback rule: disconnecting is pessimistic, confirmed by typing the
 * client's name; the 204 is the revocation itself, and only then does the
 * page say the assistant is disconnected.
 */
const meta = {
  title: "Screens/Assistants",
  component: AssistantsScreen,
  parameters: { layout: "fullscreen", msw: handlersFor("default") },
} satisfies Meta<typeof AssistantsScreen>;
export default meta;
type Story = StoryObj<typeof meta>;

/** Three grants (one with the write tools, one never used), and the read-only OAuth setup. */
export const Default: Story = {
  play: async ({ canvas }) => {
    await expect(await canvas.findByText("Claude Code", { selector: "span" })).toBeVisible();
    await expect(canvas.getAllByText("name unverified")).toHaveLength(3);
    await expect(canvas.getByText("not used yet")).toBeVisible();
    await expect(await canvas.findByText("Read-only")).toBeVisible();
    const picker = canvas.getByRole("tablist", { name: "Your assistant" });
    await expect(within(picker).getAllByRole("tab")).toHaveLength(4);
    await expect(
      canvas.getByText(/^claude mcp add --transport http --scope user [\w-]+ https:\/\/console\.example\.com\/mcp$/),
    ).toBeVisible();
  },
};
export const DefaultDark = dark(Default);
export const DefaultCompact = compact(Default);
export const DefaultDarkCompact = darkCompact(Default);
export const DefaultFrench = french({ ...Default, play: undefined });

/** The client picker is a tablist: arrows move between clients, each panel is that client's own setup. */
export const PickClient: Story = {
  play: async ({ canvas }) => {
    const tab = await canvas.findByRole("tab", { name: "Claude Code" });
    tab.focus();
    await userEvent.keyboard("{ArrowRight}");
    await expect(canvas.getByRole("tab", { name: "Codex CLI" })).toHaveFocus();
    await userEvent.keyboard("{Enter}");
    await expect(canvas.getByRole("tab", { name: "Codex CLI" })).toHaveAttribute("aria-selected", "true");
    await expect(canvas.getByText(/^codex mcp login [\w-]+$/)).toBeVisible();
    await userEvent.keyboard("{End}{Enter}");
    await expect(canvas.getByRole("tab", { name: /VS Code/ })).toHaveAttribute("aria-selected", "true");
    // No add command for the generic clients: the configuration file only.
    await expect(canvas.getByText(/has no add command/)).toBeVisible();
    // Its check is in words (Go's checkNote): read, not copied. Base UI hides
    // the previous panel a render after the new tab is selected, so the Codex
    // panel's copy button can still be there for a frame: wait for it to go.
    await expect(canvas.getByText(/shows as connected once you have signed in/)).toBeVisible();
    await waitFor(() => expect(canvas.queryByRole("button", { name: /^Copy the check/ })).toBeNull());
    // Its prompt pack is the AGENTS.md section, where Go says it goes.
    await expect(canvas.getByRole("link", { name: "Download AGENTS.md" })).toHaveAttribute(
      "href",
      "/api/v1/assistant-setup/agents",
    );
  },
};

/** The write tools on: a connection may also operate, as the reader. */
export const ReadAndOperate: Story = {
  parameters: { msw: [getGetAssistantSetupMockHandler(assistantSetupOperate), ...handlersFor("default")] },
  play: async ({ canvas }) => {
    await expect(await canvas.findByText("Read and operate")).toBeVisible();
  },
};
export const ReadAndOperateDark = dark(ReadAndOperate);

/** Token sign-in on: each client also shows its static-token (break-glass) form, Go's note first. */
export const TokenLogin: Story = {
  parameters: { msw: [getGetAssistantSetupMockHandler(assistantSetupTokenLogin), ...handlersFor("default")] },
  play: async ({ canvas }) => {
    await expect(await canvas.findByRole("heading", { name: "Break-glass: a static token" })).toBeVisible();
    await expect(canvas.getByText(/headersHelper command at each connection/)).toBeVisible();
  },
};
export const TokenLoginDarkCompact = darkCompact(TokenLogin);

/** No grant: said in words, with where to go next. */
export const NoAssistant: Story = {
  parameters: { msw: [getListGrantsMockHandler(grantsEmpty), ...handlersFor("default")] },
  play: async ({ canvas }) => {
    await expect(await canvas.findByText("No assistant is connected as you.")).toBeVisible();
  },
};
export const NoAssistantDark = dark(NoAssistant);

/** MCP is off on this console: one honest state, Go's reason, no dead link or download. */
export const McpOff: Story = {
  parameters: { msw: handlersFor("mcpOff") },
  play: async ({ canvas }) => {
    await expect(await canvas.findByText("This console does not serve AI assistants.")).toBeVisible();
    await expect(canvas.getByText(/MCP_ENABLED is off/)).toBeVisible();
    await expect(canvas.queryAllByRole("link")).toHaveLength(0);
    await expect(canvas.queryAllByRole("button")).toHaveLength(0);
  },
};
export const McpOffDark = dark(McpOff);
export const McpOffCompact = compact(McpOff);

export const Loading: Story = { parameters: { msw: handlersFor("loading") } };
export const LoadingDark = dark(Loading);
export const ServerError: Story = {
  parameters: { msw: handlersFor("unavailable") },
  play: async ({ canvas }) => {
    await waitFor(() => expect(canvas.getAllByRole("alert").length).toBeGreaterThan(0));
    await expect(canvas.getAllByText(/Request id req-fixture-0001/).length).toBeGreaterThan(0);
  },
};
export const ServerErrorDark = dark(ServerError);

/** The prompt pack could not be read: the error stays inside its own disclosure. */
export const PackFailed: Story = {
  parameters: {
    msw: [
      getGetAssistantSkillMockHandler(() => {
        throw HttpResponse.json(
          { title: "Service Unavailable", status: 503, detail: "try again" },
          { status: 503, headers: { "Content-Type": "application/problem+json", "X-Request-Id": "req-pack-1" } },
        );
      }),
      ...handlersFor("default"),
    ],
  },
  play: async ({ canvas }) => {
    await userEvent.click(await canvas.findByText("Show SKILL.md to copy it"));
    await waitFor(() => expect(canvas.getByRole("alert")).toHaveTextContent("req-pack-1"));
  },
};

/** Disconnect: typed client name, then the 204; the card leaves the list and the page says what happened. */
export const Disconnect: Story = {
  parameters: { msw: [...assistantHandlers("on"), ...handlersFor("default")] },
  play: async ({ canvas, canvasElement }) => {
    await userEvent.click(await canvas.findByRole("button", { name: "Disconnect Claude Code" }));
    const dialog = await within(canvasElement.ownerDocument.body).findByRole("alertdialog");
    await expect(dialog).toHaveTextContent(/mcp\.revoke/);
    const submit = within(dialog).getByRole("button", { name: "Disconnect" });
    await expect(submit).toBeDisabled();
    await userEvent.type(within(dialog).getByLabelText(/To confirm, type/), "Claude Code");
    await userEvent.click(submit);
    await waitFor(() => expect(canvas.getByText(/Claude Code is disconnected/)).toBeVisible());
    await waitFor(() => expect(canvas.queryByRole("button", { name: "Disconnect Claude Code" })).toBeNull());
  },
};

/** Disconnect all: a fixed phrase to type, then the 204; the list empties and the page says so. */
export const DisconnectAll: Story = {
  parameters: { msw: [...assistantHandlers("on"), ...handlersFor("default")] },
  play: async ({ canvas, canvasElement }) => {
    await userEvent.click(await canvas.findByRole("button", { name: "Disconnect all" }));
    const dialog = await within(canvasElement.ownerDocument.body).findByRole("alertdialog");
    await expect(dialog).toHaveTextContent(/Disconnect all 3 assistants/);
    await expect(dialog).toHaveTextContent(/mcp\.revoke/);
    const submit = within(dialog).getByRole("button", { name: "Disconnect all" });
    await expect(submit).toBeDisabled();
    await userEvent.type(within(dialog).getByLabelText(/To confirm, type/), "disconnect all");
    await userEvent.click(submit);
    await waitFor(() => expect(canvas.getByText(/Every assistant is disconnected/)).toBeVisible());
    await waitFor(() => expect(canvas.getByText("No assistant is connected as you.")).toBeVisible());
  },
};

/** A grant already gone (another tab, the revocation list): Go's 404 stays in the dialog. */
export const DisconnectRefused: StoryObj<typeof ConnectedAssistantsView> = {
  render: (args) => <ConnectedAssistantsView {...args} />,
  args: {
    grants: grants.items ?? [],
    actor: "oidc:jane@corp.example",
    onRevoke: fn(async () => {
      throw new ApiError(404, grantNotFound, "req-41");
    }),
    onRevokeAll: fn(async () => {}),
  },
  play: async ({ canvas, canvasElement }) => {
    await userEvent.click(canvas.getByRole("button", { name: "Disconnect opencode" }));
    const dialog = await within(canvasElement.ownerDocument.body).findByRole("alertdialog");
    await userEvent.type(within(dialog).getByLabelText(/To confirm, type/), "opencode");
    await userEvent.click(within(dialog).getByRole("button", { name: "Disconnect" }));
    await waitFor(() => expect(within(dialog).getByRole("alert")).toHaveTextContent("no such grant of yours"));
    await expect(canvas.queryByText(/is disconnected/)).toBeNull();
  },
};

/** Any other refusal of the setup (a 503): the usual error with its request id, never "MCP off". */
export const SetupUnavailable: Story = {
  parameters: {
    msw: [
      http.get("*/api/v1/assistant-setup", () =>
        HttpResponse.json(
          { title: "Service Unavailable", status: 503 },
          { status: 503, headers: { "Content-Type": "application/problem+json", "X-Request-Id": "req-setup-1" } },
        ),
      ),
      ...handlersFor("default"),
    ],
  },
  play: async ({ canvas }) => {
    await waitFor(() => expect(canvas.getByRole("alert")).toHaveTextContent("req-setup-1"));
    await expect(canvas.queryByText("This console does not serve AI assistants.")).toBeNull();
  },
};
