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

// Package evidence holds the console's projection of what an evidence
// agent publishes over the evidence contract: reason codes, states, counts,
// and times only, re-rendered and redacted. Free-text messages from the
// evidence agent never reach these types, so a template cannot proxy them
// to a browser.
//
// The projection preserves uncertainty twice over: the evidence agent
// reports its own staleness against the repository, and the console tracks
// its own contact with the evidence agent. Both surface independently;
// neither can mask the other, and no failure here touches console
// readiness.
package evidence

import "time"

// FailureKind is the closed, output-safe vocabulary of consumer-side
// poll failures. Rendering and logging key on these values; raw
// transport or response detail never leaves the package.
type FailureKind string

// The closed set of failure kinds.
const (
	// FailureNone marks a successful latest poll.
	FailureNone FailureKind = ""
	// FailureUnavailable reports that the evidence agent could not be reached.
	FailureUnavailable FailureKind = "unavailable"
	// FailureTimeout reports that the request exceeded the contract's
	// five-second deadline.
	FailureTimeout FailureKind = "timeout"
	// FailureCanceled reports that the poll was canceled by shutdown.
	FailureCanceled FailureKind = "canceled"
	// FailureUnauthenticated reports that the evidence agent rejected the
	// console's token.
	FailureUnauthenticated FailureKind = "unauthenticated"
	// FailureBusy reports the evidence agent's concurrency budget was reached.
	FailureBusy FailureKind = "busy"
	// FailureIncompatible reports a version, kind, or media-type
	// disagreement: an unlisted runtime pair degrades, never guesses.
	FailureIncompatible FailureKind = "incompatible-schema"
	// FailureInvalid reports a response that violated the contract's
	// own invariants or size bounds.
	FailureInvalid FailureKind = "invalid-response"
	// FailureIdentityMismatch reports evidence whose repository
	// identity does not match the operator-configured expectation.
	FailureIdentityMismatch FailureKind = "identity-mismatch"
	// FailurePublicationChanged reports that the evidence agent's publication
	// advanced during collection assembly; the partial assembly is
	// discarded and the next poll restarts from the snapshot.
	FailurePublicationChanged FailureKind = "publication-changed"
)

// StateFact is one semantic result reduced to its typed parts: the
// four-state vocabulary value and the stable reason code. The
// producer's diagnostic message is deliberately absent.
type StateFact struct {
	// State is exactly healthy, warning, unhealthy, or unknown.
	State string
	// Code is the stable lower-kebab-case reason code.
	Code string
}

// CapabilityFact is one format-neutral capability projection.
type CapabilityFact struct {
	// ID identifies the evidence operation.
	ID string
	// Support states whether the repository format proves it.
	Support string
	// State is the current evidence state.
	State string
	// Code is the stable reason code.
	Code string
}

// InventoryFacts is the provider-neutral inventory projection. Nil
// counts are unknown, never zero.
type InventoryFacts struct {
	// Known reports whether the totals below are known.
	Known bool
	// ObjectCount is the scoped object total.
	ObjectCount *uint64
	// StoredBytes is the scoped stored-byte total.
	StoredBytes *uint64
	// UnscopedObjectCount counts objects outside the configured scope.
	UnscopedObjectCount *uint64
	// PagesExamined counts listing pages of the last attempt.
	PagesExamined uint64
	// ObjectsExamined counts objects of the last attempt.
	ObjectsExamined uint64
	// LastFailureCategory is the producer's redacted failure category
	// of a failed latest attempt, empty otherwise.
	LastFailureCategory string
}

// StateCountFacts counts every normative evidence state.
type StateCountFacts struct {
	// Healthy counts healthy items.
	Healthy uint64
	// Warning counts warning items.
	Warning uint64
	// Unhealthy counts unhealthy items.
	Unhealthy uint64
	// Unknown counts unknown items.
	Unknown uint64
}

