// The recovery window's sentences, said in the reader's language. Go
// writes each with a stable code and the values it names
// (viewmodel.Phrase): an instant under a key ending in "At", a duration
// under one ending in "Seconds". A code this file does not know, or a
// value it needs that is missing, shows Go's English text: never a blank
// and never a guess.
import { formatDuration, formatNumber } from "../ds/format";
import { healthWord } from "../ds/health";
import { formatUtcMinute } from "../ds/time";
import { m } from "../paraglide/messages.js";

type Params = Record<string, string> | undefined;

/** An instant parameter to the minute, in UTC; undefined when absent or not an instant. */
function instant(p: Params, key: string): string | undefined {
  const v = p?.[key];
  return v === undefined || Number.isNaN(Date.parse(v)) ? undefined : formatUtcMinute(v);
}

/** A count parameter in the reader's digits ("1,000", "1 000"); undefined when absent or not a number. */
function count(p: Params, key: string): string | undefined {
  const v = p?.[key];
  const n = v === undefined || v === "" ? Number.NaN : Number(v);
  return Number.isFinite(n) ? formatNumber(n) : undefined;
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
  const segments = p?.segments;
  if (kind === "archivedUnknownBacklog")
    return segments ? `${prefix} ${m.rp_tail_archived_unknown_backlog({ segments })}` : undefined;
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
    case "beforeArchivedBacklog":
      return segments ? `${prefix} ${m.rp_tail_before_archived_backlog({ ...args, segments })}` : undefined;
    case "afterArchivedBacklog":
      return segments ? `${prefix} ${m.rp_tail_after_archived_backlog({ ...args, segments })}` : undefined;
    case "afterArchived":
      return `${prefix} ${m.rp_tail_after_archived(args)}`;
    case "afterArchivedFailing":
      return `${prefix} ${m.rp_tail_after_archived_failing(args)}`;
    default:
      return undefined;
  }
}

/** A timeline gap (Gap in the agent's timeline package) from its kind and timeline. */
function timelineGap(p: Params): string | undefined {
  const timeline = p?.gapTimeline;
  switch (p?.gap) {
    case "invalid":
      return timeline ? m.tl_gap_invalid({ timeline }) : undefined;
    case "unreadable":
      return timeline ? m.tl_gap_unreadable({ timeline }) : undefined;
    case "missing":
      return timeline ? m.tl_gap_missing({ timeline }) : undefined;
    case "notAllRead":
      return m.tl_gap_not_all_read();
    default:
      return undefined;
  }
}

/** A history row's problem (barmancloud narrowHistories), from its code. */
function historyProblem(code: string | undefined): string | undefined {
  switch (code) {
    case "duplicate":
      return m.tl_problem_duplicate();
    case "unreadable":
      return m.tl_problem_unreadable();
    case "malformed":
      return m.tl_problem_malformed();
    case "contradictory":
      return m.tl_problem_contradictory();
    case "cyclic":
      return m.tl_problem_cyclic();
    case "rangeDisagrees":
      return m.tl_problem_range_disagrees();
    default:
      return undefined;
  }
}

/** The agent's timeline reasons, recognised by the console (timelinereason.go). */
function timelineReason(code: string, p: Params): string | undefined {
  const timeline = p?.timeline;
  const file = p?.historyFile;
  if (code === "timeline.reason.historyMissingNoFollow")
    return timeline ? m.tl_reason_history_missing_no_follow({ timeline }) : undefined;
  if (code === "timeline.reason.needsTargetSibling")
    return timeline && p?.latest ? m.tl_reason_needs_target_sibling({ latest: p.latest, timeline }) : undefined;
  if (code === "timeline.reason.ancestryInvalid") {
    if (!p?.problem) return m.tl_reason_ancestry_invalid();
    const problem = historyProblem(p.problem);
    return file && problem ? m.tl_reason_ancestry_invalid_file({ file, problem }) : undefined;
  }
  if (code === "timeline.reason.unneededInvalid") {
    const problem = historyProblem(p?.problem);
    return file && problem ? m.tl_reason_unneeded_invalid({ file, problem }) : undefined;
  }
  const gap = timelineGap(p);
  if (!gap) return undefined;
  switch (code) {
    case "timeline.reason.needsTarget":
      return timeline ? m.tl_reason_needs_target({ timeline, gap }) : undefined;
    case "timeline.reason.latestUnknown":
      return m.tl_reason_latest_unknown({ gap });
    case "timeline.reason.latestStops":
      return p?.latest && p.newest ? m.tl_reason_latest_stops({ latest: p.latest, newest: p.newest, gap }) : undefined;
    default:
      return undefined;
  }
}

