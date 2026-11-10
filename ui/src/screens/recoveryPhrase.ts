// The recovery window's sentences, said in the reader's language. Go
// writes each with a stable code and the values it names
// (viewmodel.Phrase): an instant under a key ending in "At", a duration
// under one ending in "Seconds". A code this file does not know, or a
// value it needs that is missing, shows Go's English text: never a blank
// and never a guess.
import { formatDuration } from "../ds/format";
import { formatUtcMinute } from "../ds/time";
import { m } from "../paraglide/messages.js";

type Params = Record<string, string> | undefined;

/** An instant parameter to the minute, in UTC; undefined when absent or not an instant. */
function instant(p: Params, key: string): string | undefined {
  const v = p?.[key];
  return v === undefined || Number.isNaN(Date.parse(v)) ? undefined : formatUtcMinute(v);
}

/** A whole-seconds parameter as words; undefined when absent or not a number. */
function seconds(p: Params, key: string): string | undefined {
  const v = p?.[key];
  const n = v === undefined || v === "" ? Number.NaN : Number(v);
  return Number.isFinite(n) ? formatDuration(n) : undefined;
}

/** The reported tail's sentence: who reported the window's end, then what the archiver says. */
function reportedTail(kind: string, p: Params): string | undefined {
  const end = instant(p, "endAt");
  if (!end) return undefined;
  const prefix =
    p?.windowOrigin === "evidence" ? m.rp_tail_prefix_evidence({ end }) : m.rp_tail_prefix_operator({ end });
  if (kind === "archivedUnknown") return `${prefix} ${m.rp_tail_archived_unknown()}`;
  if (kind === "archivedUnknownFailing") return `${prefix} ${m.rp_tail_archived_unknown_failing()}`;
  const archived = instant(p, "archivedAt");
  if (!archived) return undefined;
  const age = seconds(p, "ageSeconds");
  const report = age ? m.rp_tail_report_age({ age }) : m.rp_tail_report();
  const args = { archived, report };
  switch (kind) {
    case "beforeArchived":
      return `${prefix} ${m.rp_tail_before_archived(args)}`;
    case "beforeArchivedFailing":
      return `${prefix} ${m.rp_tail_before_archived_failing(args)}`;
    case "beforeArchivedUnconfirmed":
      return `${prefix} ${m.rp_tail_before_archived_unconfirmed(args)}`;
    case "afterArchived":
      return `${prefix} ${m.rp_tail_after_archived(args)}`;
    case "afterArchivedFailing":
      return `${prefix} ${m.rp_tail_after_archived_failing(args)}`;
    default:
      return undefined;
  }
}

function translate(code: string, p: Params): string | undefined {
  if (code.startsWith("recovery.reportedTail.")) return reportedTail(code.slice("recovery.reportedTail.".length), p);
  const id = p?.backupId;
  const margin = seconds(p, "marginSeconds");
  switch (code) {
    case "recovery.segment.reported":
      return p?.source ? m.rp_segment_reported({ source: p.source }) : undefined;
    case "recovery.segment.covered":
      return id ? m.rp_segment_covered({ id }) : undefined;
    case "recovery.segment.coveredCut":
      return m.rp_segment_covered_cut();
    case "recovery.segment.nearHole":
      return margin ? m.rp_segment_near_hole({ margin }) : undefined;
    case "recovery.segment.missingWAL":
      return m.rp_segment_missing_wal();
    case "recovery.segment.candidateGap":
      return m.rp_segment_candidate_gap();
    case "recovery.segment.continuationUnknown":
      return m.rp_segment_continuation_unknown();
    case "recovery.segment.nearEnd":
      return margin ? m.rp_segment_near_end({ margin }) : undefined;
    case "recovery.segment.nearEndNoTimeout":
      return m.rp_segment_near_end_no_timeout();
    case "recovery.segment.archivingFailed": {
      const failed = instant(p, "failedAt");
      return failed ? m.rp_segment_archiving_failed({ failed }) : undefined;
    }
    case "recovery.segment.purgeEdge":
      return margin ? m.rp_segment_purge_edge({ margin }) : undefined;
    case "recovery.segment.missingAfterLatest":
      return m.rp_segment_missing_after_latest();
    case "recovery.segment.collectionCut":
      return m.rp_segment_collection_cut();
    case "recovery.segment.evidenceStale":
      return m.rp_segment_evidence_stale();
    case "recovery.segment.afterLastRead": {
      const scanned = instant(p, "scannedAt");
      return scanned ? m.rp_segment_after_last_read({ scanned }) : undefined;
    }
    case "recovery.segment.afterFrontier":
      return m.rp_segment_after_frontier();
    case "recovery.check.noWindow":
      // The reason is Go's own sentence, shown as written.
      return p?.reason ? m.rp_check_no_window({ reason: p.reason }) : undefined;
    case "recovery.check.beforeEarliestCut":
    case "recovery.check.beforeEarliest":
    case "recovery.check.beforeEarliestReported": {
      const earliest = instant(p, "earliestAt");
      if (!earliest) return undefined;
      if (code === "recovery.check.beforeEarliestCut") return m.rp_check_before_earliest_cut({ earliest });
      if (code === "recovery.check.beforeEarliest") return m.rp_check_before_earliest({ earliest });
      return m.rp_check_before_earliest_reported({ earliest });
    }
    case "recovery.check.reported":
      return p?.source ? m.rp_check_reported({ source: p.source }) : undefined;
    case "recovery.check.reportedByAgent":
      return p?.source ? m.rp_check_reported_by_agent({ source: p.source }) : undefined;
    case "recovery.check.covered":
      return id ? m.rp_check_covered({ id }) : undefined;
    case "recovery.check.notPlaced":
      return m.rp_check_not_placed();
    case "recovery.caveat.segmentNames":
      return m.rp_caveat_segment_names();
    case "recovery.caveat.latestBounded":
      return m.rp_caveat_latest_bounded();
    case "recovery.caveat.collectionsCut":
      return m.rp_caveat_collections_cut();
    case "recovery.caveat.evidenceStale":
      return m.rp_caveat_evidence_stale();
    case "recovery.caveat.windowStale":
      return m.rp_caveat_window_stale();
    case "recovery.caveat.archivingFailing":
      return m.rp_caveat_archiving_failing();
    case "recovery.caveat.archivingFailingSince": {
      const archived = instant(p, "archivedAt");
      return archived ? m.rp_caveat_archiving_failing_since({ archived }) : undefined;
    }
    case "recovery.caveat.notRestoreTested":
      return p?.days ? m.not_restore_tested({ days: p.days }) : undefined;
    default:
      return undefined;
  }
}

/** A Go sentence in the reader's language; Go's English text when the code or a value is not known here. */
export function phraseText(code: string | undefined, params: Params, text: string | undefined): string {
  return (code ? translate(code, params) : undefined) ?? text ?? "";
}
