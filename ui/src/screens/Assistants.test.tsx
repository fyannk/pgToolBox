// "Connect an assistant" renders Go's setup as it comes: the access in Go's
// words, one panel per client Go offered, the static-token form only when Go
// sends it, the prompt pack from Go's paths. (Connected assistants:
// ConnectedAssistants.test.tsx.)
import { expect, test, vi } from "vitest";
import { userEvent } from "vitest/browser";
import {
  assistantAgents,
  assistantSetup,
  assistantSetupOperate,
  assistantSetupTokenLogin,
  assistantSkill,
} from "../../mocks/fixtures";
import { renderInFrame } from "../testing/render";
import { ConnectAssistantView } from "./Assistants";

type Props = Parameters<typeof ConnectAssistantView>[0];
const text = (data: string) => ({ data, error: null, isPending: false, refetch: vi.fn() }) as unknown as Props["skill"];

test("connect: Go's access decides the explanation; the MCP URL and every client Go offered are there", async () => {
  const read = await renderInFrame(
    <ConnectAssistantView setup={assistantSetup} skill={text(assistantSkill)} agents={text(assistantAgents)} />,
  );
  await expect.element(read.getByText("Read-only")).toBeVisible();
  await expect.element(read.getByText("https://console.example.com/mcp", { exact: true })).toBeVisible();
  expect(
    read
      .getByRole("tab")
      .elements()
      .map((t) => t.textContent),
  ).toEqual((assistantSetup.clients ?? []).map((c) => c.name));
  // OAuth only: no static-token form anywhere.
  expect(read.container.textContent).not.toMatch(/Break-glass/);
  // The prompt pack of the client on screen downloads from Go's path, and goes where Go says.
  const claude = assistantSetup.clients?.[0]?.pack;
  const links = read.getByRole("link", { name: /^Download/ }).elements() as HTMLAnchorElement[];
  expect(links.map((a) => [a.getAttribute("href"), a.getAttribute("download")])).toEqual([
    [claude?.download, claude?.fileName],
  ]);
  expect(read.container.textContent).toContain(claude?.projectPath);
  expect(read.container.textContent).toContain(claude?.userPath);
  read.unmount();

  const operate = await renderInFrame(
    <ConnectAssistantView setup={assistantSetupOperate} skill={text("")} agents={text("")} />,
  );
  await expect.element(operate.getByText("Read and operate")).toBeVisible();
  operate.unmount();
});

test("connect: a check in words is read, not copied; the AGENTS.md pack says to merge", async () => {
  const generic = assistantSetup.clients?.find((c) => c.checkNote);
  if (!generic?.checkNote) throw new Error("the fixture has no client with a check in words");
  const setup = { ...assistantSetup, clients: [generic] };
  const s = await renderInFrame(<ConnectAssistantView setup={setup} skill={text("")} agents={text("")} />);
  await expect.element(s.getByText(generic.checkNote)).toBeVisible();
  // Not in a code box, and no copy button for it.
  expect(s.getByText(generic.checkNote).element().tagName).toBe("P");
  expect(s.getByRole("button", { name: /^Copy the check/ }).elements()).toHaveLength(0);
  await expect.element(s.getByText(/do not replace the file/)).toBeVisible();
  s.unmount();
});

// Keep this one last: after its keyboard focus moves, a later render in the same file comes out blank.
test("connect: the client picker is one tab stop; arrows and End move between Go's clients", async () => {
  const s = await renderInFrame(
    <ConnectAssistantView setup={assistantSetupTokenLogin} skill={text("")} agents={text("")} />,
  );
  await expect.element(s.getByRole("tab", { name: "Claude Code" })).toBeVisible();
  const tabs = s.getByRole("tab").elements() as HTMLElement[];
  expect(tabs.filter((t) => t.tabIndex === 0)).toHaveLength(1);
  tabs[0]?.focus();
  await userEvent.keyboard("{ArrowRight}");
  expect(document.activeElement?.textContent).toBe("Codex CLI");
  await userEvent.keyboard("{End}");
  expect(document.activeElement?.textContent).toBe(assistantSetupTokenLogin.clients?.at(-1)?.name);
  // Token sign-in on: the static-token form with Go's note.
  await expect.element(s.getByRole("heading", { name: "Break-glass: a static token" })).toBeVisible();
  s.unmount();
});
