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

// Package evidenceapi projects one immutable inventory generation into the
// versioned the console evidence contract. It owns no HTTP or provider behavior.
package evidenceapi

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/barmancloud"
	"github.com/fyannk/pgToolBox/internal/evidence/inventory"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// Options contains only operator-resolved, credential-free producer identity.
type Options struct {
	ProducerVersion string
	// ProducerInstance is the per-process random publication epoch. Empty
	// omits it; NewEngine fills it when the caller leaves it empty.
	ProducerInstance string
	ClusterNamespace string
	ClusterUID       string
	ClusterName      *string
	// Repository is the credential-free repository identity (S3, Azure or
	// GCS), hashed with evidencev1alpha1.FingerprintRepository.
	Repository evidencev1alpha1.RepositoryFingerprintInput
	// PGBackRestArchiveStanza is the stanza holding the WAL of a pgBackRest
	// repository (the scope's stanza when empty); PGBackRestRepository is
	// its repository number (1 when zero), reported only.
	PGBackRestArchiveStanza string
	PGBackRestRepository    uint64
}

// Publication contains one snapshot and its immutable generation collections.
// It is an internal projection, not a combined wire response.
type Publication struct {
	Snapshot      evidencev1alpha1.RepositoryEvidenceSnapshot
	Backups       []evidencev1alpha1.BarmanBackup
	WALRanges     []evidencev1alpha1.BarmanWALRange
	WALGaps       []evidencev1alpha1.BarmanWALGap
	RecoveryPaths []evidencev1alpha1.BarmanRecoveryPath
	// PGBackRest, when set, is the whole publication of a pgBackRest
	// repository and every field above is empty.
	PGBackRest *PGBackRestPublication
}

// Project creates a deterministic, key-free publication without provider I/O.
func Project(source inventory.Snapshot, options Options) (Publication, error) {
	if err := source.Validate(); err != nil {
		return Publication{}, fmt.Errorf("project evidence publication: %w", err)
	}
	canonical, err := evidencev1alpha1.CanonicalRepositoryFingerprintInput(options.Repository)
	if err == nil && canonical.Format == source.Evidence.RepositoryFormat && canonical.Format == evidencev1alpha1.FormatPGBackRest {
		return projectPGBackRest(source, options, canonical)
	}
	if err != nil || canonical.Format != source.Evidence.RepositoryFormat || source.Evidence.Scope.Kind != "server" || len(source.PGBackRest) != 0 {
		return Publication{}, errors.New("project evidence publication: incompatible repository identity")
	}
	if source.TotalsKnown && (len(source.Scopes) != 1 || source.Scopes[0].Name != canonical.ScopeName) {
		return Publication{}, errors.New("project evidence publication: snapshot is not confined to one configured scope")
	}
	if err := validateSourceProfile(source, canonical.ScopeName); err != nil {
		return Publication{}, err
	}
	fingerprint, err := evidencev1alpha1.FingerprintRepository(canonical)
	if err != nil {
		return Publication{}, errors.New("project evidence publication: invalid repository fingerprint")
	}

	publication := Publication{}
	publication.Backups = projectBackups(source, canonical.ScopeName)
	publication.WALRanges, publication.WALGaps = projectWAL(source, canonical.ScopeName)
	publication.RecoveryPaths = projectRecoveryPaths(source, canonical.ScopeName)
	publication.Snapshot = projectSnapshot(source, options, canonical, fingerprint, publication)
	if err := publication.Validate(); err != nil {
		return Publication{}, fmt.Errorf("project evidence publication: %w", err)
	}
	return publication, nil
}

func validateSourceProfile(source inventory.Snapshot, server string) error {
	if !source.TotalsKnown && (len(source.BarmanCatalog.Backups) != 0 || len(source.BarmanWAL.Servers) != 0 || len(source.BarmanRecovery.Servers) != 0) {
		return errors.New("project evidence publication: incomplete source retained partial format evidence")
	}
	for _, backup := range source.BarmanCatalog.Backups {
		if backup.Server != server || backup.PostgreSQLVersion < 0 || backup.SegmentSize < 0 || backup.LogicalBytes < 0 || backup.StoredBytes < 0 || backup.DeduplicatedBytes != nil && *backup.DeduplicatedBytes < 0 {
			return errors.New("project evidence publication: invalid or cross-scope backup evidence")
		}
	}
	for _, value := range source.BarmanWAL.Servers {
		if value.Server != server {
			return errors.New("project evidence publication: cross-scope WAL evidence")
		}
	}
	for _, value := range source.BarmanRecovery.Servers {
		if value.Server != server {
			return errors.New("project evidence publication: cross-scope recovery evidence")
		}
	}
	return nil
}

