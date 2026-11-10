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

package v1alpha1

// The pgBackRest profile of the contract: a snapshot whose
// identity says format pgbackrest and scope kind stanza carries a
// pgbackrest/v1alpha1 details payload, and its four collection routes answer
// with the PGBackRest* pages below. A Barman snapshot and its pages are
// unchanged.

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
)

// Repository formats and their scope kinds. A snapshot's identity pairs a
// format with exactly its scope kind.
const (
	// FormatBarmanCloud is a repository written by barman-cloud; its scope
	// is one Barman server name.
	FormatBarmanCloud = "barman-cloud"
	// ScopeKindBarmanServer is the scope kind of FormatBarmanCloud.
	ScopeKindBarmanServer = "barman-server"
	// FormatPGBackRest is a pgBackRest repository; its scope is one stanza.
	FormatPGBackRest = "pgbackrest"
	// ScopeKindStanza is the scope kind of FormatPGBackRest.
	ScopeKindStanza = "stanza"
)

const (
	// PGBackRestDetailsType is the Details type tag of the pgBackRest summary.
	PGBackRestDetailsType = "pgbackrest/v1alpha1"
	// PGBackRestBackupPageKind is the kind of a PGBackRestBackupPage.
	PGBackRestBackupPageKind = "PGBackRestBackupPage"
	// PGBackRestWALRangePageKind is the kind of a PGBackRestWALRangePage.
	PGBackRestWALRangePageKind = "PGBackRestWALRangePage"
	// PGBackRestWALGapPageKind is the kind of a PGBackRestWALGapPage.
	PGBackRestWALGapPageKind = "PGBackRestWALGapPage"
	// PGBackRestRecoveryPageKind is the kind of a PGBackRestRecoveryPathPage.
	PGBackRestRecoveryPageKind = "PGBackRestRecoveryPathPage"
)

// The pgBackRest backup types, as backup.info names them.
const (
	PGBackRestFull         = "full"
	PGBackRestDifferential = "diff"
	PGBackRestIncremental  = "incr"
)

// The WAL segment size sources: the backups' LSN ranges decide it, the
// archive's segment names do (the segment number at which a log id rolls
// over), or the 16 MiB default is assumed because neither can (pgBackRest
// stores the size nowhere in the repository).
const (
	SegmentSizeFromBackupLSN   = "backup-lsn"
	SegmentSizeFromWALNames    = "wal-names"
	SegmentSizeAssumedDefault  = "assumed-default"
	maxPGBackRestFindings      = 200
	maxPGBackRestArchiveIDs    = 256
	maxPGBackRestReferences    = 1000
	maxPGBackRestAnnotations   = 32
	maxPGBackRestTimelines     = 128
	maxPGBackRestSubjectBytes  = 1024
	maxPGBackRestUnusableCodes = 16
)

// The finding codes the agent's reader raises (stable; extend, never
// rename). A consumer must accept a code it does not know: the list grows.
const (
	FindingChecksumMismatch            = "ChecksumMismatch"
	FindingCopyMismatch                = "CopyMismatch"
	FindingInfoUnreadable              = "InfoUnreadable"
	FindingRepositoryFormatUnsupported = "RepositoryFormatUnsupported"
	FindingMissingPrior                = "MissingPrior"
	FindingMissingReference            = "MissingReference"
	FindingChainInconsistent           = "ChainInconsistent"
	FindingManifestMissing             = "ManifestMissing"
	FindingManifestUnreadable          = "ManifestUnreadable"
	FindingManifestMismatch            = "ManifestMismatch"
	FindingWALGap                      = "WALGap"
	FindingWALUnknown                  = "WALUnknown"
	FindingStanzaSplit                 = "StanzaSplit"
	// FindingManifestBudgetExceeded: the scan stopped reading manifests at
	// its cap on manifests or on bytes; the backups past it have a manifest
	// of unknown state (warning).
	FindingManifestBudgetExceeded = "ManifestBudgetExceeded"
	// FindingRetentionNotConfigured: repo-retention-full is not set, so
	// pgBackRest never expires a full backup (warning).
	FindingRetentionNotConfigured = "RetentionNotConfigured"
	// FindingRetentionNotApplied: the stanza holds a backup or WAL the
	// configured retention would have expired at the newest backup (warning).
	FindingRetentionNotApplied = "RetentionNotApplied"
	// FindingRetentionShortfall: the stanza lacks a full backup an earlier
	// scan saw, or a backup's WAL from its archive start, that the
	// retention keeps (error).
	FindingRetentionShortfall = "RetentionShortfall"
)

// The retention types, as pgBackRest names them: repo-retention-full-type
// and repo-retention-archive-type.
const (
	RetentionFullCount = "count"
	RetentionFullTime  = "time"
	// MaxPGBackRestRetention bounds repo-retention-full, -diff and -archive
	// (pgBackRest's own bound, and both plugins' CRDs').
	MaxPGBackRestRetention = 9_999_999
	// MaxPGBackRestRepository is pgBackRest's highest repository number
	// (repo1 to repo256).
	MaxPGBackRestRepository = 256
)

