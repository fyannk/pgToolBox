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

package evidenceapi

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/pgbackrest"
	"github.com/fyannk/pgToolBox/internal/evidence/inventory"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// PGBackRestPublication is a pgBackRest repository's publication. Its
// snapshot carries producer epoch 0, which the agent handler stamps.
type PGBackRestPublication struct {
	Snapshot      evidencev1alpha1.RepositoryEvidenceSnapshot
	Backups       []evidencev1alpha1.PGBackRestBackup
	WALRanges     []evidencev1alpha1.PGBackRestWALRange
	WALGaps       []evidencev1alpha1.PGBackRestWALGap
	RecoveryPaths []evidencev1alpha1.PGBackRestRecoveryPath
}

// validationEpoch stands in for the producer epoch while a publication is
// validated before the handler stamps the real one (always at least 1).
const validationEpoch = 1

// The assumptions the reader writes in prose, as contract codes.
var pgbackrestAssumptionCodes = map[string]string{
	"the window ends at the end of the last contiguous segment; its time is the segment's upload time, not a commit time": "window-end-at-segment-upload-time",
	"the WAL segment size is assumed to be the 16 MiB default: the backups' LSNs do not decide it":                        "wal-segment-size-assumed-default",
}

// pathAssumptionCodes are what every pgBackRest recovery path assumes: it
// is judged from segment names, never from WAL bytes or a restore.
var pathAssumptionCodes = []string{"segment-name-presence-only", "wal-bytes-and-restore-not-verified"}

func projectPGBackRest(source inventory.Snapshot, options Options, canonical evidencev1alpha1.RepositoryFingerprintInput) (Publication, error) {
	stanza := canonical.ScopeName
	archive := options.PGBackRestArchiveStanza
	if archive == "" {
		archive = stanza
	}
	if source.Evidence.Scope.Kind != pgbackrest.ScopeKind {
		return Publication{}, errors.New("project evidence publication: incompatible repository identity")
	}
	if source.TotalsKnown {
		names := make([]string, 0, len(source.Scopes))
		for _, scope := range source.Scopes {
			if scope.Name != stanza && scope.Name != archive {
				return Publication{}, errors.New("project evidence publication: snapshot is not confined to the configured stanzas")
			}
			names = append(names, scope.Name)
		}
		if !slices.Contains(names, stanza) {
			return Publication{}, errors.New("project evidence publication: snapshot is not confined to the configured stanzas")
		}
	}
	if len(source.BarmanCatalog.Backups) != 0 || len(source.BarmanWAL.Servers) != 0 || len(source.BarmanRecovery.Servers) != 0 || !source.TotalsKnown && len(source.PGBackRest) != 0 || len(source.PGBackRest) > 1 {
		return Publication{}, errors.New("project evidence publication: the source carries evidence of another profile")
	}
	report := pgbackrest.Report{Stanza: stanza, ArchiveStanza: archive, State: evidence.Unknown, Reason: "no completed scan"}
	if len(source.PGBackRest) == 1 {
		report = source.PGBackRest[0]
		if report.Stanza != stanza || report.ArchiveStanza != archive {
			return Publication{}, errors.New("project evidence publication: cross-scope pgBackRest evidence")
		}
	}
	fingerprint, err := evidencev1alpha1.FingerprintRepository(canonical)
	if err != nil {
		return Publication{}, errors.New("project evidence publication: invalid repository fingerprint")
	}
	native := &PGBackRestPublication{}
	assessments := report.Assess()
	native.Backups = projectPGBackRestBackups(report, assessments)
	native.WALRanges, native.WALGaps = projectPGBackRestWAL(report)
	native.RecoveryPaths = projectPGBackRestPaths(report)
	native.Snapshot = projectPGBackRestSnapshot(source, options, canonical, fingerprint, report, native)
	publication := Publication{PGBackRest: native}
	if err := publication.Validate(); err != nil {
		return Publication{}, fmt.Errorf("project evidence publication: %w", err)
	}
	return publication, nil
}