// Validate proves that summary counts and typed collections describe one
// generation and contain only values allowed by the public contract.
func (p Publication) Validate() error {
	if p.PGBackRest != nil {
		if p.Snapshot.APIVersion != "" || len(p.Backups) != 0 || len(p.WALRanges) != 0 || len(p.WALGaps) != 0 || len(p.RecoveryPaths) != 0 {
			return errors.New("a pgBackRest publication carries Barman evidence")
		}
		return p.PGBackRest.validate()
	}
	if err := p.Snapshot.Validate(); err != nil {
		return err
	}
	for _, backup := range p.Backups {
		if err := backup.Validate(); err != nil {
			return err
		}
	}
	for index := 1; index < len(p.Backups); index++ {
		if p.Backups[index-1].BackupID > p.Backups[index].BackupID || p.Backups[index-1].BackupID == p.Backups[index].BackupID && p.Backups[index-1].Server >= p.Backups[index].Server {
			return errors.New("publication backups are not uniquely sorted")
		}
	}
	for _, value := range p.WALRanges {
		if err := value.Validate(); err != nil {
			return err
		}
	}
	for index := 1; index < len(p.WALRanges); index++ {
		left, right := p.WALRanges[index-1], p.WALRanges[index]
		if left.Server > right.Server || left.Server == right.Server && (left.Timeline > right.Timeline || left.Timeline == right.Timeline && left.StartPosition >= right.StartPosition) {
			return errors.New("publication WAL ranges are not uniquely sorted")
		}
	}
	for _, value := range p.WALGaps {
		if err := value.Validate(); err != nil {
			return err
		}
	}
	for index := 1; index < len(p.WALGaps); index++ {
		left, right := p.WALGaps[index-1], p.WALGaps[index]
		if left.Server > right.Server || left.Server == right.Server && (left.Timeline > right.Timeline || left.Timeline == right.Timeline && left.StartPosition >= right.StartPosition) {
			return errors.New("publication WAL gaps are not uniquely sorted")
		}
	}
	for _, value := range p.RecoveryPaths {
		if err := value.Validate(); err != nil {
			return err
		}
	}
	for index := 1; index < len(p.RecoveryPaths); index++ {
		left, right := p.RecoveryPaths[index-1], p.RecoveryPaths[index]
		if left.BackupID > right.BackupID || left.BackupID == right.BackupID && (left.TargetTimeline > right.TargetTimeline || left.TargetTimeline == right.TargetTimeline && left.Server >= right.Server) {
			return errors.New("publication recovery paths are not uniquely sorted")
		}
	}
	details := p.Snapshot.Details.BarmanCloud
	if details != nil && p.Snapshot.Completeness == evidencev1alpha1.Complete {
		if !equalsCount(details.BackupItems, len(p.Backups)) || !equalsCount(details.WALRangeItems, len(p.WALRanges)) || !equalsCount(details.WALGapItems, len(p.WALGaps)) || !equalsCount(details.RecoveryPathItems, len(p.RecoveryPaths)) {
			return errors.New("publication summary and collection counts disagree")
		}
	}
	return nil
}

