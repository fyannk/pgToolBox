import { expect, test } from "vitest";
import {
  promoteTargetFenced,
  promoteTargetLagging,
  promoteTargetReady,
  promoteTargetUnknown,
} from "../../mocks/fixtures/extra";
import { renderInFrame } from "../testing/render";
import { PromoteReadiness } from "./PromoteReadiness";

const healthOf = (root: Element, key: string) =>
  root.querySelector(`[data-fact="${key}"]`)?.getAttribute("data-health");

test("readiness: every fact carries Go's verdict word, its text and its source", async () => {
  const screen = await renderInFrame(<PromoteReadiness target={promoteTargetReady} />);
  for (const f of promoteTargetReady.readiness ?? []) {
    expect(healthOf(screen.container, f.key)).toBe(f.health);
    await expect.element(screen.getByText(f.text)).toBeVisible();
    if (f.provenance.source) await expect.element(screen.getByText(f.provenance.source).first()).toBeVisible();
  }
  // The verdict is a word with a glyph, not colour alone.
  expect(screen.container.querySelectorAll('[data-fact] [data-health="ok"]').length).toBe(5);
  expect(screen.container.textContent).not.toMatch(/would refuse/);
});

test("readiness: an unread fact is unknown and marked stale, never ok", async () => {
  const screen = await renderInFrame(<PromoteReadiness target={promoteTargetUnknown} />);
  for (const key of ["streaming", "replayLag", "syncState"]) expect(healthOf(screen.container, key)).toBe("unknown");
  expect(screen.container.querySelectorAll("[data-fact] [data-stale]").length).toBe(3);
});

test("readiness: lag is shown as a fact, not as a refusal", async () => {
  const screen = await renderInFrame(<PromoteReadiness target={promoteTargetLagging} />);
  expect(healthOf(screen.container, "replayLag")).toBe("info");
  expect(screen.container.textContent).not.toMatch(/would refuse/);
});

test("readiness: a fenced target states what CloudNativePG would refuse", async () => {
  const screen = await renderInFrame(<PromoteReadiness target={promoteTargetFenced} />);
  await expect.element(screen.getByText("CloudNativePG would refuse:")).toBeVisible();
  expect(healthOf(screen.container, "fencing")).toBe("critical");
});

test("readiness: no facts served reads unknown, never ready", async () => {
  const screen = await renderInFrame(<PromoteReadiness target={{ ...promoteTargetReady, readiness: undefined }} />);
  expect(screen.container.querySelector('[data-health="unknown"]')).not.toBeNull();
  expect(screen.container.querySelector('[data-health="ok"]')).toBeNull();
  await expect.element(screen.getByText(/does not serve readiness facts/)).toBeVisible();
});
