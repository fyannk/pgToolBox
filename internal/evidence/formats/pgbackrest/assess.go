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

package pgbackrest

import (
	"fmt"
	"maps"
	"slices"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
)

// This file turns a Report into the agent's four-state vocabulary: one state
// per backup and one per aspect (catalog, chains, WAL, timelines, recovery
// coverage). The roll-up is the Barman one (internal/evidence/inventory):
// the worst aspect wins and unknown dominates, so a repository with one
// unusable backup is not healthy even when another backup gives it a
// recovery window. The reader's own Report.State is kept for its golden
// tests; the agent publishes this one.

// UnusableReason is why a backup is not usable. The values are the
// evidence contract's (pkg/evidence/v1alpha1 Unusable*).
type UnusableReason string

// The unusable reasons.
const (
	UnusableChainBroken          UnusableReason = "chain-broken"
	UnusableManifestUnhealthy    UnusableReason = "manifest-unhealthy"
	UnusableManifestUnknown      UnusableReason = "manifest-unknown"
	UnusableManifestNotInspected UnusableReason = "manifest-not-inspected"
	UnusableDependencyManifest   UnusableReason = "dependency-manifest-not-verified"
	UnusableBackupError          UnusableReason = "backup-error"
	UnusableWALToConsistency     UnusableReason = "wal-to-consistency-missing"
	UnusableWALUnknown           UnusableReason = "wal-unknown"
	UnusableWALToTargetTimeline  UnusableReason = "wal-to-target-timeline-missing"
	UnusableOffTargetTimeline    UnusableReason = "off-target-timeline"
)

// Assessment is one backup's state, its reason and, when it is not usable,
// every reason why (sorted).
type Assessment struct {
	State    evidence.State
	Reason   string
	Usable   bool
	Unusable []UnusableReason
}

// Aspect is the state of one evidence aspect of a stanza with its reason.
type Aspect struct {
	State  evidence.State
	Reason string
}

// Aspects is the state of every aspect of a stanza and their roll-up.
type Aspects struct {
	Catalog, Chains, WAL, Timeline, Coverage, Encryption, Retention Aspect
	// Overall is the conservative roll-up of Catalog, Chains, WAL, Timeline
	// and Coverage, and of Retention when an expectation was compared
	// (Encryption is reflected in Catalog: a passphrase that does not open
	// the info files leaves no catalog).
	Overall Aspect
}

// Assess returns the assessment of every backup, in r.Backups order.
func (r Report) Assess() []Assessment {
	byLabel := make(map[string]int, len(r.Backups))
	for index, backup := range r.Backups {
		byLabel[backup.Backup.Label] = index
	}
	chains := newChainIndex(r.Backups, byLabel)
	dependencies := foldDependencies(chains, dependencyManifests{first: -1}, func(acc dependencyManifests, dependency int) dependencyManifests {
		var state evidence.State
		switch r.Backups[dependency].Manifest.State {
		case evidence.Healthy:
			return acc
		case evidence.Unhealthy:
			state = evidence.Unhealthy
		default:
			state = evidence.Unknown
		}
		acc.worst = worse(acc.worst, state)
		if acc.first < 0 || dependency < acc.first {
			acc.first = dependency
		}
		return acc
	})
	result := make([]Assessment, len(r.Backups))
	for index, backup := range r.Backups {
		result[index] = assess(r.Backups, dependencies[index], backup)
	}
	return result
}

// dependencyManifests is what the manifests of a backup's dependencies
// say: the worst state of those not verified ("" when all are), and the
// first of them in backup order (-1 when none).
type dependencyManifests struct {
	worst evidence.State
	first int
}