// WALCountFacts counts the WAL object classes of either format.
type WALCountFacts struct {
	// Segment counts complete segments.
	Segment uint64
	// Partial counts partial segments.
	Partial uint64
	// History counts timeline history files.
	History uint64
	// BackupHistory counts backup history files.
	BackupHistory uint64
	// Unknown counts unclassified objects.
	Unknown uint64
	// Duplicate counts duplicate objects.
	Duplicate uint64
}

// RetentionFacts is the retention projection: Barman reports one always,
// pgBackRest when the agent was given the repository's retention (the
// summary's optional retention block).
type RetentionFacts struct {
	// VisibleBackups counts backups the repository shows.
	VisibleBackups *uint64
	// StructurallyUsableBackups counts structurally usable backups.
	StructurallyUsableBackups *uint64
	// OldestCompletionAt is the oldest backup completion time.
	OldestCompletionAt *time.Time
	// NewestCompletionAt is the newest backup completion time.
	NewestCompletionAt *time.Time
	// MinimumConfigured reports a configured redundancy expectation.
	MinimumConfigured bool
	// MinimumRedundancy is the configured minimum, when configured.
	MinimumRedundancy *uint64
	// Result is the retention state and reason code.
	Result StateFact
	// PGBackRest is the pgBackRest comparison's detail; nil for Barman.
	// MinimumConfigured mirrors its Configured, VisibleBackups its
	// backup counts.
	PGBackRest *PGBackRestRetentionFacts
}

// PGBackRestRetentionFacts is a stanza against the repository's
// repo-retention-* options, as of the newest backup (pgBackRest expires at
// the end of each backup). A nil option is unset.
type PGBackRestRetentionFacts struct {
	// Configured is false when repo-retention-full is unset: pgBackRest
	// then never expires a full backup.
	Configured bool
	// Full is a count of full backups, or days when FullType is time.
	Full     *uint64
	FullType string
	Diff     *uint64
	Archive  *uint64
	// ArchiveType is the backup type Archive counts (full, diff, incr).
	ArchiveType string
	// EffectiveArchive is the archive retention expire applies; nil when
	// WAL is never expired.
	EffectiveArchive *uint64
	// The backups of each type the stanza holds.
	FullBackups, DifferentialBackups, IncrementalBackups uint64
	// EvaluatedAt is the newest backup's stop; WindowStartAt is EvaluatedAt
	// minus Full days (time-based retention only).
	EvaluatedAt, WindowStartAt *time.Time
}

// The repository formats a details variant can carry.
const (
	FormatBarmanCloud = "barman-cloud"
	FormatPGBackRest  = "pgbackrest"
)

// FindingFact is one problem the repository reader found, reduced to its
// stable code, its severity and its subject (an object key relative to
// the repository root, or a backup label). The producer's message is
// absent, as everywhere in this package.
type FindingFact struct {
	// Code is the stable UpperCamelCase finding code (StanzaSplit, ...).
	Code string
	// Severity is warning or error.
	Severity string
	// Subject is the key or label the finding is about.
	Subject string
}

// RecoveryWindowFacts is the span a restore can reach as the producer
// states it: from the oldest usable backup's stop to the receipt of the
// last contiguous WAL segment. pgBackRest summaries carry it; Barman
// summaries do not (their window is drawn from the recovery paths).
type RecoveryWindowFacts struct {
	// OldestBackup and NewestBackup are the usable backups' labels.
	OldestBackup string
	NewestBackup string
	// StartAt is the first recoverability point: the oldest usable
	// backup's stop time.
	StartAt time.Time
	// NewestStopAt is the newest usable backup's stop time.
	NewestStopAt time.Time
	// EndTimeline and EndWAL locate the last contiguous segment.
	EndTimeline uint64
	EndWAL      string
	// EndReceiptAt is when that segment reached the repository: the
	// window's end in time, nil when unknown.
	EndReceiptAt *time.Time
	// UsableBackups counts the backups the window rests on.
	UsableBackups uint64
}