/** Why a phase or condition reading was capped below its table's ok. */
function tableCap(code: string, p: Params): string | undefined {
  const minor = p?.minor;
  const table = p?.table;
  switch (code) {
    case "phase.cap.unverifiedMinor":
      return minor ? m.cap_phase_unverified({ minor }) : undefined;
    case "phase.cap.undetectedMinor":
      return table ? m.cap_phase_undetected({ table }) : undefined;
    case "condition.cap.unverifiedMinor":
      return minor ? m.cap_condition_unverified({ minor }) : undefined;
    case "condition.cap.undetectedMinor":
      return table ? m.cap_condition_undetected({ table }) : undefined;
    default:
      return undefined;
  }
}

/** The recovery timeline's reasons (recovery.reason.*). */
function recoveryReason(code: string, p: Params): string | undefined {
  const detail = p?.detail;
  const segments = p?.segments;
  switch (code) {
    case "recovery.reason.backupsCut":
      return m.rp_reason_backups_cut();
    case "recovery.reason.reportedWindow":
      return m.rp_reason_reported_window();
    case "recovery.reason.noEvidence":
      return m.rp_reason_no_evidence();
    case "recovery.reason.noEvidenceBecause":
      return detail ? m.rp_reason_no_evidence_because({ detail }) : undefined;
    case "recovery.reason.noPaths":
      return m.rp_reason_no_paths();
    case "recovery.reason.pathsNotCoverage":
    case "recovery.reason.agentCoverage":
    case "recovery.reason.timelineWarning":
      // The evidence's own sentence, in its words.
      return detail ? m.rp_reason_from_evidence({ detail }) : undefined;
    case "recovery.reason.noConsistentPath":
      return m.rp_reason_no_consistent_path();
    case "recovery.reason.pathsStale":
      return detail ? m.rp_reason_paths_stale({ detail }) : undefined;
    case "recovery.reason.evidenceStale":
      return detail ? m.rp_reason_evidence_stale({ detail }) : undefined;
    case "recovery.reason.pathsCut":
      return m.rp_reason_paths_cut();
    case "recovery.reason.missingAfterLatestFormerPrimaryRejoined":
      return p?.instance
        ? m.rp_reason_missing_after_latest_former_primary_rejoined({ instance: p.instance })
        : undefined;
    case "recovery.reason.missingAfterLatestFormerPrimary":
      return p?.instance
        ? m.rp_reason_missing_after_latest_former_primary_named({ instance: p.instance })
        : m.rp_reason_missing_after_latest_former_primary();
    case "recovery.reason.missingAfterLatest":
      return m.rp_reason_missing_after_latest();
    case "recovery.reason.holes":
      return m.rp_reason_holes();
    case "recovery.reason.candidateGaps":
      return m.rp_reason_candidate_gaps();
    case "recovery.reason.failedRecently":
      return m.rp_reason_failed_recently();
    case "recovery.reason.archivingFailing":
      return m.rp_reason_archiving_failing();
    case "recovery.reason.archivingFailedPrimaryUnread": {
      if (p?.sinceAt === undefined) return m.rp_reason_archiving_failed_primary_unread();
      const since = instant(p, "sinceAt");
      return since ? m.rp_reason_archiving_failed_primary_unread_since({ since }) : undefined;
    }
    case "recovery.reason.archiveBacklog":
      return segments ? m.rp_reason_archive_backlog({ segments }) : undefined;
    case "recovery.reason.archiverUnread":
      return m.rp_reason_archiver_unread();
    case "recovery.reason.archiverHibernated":
      return m.rp_reason_archiver_hibernated();
    case "recovery.reason.archivingFailedHibernated": {
      if (p?.sinceAt === undefined) return m.rp_reason_archiving_failed_hibernated();
      const since = instant(p, "sinceAt");
      return since ? m.rp_reason_archiving_failed_hibernated_since({ since }) : undefined;
    }
    case "recovery.reason.archiverNotReported":
      return m.rp_reason_archiver_not_reported();
    case "recovery.reason.pathNotHealthy": {
      const state = evidenceStateWord(p?.state);
      const id = p?.backupId;
      if (!state || !id) return undefined;
      return detail
        ? m.rp_reason_path_not_healthy({ id, state, detail })
        : m.rp_reason_path_not_healthy_bare({ id, state });
    }
    case "recovery.reason.backupNotListed": {
      const limit = count(p, "limit");
      return p?.backupId && limit ? m.rp_reason_backup_not_listed({ id: p.backupId, limit }) : undefined;
    }
    case "recovery.reason.backupGone":
      return p?.backupId ? m.rp_reason_backup_gone({ id: p.backupId }) : undefined;
    case "recovery.reason.backupUnconfirmed":
      return p?.backupId ? m.rp_reason_backup_unconfirmed({ id: p.backupId }) : undefined;
    case "recovery.reason.liveTimelineNotArchived":
      return p?.timeline ? m.rp_reason_live_timeline_not_archived({ timeline: p.timeline }) : undefined;
    case "recovery.reason.liveTimelineNotArchivedPrimaryUnread":
      return p?.timeline ? m.rp_reason_live_timeline_not_archived_primary_unread({ timeline: p.timeline }) : undefined;
    case "recovery.reason.repositoryAheadOfLive":
      return p?.timeline && p?.liveTimeline
        ? m.rp_reason_repository_ahead_of_live({ timeline: p.timeline, live: p.liveTimeline })
        : undefined;
    case "recovery.reason.keptTooOld":
      return m.rp_reason_kept_too_old();
    case "recovery.reason.keptRepositoryChanged":
      return m.rp_reason_kept_repository_changed();
    case "recovery.reason.backupNotUsable": {
      const id = p?.backupId;
      if (!id) return undefined;
      if (detail) return m.rp_reason_backup_not_usable({ id, detail });
      return p?.reason ? m.rp_reason_backup_not_usable_bare({ id, reason: p.reason }) : undefined;
    }
    case "recovery.reason.orphanTimeline":
      return p?.timeline ? m.rp_reason_orphan_timeline({ timeline: p.timeline }) : undefined;
    case "recovery.reason.undrawn":
      return m.rp_reason_undrawn();
    case "recovery.reason.agentPredates":
      return m.rp_reason_agent_predates();
    case "recovery.reason.timelineNotEstablished": {
      const state = evidenceStateWord(p?.state);
      if (!state) return undefined;
      if (!detail) return m.rp_reason_timeline_not_established_bare({ state });
      // The agent's own reason inside, said too when the console knew it.
      const inner = (p?.inner ? translate(p.inner, p) : undefined) ?? detail;
      return m.rp_reason_timeline_not_established({ state, detail: inner });
    }
    default:
      return undefined;
  }
}