func assess(reports []BackupReport, dependencies dependencyManifests, backup BackupReport) Assessment {
	causes := make(map[UnusableReason]evidence.State)
	message := ""
	add := func(reason UnusableReason, state evidence.State, text string) {
		if _, seen := causes[reason]; !seen && message == "" {
			message = text
		}
		causes[reason] = worse(causes[reason], state)
	}
	if backup.ChainState != evidence.Healthy {
		add(UnusableChainBroken, evidence.Unhealthy, backup.ChainReason)
	}
	switch {
	case !backup.Manifest.Inspected:
		add(UnusableManifestNotInspected, evidence.Unknown, backup.Manifest.Reason)
	case backup.Manifest.State == evidence.Unhealthy:
		add(UnusableManifestUnhealthy, evidence.Unhealthy, backup.Manifest.Reason)
	case backup.Manifest.State != evidence.Healthy:
		add(UnusableManifestUnknown, evidence.Unknown, backup.Manifest.Reason)
	}
	if first := dependencies.first; first >= 0 {
		// The message names the first dependency not verified, in backup
		// order; the state is the worst of them.
		text := "the manifest of " + reports[first].Backup.Label + " was not verified"
		if reports[first].Manifest.State == evidence.Unhealthy {
			text = "the manifest of " + reports[first].Backup.Label + " does not verify"
		}
		add(UnusableDependencyManifest, dependencies.worst, text)
	}
	if backup.Backup.Error != nil && *backup.Backup.Error {
		add(UnusableBackupError, evidence.Unhealthy, "pgBackRest recorded errors (page checksums) in this backup")
	}
	path := backup.Path
	switch {
	case path.State == evidence.Unknown || path.TargetTimeline == 0:
		add(UnusableWALUnknown, evidence.Unknown, path.Reason)
	case !path.Consistent:
		add(UnusableWALToConsistency, evidence.Unhealthy, path.Reason)
	case path.OffTarget:
		add(UnusableOffTargetTimeline, evidence.Warning, path.Reason)
	case path.FrontierTimeline != path.TargetTimeline:
		add(UnusableWALToTargetTimeline, stateOr(path.State, evidence.Unhealthy), path.Reason)
	}
	if len(causes) == 0 {
		if !backup.Usable {
			// The reader's usability is the authority; a cause this file
			// does not name still keeps the backup unusable.
			return Assessment{State: evidence.Unknown, Reason: "the backup is not proven usable", Unusable: []UnusableReason{UnusableWALUnknown}}
		}
		return Assessment{State: evidence.Healthy, Reason: "chain, manifests and WAL to consistency are present and verified", Usable: true, Unusable: []UnusableReason{}}
	}
	// One backup: a proven failure says more than an unknown beside it.
	state := evidence.Warning
	for _, cause := range causes {
		switch {
		case cause == evidence.Unhealthy:
			state = evidence.Unhealthy
		case cause == evidence.Unknown && state != evidence.Unhealthy:
			state = evidence.Unknown
		}
	}
	reasons := slices.Sorted(maps.Keys(causes))
	return Assessment{State: state, Reason: boundedMessage(message, "the backup is not usable"), Unusable: reasons}
}

// Aspects rolls the report up. A StanzaSplit finding (added by the agent)
// makes coverage at most a warning.
func (r Report) Aspects() Aspects {
	assessments := r.Assess()
	result := Aspects{
		Catalog: r.catalogAspect(assessments), Chains: r.chainAspect(), WAL: r.walAspect(),
		Timeline: r.timelineAspect(), Coverage: r.coverageAspect(), Encryption: r.encryptionAspect(),
		Retention: r.retentionAspect(),
	}
	rolled := []Aspect{result.Catalog, result.Chains, result.WAL, result.Timeline, result.Coverage}
	if r.Retention != nil {
		rolled = append(rolled, result.Retention)
	}
	overall := evidence.Healthy
	for _, aspect := range rolled {
		overall = Conservative(overall, aspect.State)
	}
	result.Overall = Aspect{State: overall, Reason: overallReason(overall, r.Retention != nil)}
	return result
}

func overallReason(state evidence.State, retention bool) string {
	switch {
	case state == evidence.Healthy && retention:
		return "pgBackRest catalog, chains, WAL, timelines, recovery coverage and retention evaluated"
	case state == evidence.Healthy:
		return "pgBackRest catalog, chains, WAL, timelines and recovery coverage evaluated"
	case retention:
		return "pgBackRest catalog, chain, WAL, timeline, recovery or retention evidence is not healthy"
	default:
		return "pgBackRest catalog, chain, WAL, timeline or recovery evidence is not healthy"
	}
}

func (r Report) infoReadable() bool { return r.Database != nil }

func (r Report) infoUnreadableReason() string {
	if r.Reason != "" {
		return r.Reason
	}
	return "backup.info or archive.info cannot be used"
}

func (r Report) catalogAspect(assessments []Assessment) Aspect {
	if !r.infoReadable() {
		return Aspect{evidence.Unknown, r.infoUnreadableReason()}
	}
	if len(r.Backups) == 0 {
		return Aspect{evidence.Unhealthy, "backup.info lists no backup"}
	}
	state, notInspected := evidence.Healthy, 0
	for index, assessment := range assessments {
		if !r.Backups[index].Manifest.Inspected && assessment.State != evidence.Unhealthy {
			// Older than the newest DefaultMaxManifests backups and not a
			// dependency of one: not read, so not judged.
			notInspected++
			continue
		}
		state = Conservative(state, assessment.State)
	}
	for _, finding := range r.Findings {
		if finding.Code == FindingCopyMismatch || finding.Code == FindingChecksumMismatch || finding.Code == FindingManifestBudgetExceeded {
			state = Conservative(state, evidence.Warning)
		}
	}
	reason := map[evidence.State]string{
		evidence.Healthy:   "every backup is usable",
		evidence.Warning:   "a backup or an info file needs attention",
		evidence.Unhealthy: "a backup is not usable",
		evidence.Unknown:   "a backup's evidence is unknown",
	}[state]
	if notInspected > 0 {
		reason += fmt.Sprintf("; %d older backup(s) not read", notInspected)
	}
	return Aspect{state, reason}
}