func projectSnapshot(source inventory.Snapshot, options Options, canonical evidencev1alpha1.RepositoryFingerprintInput, fingerprint string, publication Publication) evidencev1alpha1.RepositoryEvidenceSnapshot {
	evidenceGeneration := uint64(0)
	var startedAt, completedAt *time.Time
	if source.TotalsKnown {
		evidenceGeneration = source.Evidence.Generation
		startedAt = timePointer(source.Evidence.StartedAt)
		completedAt = timePointer(source.Evidence.CompletedAt)
	}
	completeness := evidencev1alpha1.Completeness(source.Evidence.Completeness)
	state := evidencev1alpha1.State(source.Evidence.State)
	topReasonCode := "snapshot-" + string(state)
	if source.RefreshFailure != "" {
		topReasonCode = "refresh-" + strings.ReplaceAll(string(source.RefreshFailure), "_", "-")
	}
	result := evidencev1alpha1.RepositoryEvidenceSnapshot{
		APIVersion: evidencev1alpha1.APIVersion,
		Kind:       evidencev1alpha1.SnapshotKind,
		Producer: evidencev1alpha1.Producer{
			Name: evidencev1alpha1.ProducerName, Version: options.ProducerVersion, Features: evidencev1alpha1.ProducerFeatures(),
		},
		ProducerInstance: options.ProducerInstance,
		// The handler stamps the deployed epoch on every served copy; the
		// stand-in keeps the stored snapshot valid on its own.
		ProducerEpoch: validationEpoch,
		Identity: evidencev1alpha1.Identity{
			Cluster: evidencev1alpha1.ClusterIdentity{Namespace: options.ClusterNamespace, UID: options.ClusterUID, Name: cloneString(options.ClusterName)},
			Repository: evidencev1alpha1.RepositoryIdentity{
				Provider: repositoryProvider(options), Format: canonical.Format, DestinationFingerprint: fingerprint,
				Scope: evidencev1alpha1.ScopeIdentity{Kind: canonical.ScopeKind, Name: canonical.ScopeName},
			},
		},
		Revision: source.RefreshGeneration, EvidenceGeneration: evidenceGeneration,
		StartedAt: startedAt, CompletedAt: completedAt, LastAttemptAt: timePointer(source.LastAttemptAt),
		Completeness: completeness, Stale: source.Evidence.Stale || source.RefreshFailure != "", State: state,
		Reason:       typedReason(topReasonCode, source.Evidence.Reason),
		Capabilities: projectCapabilities(source.Evidence.Capabilities),
		Inventory:    projectInventory(source),
	}
	result.Details = projectDetails(source, canonical.ScopeName, publication)
	return result
}

// repositoryProvider is the configured provider, s3 when none is named.
func repositoryProvider(options Options) string {
	if options.Repository.Provider == "" {
		return evidencev1alpha1.ProviderS3
	}
	return options.Repository.Provider
}

func projectCapabilities(values []evidence.Capability) []evidencev1alpha1.Capability {
	result := make([]evidencev1alpha1.Capability, 0, len(values))
	for _, value := range values {
		state := evidencev1alpha1.State(value.State)
		result = append(result, evidencev1alpha1.Capability{
			ID: evidencev1alpha1.CapabilityID(value.ID), Support: evidencev1alpha1.Support(value.Support), State: state,
			Reason: typedReason("capability-"+string(value.ID)+"-"+string(state), value.Reason),
		})
	}
	slices.SortFunc(result, func(left, right evidencev1alpha1.Capability) int {
		return strings.Compare(string(left.ID), string(right.ID))
	})
	return result
}

func projectInventory(source inventory.Snapshot) evidencev1alpha1.InventorySummary {
	// #nosec G115 -- pages and objects examined are non-negative scan tallies.
	result := evidencev1alpha1.InventorySummary{Known: source.TotalsKnown, PagesExamined: uint64(source.PagesExamined), ObjectsExamined: uint64(source.ObjectsExamined)}
	// #nosec G115 -- every scan counter here is non-negative: the scanner
	// rejects negative object sizes and guards its accumulators against
	// math.MaxInt64, so each int64 -> uint64 conversion is exact.
	if source.TotalsKnown {
		result.ObjectCount = uint64Pointer(uint64(source.ObjectCount))
		result.StoredBytes = uint64Pointer(uint64(source.StoredBytes))
		result.UnscopedObjectCount = uint64Pointer(uint64(source.UnscopedObjectCount))
	}
	if source.RefreshFailure != "" {
		category := evidencev1alpha1.FailureCategory(source.RefreshFailure)
		result.LastFailureCategory = &category
	}
	return result
}

