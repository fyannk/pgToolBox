// "Connected assistants" renders Go's grants as they come: each name marked
// unverified, the scopes in words, a disconnection announced only after the
// 204; and the console that does not serve MCP says so with nothing to click.
import { expect, test, vi } from "vitest";
import { grants } from "../../mocks/fixtures";
import { renderInFrame } from "../testing/render";
import { AssistantsNotServed, ConnectedAssistantsView } from "./Assistants";

test("connected: each grant says its name is unverified, its scopes in words, a never-used one says so", async () => {
  const s = await renderInFrame(
    <ConnectedAssistantsView
      grants={grants.items ?? []}
      actor="oidc:jane@corp.example"
      onRevoke={vi.fn()}
      onRevokeAll={vi.fn()}
    />,
  );
  await expect.element(s.getByText("not used yet")).toBeVisible();
  expect(s.getByText("name unverified").elements()).toHaveLength(3);
  const claude = s.container.querySelector('[data-grant="grant-0c1d2e3f405162738495a6b7c8d9eaf0"]');
  expect(claude?.textContent).toMatch(/read\s*mcp:read/);
  expect(claude?.textContent).toMatch(/operate\s*mcp:operate/);
  // Consent proposes read-only first: a grant approved that way shows read alone.
  const codex = s.container.querySelector('[data-grant="grant-1f2e3d4c5b6a79881726354453627180"]');
  expect(codex?.textContent).toMatch(/read\s*mcp:read/);
  expect(codex?.textContent).not.toMatch(/operate/);
  s.unmount();
});

test("connected: disconnect waits for the typed name and the server, then announces it", async () => {
  let resolve: () => void = () => {};
  const onRevoke = vi.fn(() => new Promise<void>((r) => (resolve = r)));
  const s = await renderInFrame(
    <ConnectedAssistantsView
      grants={grants.items ?? []}
      actor="oidc:jane@corp.example"
      onRevoke={onRevoke}
      onRevokeAll={vi.fn()}
    />,
  );
  await s.getByRole("button", { name: "Disconnect codex-mcp-client" }).click();
  const dialog = s.getByRole("alertdialog");
  await expect.element(dialog).toBeVisible();
  // A console action: recorded in the console's audit log, not the API server's.
  expect(dialog.element().textContent).toContain("mcp.revoke");
  await dialog.getByLabelText(/To confirm, type/).fill("codex-mcp-client");
  await dialog.getByRole("button", { name: "Disconnect" }).click();
  expect(onRevoke).toHaveBeenCalledWith(expect.objectContaining({ id: "grant-1f2e3d4c5b6a79881726354453627180" }));
  // Pessimistic: nothing is said before the 204.
  expect(s.container.querySelector('[role="status"]')?.textContent).toBe("");
  resolve();
  await expect.element(s.getByText(/codex-mcp-client is disconnected/)).toBeVisible();
  s.unmount();
});

test("connected: disconnect all waits for the typed phrase and the server; one grant has no such button", async () => {
  let resolve: () => void = () => {};
  const onRevokeAll = vi.fn(() => new Promise<void>((r) => (resolve = r)));
  const s = await renderInFrame(
    <ConnectedAssistantsView
      grants={grants.items ?? []}
      actor="oidc:jane@corp.example"
      onRevoke={vi.fn()}
      onRevokeAll={onRevokeAll}
    />,
  );
  await s.getByRole("button", { name: "Disconnect all" }).click();
  const dialog = s.getByRole("alertdialog");
  await expect.element(dialog).toBeVisible();
  expect(dialog.element().textContent).toContain("Disconnect all 3 assistants");
  expect(dialog.element().textContent).toContain("Your browser session stays");
  const submit = dialog.getByRole("button", { name: "Disconnect all" });
  await expect.element(submit).toBeDisabled();
  await dialog.getByLabelText(/To confirm, type/).fill("disconnect all");
  await submit.click();
  expect(onRevokeAll).toHaveBeenCalledOnce();
  expect(s.container.querySelector('[role="status"]')?.textContent).toBe("");
  resolve();
  await expect.element(s.getByText(/Every assistant is disconnected/)).toBeVisible();
  s.unmount();

  const one = await renderInFrame(
    <ConnectedAssistantsView
      grants={(grants.items ?? []).slice(0, 1)}
      actor="x"
      onRevoke={vi.fn()}
      onRevokeAll={vi.fn()}
    />,
  );
  await expect.element(one.getByRole("button", { name: "Disconnect Claude Code" })).toBeVisible();
  expect(one.getByRole("button", { name: "Disconnect all" }).elements()).toHaveLength(0);
  one.unmount();
});

test("connected: no grant is said in words", async () => {
  const s = await renderInFrame(
    <ConnectedAssistantsView grants={[]} actor="x" onRevoke={vi.fn()} onRevokeAll={vi.fn()} />,
  );
  await expect.element(s.getByText("No assistant is connected as you.")).toBeVisible();
  s.unmount();
});

test("MCP off: Go's reason, and nothing to click", async () => {
  const s = await renderInFrame(<AssistantsNotServed detail="this console does not serve MCP (MCP_ENABLED is off)" />);
  await expect.element(s.getByText(/MCP_ENABLED is off/)).toBeVisible();
  expect(s.container.querySelectorAll("a, button")).toHaveLength(0);
  s.unmount();
});