// PGBackRestRetentionError names the repo-retention-* option a retention
// is refused for ("full", "full-type", "diff", "archive", "archive-type")
// and why, so each caller can say it in its own terms (an environment
// variable, a CRD field).
type PGBackRestRetentionError struct {
	Option, Problem string
}

func (e *PGBackRestRetentionError) Error() string {
	return "repo-retention-" + e.Option + " " + e.Problem
}

// ValidatePGBackRestRetention checks repo-retention-* options as pgBackRest
// bounds them, before any default is applied: each count from 0 (unset) to
// MaxPGBackRestRetention, the full type empty (pgBackRest's default, count),
// count or time, the archive type empty (full), full, diff or incr. It is
// the one check of the operator's resolution, the agent's settings and its
// reader; the error is a *PGBackRestRetentionError.
func ValidatePGBackRestRetention(full int, fullType string, diff, archive int, archiveType string) error {
	for _, count := range []struct {
		option string
		value  int
	}{{"full", full}, {"diff", diff}, {"archive", archive}} {
		if count.value < 0 || count.value > MaxPGBackRestRetention {
			return &PGBackRestRetentionError{Option: count.option, Problem: fmt.Sprintf("must be 0 (unset) to %d", MaxPGBackRestRetention)}
		}
	}
	switch fullType {
	case "", RetentionFullCount, RetentionFullTime:
	default:
		return &PGBackRestRetentionError{Option: "full-type", Problem: "must be count or time"}
	}
	switch archiveType {
	case "", PGBackRestFull, PGBackRestDifferential, PGBackRestIncremental:
	default:
		return &PGBackRestRetentionError{Option: "archive-type", Problem: "must be full, diff or incr"}
	}
	return nil
}

// The finding severities.
const (
	FindingWarning = "warning"
	FindingError   = "error"
)

// PGBackRestDatabase is the PostgreSQL cluster a stanza currently holds
// (backup.info [db]).
type PGBackRestDatabase struct {
	DBID              uint64  `json:"db_id"`
	SystemID          string  `json:"system_id"`
	Version           string  `json:"version"`
	PostgreSQLVersion *uint64 `json:"postgresql_version"`
}

// PGBackRestWALCounts counts the objects of every archive id of the archive
// stanza per class.
type PGBackRestWALCounts struct {
	Segment       uint64 `json:"segment"`
	Partial       uint64 `json:"partial"`
	History       uint64 `json:"history"`
	BackupHistory uint64 `json:"backup_history"`
	Unknown       uint64 `json:"unknown"`
	Duplicate     uint64 `json:"duplicate"`
}

// PGBackRestRecoveryWindow is the span a restore can reach: from the stop
// time of the oldest usable backup that reaches the newest frontier to the
// end of the last contiguous WAL segment. The end is a WAL position; its
// time is only the upload time of that segment.
type PGBackRestRecoveryWindow struct {
	OldestBackup string    `json:"oldest_backup"`
	StartAt      time.Time `json:"start_at"`
	NewestBackup string    `json:"newest_backup"`
	NewestStopAt time.Time `json:"newest_stop_at"`
	EndTimeline  uint64    `json:"end_timeline"`
	EndWAL       string    `json:"end_wal"`
	// EndLSN is an exclusive upper bound: the first LSN past the end of
	// EndWAL, which the archive does not hold. It is not a restore target
	// (recovery_target_lsn) and not the last commit; replay ends at or
	// before it.
	EndLSN          string     `json:"end_lsn"`
	EndReceiptAt    *time.Time `json:"end_receipt_at"`
	UsableBackups   uint64     `json:"usable_backups"`
	AssumptionCodes []string   `json:"assumption_codes"`
}

// PGBackRestFinding is one problem the reader found. Code is a stable
// identifier (the Finding* constants); Subject is an object key relative to
// the repository root or a backup label; Message is fixed text. None of them
// carries file content or secret material.
type PGBackRestFinding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Subject  string `json:"subject"`
	Message  string `json:"message"`
}