func projectPGBackRestSnapshot(source inventory.Snapshot, options Options, canonical evidencev1alpha1.RepositoryFingerprintInput, fingerprint string, report pgbackrest.Report, publication *PGBackRestPublication) evidencev1alpha1.RepositoryEvidenceSnapshot {
	capabilities := projectCapabilities(source.Evidence.Capabilities)
	inventorySummary := projectInventory(source)
	evidenceGeneration := uint64(0)
	var startedAt, completedAt *time.Time
	if source.TotalsKnown {
		evidenceGeneration = source.Evidence.Generation
		startedAt = timePointer(source.Evidence.StartedAt)
		completedAt = timePointer(source.Evidence.CompletedAt)
	}
	state := evidencev1alpha1.State(source.Evidence.State)
	topReasonCode := "snapshot-" + string(state)
	if source.RefreshFailure != "" {
		topReasonCode = "refresh-" + strings.ReplaceAll(string(source.RefreshFailure), "_", "-")
	}
	return evidencev1alpha1.RepositoryEvidenceSnapshot{
		APIVersion:       evidencev1alpha1.APIVersion,
		Kind:             evidencev1alpha1.SnapshotKind,
		Producer:         evidencev1alpha1.Producer{Name: evidencev1alpha1.ProducerName, Version: options.ProducerVersion, Features: evidencev1alpha1.ProducerFeatures()},
		ProducerInstance: options.ProducerInstance,
		Identity: evidencev1alpha1.Identity{
			Cluster: evidencev1alpha1.ClusterIdentity{Namespace: options.ClusterNamespace, UID: options.ClusterUID, Name: cloneString(options.ClusterName)},
			Repository: evidencev1alpha1.RepositoryIdentity{
				Provider: repositoryProvider(options), Format: canonical.Format, DestinationFingerprint: fingerprint,
				Scope: evidencev1alpha1.ScopeIdentity{Kind: canonical.ScopeKind, Name: canonical.ScopeName},
			},
		},
		Revision: source.RefreshGeneration, EvidenceGeneration: evidenceGeneration,
		StartedAt: startedAt, CompletedAt: completedAt, LastAttemptAt: timePointer(source.LastAttemptAt),
		Completeness: evidencev1alpha1.Completeness(source.Evidence.Completeness), Stale: source.Evidence.Stale || source.RefreshFailure != "",
		State: state, Reason: typedReason(topReasonCode, source.Evidence.Reason),
		Capabilities: capabilities, Inventory: inventorySummary,
		Details: evidencev1alpha1.Details{
			Type:       evidencev1alpha1.PGBackRestDetailsType,
			PGBackRest: projectPGBackRestSummary(source.TotalsKnown, options, report, publication),
		},
	}
}