func (r Report) chainAspect() Aspect {
	if !r.infoReadable() {
		return Aspect{evidence.Unknown, r.infoUnreadableReason()}
	}
	for _, backup := range r.Backups {
		if backup.ChainState != evidence.Healthy {
			return Aspect{evidence.Unhealthy, "a backup's prior or references are missing or inconsistent"}
		}
	}
	return Aspect{evidence.Healthy, "every prior and reference is present and consistent"}
}

func (r Report) walAspect() Aspect {
	switch {
	case !r.infoReadable():
		return Aspect{evidence.Unknown, r.infoUnreadableReason()}
	case r.SegmentSize == 0:
		return Aspect{evidence.Unknown, "WAL segment size cannot be established"}
	case len(r.WAL) == 0:
		return Aspect{evidence.Unknown, "archive.info lists no archive id"}
	}
	state, reason := evidence.Healthy, "WAL continuity evaluated"
	for _, wal := range r.WAL {
		if wal.RangesTruncated || wal.DiagnosticsTruncated {
			return Aspect{evidence.Unknown, "too many WAL ranges or anomalies to evaluate"}
		}
		state = Conservative(state, wal.State)
		if wal.State == evidence.Unknown {
			reason = boundedMessage("WAL continuity is unknown: "+wal.Reason, "WAL continuity is unknown")
		}
	}
	switch state {
	case evidence.Unhealthy:
		reason = "confirmed WAL gaps observed"
	case evidence.Warning:
		reason = "candidate WAL gaps observed"
	case evidence.Healthy, evidence.Unknown:
		// The reason above stands.
	}
	return Aspect{state, reason}
}

func (r Report) timelineAspect() Aspect {
	if !r.infoReadable() || r.SegmentSize == 0 {
		return Aspect{evidence.Unknown, "timeline histories cannot be evaluated without the info files and the WAL segment size"}
	}
	state := evidence.Healthy
	known := make(map[string]bool)
	for _, history := range r.Timelines {
		state = Conservative(state, history.State)
		if history.State == evidence.Healthy {
			known[fmt.Sprintf("%s/%d", history.ArchiveID, history.Timeline)] = true
		}
	}
	for _, wal := range r.WAL {
		for _, value := range wal.Ranges {
			if value.Timeline > 1 && !known[fmt.Sprintf("%s/%d", wal.ArchiveID, value.Timeline)] {
				state = evidence.Unknown
			}
		}
	}
	if state != evidence.Healthy {
		return Aspect{state, "a timeline history file is missing or unreadable"}
	}
	return Aspect{evidence.Healthy, "timeline history evaluated"}
}

func (r Report) coverageAspect() Aspect {
	if r.Window == nil {
		return Aspect{evidence.Unknown, "no usable backup to recover from"}
	}
	state := evidence.Healthy
	for _, backup := range r.Backups {
		if backup.Usable {
			state = Conservative(state, backup.Path.State)
		}
	}
	reason := map[evidence.State]string{
		evidence.Healthy:   "every usable backup reaches the newest archived WAL",
		evidence.Warning:   "a recovery path stops at a candidate WAL gap",
		evidence.Unhealthy: "a recovery path stops at a confirmed WAL gap",
		evidence.Unknown:   "a recovery path is unknown",
	}[state]
	if slices.ContainsFunc(r.Findings, func(finding Finding) bool { return finding.Code == FindingStanzaSplit }) {
		state = Conservative(state, evidence.Warning)
		reason = "the WAL is under another stanza: a restore must name it"
	}
	return Aspect{state, reason}
}

