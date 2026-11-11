// Copyright 2026 The pgToolBox Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package evidence

import (
	"slices"

	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// Reasons of an uncertain Recovery.
const (
	reasonProducerPredatesTimelineLatest = "the evidence agent is an older build that does not resolve recovery_target_timeline 'latest' " +
		"as PostgreSQL does: its recovery paths are not shown as coverage; upgrade the agent"
	reasonTimelineNotHealthy = "the agent's timeline evidence is "
	capCode                  = "coverage-path-unknown"
	maxCapReasonBytes        = 256
)

// RollupCapCode is the reason code of a path the console capped at a
// warning because the agent's coverage roll-up says something no path
// explains (capAtRollup). It is distinct from the agent's own
// "coverage-path-warning", which a path the agent rates a warning
// carries.
const RollupCapCode = "coverage-rollup-cap"

// requiredFeatures are the producer guarantees the console's reading of
// recovery paths relies on: paths judged against the timeline 'latest'
// resolves to, and against its ancestry (a sibling branch is off target).
var requiredFeatures = []evidencev1alpha1.Feature{evidencev1alpha1.FeatureRecoveryTimelineLatest, evidencev1alpha1.FeatureRecoveryTimelineAncestry}

// capRecovery applies the console's own limits to what an agent's recovery
// paths may claim, at ingest, for Barman Cloud and pgBackRest alike:
//
//   - a producer without recovery-timeline-latest and
//     recovery-timeline-ancestry may call a path healthy that a default
//     restore (recovery_target_timeline 'latest') does not follow, so none
//     of its paths is coverage;
//   - when the publication's own timeline evidence is unknown or unhealthy
//     (a history file recovery needs is missing, unreadable or
//     contradictory), the timeline a default restore follows is not
//     established, so no path is coverage. A timeline warning (a history
//     file no default restore reads is invalid) caps nothing: its reason,
//     which names the file, is kept as TimelineWarning;
//   - no path claims more than the agent's own coverage roll-up says of
//     every path: when the roll-up is not healthy for a reason no path
//     explains (capAtRollup), a healthy path is at most a warning. The
//     roll-up is the worst over every backup's own path, so a roll-up an
//     older backup's gap or unknown stop makes unhealthy caps nothing:
//     each path carries its own state, and the roll-up's reason is kept
//     (CoverageReason) for the recovery bar to say beside the spans.
//
// A capped Recovery is RecoveryUncertain with the reason; each path is at
// most unknown (a proven loss stays unhealthy), keeps its own reason after
// the cap's, and loses its frontier receipt, which is what a recovery bar
// draws a covered span to.
func capRecovery(snapshot *evidencev1alpha1.RepositoryEvidenceSnapshot, rec Recovery, items []BackupItem) Recovery {
	if rec.State != RecoveryAvailable {
		return rec
	}
	coverage := snapshotCoverage(snapshot)
	if coverage.State != evidencev1alpha1.Healthy {
		rec.CoverageReason = boundedReason(coverage.Reason.Message, "")
	}
	reason, short := "", ""
	timeline := snapshotTimeline(snapshot)
	switch {
	case slices.ContainsFunc(requiredFeatures, func(feature evidencev1alpha1.Feature) bool { return !snapshot.Producer.Has(feature) }):
		reason, short = reasonProducerPredatesTimelineLatest, "the agent predates "+string(evidencev1alpha1.FeatureRecoveryTimelineAncestry)
	case timeline.State == evidencev1alpha1.Warning:
		rec.TimelineWarning = boundedReason(timeline.Reason.Message, "")
	case timeline.State != evidencev1alpha1.Healthy:
		reason = reasonTimelineNotHealthy + string(timeline.State)
		if timeline.Reason.Message != "" {
			reason += " (" + timeline.Reason.Message + ")"
		}
		reason += ": no recovery path is shown as coverage until the timeline a default restore follows is established"
		short = "the timeline evidence is " + string(timeline.State)
	}
	if reason == "" {
		return capAtRollup(snapshot, rec, coverage, items)
	}
	reason = boundedReason(reason, "")
	rec.State, rec.Reason = RecoveryUncertain, reason
	rec.Paths = slices.Clone(rec.Paths)
	for index := range rec.Paths {
		path := &rec.Paths[index]
		if path.State == evidencev1alpha1.Healthy || path.State == evidencev1alpha1.Warning {
			path.State, path.Stop = evidencev1alpha1.Unknown, evidencev1alpha1.CoverageUnknownLimited
			path.Reason.Code = capCode
		}
		// The path's own reason first (an off-target path's names the
		// timeline a restore must ask for), then why it is not coverage.
		path.Reason.Message = boundedReason(path.Reason.Message, "not coverage: "+short)
		path.FrontierReceiptAt = nil
		path.Assumptions = slices.Clone(path.Assumptions)
	}
	return rec
}

// capAtRollup makes every healthy path at most a warning when the agent's
// coverage roll-up is not healthy for a reason no path explains
// (rollupUnexplained); the path keeps its frontier and its own reason,
// followed by the roll-up's. A roll-up explained by the paths caps
// nothing: an old backup that stops at a confirmed gap, an orphan
// timeline or one unknown own path says nothing of a newer backup whose
// own path reaches the frontier (round 7, P1).
func capAtRollup(snapshot *evidencev1alpha1.RepositoryEvidenceSnapshot, rec Recovery, coverage evidencev1alpha1.StateReason, items []BackupItem) Recovery {
	if coverage.State == evidencev1alpha1.Healthy || !slices.ContainsFunc(rec.Paths, func(path RecoveryPath) bool { return path.State == evidencev1alpha1.Healthy }) {
		return rec
	}
	if !rollupUnexplained(snapshot, rec.Paths, coverage.State, items) {
		return rec
	}
	return rollupCap(rec, coverage)
}

// rollupCap makes every healthy path of rec at most a warning, with the
// roll-up's reason after its own (RollupCapCode).
func rollupCap(rec Recovery, coverage evidencev1alpha1.StateReason) Recovery {
	rec.Paths = slices.Clone(rec.Paths)
	for index := range rec.Paths {
		path := &rec.Paths[index]
		if path.State != evidencev1alpha1.Healthy {
			continue
		}
		path.State, path.Reason.Code = evidencev1alpha1.Warning, RollupCapCode
		path.Reason.Message = boundedReason(path.Reason.Message, "the agent's coverage is "+string(coverage.State)+": "+coverage.Reason.Message)
		path.Assumptions = slices.Clone(path.Assumptions)
	}
	return rec
}

// rollupUnexplained reports a coverage roll-up whose degradation is not
// the paths' own, so that it may bear on a path the agent rates healthy:
//
//   - every backup is left behind by the newest timeline (each one's
//     lowest-target path is off target, the agent's
//     NoBackupOnNewestTimeline rule, Barman Cloud and pgBackRest alike): a
//     default restore succeeds from none, whatever a path says;
//   - a pgBackRest stanza split (the WAL under another stanza than the
//     backups, finding StanzaSplit): a restore must name that stanza;
//   - no path is as degraded as the roll-up (an unknown roll-up over
//     healthy and warning paths): the agent knows something the paths do
//     not carry, so the console does not read past it. For pgBackRest the
//     roll-up is the worst over the usable backups' paths only
//     (pgbackrest assess.go coverageAspect), so only a path whose backup
//     the agent rates usable explains it: "no usable backup to recover
//     from" (every backup error:true after a data-checksum failure, say)
//     is explained by none, whatever the unusable backups' paths say
//     (round 8, P2-1). A backup not in the collection read (cut) is not
//     known to be usable.
//
// Otherwise the roll-up is the worst of the paths' own states, and each
// path already says its own.
func rollupUnexplained(snapshot *evidencev1alpha1.RepositoryEvidenceSnapshot, paths []RecoveryPath, rollup evidencev1alpha1.State, items []BackupItem) bool {
	if noBackupOnNewestTimeline(paths) {
		return true
	}
	if d := snapshot.Details.PGBackRest; d != nil && !snapshot.Details.Unknown() {
		if d.ArchiveStanza != "" && d.Stanza != "" && d.ArchiveStanza != d.Stanza {
			return true
		}
		if slices.ContainsFunc(d.Findings, func(f evidencev1alpha1.PGBackRestFinding) bool { return f.Code == evidencev1alpha1.FindingStanzaSplit }) {
			return true
		}
	}
	explains := func(path RecoveryPath) bool { return severity(path.State) >= severity(rollup) }
	if snapshot.Details.PGBackRest != nil {
		usable := map[string]bool{}
		for _, item := range items {
			if item.PgBackRest != nil && item.PgBackRest.Usable {
				usable[item.ID] = true
			}
		}
		explains = func(path RecoveryPath) bool { return usable[path.BackupID] && severity(path.State) >= severity(rollup) }
	}
	return !slices.ContainsFunc(paths, explains)
}

// noBackupOnNewestTimeline reports paths of which every backup's
// lowest-target path is off target: no backup is on the line a default
// restore follows.
func noBackupOnNewestTimeline(paths []RecoveryPath) bool {
	own := map[string]uint64{}
	for _, path := range paths {
		if current, ok := own[path.BackupID]; !ok || path.TargetTimeline < current {
			own[path.BackupID] = path.TargetTimeline
		}
	}
	if len(own) == 0 {
		return false
	}
	behind := map[string]bool{}
	for _, path := range paths {
		if path.TargetTimeline == own[path.BackupID] && (path.OffTarget || path.Reason.Code == evidencev1alpha1.ReasonPathNeedsTargetTimeline) {
			behind[path.BackupID] = true
		}
	}
	return len(behind) == len(own)
}

// severity orders evidence states as the agent's conservative roll-up
// does: unknown is the worst, then unhealthy, warning and healthy. A state
// the console does not know is the worst.
func severity(state evidencev1alpha1.State) int {
	switch state {
	case evidencev1alpha1.Healthy:
		return 0
	case evidencev1alpha1.Warning:
		return 1
	case evidencev1alpha1.Unhealthy:
		return 2
	case evidencev1alpha1.Unknown:
	}
	return 3
}

// snapshotTimeline is the publication's timeline conclusion, unknown when
// its details are missing or of a format the console does not read.
func snapshotTimeline(snapshot *evidencev1alpha1.RepositoryEvidenceSnapshot) evidencev1alpha1.StateReason {
	switch {
	case snapshot.Details.Unknown():
	case snapshot.Details.BarmanCloud != nil:
		return snapshot.Details.BarmanCloud.Timeline
	case snapshot.Details.PGBackRest != nil:
		return snapshot.Details.PGBackRest.Timeline
	}
	return evidencev1alpha1.StateReason{State: evidencev1alpha1.Unknown}
}

// snapshotCoverage is the publication's coverage roll-up, by format.
func snapshotCoverage(snapshot *evidencev1alpha1.RepositoryEvidenceSnapshot) evidencev1alpha1.StateReason {
	switch {
	case snapshot.Details.Unknown():
	case snapshot.Details.BarmanCloud != nil:
		return snapshot.Details.BarmanCloud.Coverage
	case snapshot.Details.PGBackRest != nil:
		return snapshot.Details.PGBackRest.Coverage
	}
	return evidencev1alpha1.StateReason{State: evidencev1alpha1.Unknown}
}

// boundedReason joins two reasons, within the wire bound of a reason
// message, cutting the end at a rune boundary.
func boundedReason(first, second string) string {
	value := first
	switch {
	case value == "":
		value = second
	case second != "":
		value += "; " + second
	}
	if len(value) <= maxCapReasonBytes {
		return value
	}
	cut := maxCapReasonBytes
	for cut > 0 && value[cut]&0xC0 == 0x80 {
		cut--
	}
	return value[:cut]
}