func projectPGBackRestSummary(complete bool, options Options, report pgbackrest.Report, publication *PGBackRestPublication) *evidencev1alpha1.PGBackRestSummary {
	aspects := report.Aspects()
	repository := options.PGBackRestRepository
	if repository == 0 {
		repository = 1
	}
	summary := &evidencev1alpha1.PGBackRestSummary{
		Stanza: report.Stanza, ArchiveStanza: report.ArchiveStanza, Repository: repository,
		ArchiveIDs: slices.Clone(report.ArchiveIDs),
		Catalog:    aspectReason(aspects.Catalog, "catalog"),
		WAL:        aspectReason(aspects.WAL, "wal"),
		Timeline:   aspectReason(aspects.Timeline, "timeline"),
		Coverage:   aspectReason(aspects.Coverage, "coverage"),
		Findings:   []evidencev1alpha1.PGBackRestFinding{},
	}
	if summary.ArchiveIDs == nil {
		summary.ArchiveIDs = []string{}
	}
	if format := report.RepositoryFormat(); format > 0 {
		// #nosec G115 -- a parsed, positive repository format number.
		summary.RepositoryFormat = uint64Pointer(uint64(format))
	}
	if report.BackupInfo.Main.Present || report.BackupInfo.Copy.Present {
		encrypted := report.Encrypted
		summary.Encrypted = &encrypted
	}
	if count := len(report.Backups); count > 0 {
		summary.PGBackRestVersion = stringPointer(report.Backups[count-1].Backup.BackrestVersion)
	}
	if report.Database != nil {
		summary.Database = &evidencev1alpha1.PGBackRestDatabase{
			DBID: uint64(report.Database.ID), SystemID: strconv.FormatUint(report.Database.SystemID, 10),
			Version: report.Database.Version, PostgreSQLVersion: positiveIntPointer(report.Database.VersionNumber),
		}
	}
	if report.SegmentSize > 0 {
		summary.WALSegmentSizeBytes = positiveInt64Pointer(report.SegmentSize)
		source := string(report.SegmentSizeSource)
		summary.WALSegmentSizeSource = &source
	}
	var counts evidencev1alpha1.PGBackRestWALCounts
	for _, wal := range report.WAL {
		// #nosec G115 -- WAL class counts are non-negative tallies.
		counts.Segment, counts.Partial, counts.History = counts.Segment+uint64(wal.Counts.Segments), counts.Partial+uint64(wal.Counts.Partials), counts.History+uint64(wal.Counts.History)
		// #nosec G115 -- WAL class counts are non-negative tallies.
		counts.BackupHistory, counts.Unknown, counts.Duplicate = counts.BackupHistory+uint64(wal.Counts.BackupHistory), counts.Unknown+uint64(wal.Counts.Unknown), counts.Duplicate+uint64(wal.Counts.Duplicates)
		summary.RangesTruncated = summary.RangesTruncated || wal.RangesTruncated
		summary.DiagnosticsTruncated = summary.DiagnosticsTruncated || wal.DiagnosticsTruncated
		if latest := timePointer(wal.LatestArchiveReceipt); latest != nil && (summary.LatestArchiveReceiptAt == nil || latest.After(*summary.LatestArchiveReceiptAt)) {
			summary.LatestArchiveReceiptAt = latest
		}
	}
	if (summary.RangesTruncated || summary.DiagnosticsTruncated) && summary.WAL.State != evidencev1alpha1.Unknown {
		summary.WAL = evidencev1alpha1.StateReason{State: evidencev1alpha1.Unknown, Reason: typedReason("wal-unknown", "the WAL listing exceeded its bounds")}
	}
	if window := report.Window; window != nil {
		summary.RecoveryWindow = &evidencev1alpha1.PGBackRestRecoveryWindow{
			OldestBackup: window.OldestBackup, StartAt: window.Start.UTC(), NewestBackup: window.NewestBackup, NewestStopAt: window.NewestStop.UTC(),
			EndTimeline: uint64(window.EndTimeline), EndWAL: window.EndWAL, EndLSN: window.EndLSN, EndReceiptAt: timePointer(window.EndReceipt),
			// #nosec G115 -- a count of backups.
			UsableBackups: uint64(window.UsableBackups), AssumptionCodes: mappedCodes(window.Assumptions, pgbackrestAssumptionCodes),
		}
	}
	for _, finding := range report.Findings {
		summary.Findings = append(summary.Findings, evidencev1alpha1.PGBackRestFinding{
			Code: string(finding.Code), Severity: string(finding.Severity), Subject: finding.Subject, Message: finding.Message,
		})
	}
	summary.Retention = projectPGBackRestRetention(report.Retention)
	slices.SortFunc(summary.Findings, compareFindings)
	summary.Findings = slices.CompactFunc(summary.Findings, func(left, right evidencev1alpha1.PGBackRestFinding) bool { return compareFindings(left, right) == 0 })
	summary.Findings, summary.FindingsTruncated = capFindings(summary.Findings, maxPublishedFindings)
	if complete {
		summary.BackupItems = countPointer(len(publication.Backups))
		summary.WALRangeItems = countPointer(len(publication.WALRanges))
		summary.WALGapItems = countPointer(len(publication.WALGaps))
		summary.RecoveryPathItems = countPointer(len(publication.RecoveryPaths))
		var states evidencev1alpha1.StateCounts
		var usable uint64
		for _, backup := range publication.Backups {
			switch backup.State {
			case evidencev1alpha1.Healthy:
				states.Healthy++
			case evidencev1alpha1.Warning:
				states.Warning++
			case evidencev1alpha1.Unhealthy:
				states.Unhealthy++
			default:
				states.Unknown++
			}
			if backup.Usable {
				usable++
			}
		}
		summary.BackupStates, summary.UsableBackups, summary.WALCounts = &states, &usable, &counts
	}
	return summary
}