func projectDetails(source inventory.Snapshot, server string, publication Publication) evidencev1alpha1.Details {
	wal := walServer(source.BarmanWAL, server)
	recovery := recoveryServer(source.BarmanRecovery, server)
	walResult := stateReason(wal.State, "wal", wal.Reason, "no Barman WAL evidence observed")
	timelineResult := stateReason(recovery.TimelineState, "timeline", recovery.TimelineReason, "no timeline evidence observed")
	coverageResult := stateReason(recovery.CoverageState, "coverage", recovery.CoverageReason, "no observed recovery coverage")
	summary := evidencev1alpha1.BarmanCloudSummary{
		WAL: walResult, Timeline: timelineResult, Coverage: coverageResult,
		Retention:              projectRetention(recovery.Retention),
		LatestArchiveReceiptAt: timePointer(wal.LatestArchiveReceipt),
		RangesTruncated:        wal.RangesTruncated,
		DiagnosticsTruncated:   wal.DiagnosticsTruncated,
	}
	if source.TotalsKnown {
		summary.BackupItems = countPointer(len(publication.Backups))
		summary.WALRangeItems = countPointer(len(publication.WALRanges))
		summary.WALGapItems = countPointer(len(publication.WALGaps))
		summary.RecoveryPathItems = countPointer(len(publication.RecoveryPaths))
		states, usable := backupCounts(publication.Backups)
		summary.BackupStates = &states
		summary.StructurallyUsableBackups = uint64Pointer(usable)
		summary.WALCounts = projectWALCounts(wal.Counts)
	}
	return evidencev1alpha1.Details{Type: evidencev1alpha1.BarmanDetailsType, BarmanCloud: &summary}
}

func projectBackups(source inventory.Snapshot, server string) []evidencev1alpha1.BarmanBackup {
	result := make([]evidencev1alpha1.BarmanBackup, 0, len(source.BarmanCatalog.Backups))
	for _, backup := range source.BarmanCatalog.Backups {
		if backup.Server != server {
			continue
		}
		// #nosec G115 -- StoredBytes is the sum of visible object sizes; the
		// scanner rejects negative sizes and guards the sum against math.MaxInt64.
		result = append(result, evidencev1alpha1.BarmanBackup{
			Server: server, BackupID: backup.ID, Status: stringPointer(backup.Status), BackupType: stringPointer(backup.Type),
			State: evidencev1alpha1.State(backup.State), Reason: typedReason("backup-"+string(backup.State), backup.Reason),
			SystemID: stringPointer(backup.SystemID), PostgreSQLVersion: positiveInt64Pointer(backup.PostgreSQLVersion), Timeline: positiveUint32Pointer(backup.Timeline),
			WALSegmentSizeBytes: positiveInt64Pointer(backup.SegmentSize), BeginWAL: stringPointer(backup.BeginWAL), EndWAL: stringPointer(backup.EndWAL), BeginLSN: stringPointer(backup.BeginLSN), EndLSN: stringPointer(backup.EndLSN),
			BeginAt: timePointer(backup.BeginAt), EndAt: timePointer(backup.EndAt), LogicalBytes: positiveInt64Pointer(backup.LogicalBytes), DeduplicatedBytes: int64Pointer(backup.DeduplicatedBytes),
			StoredArtifactBytes: uint64Pointer(uint64(backup.StoredBytes)), Compression: stringPointer(backup.Compression), Encryption: stringPointer(backup.Encryption),
			ArtifactCount: countPointer(len(backup.Artifacts)), TablespaceCount: countPointer(len(backup.TablespaceOIDs)),
		})
	}
	slices.SortFunc(result, func(left, right evidencev1alpha1.BarmanBackup) int {
		if order := strings.Compare(left.BackupID, right.BackupID); order != 0 {
			return order
		}
		return strings.Compare(left.Server, right.Server)
	})
	return result
}

