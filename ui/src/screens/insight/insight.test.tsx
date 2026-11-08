// The insight screens: Go's horizons stated as given, the queryid filled into
// Go's SQL, the lock tree drawn as Go nests it, polling at most every 2 s and
// only while visible, and panels without data saying so.
import { expect, test } from "vitest";
import { page } from "vitest/browser";
import { insightBlocking, insightEnabled, insightNotEnabled } from "../../../mocks/fixtures";
import {
  insightBlockingMissingCycle,
  insightBlockingPrepared,
  insightHorizonHolders,
  insightIndexesConstraints,
  insightPackTruncated,
  insightPackUnknown,
  insightPartial,
  insightStatementsUncertain,
} from "../../../mocks/fixtures/insight";
import { renderInFrame } from "../../testing/render";
import { BlockTree } from "./Blocking";
import { ConnectionsView } from "./Connections";
import { PartialNotice, packStateWord } from "./common";
import { IndexesView } from "./Indexes";
import { blockingPollMs, readHorizon, statementSql } from "./logic";
import { InsightPackView } from "./Pack";
import { StatementsView } from "./Statements";
import { HorizonCell, HorizonHolders, VacuumView } from "./Vacuum";

const rate = { perSecond: 1500, windowSeconds: 3600, from: "2026-10-29T11:00:00Z", to: "2026-10-29T12:00:00Z" };

test("a horizon says what Go measured: past, a time at the rate, no rate yet, or not growing", () => {
  expect(readHorizon({ threshold: 2e8, remaining: -1.4e8, past: true }, rate)).toEqual({ kind: "past", over: 1.4e8 });
  expect(readHorizon({ threshold: 1.6e9, remaining: 1.26e9, past: false, inSeconds: 840_000 }, rate)).toEqual({
    kind: "in",
    seconds: 840_000,
  });
  expect(readHorizon({ threshold: 4e8, remaining: 3.88e8, past: false }, undefined)).toEqual({ kind: "noRate" });
  expect(readHorizon({ threshold: 4e8, remaining: 3.88e8, past: false }, { ...rate, perSecond: 0 })).toEqual({
    kind: "notApproaching",
  });
});

test("horizon cells read in words, never a guessed time", async () => {
  const s = await renderInFrame(
    <div>
      <HorizonCell h={{ threshold: 2e8, remaining: -1.4e8, past: true }} rate={rate} />
      <HorizonCell h={{ threshold: 1.6e9, remaining: 1.26e9, past: false, inSeconds: 840_000 }} rate={rate} />
      <HorizonCell h={{ threshold: 4e8, remaining: 3.88e8, past: false }} rate={undefined} />
    </div>,
  );
  await expect.element(s.getByText("passed, by 140M")).toBeVisible();
  await expect.element(s.getByText("in 9 d 17 h at the measured rate")).toBeVisible();
  await expect.element(s.getByText("no rate measured yet")).toBeVisible();
  await s.unmount();
});

test("the statement's queryid replaces QUERYID in Go's text query; anything but an integer is refused", () => {
  const q = "SELECT query FROM pg_stat_statements WHERE queryid = QUERYID LIMIT 1;";
  expect(statementSql(q, "-1840029318271940210")).toBe(
    "SELECT query FROM pg_stat_statements WHERE queryid = -1840029318271940210 LIMIT 1;",
  );
  expect(statementSql(q, "1 OR true")).toBeNull();
  expect(statementSql(q, "")).toBeNull();
  expect(statementSql("SELECT 1", "42")).toBeNull();
});

test("the lock graph is polled at Go's interval but never faster than every 2 s", () => {
  expect(blockingPollMs(1)).toBe(2_000);
  expect(blockingPollMs(undefined)).toBe(2_000);
  expect(blockingPollMs(5)).toBe(5_000);
});

test("the lock tree nests waiters under their blocker; missing and cycle nodes say so", async () => {
  const s = await renderInFrame(<BlockTree nodes={insightBlocking.instances[0]?.roots ?? []} root />);
  await expect.element(s.getByText("blocks 2")).toBeVisible();
  const leaf = s.container.querySelector('li[data-pid="5133"]');
  expect(leaf?.parentElement?.closest("li")?.getAttribute("data-pid")).toBe("5120");
  expect(leaf?.parentElement?.closest("li")?.parentElement?.closest("li")?.getAttribute("data-pid")).toBe("4711");
  await s.unmount();
  const odd = await renderInFrame(<BlockTree nodes={insightBlockingMissingCycle.instances[0]?.roots ?? []} root />);
  await expect.element(odd.getByText(/a lock cycle/)).toBeVisible();
  expect(odd.container.querySelector('li[data-pid="900"]')?.hasAttribute("data-missing")).toBe(true);
  expect(odd.container.querySelector("li[data-cycle]")?.getAttribute("data-pid")).toBe("901");
  await odd.unmount();
});

