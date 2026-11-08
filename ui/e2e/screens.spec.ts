import { expect, expectAccessible, settled, test } from "./harness";

const ROUTES = [
  { name: "clusters", path: "/", h1: "Clusters" },
  { name: "overview-failing-over", path: "/c/team-a/billing", h1: "billing" },
  { name: "overview-healthy", path: "/c/team-a/orders", h1: "orders" },
  { name: "instances", path: "/c/team-a/billing/instances", h1: "billing" },
  { name: "events", path: "/c/team-a/billing/events", h1: "billing" },
  { name: "diagnostics", path: "/c/team-a/billing/diagnostics", h1: "billing" },
  { name: "replication", path: "/c/team-a/orders/replication", h1: "orders" },
  { name: "backups", path: "/c/team-a/orders/backups", h1: "orders" },
  { name: "backups-pgbackrest", path: "/c/team-a/billing/backups", h1: "billing" },
  { name: "storage", path: "/c/team-a/orders/storage", h1: "orders" },
  { name: "objects", path: "/c/team-a/orders/objects", h1: "orders" },
  { name: "history", path: "/c/team-a/orders/history?view=revisions", h1: "orders" },
  { name: "timeline", path: "/c/team-a/billing/history", h1: "billing" },
  { name: "revision", path: "/c/team-a/orders/history/42", h1: "orders" },
  { name: "metrics", path: "/c/team-a/orders/metrics", h1: "orders" },
  { name: "operations", path: "/c/team-a/orders/operations", h1: "orders" },
  { name: "logs", path: "/c/team-a/orders/logs/orders-1", h1: "orders" },
  { name: "insight-pack", path: "/c/team-a/orders/insight", h1: "orders" },
  { name: "insight-not-enabled", path: "/c/team-a/billing/insight", h1: "billing" },
  { name: "insight-vacuum", path: "/c/team-a/orders/insight/vacuum", h1: "orders" },
  { name: "insight-blocking", path: "/c/team-a/orders/insight/blocking", h1: "orders" },
  { name: "insight-statements", path: "/c/team-a/orders/insight/statements", h1: "orders" },
  { name: "insight-indexes", path: "/c/team-a/orders/insight/indexes", h1: "orders" },
  { name: "insight-connections", path: "/c/team-a/orders/insight/connections", h1: "orders" },
  { name: "fleet-insight", path: "/insight", h1: "Query insight" },
  { name: "admin", path: "/admin", h1: "Administration" },
  { name: "digests", path: "/digests", h1: "Digests" },
  { name: "digest", path: "/digests/20261029T0700Z", h1: "Digest 20261029T0700Z" },
  { name: "access", path: "/access", h1: "Access requests" },
  { name: "assistants", path: "/assistants", h1: "Assistants" },
] as const;
const WIDTHS = [375, 768, 1440] as const;

for (const theme of ["light", "dark"] as const) {
  test.describe(`${theme} theme`, () => {
    test.use({ theme });
    for (const route of ROUTES) {
      test(`${route.name}: one h1, axe clean, screenshots at ${WIDTHS.join("/")}`, async ({ page, violations }) => {
        void violations;
        await page.goto(route.path);
        await settled(page);
        await expect(page.locator("h1")).toHaveCount(1);
        await expect(page.locator("h1")).toHaveText(route.h1);
        await expectAccessible(page);
        for (const width of WIDTHS) {
          await page.setViewportSize({ width, height: 900 });
          await expect(page).toHaveScreenshot(`${route.name}-${theme}-${width}.png`, { fullPage: true });
        }
      });
    }
  });
}