/** The evidence agent's state word, in the reader's language. */
function evidenceStateWord(state: string | undefined): string | undefined {
  switch (state) {
    case "unknown":
      return healthWord("unknown");
    case "unhealthy":
      return healthWord("critical");
    case "warning":
      return healthWord("warning");
    default:
      return undefined;
  }
}

function translate(code: string, p: Params): string | undefined {
  if (code.startsWith("recovery.reportedTail.")) return reportedTail(code.slice("recovery.reportedTail.".length), p);
  if (code.startsWith("recovery.reason.")) return recoveryReason(code, p);
  if (code.startsWith("timeline.reason.")) return timelineReason(code, p);
  if (code.startsWith("phase.cap.") || code.startsWith("condition.cap.")) return tableCap(code, p);
  const segments = p?.segments;
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
    case "recovery.segment.missingAfterLatestFormerPrimary":
      return p?.instance
        ? m.rp_segment_missing_after_latest_former_primary_named({ instance: p.instance })
        : m.rp_segment_missing_after_latest_former_primary();
    case "recovery.segment.missingAfterLatestFormerPrimaryRejoined":
      return p?.instance
        ? m.rp_segment_missing_after_latest_former_primary_rejoined({ instance: p.instance })
        : undefined;
    case "recovery.segment.missingWALFormerPrimaryRejoined":
      return p?.instance ? m.rp_segment_missing_wal_former_primary_rejoined({ instance: p.instance }) : undefined;
    case "recovery.segment.liveTimelineNotArchived":
      return p?.timeline ? m.rp_segment_live_timeline_not_archived({ timeline: p.timeline }) : undefined;
    case "recovery.segment.liveTimelineNoSegment":
      return p?.timeline ? m.rp_segment_live_timeline_no_segment({ timeline: p.timeline }) : undefined;
    case "recovery.segment.repositoryAheadOfLive":
      return p?.timeline && p?.liveTimeline
        ? m.rp_segment_repository_ahead_of_live({ timeline: p.timeline, live: p.liveTimeline })
        : undefined;
    case "recovery.segment.missingWALFormerPrimary":
      return p?.instance
        ? m.rp_segment_missing_wal_former_primary_named({ instance: p.instance })
        : m.rp_segment_missing_wal_former_primary();
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
    case "recovery.segment.afterLastReadFailing": {
      const scanned = instant(p, "scannedAt");
      return scanned ? m.rp_segment_after_last_read_failing({ scanned }) : undefined;
    }
    case "recovery.segment.afterFrontierFailing":
      return m.rp_segment_after_frontier_failing();
    case "recovery.segment.afterLastReadBacklog": {
      const scanned = instant(p, "scannedAt");
      return scanned && segments ? m.rp_segment_after_last_read_backlog({ scanned, segments }) : undefined;
    }
    case "recovery.segment.afterFrontierBacklog":
      return segments ? m.rp_segment_after_frontier_backlog({ segments }) : undefined;
    case "recovery.segment.archiveBacklog":
      return segments ? m.rp_segment_archive_backlog({ segments }) : undefined;
    case "recovery.segment.archiveBacklogSince": {
      const quiet = instant(p, "quietAt");
      return segments && quiet ? m.rp_segment_archive_backlog_since({ segments, quiet }) : undefined;
    }
    case "recovery.segment.archiverUnread":
      return m.rp_segment_archiver_unread();
    case "recovery.segment.archiverNotReported":
      return m.rp_segment_archiver_not_reported();
    case "recovery.segment.archiverHibernated":
      return m.rp_segment_archiver_hibernated();
    case "recovery.segment.archiverUnreadSince": {
      const quiet = instant(p, "quietAt");
      return quiet ? m.rp_segment_archiver_unread_since({ quiet }) : undefined;
    }
    case "recovery.segment.archiveBacklogBound": {
      const read = instant(p, "readAt");
      const bound = instant(p, "boundAt");
      return segments && read && bound ? m.rp_segment_archive_backlog_bound({ segments, read, bound }) : undefined;
    }
    case "recovery.segment.archiverUnreadBound": {
      const read = instant(p, "readAt");
      const bound = instant(p, "boundAt");
      return read && bound ? m.rp_segment_archiver_unread_bound({ read, bound }) : undefined;
    }
    case "recovery.segment.formerArchiverUnknown":
    case "recovery.segment.formerArchiverBacklog":
    case "recovery.segment.formerArchiverSince":
    case "recovery.segment.formerArchiverBound": {
      const timeline = p?.timeline;
      if (!timeline) return undefined;
      if (code === "recovery.segment.formerArchiverUnknown") return m.rp_segment_former_archiver_unknown({ timeline });
      if (code === "recovery.segment.formerArchiverBacklog") return m.rp_segment_former_archiver_backlog({ timeline });
      if (code === "recovery.segment.formerArchiverSince") {
        const quiet = instant(p, "quietAt");
        return quiet ? m.rp_segment_former_archiver_since({ timeline, quiet }) : undefined;
      }
      const read = instant(p, "readAt");
      const bound = instant(p, "boundAt");
      return read && bound ? m.rp_segment_former_archiver_bound({ timeline, read, bound }) : undefined;
    }
    case "recovery.segment.pathNotHealthy": {
      const state = evidenceStateWord(p?.state);
      if (!state || !id) return undefined;
      return p?.detail
        ? m.rp_segment_path_not_healthy({ id, state, detail: p.detail })
        : m.rp_segment_path_not_healthy_bare({ id, state });
    }
    case "recovery.segment.backupNotListed": {
      const limit = count(p, "limit");
      return id && limit ? m.rp_segment_backup_not_listed({ id, limit }) : undefined;
    }
    case "recovery.segment.backupGone":
      return id ? m.rp_segment_backup_gone({ id }) : undefined;
    case "recovery.segment.backupUnconfirmed":
      return id ? m.rp_segment_backup_unconfirmed({ id }) : undefined;
    case "recovery.segment.backupNotUsable": {
      if (!id) return undefined;
      if (p?.detail) return m.rp_segment_backup_not_usable({ id, detail: p.detail });
      return p?.reason ? m.rp_segment_backup_not_usable_bare({ id, reason: p.reason }) : undefined;
    }
    case "recovery.segment.walEnd": {
      const end = instant(p, "endAt");
      return end && margin ? m.rp_segment_wal_end({ end, margin }) : undefined;
    }
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
    case "recovery.caveat.archiverHibernated":
      return m.rp_caveat_archiver_hibernated();
    case "recovery.caveat.archiverUnread":
      return m.rp_caveat_archiver_unread();
    case "recovery.caveat.archiverNotReported":
      return m.rp_caveat_archiver_not_reported();
    case "recovery.caveat.archivingFailingSince": {
      const archived = instant(p, "archivedAt");
      return archived ? m.rp_caveat_archiving_failing_since({ archived }) : undefined;
    }
    case "recovery.offTarget.needsTarget":
      return id && p?.timeline ? m.rp_off_target_needs_target({ id, timeline: p.timeline }) : undefined;
    case "recovery.caveat.timelineWarning":
      if (p?.historyFile) return m.rp_caveat_timeline_warning_file({ file: p.historyFile });
      return p?.detail ? m.rp_caveat_timeline_warning({ detail: p.detail }) : undefined;
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

type Said = { code?: string; params?: Record<string, string>; text?: string };

/**
 * A reason Go joined from phrases ("; " between them): each part in the
 * reader's language when Go sent the parts, else Go's joined English.
 */
export function reasonsText(reasons: readonly Said[] | null | undefined, joined: string | undefined): string {
  if (!reasons || reasons.length === 0) return joined ?? "";
  return reasons.map((r) => phraseText(r.code, r.params, r.text)).join("; ");
}

/** The restore check's sentence: "no window" says the timeline's reasons, each translated. */
export function restoreAnswerText(r: Said & { reasons?: readonly Said[] | null }): string {
  if (r.code === "recovery.check.noWindow" && r.reasons && r.reasons.length > 0)
    return m.rp_check_no_window({ reason: reasonsText(r.reasons, r.params?.reason) });
  return phraseText(r.code, r.params, r.text);
}