// RepositoryFacts is the format-neutral projection of a recognized
// details variant (barman-cloud or pgbackrest): the same questions about
// any repository — how many backups and how many usable, whether the WAL
// is continuous, how far coverage reaches, how fresh the archive is —
// with the format-only parts optional. A nil pointer is unknown or not
// reported by the format, never zero or healthy.
type RepositoryFacts struct {
	// Format is barman-cloud or pgbackrest.
	Format string
	// BackupItems counts backup evidence items of the generation.
	BackupItems *uint64
	// WALRangeItems counts contiguous WAL ranges.
	WALRangeItems *uint64
	// WALGapItems counts candidate and confirmed WAL gaps.
	WALGapItems *uint64
	// RecoveryPathItems counts observed recovery paths.
	RecoveryPathItems *uint64
	// UsableBackups counts the backups the producer calls usable
	// (Barman: structurally usable; pgBackRest: chain, manifests and WAL
	// to consistency verified).
	UsableBackups *uint64
	// BackupStates counts backups per evidence state.
	BackupStates *StateCountFacts
	// WALCounts counts WAL objects per class.
	WALCounts *WALCountFacts
	// Catalog is the backup catalog's integrity (pgBackRest info files
	// and manifests); nil for a format that reports none.
	Catalog *StateFact
	// WAL is the WAL continuity result.
	WAL StateFact
	// Timeline is the timeline traversal result.
	Timeline StateFact
	// Coverage is the observed recovery coverage result.
	Coverage StateFact
	// Retention is the retention comparison; nil when the format reports
	// none (a pgBackRest agent given no retention).
	Retention *RetentionFacts
	// OldestBackupAt and NewestBackupAt are the oldest and newest backup
	// completion times the producer reports.
	OldestBackupAt *time.Time
	NewestBackupAt *time.Time
	// RecoveryWindow is the producer's own window; nil when it states
	// none (Barman, or no usable backup).
	RecoveryWindow *RecoveryWindowFacts
	// LatestArchiveReceiptAt is the newest WAL receipt time.
	LatestArchiveReceiptAt *time.Time
	// RangesTruncated reports a producer safety ceiling on ranges.
	RangesTruncated bool
	// DiagnosticsTruncated reports a producer diagnostics ceiling.
	DiagnosticsTruncated bool
	// Findings are the reader's findings, sorted as the producer sends
	// them; FindingsTruncated reports that it cut the list.
	Findings          []FindingFact
	FindingsTruncated bool
}

// HasFinding reports whether the facts carry a finding of that code.
func (f *RepositoryFacts) HasFinding(code string) bool {
	if f == nil {
		return false
	}
	for _, finding := range f.Findings {
		if finding.Code == code {
			return true
		}
	}
	return false
}