// PGBackRestSummary is the bounded format-owned snapshot summary of one
// pgBackRest stanza in one repository.
type PGBackRestSummary struct {
	// Stanza holds the backups; ArchiveStanza holds the WAL. They differ
	// only for a split stanza (finding StanzaSplit).
	Stanza        string `json:"stanza"`
	ArchiveStanza string `json:"archive_stanza"`
	// Repository is the repository number (repo1..repo256) the agent reads;
	// it is reported, not used to find the files.
	Repository uint64 `json:"repository"`
	// RepositoryFormat is the backrest-format the info files declare (5 is
	// read; 6 is detected and refused), null before they are read.
	RepositoryFormat *uint64 `json:"repository_format"`
	// Encrypted is null until the info files are read.
	Encrypted              *bool                     `json:"encrypted"`
	PGBackRestVersion      *string                   `json:"pgbackrest_version"`
	Database               *PGBackRestDatabase       `json:"database"`
	ArchiveIDs             []string                  `json:"archive_ids"`
	WALSegmentSizeBytes    *uint64                   `json:"wal_segment_size_bytes"`
	WALSegmentSizeSource   *string                   `json:"wal_segment_size_source"`
	BackupItems            *uint64                   `json:"backup_items"`
	WALRangeItems          *uint64                   `json:"wal_range_items"`
	WALGapItems            *uint64                   `json:"wal_gap_items"`
	RecoveryPathItems      *uint64                   `json:"recovery_path_items"`
	UsableBackups          *uint64                   `json:"usable_backups"`
	BackupStates           *StateCounts              `json:"backup_states"`
	WALCounts              *PGBackRestWALCounts      `json:"wal_counts"`
	Catalog                StateReason               `json:"catalog"`
	WAL                    StateReason               `json:"wal"`
	Timeline               StateReason               `json:"timeline"`
	Coverage               StateReason               `json:"coverage"`
	RecoveryWindow         *PGBackRestRecoveryWindow `json:"recovery_window"`
	LatestArchiveReceiptAt *time.Time                `json:"latest_archive_receipt_at"`
	RangesTruncated        bool                      `json:"ranges_truncated"`
	DiagnosticsTruncated   bool                      `json:"diagnostics_truncated"`
	Findings               []PGBackRestFinding       `json:"findings"`
	FindingsTruncated      bool                      `json:"findings_truncated"`
	// Retention compares the stanza with its repository's configured
	// retention; absent when the agent was given none (additive: an older
	// producer never sends it).
	Retention *PGBackRestRetention `json:"retention,omitempty"`
}

// PGBackRestRetention is the comparison of a stanza with the repository's
// repo-retention-* options, as of the newest backup (pgBackRest expires at
// the end of each backup, never on its own). The options are as the
// plugin's CRD sets them, null when unset; the types carry pgBackRest's
// defaults. Its findings are in the summary's (RetentionNotConfigured,
// RetentionNotApplied, RetentionShortfall).
type PGBackRestRetention struct {
	// Configured is false when repo-retention-full is unset: pgBackRest
	// then never expires a full backup (finding RetentionNotConfigured).
	Configured bool `json:"configured"`
	// Full is a count of full backups, or days when FullType is time.
	Full        *uint64 `json:"full"`
	FullType    string  `json:"full_type"`
	Diff        *uint64 `json:"diff"`
	Archive     *uint64 `json:"archive"`
	ArchiveType string  `json:"archive_type"`
	// EffectiveArchive is the archive retention expire applies (Archive, or
	// its default from Full or Diff), in backups of ArchiveType; null when
	// WAL is never expired.
	EffectiveArchive    *uint64 `json:"effective_archive"`
	FullBackups         uint64  `json:"full_backups"`
	DifferentialBackups uint64  `json:"differential_backups"`
	IncrementalBackups  uint64  `json:"incremental_backups"`
	// EvaluatedAt is the stop time of the newest backup, the last time the
	// retention was applied; WindowStartAt is EvaluatedAt minus Full days
	// (time-based retention only).
	EvaluatedAt   *time.Time `json:"evaluated_at"`
	WindowStartAt *time.Time `json:"window_start_at"`
	State         State      `json:"state"`
	Reason        Reason     `json:"reason"`
}

// PGBackRestAnnotation is one backup annotation (pgbackrest --annotation),
// sorted by key in a backup.
type PGBackRestAnnotation struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// The codes of PGBackRestBackup.UnusableReasons.
const (
	UnusableChainBroken          = "chain-broken"
	UnusableManifestUnhealthy    = "manifest-unhealthy"
	UnusableManifestUnknown      = "manifest-unknown"
	UnusableManifestNotInspected = "manifest-not-inspected"
	UnusableDependencyManifest   = "dependency-manifest-not-verified"
	UnusableBackupError          = "backup-error"
	UnusableWALToConsistency     = "wal-to-consistency-missing"
	UnusableWALUnknown           = "wal-unknown"
	UnusableWALToTargetTimeline  = "wal-to-target-timeline-missing"
	UnusableOffTargetTimeline    = "off-target-timeline"
)

// PGBackRestBackup is one backup of backup.info with its chain, manifest and
// WAL evidence. Usable is the strongest claim: its chain, every manifest it
// depends on and the WAL to consistency are present and verified, on the
// timeline a restore would follow. Times come from the stored epochs, never
// from the label (whose time is the server's local time).
type PGBackRestBackup struct {
	Stanza               string                 `json:"stanza"`
	Label                string                 `json:"label"`
	Type                 string                 `json:"type"`
	Prior                *string                `json:"prior"`
	References           []string               `json:"references"`
	State                State                  `json:"state"`
	Reason               Reason                 `json:"reason"`
	Usable               bool                   `json:"usable"`
	UnusableReasons      []string               `json:"unusable_reasons"`
	ArchiveID            *string                `json:"archive_id"`
	SystemID             *string                `json:"system_id"`
	PostgreSQLVersion    *uint64                `json:"postgresql_version"`
	Timeline             *uint64                `json:"timeline"`
	StartAt              *time.Time             `json:"start_at"`
	StopAt               *time.Time             `json:"stop_at"`
	StartLSN             *string                `json:"start_lsn"`
	StopLSN              *string                `json:"stop_lsn"`
	ArchiveStart         *string                `json:"archive_start"`
	ArchiveStop          *string                `json:"archive_stop"`
	SizeBytes            uint64                 `json:"size_bytes"`
	DeltaBytes           uint64                 `json:"delta_bytes"`
	RepositorySizeBytes  uint64                 `json:"repository_size_bytes"`
	RepositoryDeltaBytes uint64                 `json:"repository_delta_bytes"`
	Error                *bool                  `json:"error"`
	PGBackRestVersion    *string                `json:"pgbackrest_version"`
	CNPGBackupName       *string                `json:"cnpg_backup_name"`
	Annotations          []PGBackRestAnnotation `json:"annotations"`
	AnnotationsWithheld  uint64                 `json:"annotations_withheld"`
}