// projectPGBackRestRetention is the retention block; nil without an
// expectation.
func projectPGBackRestRetention(value *pgbackrest.RetentionReport) *evidencev1alpha1.PGBackRestRetention {
	if value == nil {
		return nil
	}
	want := value.Expectation
	count := func(number int) *uint64 {
		if number <= 0 {
			return nil
		}
		// #nosec G115 -- a positive retention count, at most 9999999.
		return uint64Pointer(uint64(number))
	}
	result := &evidencev1alpha1.PGBackRestRetention{
		Configured: want.Full > 0, Full: count(want.Full), FullType: string(want.FullType),
		Diff: count(want.Diff), Archive: count(want.Archive), ArchiveType: string(want.ArchiveType),
		EffectiveArchive: count(value.ArchiveRetention),
		// #nosec G115 -- non-negative backup counts.
		FullBackups: uint64(value.FullBackups), DifferentialBackups: uint64(value.DifferentialBackups), IncrementalBackups: uint64(value.IncrementalBackups),
		EvaluatedAt: timePointer(value.EvaluatedAt), WindowStartAt: timePointer(value.WindowStart),
		State:  evidencev1alpha1.State(stateOrUnknown(value.State)),
		Reason: typedReason("retention-"+string(stateOrUnknown(value.State)), messageOr(value.Reason, "retention evaluated")),
	}
	return result
}

func stateOrUnknown(state evidence.State) evidence.State {
	if state == "" {
		return evidence.Unknown
	}
	return state
}

// maxPublishedFindings is the contract's bound on a summary's findings.
const maxPublishedFindings = 200

// capFindings bounds sorted, unique findings to limit, keeping the list
// sorted. A cut does not take the head of the sorted list: hundreds of
// findings of one code (ManifestMissing on a crafted chain) would push out
// the one finding that says why the scan stopped (ManifestBudgetExceeded),
// or any other code met once. So one finding of every code is kept first
// (its most severe, then the first in order), errors before warnings when
// even that does not fit, and the remaining room goes to the other
// findings, errors first, in order.
func capFindings(findings []evidencev1alpha1.PGBackRestFinding, limit int) ([]evidencev1alpha1.PGBackRestFinding, bool) {
	if len(findings) <= limit {
		return findings, false
	}
	rank := func(severity string) int {
		switch pgbackrest.Severity(severity) {
		case pgbackrest.SeverityError:
			return 0
		case pgbackrest.SeverityWarning:
			return 1
		default:
			return 2
		}
	}
	// priority orders by severity, then by the sorted position.
	priority := make([]int, len(findings))
	for index := range priority {
		priority[index] = index
	}
	slices.SortStableFunc(priority, func(left, right int) int { return rank(findings[left].Severity) - rank(findings[right].Severity) })
	kept := make([]bool, len(findings))
	codes := make(map[string]bool)
	room := limit
	for _, index := range priority {
		if room > 0 && !codes[findings[index].Code] {
			codes[findings[index].Code], kept[index] = true, true
			room--
		}
	}
	for _, index := range priority {
		if room > 0 && !kept[index] {
			kept[index] = true
			room--
		}
	}
	out := make([]evidencev1alpha1.PGBackRestFinding, 0, limit)
	for index, finding := range findings {
		if kept[index] {
			out = append(out, finding)
		}
	}
	return out, true
}

func compareFindings(left, right evidencev1alpha1.PGBackRestFinding) int {
	for _, pair := range [][2]string{{left.Code, right.Code}, {left.Subject, right.Subject}, {left.Message, right.Message}, {left.Severity, right.Severity}} {
		if result := strings.Compare(pair[0], pair[1]); result != 0 {
			return result
		}
	}
	return 0
}