// Report is the re-rendered, redacted projection of one validated
// snapshot response. Every field is either a typed enum value, a
// bounded identifier the contract validates, a count, or a time; no
// free-text producer message survives the projection.
type Report struct {
	// ProducerVersion is the emitting the evidence agent build version.
	ProducerVersion string
	// ClusterNamespace is the producer-bound cluster namespace.
	ClusterNamespace string
	// ClusterUID is the producer-bound immutable cluster UID. Current
	// agreement requires it to equal the console's own observed UID;
	// the comparison happens at rendering against the observed
	// snapshot, never against injected configuration.
	ClusterUID string
	// ClusterName is the display-only operator-injected name.
	ClusterName string
	// Provider is the repository provider: "s3", "azure" or "gcs".
	Provider string
	// Format is the repository format: "barman-cloud" or "pgbackrest".
	Format string
	// ScopeKind is the format-owned scope kind: "barman-server" or
	// "stanza".
	ScopeKind string
	// ScopeName is the exact Barman server name or pgBackRest stanza.
	ScopeName string
	// Fingerprint is the producer's redacted destination fingerprint —
	// the only repository destination identity the console shows.
	Fingerprint string
	// ProducerInstance is the producer process's random instance token,
	// empty from producers that predate it. Revision and
	// EvidenceGeneration restart at zero with every process, so only
	// (ProducerInstance, EvidenceGeneration) names one immutable
	// evidence generation. It is a cache key, never displayed.
	ProducerInstance string
	// Revision identifies the published attempt result.
	Revision uint64
	// EvidenceGeneration identifies the last complete evidence.
	EvidenceGeneration uint64
	// StartedAt is the evidence generation's scan start time.
	StartedAt *time.Time
	// CompletedAt is the evidence generation's scan completion time.
	CompletedAt *time.Time
	// LastAttemptAt is the last refresh attempt time.
	LastAttemptAt *time.Time
	// Completeness is complete, incomplete, or no-completed-scan.
	Completeness string
	// SourceStale is the evidence agent's own staleness claim against the
	// repository: the retained evidence no longer reflects the latest
	// refresh attempt.
	SourceStale bool
	// Overall is the snapshot state and reason code.
	Overall StateFact
	// Capabilities is the complete, sorted capability projection.
	Capabilities []CapabilityFact
	// Inventory is the provider-neutral inventory projection.
	Inventory InventoryFacts
	// DetailsType is the bounded tagged-union type tag.
	DetailsType string
	// Facts is the recognized summary, barman-cloud or pgbackrest, in
	// one format-neutral shape; nil when the variant tag is unknown to
	// this consumer, which renders as explicitly unknown, never as
	// absent-and-healthy.
	Facts *RepositoryFacts
}

// RepoBackup is one re-rendered repository backup evidence item from
// the generation-consistent collection pages: bounded identifiers,
// typed state with its stable code, counts, and times only.
type RepoBackup struct {
	// Server is the Barman server scope of the item.
	Server string
	// BackupID is the exact repository backup identity — the only
	// correlation key the contract accepts.
	BackupID string
	// Status is the repository's bounded status word, empty when not
	// reported.
	Status string
	// BackupType is the bounded backup type, empty when not reported.
	BackupType string
	// Result is the structural evidence state and reason code.
	Result StateFact
	// Timeline is the PostgreSQL timeline, nil when unknown.
	Timeline *uint64
	// BeginAt is the backup begin time, nil when unknown.
	BeginAt *time.Time
	// EndAt is the backup end time, nil when unknown.
	EndAt *time.Time
	// StoredArtifactBytes is the stored artifact size, nil when
	// unknown.
	StoredArtifactBytes *uint64
}

// Snapshot is the console-side publication: one validated report, the
// atomically assembled backup collection of the same publication
// identity, and the console's own contact bookkeeping, mirroring the
// Kubernetes snapshot vocabulary of generation, observation time, and
// staleness.
type Snapshot struct {
	// Generation increases by one on every publication.
	Generation uint64
	// ObservedAt is the time of the last successful poll.
	ObservedAt time.Time
	// Stale reports the console has lost contact with the evidence agent and
	// the report below is the retained last-good projection.
	Stale bool
	// Failure is the kind of the latest failed poll; FailureNone while
	// contact holds.
	Failure FailureKind
	// Report is the last validated projection.
	Report Report
	// Backups is the assembled backup evidence of the report's
	// publication identity, sorted as the producer serves it. Empty
	// with evidence generation zero: no complete scan, no items.
	Backups []RepoBackup
	// BackupsTruncated reports the consumer's own assembly ceiling
	// cut the collection; absence conclusions are then unknown.
	BackupsTruncated bool
}

// Status is what the console's evidence reader reports to the renderer:
// the snapshot when one exists, and the latest failure kind either way, so
// an evidence agent that has never answered still renders an attributed unknown.
type Status struct {
	// HasReport reports whether Snapshot carries a validated report.
	HasReport bool
	// Snapshot is the current publication, meaningful with HasReport.
	Snapshot Snapshot
	// Failure is the latest poll failure kind; FailureNone while
	// contact holds.
	Failure FailureKind
}