// PGBackRestWALRange is one compact, contiguous WAL segment range of one
// archive id.
type PGBackRestWALRange struct {
	Stanza          string     `json:"stanza"`
	ArchiveID       string     `json:"archive_id"`
	Timeline        uint64     `json:"timeline"`
	StartPosition   uint64     `json:"start_position"`
	EndPosition     uint64     `json:"end_position"`
	SegmentCount    uint64     `json:"segment_count"`
	FirstWAL        string     `json:"first_wal"`
	LastWAL         string     `json:"last_wal"`
	LatestReceiptAt *time.Time `json:"latest_receipt_at"`
	EndReceiptAt    *time.Time `json:"end_receipt_at"`
}

// PGBackRestWALGap is one candidate or confirmed missing WAL range of one
// archive id.
type PGBackRestWALGap struct {
	Stanza                  string    `json:"stanza"`
	ArchiveID               string    `json:"archive_id"`
	Timeline                uint64    `json:"timeline"`
	StartPosition           uint64    `json:"start_position"`
	EndPosition             uint64    `json:"end_position"`
	SegmentCount            uint64    `json:"segment_count"`
	FirstWAL                string    `json:"first_wal"`
	LastWAL                 string    `json:"last_wal"`
	Status                  GapStatus `json:"status"`
	FirstObservedGeneration uint64    `json:"first_observed_generation"`
	LastObservedGeneration  uint64    `json:"last_observed_generation"`
}

// PGBackRestRecoveryPath is how far the archived WAL rolls one backup
// forward: from its archive start, across the timeline switches, to the end
// of the contiguous WAL of the target timeline (the newest of its archive
// id, or the backup's own when it cannot reach it: OffTarget).
type PGBackRestRecoveryPath struct {
	Stanza            string       `json:"stanza"`
	Label             string       `json:"label"`
	ArchiveID         *string      `json:"archive_id"`
	TargetTimeline    uint64       `json:"target_timeline"`
	State             State        `json:"state"`
	Reason            Reason       `json:"reason"`
	Stop              CoverageStop `json:"stop"`
	Timelines         []uint64     `json:"timelines"`
	StartWAL          *string      `json:"start_wal"`
	ConsistentWAL     *string      `json:"consistent_wal"`
	Consistent        bool         `json:"consistent"`
	OffTarget         bool         `json:"off_target"`
	FrontierTimeline  *uint64      `json:"frontier_timeline"`
	FrontierPosition  *uint64      `json:"frontier_position"`
	FrontierWAL       *string      `json:"frontier_wal"`
	FrontierReceiptAt *time.Time   `json:"frontier_receipt_at"`
	AssumptionCodes   []string     `json:"assumption_codes"`
}

// PGBackRestBackupPage carries one bounded generation-consistent backup page.
type PGBackRestBackupPage struct {
	PageHeader
	Items []PGBackRestBackup `json:"items"`
}

// PGBackRestWALRangePage carries one bounded generation-consistent WAL-range page.
type PGBackRestWALRangePage struct {
	PageHeader
	Items []PGBackRestWALRange `json:"items"`
}

// PGBackRestWALGapPage carries one bounded generation-consistent WAL-gap page.
type PGBackRestWALGapPage struct {
	PageHeader
	Items []PGBackRestWALGap `json:"items"`
}

// PGBackRestRecoveryPathPage carries one bounded recovery-path page.
type PGBackRestRecoveryPathPage struct {
	PageHeader
	Items []PGBackRestRecoveryPath `json:"items"`
}

// validScopeProfile reports whether format, kind and name are one of the
// two profiles: a Barman server, or a pgBackRest stanza.
func validScopeProfile(format, kind, name string) bool {
	switch format {
	case FormatBarmanCloud:
		return kind == ScopeKindBarmanServer && validBarmanServer(name)
	case FormatPGBackRest:
		return kind == ScopeKindStanza && ValidPGBackRestStanza(name)
	default:
		return false
	}
}

// MaxPGBackRestStanzaBytes bounds a pgBackRest stanza name wherever one is
// accepted: the operator's and the self-resolving agent's resolution, the
// agent's settings, its reader and this contract. pgBackRest itself sets no
// bound (the stanza is a string option and a path segment of every key, so
// the file system's 255-byte name is its only limit); one shared bound
// means a stanza the operator accepts is one the agent reads.
const MaxPGBackRestStanzaBytes = 128

