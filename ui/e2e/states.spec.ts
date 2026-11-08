import { expect, expectAccessible, settled, test } from "./harness";

test.describe("stream down", () => {
  test.use({ scenario: "streamDown" });
  test("the shell says live updates are paused and how often it re-reads", async ({ page, violations }) => {
    void violations;
    await page.goto("/c/team-a/billing");
    await settled(page);
    await expect(page.getByRole("status").filter({ hasText: "Live updates paused" })).toContainText("every 2 s");
    await expectAccessible(page);
    await expect(page).toHaveScreenshot("stream-down-light-1440.png");
  });
});

test.describe("session expired", () => {
  test.use({ scenario: "reauth" });
  test("a 401 offers a same-origin sign-in that returns here, without retrying", async ({ page, violations }) => {
    void violations;
    await page.goto("/c/team-a/billing");
    const banner = page.getByRole("alert").filter({ hasText: "Your session has ended" });
    await expect(banner).toBeVisible();
    await expect(banner.getByRole("link", { name: "Sign in again" })).toHaveAttribute(
      "href",
      "/auth/login?rd=%2Fc%2Fteam-a%2Fbilling",
    );
    await expectAccessible(page);
    await expect(page).toHaveScreenshot("reauth-light-1440.png");
  });
});

test.describe("forbidden", () => {
  test.use({ scenario: "forbidden" });
  test("a Cluster the user may not get is not listed, and its page is a 403 that names the role", async ({
    page,
    violations,
  }) => {
    void violations;
    await page.goto("/");
    await expect(page.getByRole("link", { name: "team-a/orders" })).toBeVisible();
    await expect(page.getByRole("link", { name: "team-a/billing" })).toHaveCount(0);
    await page.goto("/c/team-a/billing");
    await expect(page.getByRole("alert")).toContainText(/ask for the [\w-]*viewer role there/);
    await expectAccessible(page);
    await expect(page).toHaveScreenshot("forbidden-light-1440.png");
  });
});

test.describe("MCP off", () => {
  test.use({ scenario: "mcpOff" });
  for (const theme of ["light", "dark"] as const) {
    test.describe(theme, () => {
      test.use({ theme });
      test("the assistants page says the console serves none, with Go's reason and nothing to click", async ({
        page,
        violations,
      }) => {
        void violations;
        await page.goto("/assistants");
        await settled(page);
        const main = page.locator("main");
        await expect(main).toContainText("This console does not serve AI assistants.");
        await expect(main).toContainText("MCP_ENABLED is off");
        await expect(main.getByRole("link")).toHaveCount(0);
        await expect(main.getByRole("button")).toHaveCount(0);
        // Features say MCP is off: no menu link to it (the direct link above still gets the honest state).
        await expect(page.getByRole("banner").getByRole("link", { name: "Assistants" })).toHaveCount(0);
        await expectAccessible(page);
        for (const width of [375, 1440]) {
          await page.setViewportSize({ width, height: 900 });
          await expect(page).toHaveScreenshot(`assistants-mcp-off-${theme}-${width}.png`, { fullPage: true });
        }
      });
    });
  }
});

test.describe("API server could not be asked", () => {
  test.use({ scenario: "unavailable" });
  test("a 503 shows the server's reason, the request id and a retry", async ({ page, violations }) => {
    void violations;
    await page.goto("/c/team-a/billing/events");
    const alert = page.getByRole("alert");
    await expect(alert).toContainText("could not be asked");
    await expect(alert).toContainText("Request id req-fixture-0001");
    await expect(alert.getByRole("button", { name: "Retry" })).toBeVisible();
    await expectAccessible(page);
    await expect(page).toHaveScreenshot("unavailable-light-1440.png");
  });
});

test.describe("partial list", () => {
  test.use({ scenario: "partial" });
  test("Clusters that could not be checked are counted, not silently dropped", async ({ page, violations }) => {
    void violations;
    await page.goto("/");
    await expect(page.getByText(/1 Clusters could not be checked/)).toBeVisible();
    await expectAccessible(page);
    await expect(page).toHaveScreenshot("partial-light-1440.png");
  });
});

test.describe("empty", () => {
  test.use({ scenario: "empty" });
  test("no cluster is said in words", async ({ page, violations }) => {
    void violations;
    await page.goto("/");
    await expect(page.getByText("No Cluster in the namespaces this console could list.")).toBeVisible();
    await expectAccessible(page);
    await expect(page).toHaveScreenshot("empty-light-1440.png");
  });
});

test.describe("loading", () => {
  test.use({ scenario: "loading" });
  test("a pending request is a busy region that says what it waits for", async ({ page, violations }) => {
    void violations;
    await page.goto("/");
    await expect(page.locator("[aria-busy=true]")).toBeVisible();
    await expect(page.getByText("Loading Clusters…")).toBeAttached();
    await expect(page).toHaveScreenshot("loading-light-1440.png");
  });
});