func projectPGBackRestBackups(report pgbackrest.Report, assessments []pgbackrest.Assessment) []evidencev1alpha1.PGBackRestBackup {
	result := make([]evidencev1alpha1.PGBackRestBackup, 0, len(report.Backups))
	for index, value := range report.Backups {
		backup, assessment := value.Backup, assessments[index]
		item := evidencev1alpha1.PGBackRestBackup{
			Stanza: report.Stanza, Label: backup.Label, Type: string(backup.Type), Prior: stringPointer(backup.Prior),
			References: slices.Clone(backup.Reference),
			State:      evidencev1alpha1.State(assessment.State), Reason: typedReason("backup-"+string(assessment.State), messageOr(assessment.Reason, "backup evaluated")),
			Usable: assessment.Usable, UnusableReasons: make([]string, 0, len(assessment.Unusable)),
			ArchiveID: stringPointer(value.ArchiveID), Timeline: walTimeline(backup.ArchiveStart),
			StartAt: timePointer(backup.TimestampStart), StopAt: timePointer(backup.TimestampStop),
			StartLSN: stringPointer(backup.LSNStart), StopLSN: stringPointer(backup.LSNStop),
			ArchiveStart: stringPointer(backup.ArchiveStart), ArchiveStop: stringPointer(backup.ArchiveStop),
			SizeBytes: backup.Size, DeltaBytes: backup.SizeDelta, RepositorySizeBytes: backup.RepoSize, RepositoryDeltaBytes: backup.RepoSizeDelta,
			Error: clonePointer(backup.Error), PGBackRestVersion: stringPointer(backup.BackrestVersion),
			Annotations: make([]evidencev1alpha1.PGBackRestAnnotation, 0, len(backup.Annotations)),
			// #nosec G115 -- a non-negative count of withheld annotations.
			AnnotationsWithheld: uint64(max(backup.AnnotationsWithheld, 0)),
		}
		if item.References == nil {
			item.References = []string{}
		}
		slices.Sort(item.References)
		for _, reason := range assessment.Unusable {
			item.UnusableReasons = append(item.UnusableReasons, string(reason))
		}
		if report.Database != nil && backup.DBID == report.Database.ID {
			item.SystemID = stringPointer(strconv.FormatUint(report.Database.SystemID, 10))
			item.PostgreSQLVersion = positiveIntPointer(report.Database.VersionNumber)
		}
		for key, annotation := range backup.Annotations {
			item.Annotations = append(item.Annotations, evidencev1alpha1.PGBackRestAnnotation{Key: key, Value: annotation})
		}
		slices.SortFunc(item.Annotations, func(left, right evidencev1alpha1.PGBackRestAnnotation) int {
			return strings.Compare(left.Key, right.Key)
		})
		item.CNPGBackupName = stringPointer(backup.Annotations["cnpg-backup-name"])
		result = append(result, item)
	}
	slices.SortFunc(result, evidencev1alpha1.ComparePGBackRestBackups)
	return result
}

func projectPGBackRestWAL(report pgbackrest.Report) ([]evidencev1alpha1.PGBackRestWALRange, []evidencev1alpha1.PGBackRestWALGap) {
	ranges := []evidencev1alpha1.PGBackRestWALRange{}
	gaps := []evidencev1alpha1.PGBackRestWALGap{}
	for _, wal := range report.WAL {
		for _, value := range wal.Ranges {
			ranges = append(ranges, evidencev1alpha1.PGBackRestWALRange{
				Stanza: report.Stanza, ArchiveID: wal.ArchiveID, Timeline: uint64(value.Timeline), StartPosition: value.Start, EndPosition: value.End,
				SegmentCount: value.Count, FirstWAL: value.First, LastWAL: value.Last,
				LatestReceiptAt: timePointer(value.LatestReceipt), EndReceiptAt: timePointer(value.EndReceipt),
			})
		}
		for _, value := range wal.Gaps {
			gaps = append(gaps, evidencev1alpha1.PGBackRestWALGap{
				Stanza: report.Stanza, ArchiveID: wal.ArchiveID, Timeline: uint64(value.Timeline), StartPosition: value.Start, EndPosition: value.End,
				SegmentCount: value.Count, FirstWAL: value.First, LastWAL: value.Last, Status: evidencev1alpha1.GapStatus(value.Status),
				FirstObservedGeneration: value.FirstObservedGeneration, LastObservedGeneration: value.LastObservedGeneration,
			})
		}
	}
	slices.SortFunc(ranges, evidencev1alpha1.ComparePGBackRestWALRanges)
	slices.SortFunc(gaps, evidencev1alpha1.ComparePGBackRestWALGaps)
	return ranges, gaps
}