// PGBackRestStanzaRule says what ValidPGBackRestStanza accepts, for the
// error messages of every place that checks a stanza name.
const PGBackRestStanzaRule = "1 to 128 bytes of UTF-8, no control character, no leading or trailing space, no / or \\, not . or .."

// ValidPGBackRestStanza is a pgBackRest stanza name as pgToolBox accepts
// it (PGBackRestStanzaRule): at most MaxPGBackRestStanzaBytes of UTF-8
// without control characters or surrounding spaces, one path segment (no
// '/' or '\\', not "." or "..").
func ValidPGBackRestStanza(value string) bool {
	return !invalidText(value, MaxPGBackRestStanzaBytes) && value != "." && value != ".." && strings.TrimSpace(value) == value &&
		!strings.ContainsAny(value, "/\\")
}

func (s PGBackRestSummary) hasGenerationCounts() bool {
	return s.BackupItems != nil || s.WALRangeItems != nil || s.WALGapItems != nil || s.RecoveryPathItems != nil || s.UsableBackups != nil || s.BackupStates != nil || s.WALCounts != nil
}

func (s PGBackRestSummary) validate() error {
	if !ValidPGBackRestStanza(s.Stanza) || !ValidPGBackRestStanza(s.ArchiveStanza) || s.Repository == 0 || s.Repository > MaxPGBackRestRepository {
		return errors.New("invalid pgBackRest stanza or repository")
	}
	if s.RepositoryFormat != nil && (*s.RepositoryFormat == 0 || *s.RepositoryFormat > 99) || invalidOptionalText(s.PGBackRestVersion, 64) {
		return errors.New("invalid pgBackRest repository format or version")
	}
	if s.Database != nil {
		if s.Database.DBID == 0 || !decimal(s.Database.SystemID) || len(s.Database.SystemID) > 20 || !validPostgreSQLVersionText(s.Database.Version) {
			return errors.New("invalid pgBackRest database")
		}
	}
	if s.ArchiveIDs == nil || len(s.ArchiveIDs) > maxPGBackRestArchiveIDs {
		return errors.New("invalid pgBackRest archive ids")
	}
	for index, id := range s.ArchiveIDs {
		if !validArchiveID(id) || slices.Contains(s.ArchiveIDs[:index], id) {
			return errors.New("invalid or duplicate pgBackRest archive id")
		}
	}
	if (s.WALSegmentSizeBytes == nil) != (s.WALSegmentSizeSource == nil) || s.WALSegmentSizeBytes != nil && (!validWALSegmentSize(*s.WALSegmentSizeBytes) || !slices.Contains([]string{SegmentSizeFromBackupLSN, SegmentSizeFromWALNames, SegmentSizeAssumedDefault}, *s.WALSegmentSizeSource)) {
		return errors.New("invalid pgBackRest WAL segment size")
	}
	for _, result := range []StateReason{s.Catalog, s.WAL, s.Timeline, s.Coverage} {
		if !validState(result.State) || invalidReason(result.Reason) {
			return errors.New("invalid pgBackRest details state")
		}
	}
	if !validOptionalTime(s.LatestArchiveReceiptAt) {
		return errors.New("invalid pgBackRest archive receipt time")
	}
	if (s.RangesTruncated || s.DiagnosticsTruncated) && s.WAL.State != Unknown {
		return errors.New("truncated pgBackRest WAL evidence must be unknown")
	}
	if s.BackupItems != nil && s.BackupStates != nil {
		states := s.BackupStates
		if states.Healthy+states.Warning+states.Unhealthy+states.Unknown != *s.BackupItems {
			return errors.New("the pgBackRest backup state counts do not reconcile")
		}
	}
	if s.BackupItems != nil && s.UsableBackups != nil && *s.UsableBackups > *s.BackupItems {
		return errors.New("usable backups exceed visible backups")
	}
	if s.RecoveryWindow != nil {
		if err := s.RecoveryWindow.validate(); err != nil {
			return err
		}
		if s.UsableBackups != nil && s.RecoveryWindow.UsableBackups > *s.UsableBackups {
			return errors.New("the recovery window counts more usable backups than the stanza")
		}
	}
	if s.Retention != nil {
		if err := s.Retention.validate(); err != nil {
			return err
		}
	}
	if s.Findings == nil || len(s.Findings) > maxPGBackRestFindings {
		return errors.New("invalid pgBackRest findings")
	}
	for index, finding := range s.Findings {
		if err := finding.validate(); err != nil {
			return err
		}
		if index > 0 && compareFinding(s.Findings[index-1], finding) >= 0 {
			return errors.New("pgBackRest findings must be uniquely sorted")
		}
	}
	return nil
}

