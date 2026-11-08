import { expect, settled, test } from "./harness";

test("keyboard: the skip link is the first stop and moves focus to the content", async ({ page, violations }) => {
  void violations;
  await page.goto("/");
  await settled(page);
  await page.keyboard.press("Tab");
  const skip = page.getByRole("link", { name: "Skip to content" });
  await expect(skip).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(page.locator("main#main")).toBeFocused();
});

test("list → overview → instances drawer → diagnostics tabs", async ({ page, violations }) => {
  void violations;
  await page.goto("/");
  await settled(page);
  await page.getByRole("link", { name: "team-a/billing" }).click();
  await expect(page.locator("h1")).toHaveText("billing");
  await expect(page.getByRole("heading", { name: "Overall health" })).toBeVisible();

  await page.getByRole("navigation", { name: "Cluster sections" }).getByRole("link", { name: "Instances" }).click();
  await expect(page.getByRole("link", { name: "Instances", exact: true })).toHaveAttribute("aria-current", "page");
  const opener = page.getByRole("button", { name: "billing-1" });
  await opener.click();
  const drawer = page.getByRole("dialog", { name: "billing-1" });
  await expect(drawer).toContainText("CrashLoopBackOff");
  await expect(page).toHaveURL(/\/c\/team-a\/billing\/instances\/billing-1$/);
  await page.keyboard.press("Escape");
  await expect(drawer).toBeHidden();
  await expect(page).toHaveURL(/\/c\/team-a\/billing\/instances$/);
  await expect(opener).toBeFocused();

  await page.getByRole("link", { name: "Diagnostics" }).click();
  const first = page.getByRole("tab").first();
  await first.focus();
  await page.keyboard.press("ArrowRight");
  await expect(page.getByRole("tab", { name: /Checks/ })).toBeFocused();
});

test("sorting is URL state: a shared link opens sorted, with aria-sort", async ({ page, violations }) => {
  void violations;
  await page.goto("/?sort=-verdict");
  await settled(page);
  await expect(page.getByRole("columnheader", { name: /Verdict/ })).toHaveAttribute("aria-sort", "descending");
  await expect(page.getByRole("row").nth(1)).toContainText("billing");
  await page
    .getByRole("button", { name: /Cluster/ })
    .first()
    .click();
  await expect(page).toHaveURL(/sort=cluster/);
});

test("the filter is URL state too", async ({ page, violations }) => {
  void violations;
  await page.goto("/");
  await settled(page);
  await page.getByLabel("Filter").fill("ord");
  await expect(page).toHaveURL(/q=ord/);
  await expect(page.getByRole("row")).toHaveCount(2);
});

test("a view the server does not have is its 404, said in words, not a blank page", async ({ page, violations }) => {
  void violations;
  await page.goto("/c/team-a/orders/events");
  await expect(page.getByRole("alert")).toContainText("no such Cluster in the console's watched namespaces");
});

test("an unknown address is a not-found page", async ({ page, violations }) => {
  void violations;
  await page.goto("/nowhere");
  await expect(page.locator("h1")).toHaveText("Not found");
});

test("an instance has its own address: /instances/<pod> opens its drawer", async ({ page, violations }) => {
  void violations;
  await page.goto("/c/team-a/billing/instances/billing-2");
  const drawer = page.getByRole("dialog", { name: "billing-2" });
  await expect(drawer).toContainText("not verifiable");
  await expect(drawer).toContainText("console.instanceStatus.caBundleConfigMap");
});

test("operation: typed confirmation → 202 → a tracker that names its sources", async ({ page, violations }) => {
  void violations;
  await page.goto("/c/team-a/orders/operations");
  await settled(page);
  await page.getByRole("button", { name: /on-demand backup/ }).click();
  const dialog = page.getByRole("alertdialog");
  const submit = dialog.getByRole("button", { name: "Request", exact: true });
  await expect(submit).toBeDisabled();
  await dialog.getByLabel(/To confirm, type/).fill("orders");
  await submit.click();
  await expect(dialog).toBeHidden();
  const tracker = page.getByRole("region", { name: "Operations in progress" });
  await expect(tracker.getByRole("status")).toHaveText(/Operator acting/);
  await expect(tracker.getByText("Backup.status.phase").first()).toBeVisible();
});