test("a Cluster without the pack: every screen shows the banner and each panel's reason, never an empty table", async () => {
  for (const View of [VacuumView, StatementsView, IndexesView, ConnectionsView]) {
    const s = await renderInFrame(<View data={insightNotEnabled} />);
    await expect.element(s.getByText("Not enabled")).toBeVisible();
    expect(s.container.querySelector("[data-pack-state=notEnabled]")).not.toBeNull();
    expect(s.container.querySelector("[data-unavailable]")).not.toBeNull();
    expect(s.container.querySelector("table")).toBeNull();
    await s.unmount();
  }
});

test("an enabled pack shows no banner", async () => {
  const s = await renderInFrame(<VacuumView data={insightEnabled} />);
  await expect.element(s.getByText("to prevent wraparound")).toBeVisible();
  expect(s.container.querySelector("[data-pack-state]")).toBeNull();
  await s.unmount();
});

test('a cut response, an unknown version and PostgreSQL 13 have their own words, never "not enabled"', () => {
  for (const state of ["truncated", "unknown", "unsupported"]) {
    expect(packStateWord(state)).not.toBe(packStateWord("notEnabled"));
    expect(packStateWord(state)).not.toBe(state);
  }
});

test("a truncated pack reads as cut on every screen, never as not enabled, and offers no spec change", async () => {
  for (const View of [VacuumView, StatementsView, IndexesView, ConnectionsView]) {
    const s = await renderInFrame(<View data={insightPackTruncated} />);
    await expect.element(s.getByText("Response cut")).toBeVisible();
    expect(s.container.querySelector("[data-pack-state=truncated]")).not.toBeNull();
    expect(s.container.textContent).not.toContain("Not enabled");
    expect(s.container.querySelector("button")).toBeNull();
    await s.unmount();
  }
});

test("an enabled pack Go marked unhealthy (unknown version, failing queries) keeps the banner", async () => {
  for (const data of [insightPackUnknown, insightPartial]) {
    const s = await renderInFrame(<VacuumView data={data} />);
    await expect.element(s.getByText(data.pack.text)).toBeVisible();
    expect(s.container.querySelector("[data-pack-state]")).not.toBeNull();
    await s.unmount();
  }
});

