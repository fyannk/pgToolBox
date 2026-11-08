import { useState } from "react";
import { expect, test } from "vitest";
import { page } from "vitest/browser";
import { renderInFrame } from "../../../testing/render";
import { fixtureLog } from "./fixture";
import { type LogLine, LogViewer } from "./LogViewer";

function Harness() {
  const [lines, setLines] = useState<LogLine[]>(() => fixtureLog(300));
  return (
    <>
      <button type="button" onClick={() => setLines((l) => [...l, ...fixtureLog(1, l.length)])}>
        append
      </button>
      <LogViewer title="Log of billing-1" lines={lines} state="live" />
    </>
  );
}

const atEnd = (el: HTMLElement) => el.scrollHeight - el.scrollTop - el.clientHeight < 8;

test("following keeps the end in view as lines arrive", async () => {
  await renderInFrame(<Harness />);
  const log = page.getByRole("log").element() as HTMLElement;
  await expect.poll(() => atEnd(log)).toBe(true);
  await page.getByRole("button", { name: "append" }).click();
  await expect.poll(() => atEnd(log)).toBe(true);
});

test("scrolling up pauses; new lines are counted, not scrolled to", async () => {
  await renderInFrame(<Harness />);
  const log = page.getByRole("log").element() as HTMLElement;
  await expect.poll(() => atEnd(log)).toBe(true);
  log.scrollTop = 0;
  log.dispatchEvent(new Event("scroll"));
  await expect.element(page.getByRole("button", { name: "Follow" })).toBeVisible();
  await page.getByRole("button", { name: "append" }).click();
  await page.getByRole("button", { name: "append" }).click();
  await expect.element(page.getByRole("button", { name: "2 new lines — jump to the end" })).toBeVisible();
  expect(log.scrollTop).toBe(0);
  await page.getByRole("button", { name: "2 new lines — jump to the end" }).click();
  await expect.poll(() => atEnd(log)).toBe(true);
});

test("lines are drawn in chunks of 200 so off-screen ones are skipped", async () => {
  await renderInFrame(<LogViewer title="Log" lines={fixtureLog(450)} state="live" />);
  expect(page.getByRole("log").element().querySelectorAll(".log-chunk")).toHaveLength(3);
});

test("the stream state is said in words", async () => {
  await renderInFrame(<LogViewer title="Log" lines={[]} state="ended" />);
  await expect.element(page.getByText(/^stream ended/)).toBeVisible();
  await expect.element(page.getByText("No line yet.")).toBeVisible();
});