func (r PGBackRestRetention) validate() error {
	if r.FullType != RetentionFullCount && r.FullType != RetentionFullTime || r.ArchiveType != PGBackRestFull && r.ArchiveType != PGBackRestDifferential && r.ArchiveType != PGBackRestIncremental {
		return errors.New("invalid pgBackRest retention type")
	}
	for _, value := range []*uint64{r.Full, r.Diff, r.Archive, r.EffectiveArchive} {
		if value != nil && (*value == 0 || *value > MaxPGBackRestRetention) {
			return errors.New("invalid pgBackRest retention count")
		}
	}
	if r.Configured != (r.Full != nil) {
		return errors.New("pgBackRest retention is configured exactly when repo-retention-full is set")
	}
	if !validState(r.State) || invalidReason(r.Reason) || !validOptionalTime(r.EvaluatedAt) || !validOptionalTime(r.WindowStartAt) {
		return errors.New("invalid pgBackRest retention state or time")
	}
	if r.WindowStartAt != nil && (r.FullType != RetentionFullTime || r.EvaluatedAt == nil || r.WindowStartAt.After(*r.EvaluatedAt)) {
		return errors.New("a pgBackRest retention window needs time-based retention and an evaluation time after it")
	}
	return nil
}

func (w PGBackRestRecoveryWindow) validate() error {
	if !validPGBackRestLabel(w.OldestBackup) || !validPGBackRestLabel(w.NewestBackup) || w.EndTimeline == 0 || !validWALName(w.EndWAL) || !validOptionalLSN(&w.EndLSN) || w.UsableBackups == 0 {
		return errors.New("invalid pgBackRest recovery window")
	}
	if !validOptionalTime(&w.StartAt) || !validOptionalTime(&w.NewestStopAt) || w.NewestStopAt.Before(w.StartAt) || !validOptionalTime(w.EndReceiptAt) {
		return errors.New("invalid pgBackRest recovery window times")
	}
	return validAssumptionCodes(w.AssumptionCodes)
}

func (f PGBackRestFinding) validate() error {
	if !validFindingCode(f.Code) || f.Severity != FindingWarning && f.Severity != FindingError || invalidText(f.Subject, maxPGBackRestSubjectBytes) || invalidText(f.Message, 256) {
		return errors.New("invalid pgBackRest finding")
	}
	return nil
}

// Validate checks one backup item without interpreting repository metadata.
func (b PGBackRestBackup) Validate() error {
	if !ValidPGBackRestStanza(b.Stanza) || !validState(b.State) || invalidReason(b.Reason) {
		return errors.New("invalid pgBackRest backup identity or state")
	}
	labelType, full, ok := pgBackRestLabelType(b.Label)
	if !ok || labelType != b.Type {
		return errors.New("invalid pgBackRest backup label or type")
	}
	if b.Type == PGBackRestFull {
		if b.Prior != nil || len(b.References) != 0 {
			return errors.New("a full pgBackRest backup has no prior or references")
		}
	} else if b.Prior == nil || !validPGBackRestLabel(*b.Prior) || !strings.HasPrefix(*b.Prior, full) {
		return errors.New("a pgBackRest differential or incremental backup needs a prior of its full backup")
	}
	if b.References == nil || len(b.References) > maxPGBackRestReferences {
		return errors.New("invalid pgBackRest backup references")
	}
	for index, reference := range b.References {
		if !validPGBackRestLabel(reference) || index > 0 && b.References[index-1] >= reference {
			return errors.New("pgBackRest backup references must be uniquely sorted labels")
		}
	}
	if b.Usable != (len(b.UnusableReasons) == 0) || b.Usable && b.State != Healthy {
		return errors.New("pgBackRest backup usability and reasons disagree")
	}
	if err := validCodes(b.UnusableReasons, maxPGBackRestUnusableCodes); err != nil {
		return err
	}
	for _, field := range []struct {
		value   *string
		maximum int
	}{{b.ArchiveID, 32}, {b.SystemID, 20}, {b.PGBackRestVersion, 64}, {b.CNPGBackupName, 256}} {
		if invalidOptionalText(field.value, field.maximum) {
			return errors.New("invalid optional value in a pgBackRest backup")
		}
	}
	if b.ArchiveID != nil && !validArchiveID(*b.ArchiveID) || b.SystemID != nil && !decimal(*b.SystemID) || b.Timeline != nil && *b.Timeline == 0 {
		return errors.New("invalid archive id, system id or timeline in a pgBackRest backup")
	}
	if !validOptionalWALName(b.ArchiveStart) || !validOptionalWALName(b.ArchiveStop) || !validOptionalLSN(b.StartLSN) || !validOptionalLSN(b.StopLSN) {
		return errors.New("invalid WAL position in a pgBackRest backup")
	}
	if !validOptionalTime(b.StartAt) || !validOptionalTime(b.StopAt) || b.StartAt != nil && b.StopAt != nil && b.StopAt.Before(*b.StartAt) {
		return errors.New("invalid time in a pgBackRest backup")
	}
	if b.Annotations == nil || len(b.Annotations) > maxPGBackRestAnnotations {
		return errors.New("invalid pgBackRest backup annotations")
	}
	for index, annotation := range b.Annotations {
		if invalidText(annotation.Key, 256) || len(annotation.Value) > 256 || strings.ToValidUTF8(annotation.Value, "") != annotation.Value || strings.IndexFunc(annotation.Value, unicode.IsControl) >= 0 || index > 0 && b.Annotations[index-1].Key >= annotation.Key {
			return errors.New("pgBackRest backup annotations must be bounded and uniquely sorted")
		}
	}
	return nil
}

