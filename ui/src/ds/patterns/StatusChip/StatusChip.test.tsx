import { describe, expect, test } from "vitest";
import { renderInFrame } from "../../../testing/render";
import { HEALTH_GLYPH, HEALTH_STATES } from "../../health";
import { StatusChip } from "./StatusChip";

describe("StatusChip", () => {
  test.each(HEALTH_STATES)("%s carries its glyph (hidden from AT) and its word", async (h) => {
    const screen = await renderInFrame(<StatusChip health={h} />);
    const chip = screen.container.querySelector(`[data-health="${h}"]`);
    expect(chip).not.toBeNull();
    const glyph = chip?.querySelector(".health-glyph");
    expect(glyph?.textContent).toBe(HEALTH_GLYPH[h]);
    expect(glyph?.getAttribute("aria-hidden")).toBe("true");
    expect(chip?.textContent?.replace(HEALTH_GLYPH[h], "").trim().length).toBeGreaterThan(0);
  });

  test("a state this build does not know is drawn as unknown, never ok", async () => {
    const screen = await renderInFrame(<StatusChip health="healthyish" />);
    await expect.element(screen.getByText("unknown")).toBeVisible();
    expect(screen.container.querySelector("[data-health]")?.getAttribute("data-health")).toBe("unknown");
  });

  test("stale keeps the state, marks the chip and says how old the reading is", async () => {
    const screen = await renderInFrame(
      <StatusChip health="ok" stale observedAt="2026-10-29T11:56:00Z" staleReason="watch lost" />,
    );
    const chip = screen.container.querySelector("[data-health]");
    expect(chip?.getAttribute("data-health")).toBe("ok");
    expect(chip?.hasAttribute("data-stale")).toBe(true);
    await expect.element(screen.getByText(/stale, as of 4 min\. ago/)).toBeVisible();
    expect(chip?.getAttribute("title")).toContain("watch lost");
  });

  test("a label replaces the visible word but the state word stays for screen readers", async () => {
    const screen = await renderInFrame(<StatusChip health="critical" label="Failing over" />);
    await expect.element(screen.getByText("Failing over")).toBeVisible();
    expect(screen.container.querySelector(".sr-only")?.textContent).toContain("critical");
  });
});
