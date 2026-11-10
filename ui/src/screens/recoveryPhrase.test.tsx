// Go's recovery sentences said from their codes: French in the French UI,
// Go's English for a code or a value this SPA does not know.
import { afterEach, expect, test } from "vitest";
import { backups } from "../../mocks/fixtures";
import { restoreCheckArchivingFailing } from "../../mocks/fixtures/extra";
import { overwriteGetLocale } from "../paraglide/runtime.js";
import { renderInFrame } from "../testing/render";
import { RestoreAnswer } from "./RecoveryWindow";
import { phraseText, reasonsText } from "./recoveryPhrase";

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

test("the reasons, the backlog, the end of WAL and the caps are said in French", () => {
  overwriteGetLocale(() => "fr");
  const cases: [string, Record<string, string> | undefined, RegExp][] = [
    ["recovery.reason.archiveBacklog", { segments: "12" }, /^12 segments WAL attendent/],
    ["recovery.segment.archiveBacklog", { segments: "12" }, /^12 segments WAL attendent .*depuis avant les relevés/],
    [
      "recovery.segment.archiveBacklogSince",
      { segments: "30", sinceAt: "2026-10-29T11:40:00Z" },
      /^30 segments WAL attendent d’être archivés sur le primaire depuis le 29 oct\. 2026, 11:40 UTC/,
    ],
    ["recovery.segment.afterLastReadFailing", { scannedAt: "2026-10-29T11:50:00Z" }, /l’archivage des WAL échoue/],
    [
      "recovery.reportedTail.afterArchivedBacklog",
      { endAt: "2026-10-29T07:08:00Z", archivedAt: "2026-10-29T11:55:00Z", segments: "12" },
      /12 segments WAL attendent/,
    ],
    [
      "recovery.segment.walEnd",
      { endAt: "2026-10-29T11:58:00Z", marginSeconds: "300" },
      /restaurez jusqu’à la fin des WAL/,
    ],
    [
      "timeline.reason.needsTarget",
      { timeline: "12", gap: "unreadable", gapTimeline: "11" },
      /^la timeline 12 demande un targetTLI explicite ; l’historique 11 est illisible/,
    ],
    [
      "timeline.reason.latestStops",
      { latest: "2", newest: "4", gap: "missing", gapTimeline: "3" },
      /s’arrête sur la timeline 2 : l’historique 3 est manquant/,
    ],
    ["phase.cap.undetectedMinor", { table: "1.30" }, /^version de CNPG non détectée/],
    ["condition.cap.unverifiedMinor", { minor: "1.31" }, /non vérifiée pour CNPG 1\.31/],
  ];
  for (const [code, params, want] of cases) {
    const said = phraseText(code, params, "Go's English.");
    expect(said, code).toMatch(want);
  }
  // A gap kind this SPA does not know keeps Go's English.
  expect(phraseText("timeline.reason.latestUnknown", { gap: "fromTheFuture" }, "Go's English.")).toBe("Go's English.");
});

test("a no-window answer says each reason in French, and names a history file", async () => {
  overwriteGetLocale(() => "fr");
  const r = {
    ...tail,
    answer: "unknown" as const,
    code: "recovery.check.noWindow",
    params: { reason: "no repository evidence" },
    text: "No restore window is known for this cluster: no repository evidence.",
    reasons: [
      { code: "recovery.reason.noEvidence", text: "no repository evidence" },
      { code: "recovery.reason.fromTheFuture", text: "an English reason" },
    ],
    historyFile: "00000005.history",
  };
  expect(reasonsText(r.reasons, "joined")).toBe("aucune preuve du dépôt; an English reason");
  const screen = await renderInFrame(<RestoreAnswer r={r} />);
  const el = screen.container;
  expect(el.querySelector("[data-answer-code]")?.textContent).toBe(
    "Aucune fenêtre de restauration n’est connue pour ce cluster : aucune preuve du dépôt; an English reason.",
  );
  expect(el.querySelector("[data-history-file]")?.textContent).toMatch(/00000005\.history/);
});

test("the ancestry reasons, the off-target hint and the timeline warning are said in French", () => {
  overwriteGetLocale(() => "fr");
  const cases: [string, Record<string, string>, RegExp][] = [
    ["timeline.reason.needsTargetSibling", { latest: "3", timeline: "2" }, /suit la timeline 3.*il faut targetTLI 2$/],
    [
      "timeline.reason.unneededInvalid",
      { historyFile: "00000002.history", problem: "malformed" },
      /^00000002\.history : l’historique de timeline est mal formé ; aucune/,
    ],
    [
      "timeline.reason.ancestryInvalid",
      { historyFile: "00000003.history", problem: "unreadable" },
      /00000003\.history : l’historique de timeline n’a pas pu être lu$/,
    ],
    [
      "timeline.reason.latestUnknown",
      { gap: "invalid", gapTimeline: "4" },
      /l’historique 4 est illisible ou invalide$/,
    ],
    [
      "recovery.reason.timelineNotEstablished",
      {
        state: "unknown",
        detail: "timeline ancestry contains missing or invalid evidence",
        inner: "timeline.reason.ancestryInvalid",
      },
      /\(l’ascendance des timelines contient des preuves manquantes ou invalides\)/,
    ],
    ["recovery.reason.agentPredates", {}, /mettez l’agent à jour$/],
    ["recovery.offTarget.needsTarget", { backupId: "b2", timeline: "2" }, /demande targetTLI 2/],
    [
      "recovery.caveat.timelineWarning",
      { historyFile: "00000002.history", detail: "x" },
      /00000002\.history, qu’aucune restauration/,
    ],
  ];
  for (const [code, params, want] of cases) {
    expect(phraseText(code, params, "Go's English."), code).toMatch(want);
  }
  expect(phraseText("timeline.reason.unneededInvalid", { historyFile: "f", problem: "new" }, "Go's English.")).toBe(
    "Go's English.",
  );
});

test("a covered answer names the timeline a default restore follows; any other answer does not", async () => {
  const covered = await renderInFrame(<RestoreAnswer r={{ ...tail, answer: "covered", backupId: "b1" }} />);
  expect(covered.container.querySelector("[data-target-timeline-latest]")?.textContent).toMatch(
    /^Default recovery_target_timeline 'latest'/,
  );
  const unknown = await renderInFrame(<RestoreAnswer r={tail} />);
  expect(unknown.container.querySelector("[data-target-timeline-latest]")).toBeNull();
});