test("operation: a tracker survives a reload (looked up again by id)", async ({ page, violations }) => {
  void violations;
  await page.goto("/c/team-a/orders/operations");
  await settled(page);
  await page.getByRole("button", { name: /Reload configuration/ }).click();
  await page
    .getByRole("alertdialog")
    .getByLabel(/To confirm, type/)
    .fill("orders");
  await page.getByRole("alertdialog").getByRole("button", { name: "Request", exact: true }).click();
  await expect(page.getByRole("region", { name: "Operations in progress" })).toBeVisible();
  await page.reload();
  await expect(page.getByRole("region", { name: "Operations in progress" }).getByRole("status")).toBeVisible();
});

test("assistants: user menu → setup per client with copy → disconnect with typed name → gone", async ({
  page,
  violations,
}) => {
  void violations;
  await page.goto("/");
  await settled(page);
  await page.getByRole("banner").getByRole("link", { name: "Assistants" }).click();
  await expect(page.locator("h1")).toHaveText("Assistants");
  await expect(page.getByText("Read-only", { exact: true })).toBeVisible();

  // The client picker: one tab stop, arrows then Enter; each panel is Go's setup for that client.
  await page.getByRole("tab", { name: "Claude Code" }).focus();
  await page.keyboard.press("ArrowRight");
  await page.keyboard.press("Enter");
  await expect(page.getByRole("tab", { name: "Codex CLI" })).toHaveAttribute("aria-selected", "true");
  await expect(page.getByText(/^codex mcp add [\w-]+ --url https:\/\/console\.example\.com\/mcp$/)).toBeVisible();

  // Codex's prompt pack is the AGENTS.md section, from the console's own endpoint, with Go's paths.
  await expect(page.getByRole("link", { name: "Download AGENTS.md" })).toHaveAttribute(
    "href",
    "/api/v1/assistant-setup/agents",
  );
  await expect(page.getByText("~/.codex/AGENTS.md")).toBeVisible();
  await page.getByText("Show AGENTS.md to copy it").click();
  await expect(page.getByText(/^## PostgreSQL clusters/m)).toBeVisible();

  // Disconnect: typed name, then the 204; the grant leaves the list.
  await page.getByRole("button", { name: "Disconnect Claude Code" }).click();
  const dialog = page.getByRole("alertdialog");
  await expect(dialog).toContainText("mcp.revoke");
  const submit = dialog.getByRole("button", { name: "Disconnect", exact: true });
  await expect(submit).toBeDisabled();
  await dialog.getByLabel(/To confirm, type/).fill("Claude Code");
  await submit.click();
  await expect(dialog).toBeHidden();
  await expect(page.getByRole("status").filter({ hasText: "Claude Code is disconnected" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Disconnect Claude Code" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: /^Disconnect (?!all$)/ })).toHaveCount(2);

  // Disconnect all: a fixed phrase, then the 204; the list empties.
  await page.getByRole("button", { name: "Disconnect all", exact: true }).click();
  const all = page.getByRole("alertdialog");
  await expect(all).toContainText("Disconnect all 2 assistants");
  await all.getByLabel(/To confirm, type/).fill("disconnect all");
  await all.getByRole("button", { name: "Disconnect all", exact: true }).click();
  await expect(all).toBeHidden();
  await expect(page.getByRole("status").filter({ hasText: "Every assistant is disconnected" })).toBeVisible();
  await expect(page.getByText("No assistant is connected as you.")).toBeVisible();
});

test("⌘K: the palette offers to connect an assistant", async ({ page, violations }) => {
  void violations;
  await page.goto("/");
  await settled(page);
  await page.keyboard.press("Control+k");
  const palette = page.getByRole("dialog", { name: "Command palette" });
  // Typed before the input has focus, the first keys go to the page and Enter
  // picks whatever is first (a cluster): wait for the input, then the option.
  await expect(palette.getByRole("combobox")).toBeFocused();
  await page.keyboard.type("connect an assistant");
  await expect(palette.getByRole("option", { name: /Connect an assistant/ })).toBeVisible();
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(/\/assistants$/);
});

test("⌘K: the palette jumps to another cluster's page", async ({ page, violations }) => {
  void violations;
  await page.goto("/c/team-a/billing");
  await settled(page);
  await page.keyboard.press("Control+k");
  const palette = page.getByRole("dialog", { name: "Command palette" });
  await expect(palette.getByRole("combobox")).toBeFocused();
  await page.keyboard.type("orders");
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(/\/c\/team-a\/orders$/);
  await expect(page.locator("h1")).toHaveText("orders");
});

test("logs: the instance drawer opens the log, which streams", async ({ page, violations }) => {
  void violations;
  await page.goto("/c/team-a/orders/logs/orders-1");
  await expect(page.getByRole("log")).toContainText("checkpoint complete");
  await expect(page.getByText(/^streaming/)).toBeVisible();
});

test("history: a revision shows Go's diff and the definition", async ({ page, violations }) => {
  void violations;
  await page.goto("/c/team-a/orders/history?view=revisions");
  await settled(page);
  await page.getByRole("link", { name: /^#42 / }).click();
  await expect(page).toHaveURL(/\/history\/42$/);
  // The preview panel names the same field: the diff is the region of the revision.
  await expect(page.getByRole("region", { name: /revision 42/ }).getByText(".spec.instances")).toBeVisible();
  await expect(page.getByRole("region", { name: "What this change sets in motion" })).toBeVisible();
});

test("timeline: the kind filter is in the URL and an entry links to the screen that explains it", async ({
  page,
  violations,
}) => {
  void violations;
  await page.goto("/c/team-a/billing/history");
  await settled(page);
  await page.getByRole("button", { name: /^Primary/ }).click();
  await page.getByRole("button", { name: /^Findings/ }).click();
  await expect(page).toHaveURL(/kind=primary%2Cfinding|kind=primary,finding/);
  await expect(page.getByText("Backup failed")).toHaveCount(0);
  await page.getByRole("link", { name: "Open the screen that explains it" }).first().click();
  await expect(page).toHaveURL(/\/c\/team-a\/billing\/(diagnostics|replication)$/);
});

test("digest: the shared link opens the digest, its entries link into the Cluster", async ({ page, violations }) => {
  void violations;
  await page.goto("/digests");
  await settled(page);
  await page.getByRole("link", { name: /→/ }).first().click();
  await expect(page).toHaveURL(/\/digests\/20261029T0700Z$/);
  await expect(page.getByText(/Partial: the console did not observe all of these 24 hours/)).toBeVisible();
  await page.getByRole("region", { name: "team-a/billing" }).getByRole("link", { name: "Open the Cluster" }).click();
  await expect(page).toHaveURL(/\/c\/team-a\/billing$/);
});

test("access review: typed name and note → deny sent with CSRF, dialog closes on the answer", async ({
  page,
  violations,
}) => {
  void violations;
  await page.goto("/access");
  await settled(page);
  const card = page.getByRole("article", { name: /oidc:victor@corp.example/ }).filter({ hasText: "operator-k2x9p" });
  await card.getByRole("button", { name: /Deny/ }).click();
  const dialog = page.getByRole("alertdialog");
  await dialog.getByLabel(/To confirm, type/).fill("operator-k2x9p");
  await expect(dialog.getByRole("button", { name: "Deny" })).toBeDisabled();
  await dialog.getByLabel(/^Note/).fill("Restarting the standby is on-call work: ask the on-call DBA");
  const sent = page.waitForRequest(
    (r) => r.method() === "POST" && r.url().endsWith("/namespaces/team-a/access-requests/operator-k2x9p/deny"),
  );
  await dialog.getByRole("button", { name: "Deny" }).click();
  const req = await sent;
  expect(req.headers()["x-pgtoolbox-request"]).toBe("1");
  expect(req.postDataJSON()).toEqual({ note: "Restarting the standby is on-call work: ask the on-call DBA" });
  await expect(dialog).toBeHidden();
});