// Validate checks one compact WAL range.
func (r PGBackRestWALRange) Validate() error {
	if !ValidPGBackRestStanza(r.Stanza) || !validArchiveID(r.ArchiveID) || r.Timeline == 0 || r.EndPosition < r.StartPosition || r.EndPosition == ^uint64(0) || r.SegmentCount != r.EndPosition-r.StartPosition+1 || !validWALName(r.FirstWAL) || !validWALName(r.LastWAL) || !validOptionalTime(r.LatestReceiptAt) || !validOptionalTime(r.EndReceiptAt) {
		return errors.New("invalid pgBackRest WAL range")
	}
	return nil
}

// Validate checks one WAL gap and its complete-generation lifecycle.
func (g PGBackRestWALGap) Validate() error {
	if !ValidPGBackRestStanza(g.Stanza) || !validArchiveID(g.ArchiveID) || g.Timeline == 0 || g.EndPosition < g.StartPosition || g.EndPosition == ^uint64(0) || g.SegmentCount != g.EndPosition-g.StartPosition+1 || !validWALName(g.FirstWAL) || !validWALName(g.LastWAL) {
		return errors.New("invalid pgBackRest WAL gap")
	}
	if g.Status != GapCandidate && g.Status != GapConfirmed || g.FirstObservedGeneration == 0 || g.LastObservedGeneration < g.FirstObservedGeneration || g.Status == GapCandidate && g.LastObservedGeneration != g.FirstObservedGeneration || g.Status == GapConfirmed && g.LastObservedGeneration == g.FirstObservedGeneration {
		return errors.New("invalid pgBackRest WAL gap lifecycle")
	}
	return nil
}

// Validate checks one recovery path.
func (p PGBackRestRecoveryPath) Validate() error {
	if !ValidPGBackRestStanza(p.Stanza) || !validPGBackRestLabel(p.Label) || p.TargetTimeline == 0 || !validState(p.State) || invalidReason(p.Reason) || !slices.Contains([]CoverageStop{CoverageFrontier, CoverageCandidateLimited, CoverageGapLimited, CoverageUnknownLimited}, p.Stop) {
		return errors.New("invalid pgBackRest recovery path")
	}
	if p.ArchiveID != nil && !validArchiveID(*p.ArchiveID) || !validOptionalWALName(p.StartWAL) || !validOptionalWALName(p.ConsistentWAL) || !validOptionalTime(p.FrontierReceiptAt) {
		return errors.New("invalid pgBackRest recovery path position")
	}
	if p.Timelines == nil || len(p.Timelines) > maxPGBackRestTimelines || slices.Contains(p.Timelines, 0) {
		return errors.New("invalid pgBackRest recovery path timelines")
	}
	if !completePosition(p.FrontierTimeline, p.FrontierPosition, p.FrontierWAL) {
		return errors.New("incomplete pgBackRest recovery path frontier")
	}
	if p.State == Healthy && (p.Stop != CoverageFrontier || !p.Consistent || p.OffTarget) {
		return errors.New("a healthy pgBackRest recovery path reaches the frontier on its target timeline")
	}
	return validAssumptionCodes(p.AssumptionCodes)
}

// Validate checks the backup page envelope, bounds, items, and contract order.
func (p PGBackRestBackupPage) Validate() error {
	return validatePage(p.PageHeader, PGBackRestBackupPageKind, p.Items, ComparePGBackRestBackups)
}

// Validate checks the WAL-range page envelope, bounds, items, and order.
func (p PGBackRestWALRangePage) Validate() error {
	return validatePage(p.PageHeader, PGBackRestWALRangePageKind, p.Items, ComparePGBackRestWALRanges)
}

// Validate checks the WAL-gap page envelope, bounds, items, and order.
func (p PGBackRestWALGapPage) Validate() error {
	return validatePage(p.PageHeader, PGBackRestWALGapPageKind, p.Items, ComparePGBackRestWALGaps)
}

// Validate checks the recovery-path page envelope, bounds, items, and order.
func (p PGBackRestRecoveryPathPage) Validate() error {
	return validatePage(p.PageHeader, PGBackRestRecoveryPageKind, p.Items, ComparePGBackRestRecoveryPaths)
}

// ComparePGBackRestBackups is the contract order of backup items: label,
// then stanza.
func ComparePGBackRestBackups(left, right PGBackRestBackup) int {
	if result := strings.Compare(left.Label, right.Label); result != 0 {
		return result
	}
	return strings.Compare(left.Stanza, right.Stanza)
}