func projectWAL(source inventory.Snapshot, server string) ([]evidencev1alpha1.BarmanWALRange, []evidencev1alpha1.BarmanWALGap) {
	selected := walServer(source.BarmanWAL, server)
	ranges := make([]evidencev1alpha1.BarmanWALRange, 0, len(selected.Ranges))
	for _, value := range selected.Ranges {
		ranges = append(ranges, evidencev1alpha1.BarmanWALRange{
			Server: server, Timeline: uint64(value.Timeline), StartPosition: value.Start, EndPosition: value.End, SegmentCount: value.Count,
			FirstWAL: value.First, LastWAL: value.Last, LatestReceiptAt: timePointer(value.LatestReceipt), EndReceiptAt: timePointer(value.EndReceipt),
		})
	}
	gaps := make([]evidencev1alpha1.BarmanWALGap, 0, len(selected.Gaps))
	for _, value := range selected.Gaps {
		gaps = append(gaps, evidencev1alpha1.BarmanWALGap{
			Server: server, Timeline: uint64(value.Timeline), StartPosition: value.Start, EndPosition: value.End, SegmentCount: value.Count,
			FirstWAL: value.First, LastWAL: value.Last, Status: evidencev1alpha1.GapStatus(value.Status),
			FirstObservedGeneration: value.FirstObservedGeneration, LastObservedGeneration: value.LastObservedGeneration,
		})
	}
	return ranges, gaps
}

func projectRecoveryPaths(source inventory.Snapshot, server string) []evidencev1alpha1.BarmanRecoveryPath {
	selected := recoveryServer(source.BarmanRecovery, server)
	result := make([]evidencev1alpha1.BarmanRecoveryPath, 0, len(selected.Paths))
	for _, path := range selected.Paths {
		if path.TargetTimeline == 0 {
			continue
		}
		result = append(result, evidencev1alpha1.BarmanRecoveryPath{
			Server: server, BackupID: path.BackupID, TargetTimeline: uint64(path.TargetTimeline), State: evidencev1alpha1.State(path.State),
			Reason: typedReason(pathReasonCode(path.State, path.OffTarget), path.Reason), Stop: evidencev1alpha1.CoverageStop(path.Stop),
			LowerBoundAt: timePointer(path.LowerBound), StartTimeline: positiveUint32Pointer(path.StartTimeline), StartPosition: positionPointer(path.StartTimeline, path.StartPosition), StartWAL: stringPointer(path.StartWAL),
			FrontierTimeline: positiveUint32Pointer(path.FrontierTimeline), FrontierPosition: positionPointer(path.FrontierTimeline, path.FrontierPosition), FrontierWAL: stringPointer(path.FrontierWAL), FrontierReceiptAt: timePointer(path.FrontierReceipt),
			AssumptionCodes: assumptionCodes(path.Assumptions), OffTarget: path.OffTarget,
		})
	}
	return result
}

func projectRetention(value barmancloud.RetentionSummary) evidencev1alpha1.BarmanRetentionSummary {
	result := evidencev1alpha1.BarmanRetentionSummary{
		OldestCompletionAt: timePointer(value.OldestCompletion), NewestCompletionAt: timePointer(value.NewestCompletion),
		MinimumConfigured: value.MinimumConfigured, PolicyConfigured: value.PolicyConfigured,
		State: evidencev1alpha1.State(value.State), Reason: typedReason("retention-"+string(value.State), messageOr(value.Reason, "no retention evidence observed")),
	}
	// #nosec G115 -- these counts derive from catalog slice lengths and a
	// validated 0..100000 configured redundancy.
	if value.State != "" {
		result.VisibleBackups = uint64Pointer(uint64(value.VisibleBackups))
		result.StructurallyUsableBackups = uint64Pointer(uint64(value.StructurallyUsable))
	}
	// #nosec G115 -- EXPECTED_MINIMUM_REDUNDANCY is validated to 0..100000.
	if value.MinimumConfigured {
		result.MinimumRedundancy = uint64Pointer(uint64(value.MinimumRedundancy))
	}
	return result
}

func projectWALCounts(value barmancloud.WALCounts) *evidencev1alpha1.BarmanWALCounts {
	// #nosec G115 -- WAL class counts are non-negative classification tallies.
	return &evidencev1alpha1.BarmanWALCounts{
		Segment: uint64(value.Segments), Partial: uint64(value.Partials), History: uint64(value.History),
		BackupHistory: uint64(value.BackupHistory), Unknown: uint64(value.Unknown), Duplicate: uint64(value.Duplicates),
	}
}