func projectPGBackRestPaths(report pgbackrest.Report) []evidencev1alpha1.PGBackRestRecoveryPath {
	result := []evidencev1alpha1.PGBackRestRecoveryPath{}
	for _, backup := range report.Backups {
		path := backup.Path
		if path.TargetTimeline == 0 {
			continue
		}
		item := evidencev1alpha1.PGBackRestRecoveryPath{
			Stanza: report.Stanza, Label: backup.Backup.Label, ArchiveID: stringPointer(backup.ArchiveID), TargetTimeline: uint64(path.TargetTimeline),
			State: evidencev1alpha1.State(path.State), Reason: typedReason("coverage-path-"+string(path.State), messageOr(path.Reason, "recovery path evaluated")),
			Stop: evidencev1alpha1.CoverageStop(path.Stop), Timelines: make([]uint64, 0, len(path.Timelines)),
			StartWAL: stringPointer(path.StartWAL), ConsistentWAL: stringPointer(path.ConsistentWAL), Consistent: path.Consistent, OffTarget: path.OffTarget,
			FrontierTimeline: positiveUint32Pointer(path.FrontierTimeline), FrontierPosition: positionPointer(path.FrontierTimeline, path.FrontierPosition),
			FrontierWAL: stringPointer(path.FrontierWAL), FrontierReceiptAt: timePointer(path.FrontierReceipt),
			AssumptionCodes: slices.Clone(pathAssumptionCodes),
		}
		if path.FrontierTimeline == 0 {
			item.FrontierWAL, item.FrontierReceiptAt = nil, nil
		}
		for _, timeline := range path.Timelines {
			item.Timelines = append(item.Timelines, uint64(timeline))
		}
		result = append(result, item)
	}
	slices.SortFunc(result, evidencev1alpha1.ComparePGBackRestRecoveryPaths)
	return result
}

// validate proves the native publication: the snapshot (with a stand-in
// epoch), every item, the contract order and the summary counts.
func (p *PGBackRestPublication) validate() error {
	snapshot := p.Snapshot
	snapshot.ProducerEpoch = validationEpoch
	if err := snapshot.Validate(); err != nil {
		return err
	}
	if err := validateItems(p.Backups, evidencev1alpha1.ComparePGBackRestBackups); err != nil {
		return err
	}
	if err := validateItems(p.WALRanges, evidencev1alpha1.ComparePGBackRestWALRanges); err != nil {
		return err
	}
	if err := validateItems(p.WALGaps, evidencev1alpha1.ComparePGBackRestWALGaps); err != nil {
		return err
	}
	if err := validateItems(p.RecoveryPaths, evidencev1alpha1.ComparePGBackRestRecoveryPaths); err != nil {
		return err
	}
	details := p.Snapshot.Details.PGBackRest
	if details == nil {
		return errors.New("a pgBackRest publication without pgBackRest details")
	}
	if p.Snapshot.Completeness == evidencev1alpha1.Complete {
		if !equalsCount(details.BackupItems, len(p.Backups)) || !equalsCount(details.WALRangeItems, len(p.WALRanges)) || !equalsCount(details.WALGapItems, len(p.WALGaps)) || !equalsCount(details.RecoveryPathItems, len(p.RecoveryPaths)) {
			return errors.New("publication summary and collection counts disagree")
		}
	}
	return nil
}

func validateItems[T interface{ Validate() error }](items []T, compare func(T, T) int) error {
	for index, item := range items {
		if err := item.Validate(); err != nil {
			return err
		}
		if index > 0 && compare(items[index-1], item) >= 0 {
			return errors.New("publication items are not uniquely sorted")
		}
	}
	return nil
}

func aspectReason(aspect pgbackrest.Aspect, prefix string) evidencev1alpha1.StateReason {
	state := aspect.State
	if state == "" {
		state = evidence.Unknown
	}
	return evidencev1alpha1.StateReason{State: evidencev1alpha1.State(state), Reason: typedReason(prefix+"-"+string(state), messageOr(aspect.Reason, prefix+" not evaluated"))}
}

func mappedCodes(values []string, mapping map[string]string) []string {
	result := []string{}
	for _, value := range values {
		if code, ok := mapping[value]; ok {
			result = append(result, code)
		}
	}
	slices.Sort(result)
	return slices.Compact(result)
}

// walTimeline is the timeline a WAL segment name starts with.
func walTimeline(name string) *uint64 {
	if len(name) != 24 {
		return nil
	}
	timeline, err := strconv.ParseUint(name[:8], 16, 32)
	if err != nil || timeline == 0 {
		return nil
	}
	return &timeline
}

func positiveIntPointer(value int) *uint64 {
	if value <= 0 {
		return nil
	}
	return uint64Pointer(uint64(value))
}
