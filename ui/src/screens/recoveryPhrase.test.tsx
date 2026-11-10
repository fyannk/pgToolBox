// Go's recovery sentences said from their codes: French in the French UI,
// Go's English for a code or a value this SPA does not know.
import { afterEach, expect, test } from "vitest";
import { backups } from "../../mocks/fixtures";
import { restoreCheckArchivingFailing } from "../../mocks/fixtures/extra";
import { overwriteGetLocale } from "../paraglide/runtime.js";
import { renderInFrame } from "../testing/render";
import { RestoreAnswer } from "./RecoveryWindow";
import { phraseText } from "./recoveryPhrase";

afterEach(() => overwriteGetLocale(() => "en"));

const tail = restoreCheckArchivingFailing;

test("a known code is said from its parameters, in the reader's language", () => {
  const en = phraseText(tail.code, tail.params, tail.text);
  expect(en).toMatch(/^After the last base backup the operator reports \(Oct 29, 2026, 07:08 UTC\)/);
  expect(en).toMatch(/last archived segment \(Oct 29, 2026, 08:10 UTC, its own report on \/pg\/status, read 8 s ago\)/);
  expect(en).toMatch(/is likely not in the repository\.$/);
  overwriteGetLocale(() => "fr");
  const fr = phraseText(tail.code, tail.params, tail.text);
  expect(fr).toMatch(/^Après la dernière sauvegarde de base que rapporte l’opérateur/);
  expect(fr).toMatch(/ne sont probablement pas dans le dépôt\.$/);
  expect(fr).not.toMatch(/repository/);
});

test("an unknown code, or a missing value, shows Go's English rather than a blank or a guess", () => {
  expect(phraseText("recovery.segment.fromTheFuture", undefined, "Go's sentence.")).toBe("Go's sentence.");
  expect(phraseText("recovery.segment.covered", {}, "Structurally covered: base backup b1.")).toBe(
    "Structurally covered: base backup b1.",
  );
  expect(phraseText("recovery.reportedTail.afterArchived", { endAt: "2026-10-29T07:08:00Z" }, "Go.")).toBe("Go.");
  expect(phraseText(undefined, undefined, "plain")).toBe("plain");
});

test("every segment of the golden timeline has a code this SPA says", () => {
  overwriteGetLocale(() => "fr");
  for (const s of backups.recoveryTimeline.segments ?? []) {
    expect(s.code, s.text).toBeTruthy();
    expect(phraseText(s.code, s.params, s.text), s.code).not.toBe(s.text);
  }
});

test("restore answer in French: the answer, its caveats and an unknown caveat in Go's English", async () => {
  overwriteGetLocale(() => "fr");
  const screen = await renderInFrame(<RestoreAnswer r={tail} />);
  const el = screen.container;
  expect(el.querySelector("[data-answer-code]")?.textContent).toMatch(/ne sont probablement pas dans le dépôt/);
  expect(el.querySelector('[data-caveat="recovery.caveat.archivingFailingSince"]')?.textContent).toMatch(
    /^L’archivage des WAL échoue : le primaire a archivé son dernier segment le 29 oct\. 2026, 08:10 UTC/,
  );
  expect(el.querySelector('[data-caveat="recovery.caveat.notRestoreTested"]')?.textContent).toMatch(
    /90 derniers jours/,
  );
  expect(el.querySelector('[data-caveat="recovery.caveat.fromTheFuture"]')?.textContent).toBe(
    "A caveat this SPA does not know yet, shown in Go's English.",
  );
});