func (r Report) encryptionAspect() Aspect {
	for _, pair := range []PairCheck{r.BackupInfo, r.ArchiveInfo} {
		for _, check := range []FileCheck{pair.Main, pair.Copy} {
			switch check.Problem {
			case ProblemEncrypted:
				return Aspect{evidence.Unknown, "the repository is encrypted and no passphrase is configured"}
			case ProblemDecryption:
				if !pair.Usable() {
					return Aspect{evidence.Unknown, "the configured passphrase does not open the info files"}
				}
			case ProblemNone, ProblemMissing, ProblemUnreadable, ProblemTooLarge, ProblemChecksum, ProblemMalformed, ProblemFormatUnsupported:
				// Not about encryption: the catalog aspect reports these.
			}
		}
	}
	switch {
	case !r.infoReadable():
		return Aspect{evidence.Unknown, r.infoUnreadableReason()}
	case r.Encrypted:
		return Aspect{evidence.Healthy, "encrypted info files and manifests opened"}
	default:
		return Aspect{evidence.Healthy, "the repository is not encrypted"}
	}
}

// RepositoryFormat is the backrest-format the info files declare, 0 when
// none was read.
func (r Report) RepositoryFormat() int {
	format := 0
	for _, check := range []FileCheck{r.BackupInfo.Main, r.BackupInfo.Copy, r.ArchiveInfo.Main, r.ArchiveInfo.Copy} {
		format = max(format, check.Format)
	}
	return format
}

// StanzaSplitFinding is the finding the agent adds when the backups and the
// WAL of its repository are under different stanzas.
func StanzaSplitFinding(stanza, archiveStanza string) Finding {
	return Finding{
		Code: FindingStanzaSplit, Severity: SeverityWarning, Subject: "archive/" + archiveStanza + "/",
		Message: "the WAL is under stanza " + archiveStanza + ", not " + stanza + "; a restore must name it",
	}
}

// AddFinding inserts a finding in the report's sorted, unique order.
func (r *Report) AddFinding(finding Finding) {
	set := &findingSet{}
	for _, existing := range r.Findings {
		set.add(existing.Code, existing.Severity, existing.Subject, existing.Message)
	}
	set.add(finding.Code, finding.Severity, finding.Subject, finding.Message)
	r.Findings = set.sorted()
}

// Conservative is the roll-up of two states: unknown, then unhealthy, then
// warning dominate.
func Conservative(left, right evidence.State) evidence.State {
	for _, state := range []evidence.State{evidence.Unknown, evidence.Unhealthy, evidence.Warning} {
		if left == state || right == state {
			return state
		}
	}
	return evidence.Healthy
}

func worse(left, right evidence.State) evidence.State {
	if left == "" {
		return right
	}
	return Conservative(left, right)
}

func stateOr(value, fallback evidence.State) evidence.State {
	if value == "" || value == evidence.Healthy {
		return fallback
	}
	return value
}

func boundedMessage(value, fallback string) string {
	if value == "" {
		return fallback
	}
	if len(value) > 256 {
		cut := 253
		for cut > 0 && (value[cut]&0xC0) == 0x80 {
			cut--
		}
		return value[:cut] + "..."
	}
	return value
}

// CloneReport returns a deep copy of report.
func CloneReport(report Report) Report {
	if report.Database != nil {
		database := *report.Database
		report.Database = &database
	}
	report.ArchiveIDs = slices.Clone(report.ArchiveIDs)
	report.Backups = slices.Clone(report.Backups)
	for index := range report.Backups {
		backup := &report.Backups[index].Backup
		backup.Reference = slices.Clone(backup.Reference)
		backup.Annotations = maps.Clone(backup.Annotations)
		backup.Error = clonePointer(backup.Error)
		backup.RepoSizeMap = clonePointer(backup.RepoSizeMap)
		backup.RepoSizeMapDelta = clonePointer(backup.RepoSizeMapDelta)
		report.Backups[index].Path.Timelines = slices.Clone(report.Backups[index].Path.Timelines)
	}
	report.WAL = slices.Clone(report.WAL)
	for index := range report.WAL {
		report.WAL[index].Ranges = slices.Clone(report.WAL[index].Ranges)
		report.WAL[index].Gaps = slices.Clone(report.WAL[index].Gaps)
		report.WAL[index].Diagnostics = slices.Clone(report.WAL[index].Diagnostics)
	}
	report.Timelines = slices.Clone(report.Timelines)
	for index := range report.Timelines {
		report.Timelines[index].Edges = slices.Clone(report.Timelines[index].Edges)
	}
	if report.Window != nil {
		window := *report.Window
		window.Assumptions = slices.Clone(window.Assumptions)
		report.Window = &window
	}
	report.Findings = slices.Clone(report.Findings)
	if report.Retention != nil {
		retention := *report.Retention
		retention.Lost = slices.Clone(retention.Lost)
		retention.Findings = slices.Clone(retention.Findings)
		report.Retention = &retention
	}
	return report
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}
