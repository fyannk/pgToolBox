import { describe, expect, test } from "vitest";
import { timeline } from "../../../../mocks/fixtures";
import type { TimelineEntry } from "../../../api/generated/model";
import { renderInFrame } from "../../../testing/render";
import { Timeline, timelineLabel } from "./Timeline";

const items = timeline.items ?? [];
const params = { namespace: "team-a", cluster: "billing" };
const byId = (id: string) => {
  const e = items.find((i) => i.id === id);
  if (!e) throw new Error(`no ${id} in getTimeline.billing.json`);
  return e;
};

describe("Timeline", () => {
  test("groups by UTC day, newest first as served, one list item per entry", async () => {
    const twoDays: TimelineEntry[] = [byId("j:118"), { ...byId("j:113"), id: "older", at: "2026-10-28T23:30:00Z" }];
    const screen = await renderInFrame(<Timeline items={twoDays} params={params} label="t" />);
    const headings = screen.container.querySelectorAll("h3");
    expect(headings).toHaveLength(2);
    expect(headings[0]?.textContent).toMatch(/29.*2026.*\(UTC\)/);
    expect(headings[1]?.textContent).toMatch(/28.*2026.*\(UTC\)/);
    expect(screen.container.querySelectorAll("li")).toHaveLength(2);
  });

  test("health is state + glyph + word, and the time says whose clock it is", async () => {
    const screen = await renderInFrame(<Timeline items={[byId("j:116")]} params={params} label="t" />);
    const chip = screen.container.querySelector("[data-health]");
    expect(chip?.getAttribute("data-health")).toBe("critical");
    expect(chip?.querySelector(".health-glyph")?.getAttribute("aria-hidden")).toBe("true");
    await expect.element(screen.getByText("Critical:", { exact: false })).toBeInTheDocument();
    await expect.element(screen.getByText("reported", { exact: true })).toBeVisible();
    expect(screen.container.querySelector("time")?.getAttribute("dateTime")).toBe("2026-10-29T11:58:25Z");
  });

  test("a primary change whose cause is ambiguous says so beside the latest cause", async () => {
    const changed: TimelineEntry = {
      ...byId("j:113"),
      id: "u",
      kind: "primary",
      action: "switchover",
      afterGap: false,
      uncertain: true,
    };
    const screen = await renderInFrame(<Timeline items={[changed]} params={params} label="t" />);
    await expect.element(screen.getByText("Cause uncertain:", { exact: false })).toBeVisible();
    const certain = await renderInFrame(
      <Timeline items={[{ ...changed, uncertain: false }]} params={params} label="t" />,
    );
    expect(certain.container.textContent).not.toMatch(/Cause uncertain/);
  });

  test("an entry after a gap is placed between two instants, never at one", async () => {
    const screen = await renderInFrame(<Timeline items={[byId("j:113")]} params={params} label="t" />);
    const note = screen.container.querySelector('[data-health="unknown"]');
    expect(note?.textContent).toMatch(/between .*12:56:00 AM UTC and .*1:00:00 AM UTC: the console was not observing/);
  });

  test("links resolve onto this build's routes; an unknown link is text, never a dead link", async () => {
    const odd = { ...byId("j:118"), id: "odd", link: "nowhere/at/all" };
    const screen = await renderInFrame(<Timeline items={[byId("j:115"), odd]} params={params} label="t" />);
    const links = [...screen.container.querySelectorAll("a")].map((a) => a.getAttribute("href"));
    expect(links).toEqual(["/c/team-a/billing/instances/billing-1"]);
    await expect.element(screen.getByText("nowhere/at/all")).toBeVisible();
  });

  test("labels: Go's action vocabulary, an Event by its type and reason, an unknown action verbatim", () => {
    expect(timelineLabel({ kind: "primary", action: "failover" })).toBe("Failover");
    expect(timelineLabel({ kind: "event", action: "Warning", ref: "Unhealthy" })).toBe("Warning: Unhealthy");
    expect(timelineLabel({ kind: "revision", action: "spec-changed" })).toBe("Revision: spec changed");
    expect(timelineLabel({ kind: "backup", action: "exploded" })).toBe("Backups exploded");
  });
});