test("a partial panel says so with glyph, word and Go's reason above its rows; a complete one says nothing", async () => {
  const s = await renderInFrame(<PartialNotice panel={{ partial: true, partialText: "pgtoolbox_tables failed." }} />);
  await expect.element(s.getByText("Partial")).toBeVisible();
  const note = s.container.querySelector("[data-partial]");
  expect(note?.getAttribute("data-health")).toBe("warning");
  expect(note?.querySelector(".health-glyph")).not.toBeNull();
  expect(note?.textContent).toContain("pgtoolbox_tables failed.");
  await s.unmount();
  const none = await renderInFrame(
    <>
      <p>complete</p>
      <PartialNotice panel={{ partial: false }} />
    </>,
  );
  await expect.element(none.getByText("complete")).toBeVisible();
  expect(none.container.querySelector("[data-partial]")).toBeNull();
  await none.unmount();
  const v = await renderInFrame(<VacuumView data={insightPartial} />);
  await expect.element(v.getByText(/pgtoolbox_tables failed on database billing/)).toBeVisible();
  const partial = v.container.querySelector("[data-partial]");
  const table = v.container.querySelector("table");
  expect(partial && table && partial.compareDocumentPosition(table) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  await v.unmount();
});

test("the landing marks each partial panel and names the databases a query failed on", async () => {
  const s = await renderInFrame(<InsightPackView data={insightPartial} />);
  await expect.element(s.getByText("billing")).toBeVisible();
  expect(s.container.querySelectorAll("[data-panel-partial]").length).toBe(5);
  await s.unmount();
});

test("the xmin horizon: Go's holder emphasised, a missing age is nothing holding one, a missing block not reported", async () => {
  const s = await renderInFrame(<HorizonHolders horizons={insightHorizonHolders.wraparound.horizons} />);
  await expect.element(s.getByText("nothing holds one")).toBeVisible();
  const held = s.container.querySelectorAll("[data-horizon-holder]");
  expect(held.length).toBe(1);
  expect(held[0]?.textContent).toContain("Prepared transaction");
  await s.unmount();
  const none = await renderInFrame(<HorizonHolders horizons={undefined} />);
  await expect.element(none.getByText("not reported")).toBeVisible();
  await none.unmount();
});

test("tables: a TOAST table names its parent to vacuum, an absent threshold is unknown and never 0", async () => {
  const s = await renderInFrame(<VacuumView data={insightHorizonHolders} />);
  await expect.element(s.getByText("unknown: never vacuumed nor analyzed")).toBeVisible();
  expect(s.container.querySelector("[data-toast-parent]")?.getAttribute("data-toast-parent")).toBe("public.documents");
  expect(s.container.querySelectorAll("[data-threshold=unknown]").length).toBe(1);
  await s.unmount();
});

test("tables: six columns that fit a desktop width, the secondary facts folded in, last-run times on one line", async () => {
  await page.viewport(1400, 900);
  try {
    const s = await renderInFrame(<VacuumView data={insightEnabled} />);
    const region = s.getByRole("region", { name: "Tables the pack singled out" });
    await expect.element(region).toBeVisible();
    const section = region.element() as HTMLElement;
    expect(section.querySelectorAll("thead th").length).toBe(6);
    // Nothing is clipped: the table fits its scroll region at 1400 px.
    expect(section.scrollWidth).toBeLessThanOrEqual(section.clientWidth);
    const rows = section.querySelectorAll("tbody tr");
    expect(rows.length).toBe(insightEnabled.wraparound.tables?.length);
    for (const row of rows) {
      expect(row.querySelector("[data-mxid-age]")?.textContent).toContain("MXID");
      expect(row.textContent).toContain("reltuples");
      expect(row.querySelector("[data-av-enabled]")).not.toBeNull();
    }
    // audit.trail has autovacuum_enabled off.
    expect(rows[2]?.querySelector("[data-av-enabled]")?.textContent).toContain("no");
    for (const dd of section.querySelectorAll("dd")) {
      expect(getComputedStyle(dd).whiteSpace).toBe("nowrap");
    }
    await s.unmount();
  } finally {
    await page.viewport(414, 896);
  }
});

test("prepared transactions in the lock graph: no pid, how they end, and distinct rows though they share pid 0", async () => {
  const s = await renderInFrame(<BlockTree nodes={insightBlockingPrepared.instances[0]?.roots ?? []} root />);
  await expect.element(s.getByText("pid 6103")).toBeVisible();
  expect(s.container.querySelectorAll("li[data-prepared]").length).toBe(2);
  expect(s.container.querySelector('li[data-pid="0"]')).toBeNull();
  expect(s.container.textContent).not.toContain("pid 0");
  expect(s.container.textContent).toContain("COMMIT PREPARED or ROLLBACK PREPARED");
  expect(
    s.container.querySelector('li[data-pid="6103"]')?.parentElement?.closest("li")?.hasAttribute("data-prepared"),
  ).toBe(true);
  await s.unmount();
});

test("an uncertain regression carries the badge and its reason", async () => {
  const s = await renderInFrame(<StatementsView data={insightStatementsUncertain} />);
  await expect.element(s.getByText("Uncertain")).toBeVisible();
  expect(s.container.querySelectorAll("[data-uncertain]").length).toBe(1);
  expect(s.container.querySelector("[data-no-baseline]")?.getAttribute("data-no-baseline")).toBe("4");
  await s.unmount();
});

test("a duplicate pair both backing a constraint is reported not droppable", async () => {
  const s = await renderInFrame(<IndexesView data={insightIndexesConstraints} />);
  await expect.element(s.getByText("Not droppable: both back a constraint")).toBeVisible();
  expect(s.container.querySelectorAll("[data-droppable=false]").length).toBe(1);
  expect(s.container.querySelectorAll("[data-fk-support]").length).toBe(1);
  await s.unmount();
});

test("connections are counted against the usable slots, with max_connections and reserved stated", async () => {
  const s = await renderInFrame(<ConnectionsView data={insightEnabled} />);
  await expect.element(s.getByText("172 of 197 usable client slots (87.3%)")).toBeVisible();
  await s.unmount();
});
