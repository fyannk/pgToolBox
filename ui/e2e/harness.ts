import AxeBuilder from "@axe-core/playwright";
import { test as base, expect, type Page } from "@playwright/test";

/** The instant the Go goldens are anchored at (fixtures.Now). */
export const NOW = new Date("2026-10-29T12:00:00Z");

export type Scenario =
  | "default"
  | "loading"
  | "unavailable"
  | "forbidden"
  | "reauth"
  | "partial"
  | "empty"
  | "streamDown"
  | "mcpOff";

type Options = { theme: "light" | "dark"; scenario: Scenario };

/**
 * A page on the harness with the fixture clock, the chosen theme and MSW
 * scenario, and a guard that fails the test on any CSP violation or uncaught
 * error.
 */
export const test = base.extend<Options & { violations: string[] }>({
  theme: ["light", { option: true }],
  scenario: ["default", { option: true }],
  violations: async ({ page, theme, scenario }, use) => {
    const violations: string[] = [];
    await page.clock.setFixedTime(NOW);
    await page.emulateMedia({ colorScheme: theme, reducedMotion: "reduce" });
    await page.addInitScript(
      ([t, s]) => {
        localStorage.setItem("pgtoolbox-console.theme", t);
        localStorage.setItem("pgtoolbox-console.mock", s);
        localStorage.setItem("PARAGLIDE_LOCALE", "en");
        document.addEventListener("securitypolicyviolation", (e) => {
          console.error(`CSP violation: ${e.violatedDirective} ${e.blockedURI}`);
        });
      },
      [theme, scenario] as const,
    );
    page.on("console", (msg) => {
      if (msg.type() === "error" && /CSP violation|Content Security Policy/.test(msg.text()))
        violations.push(msg.text());
    });
    page.on("pageerror", (err) => violations.push(`uncaught: ${err.message}`));
    await use(violations);
    expect(violations, "no CSP violation and no uncaught error").toEqual([]);
  },
});
export { expect };

/** Serious and critical axe violations must be zero. */
export async function expectAccessible(page: Page) {
  const results = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"])
    .analyze();
  const blocking = results.violations.filter((v) => v.impact === "serious" || v.impact === "critical");
  expect(
    blocking.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(" ")).join(", ")}`),
    "axe serious/critical",
  ).toEqual([]);
}

/** Waits until the page shows data, not a skeleton. */
export async function settled(page: Page) {
  await expect(page.locator("[aria-busy=true]")).toHaveCount(0);
  await expect(page.locator("main h1")).toBeVisible();
}
