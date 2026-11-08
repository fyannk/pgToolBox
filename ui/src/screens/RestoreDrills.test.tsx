// What the Backups screen does with Go's restore drill records: the result
// and its health are Go's, a check not run is never shown as passed, and
// what a drill does not check is stated.
import { expect, test } from "vitest";
import { backups } from "../../mocks/fixtures";
import type { RestoreDrill } from "../api/generated/model";
import { renderInFrame } from "../testing/render";
import { RestoreDrillsPanel } from "./RestoreDrills";

const drills = backups.drills;
const [passed] = drills.items as [RestoreDrill];

test("restore drills: each drill carries Go's health, and the failed one leads with its reason", async () => {
  const screen = await renderInFrame(<RestoreDrillsPanel drills={drills} />);
  const items = screen.container.querySelectorAll("[data-drill]");
  expect(items.length).toBe(2);
  expect(items[0]?.getAttribute("data-drill")).toBe("passed");
  expect(items[0]?.querySelector("[data-health]")?.getAttribute("data-health")).toBe("ok");
  expect(items[1]?.querySelector("[data-health]")?.getAttribute("data-health")).toBe("critical");
  await expect.element(screen.getByText(/^size: restored 120.0 GiB/)).toBeVisible();
  expect(screen.container.querySelector("[data-drill-last]")?.getAttribute("data-drill-last")).toBe("passed");
});

test("restore drills: a check not run reads 'not checked', never as passed", async () => {
  const notChecked = {
    ...passed,
    checks: (passed.checks ?? []).map((c) =>
      c.name === "size" ? { ...c, outcome: "notChecked" as const, text: "no exporter reading" } : c,
    ),
  };
  const screen = await renderInFrame(<RestoreDrillsPanel drills={{ ...drills, items: [notChecked] }} />);
  const size = screen.container.querySelector('[data-check="size"]');
  expect(size?.querySelector("[data-health]")?.getAttribute("data-health")).toBe("unknown");
  expect(size?.textContent).toContain("not checked");
});

test("restore drills: off says so and shows no history or offer", async () => {
  const screen = await renderInFrame(<RestoreDrillsPanel drills={{ ...drills, enabled: false }} />);
  await expect.element(screen.getByText("Restore drills are off on this console.")).toBeVisible();
  expect(screen.container.querySelectorAll("[data-drill]").length).toBe(0);
});