// ComparePGBackRestWALRanges is the contract order of WAL ranges: stanza,
// archive id, timeline, start position.
func ComparePGBackRestWALRanges(left, right PGBackRestWALRange) int {
	return comparePGBackRestWAL(left.Stanza, left.ArchiveID, left.Timeline, left.StartPosition, right.Stanza, right.ArchiveID, right.Timeline, right.StartPosition)
}

// ComparePGBackRestWALGaps is the contract order of WAL gaps, that of the
// ranges.
func ComparePGBackRestWALGaps(left, right PGBackRestWALGap) int {
	return comparePGBackRestWAL(left.Stanza, left.ArchiveID, left.Timeline, left.StartPosition, right.Stanza, right.ArchiveID, right.Timeline, right.StartPosition)
}

// ComparePGBackRestRecoveryPaths is the contract order of recovery paths:
// label, target timeline, stanza.
func ComparePGBackRestRecoveryPaths(left, right PGBackRestRecoveryPath) int {
	if result := strings.Compare(left.Label, right.Label); result != 0 {
		return result
	}
	if left.TargetTimeline != right.TargetTimeline {
		return compareUint64(left.TargetTimeline, right.TargetTimeline)
	}
	return strings.Compare(left.Stanza, right.Stanza)
}

func comparePGBackRestWAL(leftStanza, leftArchive string, leftTimeline, leftStart uint64, rightStanza, rightArchive string, rightTimeline, rightStart uint64) int {
	if result := strings.Compare(leftStanza, rightStanza); result != 0 {
		return result
	}
	if result := strings.Compare(leftArchive, rightArchive); result != 0 {
		return result
	}
	if leftTimeline != rightTimeline {
		return compareUint64(leftTimeline, rightTimeline)
	}
	return compareUint64(leftStart, rightStart)
}

type validatedItem interface{ Validate() error }

func validatePage[T validatedItem](header PageHeader, kind string, items []T, compare func(T, T) int) error {
	if err := header.validate(kind, len(items)); err != nil {
		return err
	}
	if items == nil {
		return errors.New("page items must be non-null")
	}
	for index, item := range items {
		if err := item.Validate(); err != nil {
			return err
		}
		if index > 0 && compare(items[index-1], item) >= 0 {
			return errors.New("page items must be uniquely sorted")
		}
	}
	return nil
}

func compareFinding(left, right PGBackRestFinding) int {
	for _, pair := range [][2]string{{left.Code, right.Code}, {left.Subject, right.Subject}, {left.Message, right.Message}, {left.Severity, right.Severity}} {
		if result := strings.Compare(pair[0], pair[1]); result != 0 {
			return result
		}
	}
	return 0
}

// validFindingCode is an UpperCamelCase identifier: the reader's codes, and
// any later one.
func validFindingCode(value string) bool {
	if value == "" || len(value) > 64 || value[0] < 'A' || value[0] > 'Z' {
		return false
	}
	for _, character := range value {
		if !(character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || character >= '0' && character <= '9') {
			return false
		}
	}
	return true
}

func validCodes(values []string, maximum int) error {
	if values == nil || len(values) > maximum {
		return errors.New("invalid code list")
	}
	for index, code := range values {
		if invalidReason(Reason{Code: code, Message: "bounded"}) || index > 0 && values[index-1] >= code {
			return errors.New("unbounded, duplicate or unsorted codes")
		}
	}
	return nil
}

func validAssumptionCodes(values []string) error {
	if err := validCodes(values, 32); err != nil {
		return errors.New("invalid pgBackRest assumption codes")
	}
	return nil
}

// pgBackRestLabelType returns the backup type a label names and the label of
// its full backup: YYYYMMDD-HHMMSSF, or <full>_YYYYMMDD-HHMMSS{D,I}.
func pgBackRestLabelType(label string) (string, string, bool) {
	stamp := func(value string) bool {
		return len(value) == 15 && value[8] == '-' && decimal(value[:8]) && decimal(value[9:])
	}
	switch {
	case len(label) == 16 && label[15] == 'F' && stamp(label[:15]):
		return PGBackRestFull, label, true
	case len(label) == 33 && label[16] == '_' && label[15] == 'F' && stamp(label[:15]) && stamp(label[17:32]):
		switch label[32] {
		case 'D':
			return PGBackRestDifferential, label[:16], true
		case 'I':
			return PGBackRestIncremental, label[:16], true
		}
	}
	return "", "", false
}

func validPGBackRestLabel(value string) bool {
	_, _, ok := pgBackRestLabelType(value)
	return ok
}

// validArchiveID is "<db-version>-<db-id>", for example "17-1" or "9.6-2".
func validArchiveID(value string) bool {
	version, id, ok := strings.Cut(value, "-")
	return ok && len(value) <= 32 && validPostgreSQLVersionText(version) && decimal(id) && id[0] != '0'
}

// validPostgreSQLVersionText is a major version as pgBackRest writes it:
// "17", or "9.6" before PostgreSQL 10.
func validPostgreSQLVersionText(value string) bool {
	major, minor, dotted := strings.Cut(value, ".")
	if !decimal(major) || major[0] == '0' || len(major) > 2 {
		return false
	}
	return !dotted || len(minor) == 1 && decimal(minor)
}