func backupCounts(values []evidencev1alpha1.BarmanBackup) (evidencev1alpha1.StateCounts, uint64) {
	var counts evidencev1alpha1.StateCounts
	var usable uint64
	for _, value := range values {
		switch value.State {
		case evidencev1alpha1.Healthy:
			counts.Healthy++
			usable++
		case evidencev1alpha1.Warning:
			counts.Warning++
		case evidencev1alpha1.Unhealthy:
			counts.Unhealthy++
		default:
			counts.Unknown++
		}
	}
	return counts, usable
}

func walServer(catalog barmancloud.WALCatalog, name string) barmancloud.ServerWAL {
	for _, server := range catalog.Servers {
		if server.Server == name {
			return server
		}
	}
	return barmancloud.ServerWAL{Server: name, State: evidence.Unknown, Reason: "no Barman WAL evidence observed"}
}

func recoveryServer(catalog barmancloud.RecoveryCatalog, name string) barmancloud.ServerRecovery {
	for _, server := range catalog.Servers {
		if server.Server == name {
			return server
		}
	}
	return barmancloud.ServerRecovery{
		Server: name, TimelineState: evidence.Unknown, TimelineReason: "no timeline evidence observed",
		CoverageState: evidence.Unknown, CoverageReason: "no observed recovery coverage",
		Retention: barmancloud.RetentionSummary{State: evidence.Unknown, Reason: "no retention evidence observed"},
	}
}

func stateReason(state evidence.State, prefix, message, fallback string) evidencev1alpha1.StateReason {
	if state == "" {
		state = evidence.Unknown
	}
	return evidencev1alpha1.StateReason{State: evidencev1alpha1.State(state), Reason: typedReason(prefix+"-"+string(state), messageOr(message, fallback))}
}

// pathReasonCode is a recovery path's reason code: the one that says a
// restore must name the target timeline when a default restore does not
// follow the path, else its state's.
func pathReasonCode(state evidence.State, offTarget bool) string {
	if offTarget {
		return evidencev1alpha1.ReasonPathNeedsTargetTimeline
	}
	return "coverage-path-" + string(state)
}

func typedReason(code, message string) evidencev1alpha1.Reason {
	return evidencev1alpha1.Reason{Code: code, Message: message}
}

func messageOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func assumptionCodes(values []string) []string {
	mapping := map[string]string{
		"segment-name presence only":                                     "segment-name-presence-only",
		"timeline history metadata only":                                 "timeline-history-metadata-only",
		"WAL bytes and restore execution not verified":                   "wal-bytes-and-restore-not-verified",
		"backup system identifier unavailable":                           "backup-system-identifier-unavailable",
		"backup system identifier retained; WAL objects do not carry it": "wal-system-identifier-unavailable",
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if code, ok := mapping[value]; ok {
			result = append(result, code)
		}
	}
	slices.Sort(result)
	return slices.Compact(result)
}

// #nosec G115 -- callers pass slice lengths and non-negative tallies only.
func equalsCount(value *uint64, count int) bool { return value != nil && *value == uint64(count) }

// #nosec G115 -- callers pass slice lengths and non-negative tallies only.
func countPointer(value int) *uint64     { return uint64Pointer(uint64(value)) }
func uint64Pointer(value uint64) *uint64 { return &value }

func positiveUint64Pointer(value uint64) *uint64 {
	if value == 0 {
		return nil
	}
	return uint64Pointer(value)
}

func positiveUint32Pointer(value uint32) *uint64 { return positiveUint64Pointer(uint64(value)) }

func positionPointer(timeline uint32, position uint64) *uint64 {
	if timeline == 0 {
		return nil
	}
	return uint64Pointer(position)
}

func positiveInt64Pointer(value int64) *uint64 {
	if value <= 0 {
		return nil
	}
	return uint64Pointer(uint64(value))
}

func int64Pointer(value *int64) *uint64 {
	if value == nil || *value < 0 {
		return nil
	}
	return uint64Pointer(uint64(*value))
}

func stringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func timePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	utc := value.UTC()
	return &utc
}
