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
	rollupCode               = "coverage-path-warning"
	maxCapReasonBytes        = 256
)

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
//   - no path claims more than the agent's own coverage roll-up: when that
//     is not healthy (the agent says a default restore succeeds from no
//     backup, or a path stops at a gap), a healthy path is at most a
//     warning. The roll-up is conservative over every backup, so a path is
//     not made unknown by another backup's unknown path.
//
// A capped Recovery is RecoveryUncertain with the reason; each path is at
// most unknown (a proven loss stays unhealthy), keeps its own reason after
// the cap's, and loses its frontier receipt, which is what a recovery bar
// draws a covered span to.
func capRecovery(snapshot *evidencev1alpha1.RepositoryEvidenceSnapshot, rec Recovery) Recovery {
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
		return capAtRollup(rec, coverage)
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
// coverage roll-up is not healthy; the path keeps its frontier and its own
// reason, followed by the roll-up's.
func capAtRollup(rec Recovery, coverage evidencev1alpha1.StateReason) Recovery {
	if coverage.State == evidencev1alpha1.Healthy || !slices.ContainsFunc(rec.Paths, func(path RecoveryPath) bool { return path.State == evidencev1alpha1.Healthy }) {
		return rec
	}
	rec.Paths = slices.Clone(rec.Paths)
	for index := range rec.Paths {
		path := &rec.Paths[index]
		if path.State != evidencev1alpha1.Healthy {
			continue
		}
		path.State, path.Reason.Code = evidencev1alpha1.Warning, rollupCode
		path.Reason.Message = boundedReason(path.Reason.Message, "the agent's coverage is "+string(coverage.State)+": "+coverage.Reason.Message)
		path.Assumptions = slices.Clone(path.Assumptions)
	}
	return rec
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
