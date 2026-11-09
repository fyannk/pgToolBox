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
	"context"
	"fmt"
	"io"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// DefaultMaxManifests is how many of the newest backups have their manifest
// read, on top of the backups of their chains: the bound keeps one scan's
// object-store reads proportional to the recent history, not to all of it.
const DefaultMaxManifests = 64

// DefaultMaxManifestScanBytes is how many bytes of manifests (main file and
// .copy, as stored) one scan reads at most. A manifest is about 100 to 130
// bytes per file of the cluster, so this reads 64 backups' pairs of a cluster
// of about 250,000 files; a larger cluster gets fewer, newest first, and a
// ManifestBudgetExceeded finding. One manifest at MaxManifestBytes and its
// .copy take an eighth of it.
const DefaultMaxManifestScanBytes int64 = 4 << 30

// FindingCode is the stable code of a finding. Codes are API: extend, never
// rename.
type FindingCode string

// The finding codes.
const (
	// FindingChecksumMismatch: an info file or manifest does not match its
	// backrest-checksum.
	FindingChecksumMismatch FindingCode = "ChecksumMismatch"
	// FindingCopyMismatch: a file and its .copy are not the same verified
	// content (one is missing, does not verify, or has another checksum).
	FindingCopyMismatch FindingCode = "CopyMismatch"
	// FindingInfoUnreadable: neither backup.info (or archive.info) nor its
	// .copy could be used: missing, unreadable, malformed or not decryptable.
	FindingInfoUnreadable FindingCode = "InfoUnreadable"
	// FindingRepositoryFormatUnsupported: a file is at a repository format
	// other than 5 (format 6 is detected, not read).
	FindingRepositoryFormatUnsupported FindingCode = "RepositoryFormatUnsupported"
	// FindingMissingPrior: a backup's backup-prior is not in backup.info.
	FindingMissingPrior FindingCode = "MissingPrior"
	// FindingMissingReference: a backup-reference is not in backup.info.
	FindingMissingReference FindingCode = "MissingReference"
	// FindingChainInconsistent: a chain contradicts the backup types
	// (a full with a prior, a differential whose prior is not a full, a
	// reference outside the chain, a label of another full, another database).
	FindingChainInconsistent FindingCode = "ChainInconsistent"
	// FindingManifestMissing: a backup listed in backup.info has neither
	// backup.manifest nor its .copy.
	FindingManifestMissing FindingCode = "ManifestMissing"
	// FindingManifestUnreadable: a manifest is present but neither file can
	// be used (other than a checksum mismatch).
	FindingManifestUnreadable FindingCode = "ManifestUnreadable"
	// FindingManifestMismatch: a manifest contradicts its backup.info entry.
	FindingManifestMismatch FindingCode = "ManifestMismatch"
	// FindingWALGap: segments are missing between two archived ranges of a
	// timeline.
	FindingWALGap FindingCode = "WALGap"
	// FindingWALUnknown: the archive listing holds objects the reader cannot
	// account for, or the WAL arithmetic is not known.
	FindingWALUnknown FindingCode = "WALUnknown"
	// FindingStanzaSplit: backups and WAL are under different stanzas
	// (Opera's configuration.stanza); defined here, raised by the agent.
	FindingStanzaSplit FindingCode = "StanzaSplit"
	// FindingManifestBudgetExceeded: the scan stopped reading manifests at
	// its cap (MaxManifests plus DefaultMaxManifests manifests, chains
	// included) or at its byte
	// budget; the backups past it have a manifest of unknown state.
	FindingManifestBudgetExceeded FindingCode = "ManifestBudgetExceeded"
)

// Severity of a finding.
type Severity string

// The severities.
const (
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

// Finding is one problem found in a stanza. Subject is an object key or a
// backup label; Message is fixed text. Neither carries file content or
// secret material.
type Finding struct {
	Code     FindingCode
	Severity Severity
	Subject  string
	Message  string
}

// CoverageStop says why the walk along a backup's WAL stopped.
type CoverageStop string

const (
	// CoverageFrontier means the walk reached the newest contiguous segment of
	// the target timeline.
	CoverageFrontier CoverageStop = "frontier"
	// CoverageCandidateLimited means the walk stopped at a candidate gap
	// (seen by this scan only; it may still fill).
	CoverageCandidateLimited CoverageStop = "candidate-limited"
	// CoverageGapLimited means the walk stopped at a confirmed gap, or at WAL
	// that is proven lost (a backup's own WAL, or WAL before a timeline
	// switch whose child has WAL).
	CoverageGapLimited CoverageStop = "gap-limited"
	// CoverageUnknownLimited means the walk could not be evaluated.
	CoverageUnknownLimited CoverageStop = "unknown-limited"
)

// RecoveryPath is how far the archived WAL lets one backup be rolled forward
// to the target timeline (the newest timeline of its archive id, which is
// what a restore with recovery_target_timeline=latest follows).
type RecoveryPath struct {
	State            evidence.State
	Reason           string
	Stop             CoverageStop
	TargetTimeline   uint32
	Timelines        []uint32
	StartWAL         string
	ConsistentWAL    string
	Consistent       bool
	FrontierTimeline uint32
	FrontierWAL      string
	FrontierPosition uint64
	FrontierReceipt  time.Time
	// OffTarget is true when the backup cannot reach the newest timeline (it
	// is on another branch, or its timeline branched before it was
	// consistent); the path then follows the backup's own timeline.
	OffTarget bool
	// SwitchInFlight is true when the target timeline's history is archived
	// but none of its WAL is yet, and nothing in the archive proves WAL lost:
	// a promotion archives NNNNNNNN.history at once and its first segment
	// only once it fills or archive_timeout passes. The path then stops on
	// the parent (State warning, Stop unknown-limited) and the backup is
	// judged on the parent timeline up to the switch.
	SwitchInFlight bool
}

// ManifestCheck is the outcome of reading one backup's manifest.
type ManifestCheck struct {
	Inspected bool
	State     evidence.State
	Reason    string
	Pair      PairCheck
	Mismatch  string
}

// BackupReport is one backup with its chain, manifest and WAL evidence.
type BackupReport struct {
	Backup      Backup
	ArchiveID   string
	ChainState  evidence.State
	ChainReason string
	Manifest    ManifestCheck
	Path        RecoveryPath
	// Usable is true when the chain, every manifest it depends on and the
	// WAL to consistency are all present and verified, and the backup's WAL
	// reaches the target timeline (not necessarily its newest segment).
	Usable bool
}

// RecoveryWindow is the span a restore can reach: from the end of the oldest
// usable backup whose WAL is contiguous to the frontier, to the end of the
// last contiguous segment. Start is a backup's backup-timestamp-stop; the end
// is a WAL position, and EndReceipt is only the upload time of its segment.
type RecoveryWindow struct {
	OldestBackup  string
	Start         time.Time
	NewestBackup  string
	NewestStop    time.Time
	EndTimeline   uint32
	EndWAL        string
	EndLSN        string
	EndReceipt    time.Time
	UsableBackups int
	Assumptions   []string
}

// Report is the evidence of one stanza.
type Report struct {
	Stanza, ArchiveStanza string
	State                 evidence.State
	Reason                string
	BackupInfo            PairCheck
	ArchiveInfo           PairCheck
	Encrypted             bool
	Database              *PGHistory
	ArchiveIDs            []string
	SegmentSize           int64
	SegmentSizeSource     SegmentSizeSource
	Backups               []BackupReport
	WAL                   []ArchiveWAL
	Timelines             []TimelineHistory
	Window                *RecoveryWindow
	Findings              []Finding
	// Retention is the comparison with the configured retention (added by
	// the agent, ApplyRetention); nil without an expectation.
	Retention *RetentionReport `json:",omitempty"`
}

// Options are the inputs of Analyze besides the listing.
type Options struct {
	// Stanza holds the backups (backup/<Stanza>/).
	Stanza string
	// ArchiveStanza holds the WAL (archive/<ArchiveStanza>/); Stanza when empty.
	ArchiveStanza string
	// Passphrase is the repository passphrase (repo-cipher-pass); nil for an
	// unencrypted repository. It opens the info files only.
	Passphrase []byte
	// MaxManifests caps the newest backups whose manifest is read;
	// DefaultMaxManifests when zero.
	MaxManifests int
	// MaxManifestScanBytes caps the bytes of manifests one scan reads;
	// DefaultMaxManifestScanBytes when zero.
	MaxManifestScanBytes int64
	// Previous is the report of the last complete scan, for the gap lifecycle.
	Previous *Report
	// Generation numbers this scan (from 1).
	Generation uint64
}

// Analyze reads one stanza from a complete listing of the repository: its
// info files (with .copy fallback), the manifests of the newest backups and
// of every backup they depend on, and the timeline history files; the WAL is
// judged from names alone. Keys are relative to the repository root. Every
// read is bounded; a failed read makes the affected evidence unknown, never
// healthy. It is NewCollector, Add for each object, then Collector.Analyze.
func Analyze(ctx context.Context, reader store.Reader, objects []store.Object, options Options) Report {
	collector := NewCollector(options)
	for _, object := range objects {
		collector.Add(object)
	}
	return collector.Analyze(ctx, reader)
}

// maxMetadataObjects bounds the info files and manifests one collector keeps
// from the listing: a main file and its .copy per backup, plus the two info
// pairs.
const maxMetadataObjects = 2*MaxBackups + 4

// Collector gathers what Analyze needs from a listing streamed one object at
// a time: the info files and manifests of the stanza (bounded by
// maxMetadataObjects) and the compact WAL of the archive stanza. It keeps no
// other key, so a scan of a repository with millions of WAL segments holds
// only their ranges.
type Collector struct {
	options                     Options
	archiveStanza               string
	backupPrefix, archivePrefix string
	listed                      map[string]store.Object
	listedTruncated             bool
	wal                         *WALCollector
}

// NewCollector returns an empty collector for options' stanzas.
func NewCollector(options Options) *Collector {
	archiveStanza := options.ArchiveStanza
	if archiveStanza == "" {
		archiveStanza = options.Stanza
	}
	return &Collector{
		options: options, archiveStanza: archiveStanza,
		backupPrefix: "backup/" + options.Stanza + "/", archivePrefix: "archive/" + archiveStanza + "/",
		listed: make(map[string]store.Object), wal: NewWALCollector(),
	}
}

// Add folds one listed object in; objects outside the two stanza
// directories are ignored.
func (c *Collector) Add(object store.Object) {
	keep := func() {
		if len(c.listed) >= maxMetadataObjects {
			c.listedTruncated = true
			return
		}
		c.listed[object.Key] = object
	}
	if strings.HasPrefix(object.Key, c.backupPrefix) && (strings.HasSuffix(object.Key, "/backup.info") || strings.HasSuffix(object.Key, "/backup.info.copy") || strings.HasSuffix(object.Key, "/backup.manifest") || strings.HasSuffix(object.Key, "/backup.manifest.copy")) {
		keep()
	}
	if strings.HasPrefix(object.Key, c.archivePrefix) {
		if strings.HasSuffix(object.Key, "/archive.info") || strings.HasSuffix(object.Key, "/archive.info.copy") {
			keep()
		}
		c.wal.Add(object)
	}
}

// Analyze reads the collected stanza; see the package function Analyze.
func (c *Collector) Analyze(ctx context.Context, reader store.Reader) Report {
	options, archiveStanza, archivePrefix, listed, collector := c.options, c.archiveStanza, c.archivePrefix, c.listed, c.wal
	report := Report{Stanza: options.Stanza, ArchiveStanza: archiveStanza}
	if !evidencev1alpha1.ValidPGBackRestStanza(options.Stanza) || !evidencev1alpha1.ValidPGBackRestStanza(archiveStanza) {
		report.State, report.Reason = evidence.Unknown, "stanza name is invalid"
		return report
	}
	if c.listedTruncated {
		report.State, report.Reason = evidence.Unknown, "too many info files and manifests to read"
		return report
	}
	findings := &findingSet{}

	backupInfo, backupPair := LoadBackupInfo(ctx, reader, listed, options.Stanza, options.Passphrase)
	report.BackupInfo = backupPair
	pairFindings(findings, backupPair, true)
	archiveInfo, archivePair := LoadArchiveInfo(ctx, reader, listed, archiveStanza, options.Passphrase)
	report.ArchiveInfo = archivePair
	pairFindings(findings, archivePair, true)

	if backupInfo == nil || archiveInfo == nil {
		report.State, report.Reason = evidence.Unknown, "backup.info or archive.info cannot be used"
		if pairFormatUnsupported(backupPair) || pairFormatUnsupported(archivePair) {
			report.Reason = "repository format is not supported (only format 5 is read)"
		}
		report.Findings = findings.sorted()
		return report
	}
	report.Encrypted = backupInfo.Encrypted || archiveInfo.Encrypted
	current := backupInfo.Current
	report.Database = &current
	report.ArchiveIDs = archiveInfo.ArchiveIDs()

	report.SegmentSize, report.SegmentSizeSource = InferSegmentSize(backupInfo.Backups)
	if report.SegmentSize == 0 {
		findings.add(FindingWALUnknown, SeverityError, "backup/"+options.Stanza+"/backup.info", "no WAL segment size agrees with every backup's LSN range")
	}

	historyObjects, historyTruncated := collector.HistoryObjects()
	var previousWAL map[string]*ArchiveWAL
	if options.Previous != nil {
		previousWAL = make(map[string]*ArchiveWAL)
		for index := range options.Previous.WAL {
			previousWAL[options.Previous.WAL[index].ArchiveID] = &options.Previous.WAL[index]
		}
	}
	walByID := make(map[string]ArchiveWAL)
	historiesByID := make(map[string][]TimelineHistory)
	for _, archiveID := range report.ArchiveIDs {
		wal := collector.Finish(archiveStanza, archiveID, report.SegmentSize, previousWAL[archiveID], options.Generation)
		walByID[archiveID] = wal
		report.WAL = append(report.WAL, wal)
		histories := readHistories(ctx, reader, historyObjects, archiveStanza, archiveID, archiveInfo.cipherPass, report.SegmentSize)
		rejectChildWALBeforeSwitch(histories, wal.Ranges)
		historiesByID[archiveID] = histories
		report.Timelines = append(report.Timelines, histories...)
		walFindings(findings, wal, histories, archivePrefix+archiveID)
	}
	if historyTruncated {
		findings.add(FindingWALUnknown, SeverityError, archivePrefix, "too many timeline history files to read")
	}

	report.Backups = analyzeBackups(ctx, reader, listed, options, backupInfo, archiveInfo, walByID, historiesByID, historyTruncated, findings)
	report.Window = recoveryWindow(report.Backups, report.SegmentSize, report.SegmentSizeSource)
	report.Findings = findings.sorted()
	report.State, report.Reason = rollup(report)
	return report
}

func pairFormatUnsupported(pair PairCheck) bool {
	return pair.Main.Problem == ProblemFormatUnsupported || pair.Copy.Problem == ProblemFormatUnsupported
}

// pairFindings reports what reading a pair found. info is true for
// backup.info and archive.info, whose loss makes the stanza unreadable.
func pairFindings(findings *findingSet, pair PairCheck, info bool) {
	for _, check := range []FileCheck{pair.Main, pair.Copy} {
		switch check.Problem {
		case ProblemChecksum:
			findings.add(FindingChecksumMismatch, SeverityError, check.Key, "content does not match its backrest-checksum")
		case ProblemFormatUnsupported:
			findings.add(FindingRepositoryFormatUnsupported, SeverityError, check.Key, fmt.Sprintf("repository format %d is not supported; only format 5 is read", check.Format))
		case ProblemNone, ProblemMissing, ProblemUnreadable, ProblemTooLarge, ProblemMalformed, ProblemEncrypted, ProblemDecryption:
			// Reported below as InfoUnreadable when neither file can be used.
		}
	}
	if !pair.Usable() {
		if info && !pairFormatUnsupported(pair) {
			findings.add(FindingInfoUnreadable, SeverityError, pair.Main.Key, "neither the file nor its .copy can be used")
		}
		return
	}
	if !pair.Agree {
		findings.add(FindingCopyMismatch, SeverityWarning, pair.Main.Key, "the file and its .copy are not the same verified content")
	}
}

func walFindings(findings *findingSet, wal ArchiveWAL, histories []TimelineHistory, subject string) {
	for _, gap := range wal.Gaps {
		severity := SeverityWarning
		if gap.Status == GapConfirmed {
			severity = SeverityError
		}
		findings.add(FindingWALGap, severity, subject+"/"+gap.First, fmt.Sprintf("%d segment(s) missing on timeline %d, %s to %s (%s)", gap.Count, gap.Timeline, gap.First, gap.Last, gap.Status))
	}
	if wal.State == evidence.Unknown && wal.SegmentSize != 0 && len(wal.Ranges) > 0 {
		findings.add(FindingWALUnknown, SeverityError, subject, wal.Reason)
	}
	known := make(map[uint32]bool)
	for _, history := range histories {
		known[history.Timeline] = true
		if history.State != evidence.Healthy {
			findings.add(FindingWALUnknown, SeverityError, history.Key, history.Reason)
		}
	}
	for _, value := range wal.Ranges {
		if value.Timeline > 1 && !known[value.Timeline] {
			findings.add(FindingWALUnknown, SeverityError, fmt.Sprintf("%s/%08X.history", subject, value.Timeline), fmt.Sprintf("timeline %d has segments but no history file", value.Timeline))
		}
	}
}

func analyzeBackups(ctx context.Context, reader store.Reader, listed map[string]store.Object, options Options, info *BackupInfo, archive *ArchiveInfo, walByID map[string]ArchiveWAL, historiesByID map[string][]TimelineHistory, historyTruncated bool, findings *findingSet) []BackupReport {
	byLabel := make(map[string]int, len(info.Backups))
	reports := make([]BackupReport, len(info.Backups))
	for index, backup := range info.Backups {
		byLabel[backup.Label] = index
		reports[index] = BackupReport{Backup: backup}
	}
	// Chains.
	chains := newChainIndex(reports, byLabel)
	localOK := make([]bool, len(reports))
	for index := range reports {
		localOK[index] = checkChainLink(reports, byLabel, chains, index, info.History, findings)
	}
	for index, healthy := range chains.healthy(localOK) {
		if healthy {
			reports[index].ChainState, reports[index].ChainReason = evidence.Healthy, "prior and references are present and consistent"
		} else {
			reports[index].ChainState, reports[index].ChainReason = evidence.Unhealthy, "the backup or a backup it depends on has a broken chain"
		}
	}
	checkManifests(ctx, reader, listed, options, info, reports, chains, findings)
	// WAL paths.
	for index := range reports {
		backup := &reports[index]
		archiveID, ok := archiveIDFor(backup.Backup, info.History, archive.History)
		if !ok {
			backup.Path = unknownPath("no archive id in archive.info matches the backup's database")
			findings.add(FindingWALUnknown, SeverityError, backup.Backup.Label, backup.Path.Reason)
			continue
		}
		backup.ArchiveID = archiveID
		wal := walByID[archiveID]
		backup.Path = evaluatePath(backup.Backup, wal, historiesByID[archiveID], historyTruncated)
		if backup.Path.State != evidence.Healthy && backup.Path.Stop == CoverageGapLimited && !backup.Path.Consistent {
			findings.add(FindingWALGap, SeverityError, backup.Backup.Label, "WAL needed to make the backup consistent is missing")
		}
	}
	// Usability: the chain, every manifest the backup depends on, and WAL to
	// consistency on the target timeline.
	dependenciesVerified := foldDependencies(chains, true, func(verified bool, dependency int) bool {
		return verified && reports[dependency].Manifest.State == evidence.Healthy
	})
	for index := range reports {
		backup := &reports[index]
		// A switch in flight judges the backup on the parent timeline, up
		// to the switch.
		reachesTarget := backup.Path.FrontierTimeline == backup.Path.TargetTimeline || backup.Path.SwitchInFlight
		if backup.ChainState != evidence.Healthy || !backup.Path.Consistent || backup.Path.OffTarget || backup.Path.State == evidence.Unknown || !reachesTarget {
			continue
		}
		usable := backup.Manifest.State == evidence.Healthy && dependenciesVerified[index]
		if backup.Backup.Error != nil && *backup.Backup.Error {
			usable = false
		}
		backup.Usable = usable
	}
	return reports
}

// checkChainLink checks one backup's own prior and references.
func checkChainLink(reports []BackupReport, byLabel map[string]int, chains *chainIndex, index int, history []PGHistory, findings *findingSet) bool {
	backup := reports[index].Backup
	ok := true
	inconsistent := func(message string) {
		findings.add(FindingChainInconsistent, SeverityError, backup.Label, message)
		ok = false
	}
	if !slices.ContainsFunc(history, func(entry PGHistory) bool { return entry.ID == backup.DBID }) {
		inconsistent("db-id is not in backup.info's db:history")
	}
	_, full, _ := LabelType(backup.Label)
	if backup.Type == BackupFull {
		if backup.Prior != "" || len(backup.Reference) > 0 {
			inconsistent("a full backup has a prior or references")
		}
		return ok
	}
	if backup.Prior == "" {
		inconsistent("a differential or incremental backup has no prior")
		return false
	}
	priorIndex, exists := byLabel[backup.Prior]
	if !exists {
		findings.add(FindingMissingPrior, SeverityError, backup.Label, "backup-prior "+backup.Prior+" is not in backup.info")
		ok = false
	} else {
		prior := reports[priorIndex].Backup
		_, priorFull, _ := LabelType(prior.Label)
		if priorFull != full {
			inconsistent("the prior belongs to another full backup")
		}
		if backup.Type == BackupDifferential && prior.Type != BackupFull {
			inconsistent("the prior of a differential backup is not a full backup")
		}
		if prior.DBID != backup.DBID {
			inconsistent("the prior was taken of another database")
		}
	}
	complete := chains.complete[index]
	for _, reference := range backup.Reference {
		referenced, exists := byLabel[reference]
		if !exists {
			findings.add(FindingMissingReference, SeverityError, backup.Label, "backup-reference "+reference+" is not in backup.info")
			ok = false
			continue
		}
		// Ancestry is only decidable on a complete chain; a broken one is
		// already reported as MissingPrior.
		if complete && !chains.isAncestor(referenced, index) {
			inconsistent("backup-reference " + reference + " is not an ancestor of the backup")
		}
	}
	return ok
}

// checkManifests reads the manifests of the newest MaxManifests backups and
// of the backups of their chains, newest first, within two caps:
// MaxManifests plus DefaultMaxManifests manifests in all (room for the chains
// of the newest backups, not for an unbounded one), and MaxManifestScanBytes
// bytes as stored.
// Every backup past a cap keeps a manifest of unknown state, and one
// ManifestBudgetExceeded finding says which cap stopped the scan.
func checkManifests(ctx context.Context, reader store.Reader, listed map[string]store.Object, options Options, info *BackupInfo, reports []BackupReport, chains *chainIndex, findings *findingSet) {
	limit := options.MaxManifests
	if limit <= 0 {
		limit = DefaultMaxManifests
	}
	byteBudget := options.MaxManifestScanBytes
	if byteBudget <= 0 {
		byteBudget = DefaultMaxManifestScanBytes
	}
	maxReads := limit + DefaultMaxManifests
	order := make([]int, len(reports))
	for index := range order {
		order[index] = index
	}
	sort.SliceStable(order, func(i, j int) bool {
		left, right := reports[order[i]].Backup, reports[order[j]].Backup
		if !left.TimestampStop.Equal(right.TimestampStop) {
			return left.TimestampStop.After(right.TimestampStop)
		}
		return left.Label > right.Label
	})
	// Selection: the newest backups themselves, then the backups of their
	// chains, newest backup first, while the read cap allows.
	selected := make(map[int]bool)
	newest := order[:min(limit, len(order))]
	queue := slices.Clone(newest)
	for _, index := range newest {
		selected[index] = true
	}
	readCapped := false
chains:
	for _, index := range newest {
		for _, member := range chains.dependencies(index) {
			if selected[member] {
				continue
			}
			if len(queue) >= maxReads {
				readCapped = true
				break chains
			}
			selected[member] = true
			queue = append(queue, member)
		}
	}
	for index := range reports {
		if !selected[index] {
			reason := fmt.Sprintf("not read: beyond the newest %d backups", limit)
			if readCapped {
				reason = fmt.Sprintf("not read: the scan reads at most %d manifests", maxReads)
			}
			reports[index].Manifest = ManifestCheck{State: evidence.Unknown, Reason: reason}
		}
	}
	// Each pair is charged what the listing says it holds, or what was read
	// from it when that is more, so neither an understated listing nor a
	// file that fails early moves the budget.
	counter := &countingReader{Reader: reader}
	var spent int64
	byteCapped := false
	for _, index := range queue {
		key := ManifestKey(options.Stanza, reports[index].Backup.Label)
		listedSize := max(listed[key].Size, 0) + max(listed[key+".copy"].Size, 0)
		if spent+listedSize > byteBudget || spent >= byteBudget {
			byteCapped = true
			reports[index].Manifest = ManifestCheck{State: evidence.Unknown, Reason: fmt.Sprintf("not read: the scan reads at most %d bytes of manifests", byteBudget)}
			continue
		}
		before := counter.read
		reports[index].Manifest = checkManifest(ctx, counter, listed, options.Stanza, info, reports[index].Backup, findings)
		spent += max(listedSize, counter.read-before)
	}
	if readCapped {
		findings.add(FindingManifestBudgetExceeded, SeverityWarning, "backup/"+options.Stanza+"/", fmt.Sprintf("manifests not read past the scan's cap of %d manifests", maxReads))
	}
	if byteCapped {
		findings.add(FindingManifestBudgetExceeded, SeverityWarning, "backup/"+options.Stanza+"/", fmt.Sprintf("manifests not read past the scan's budget of %d bytes", byteBudget))
	}
}

// countingReader counts the bytes read through the bodies it opens, so the
// manifest byte budget holds even when the listing understates a size.
type countingReader struct {
	store.Reader
	read int64
}

func (r *countingReader) Open(ctx context.Context, request store.OpenRequest) (io.ReadCloser, error) {
	body, err := r.Reader.Open(ctx, request)
	if err != nil {
		return nil, err
	}
	return &countingBody{ReadCloser: body, counter: r}, nil
}

type countingBody struct {
	io.ReadCloser
	counter *countingReader
}

func (b *countingBody) Read(buffer []byte) (int, error) {
	read, err := b.ReadCloser.Read(buffer)
	b.counter.read += int64(read)
	return read, err
}

func checkManifest(ctx context.Context, reader store.Reader, listed map[string]store.Object, stanza string, info *BackupInfo, backup Backup, findings *findingSet) ManifestCheck {
	manifest, pair := LoadManifest(ctx, reader, listed, stanza, backup.Label, info)
	check := ManifestCheck{Inspected: true, Pair: pair}
	if !pair.Main.Present && !pair.Copy.Present {
		findings.add(FindingManifestMissing, SeverityError, backup.Label, "neither backup.manifest nor its .copy is listed")
		check.State, check.Reason = evidence.Unhealthy, "manifest is missing"
		return check
	}
	pairFindings(findings, pair, false)
	if manifest == nil {
		checksum := pair.Main.Problem == ProblemChecksum || pair.Copy.Problem == ProblemChecksum
		if checksum {
			check.State, check.Reason = evidence.Unhealthy, "manifest does not verify"
		} else {
			findings.add(FindingManifestUnreadable, SeverityError, backup.Label, "the manifest is present but cannot be read")
			check.State, check.Reason = evidence.Unknown, "manifest cannot be read"
		}
		return check
	}
	if mismatch := manifestDisagreement(manifest, backup, info.History); mismatch != "" {
		findings.add(FindingManifestMismatch, SeverityError, backup.Label, "the manifest contradicts backup.info: "+mismatch)
		check.State, check.Reason, check.Mismatch = evidence.Unhealthy, "manifest contradicts backup.info", mismatch
		return check
	}
	check.State, check.Reason = evidence.Healthy, "manifest verifies and agrees with backup.info"
	return check
}

// archiveIDFor maps a backup to the archive id its WAL is under, as
// pgBackRest's infoArchiveIdHistoryMatch does: the backup.info history entry
// of its db-id, then the archive.info entry of the same version and system id.
func archiveIDFor(backup Backup, backupHistory, archiveHistory []PGHistory) (string, bool) {
	index := slices.IndexFunc(backupHistory, func(entry PGHistory) bool { return entry.ID == backup.DBID })
	if index < 0 {
		return "", false
	}
	database := backupHistory[index]
	for position := len(archiveHistory) - 1; position >= 0; position-- {
		entry := archiveHistory[position]
		if entry.SystemID == database.SystemID && entry.Version == database.Version {
			return entry.ArchiveID(), true
		}
	}
	return "", false
}

func unknownPath(reason string) RecoveryPath {
	return RecoveryPath{State: evidence.Unknown, Reason: reason, Stop: CoverageUnknownLimited}
}

// evaluatePath walks a backup's WAL from its archive-start, across the
// timeline switches recorded in the target timeline's history file, to the
// end of the contiguous run on the target timeline. Positions are global
// segment numbers; the timeline only says which file holds a position.
//
// PostgreSQL's failover rule decides which file: for a switchpoint L and
// S = L / segsize, the promotion copies the parent's segment S up to L into
// the child's segment S, or creates a fresh S when L is on a segment
// boundary (XLogInitNewTimeline); the parent's own S is renamed .partial,
// which recovery never reads (CleanupAfterArchiveRecovery); and recovery
// reads each segment from the newest timeline whose begin segment is not
// after it (XLogFileReadAnyTLI). So the parent is needed for segments < S
// and the child for segments >= S: a parent leg ends at SwitchPosition-1.
func evaluatePath(backup Backup, wal ArchiveWAL, histories []TimelineHistory, historyTruncated bool) RecoveryPath {
	if backup.ArchiveStart == "" || backup.ArchiveStop == "" {
		return unknownPath("the backup records no WAL range")
	}
	if wal.SegmentSize == 0 {
		return unknownPath("WAL segment size is not known")
	}
	if wal.State == evidence.Unknown {
		return unknownPath("WAL evidence is unknown: " + wal.Reason)
	}
	startTimeline, startPosition, errStart := ParseWALName(backup.ArchiveStart, wal.SegmentSize)
	stopTimeline, stopPosition, errStop := ParseWALName(backup.ArchiveStop, wal.SegmentSize)
	stopLSN, okLSN := ParseLSN(backup.LSNStop)
	if errStart != nil || errStop != nil || stopTimeline != startTimeline || stopPosition < startPosition {
		return unknownPath("the backup's WAL range is not on one timeline")
	}
	target := startTimeline
	for _, value := range wal.Ranges {
		target = max(target, value.Timeline)
	}
	for _, history := range histories {
		target = max(target, history.Timeline)
	}
	path := RecoveryPath{TargetTimeline: target, StartWAL: backup.ArchiveStart, ConsistentWAL: backup.ArchiveStop}
	// childEmpty: the target timeline is known only from its history file.
	childEmpty := target != startTimeline && !slices.ContainsFunc(wal.Ranges, func(value WALRange) bool { return value.Timeline == target })
	type leg struct {
		timeline uint32
		last     uint64 // last position read from this timeline; MaxUint64 on the target
	}
	legs := []leg{{timeline: target, last: math.MaxUint64}}
	offTarget := ""
	if target != startTimeline {
		if historyTruncated {
			return withTarget(unknownPath("timeline history files were not all read"), path)
		}
		index := slices.IndexFunc(histories, func(value TimelineHistory) bool { return value.Timeline == target })
		if index < 0 || histories[index].State != evidence.Healthy {
			return withTarget(unknownPath(fmt.Sprintf("the history of timeline %d is missing or unreadable", target)), path)
		}
		edges := histories[index].Edges
		first := slices.IndexFunc(edges, func(edge HistoryEdge) bool { return edge.Parent == startTimeline })
		// A backup can follow the child when its stop LSN is not after the
		// switchpoint. pg_backup_stop returns the end of the backup-end
		// record, so recovery is consistent once it has replayed the bytes
		// before the stop LSN; the history's switchpoint is the first byte
		// of the child, and the parent holds every byte before it
		// (tliOfPointInHistory: begin <= ptr < end). PostgreSQL checks the
		// same thing at startup with minRecoveryPoint - 1 against the
		// requested timeline (InitWalRecovery, "requested timeline does not
		// contain minimum recovery point"), so stop == switchpoint is on the
		// child's history: the backup branches before it only when stop >
		// switchpoint. barmancloud.branchBeforeBackup follows the same rule.
		switch {
		case first < 0:
			offTarget = fmt.Sprintf("timeline %d is not an ancestor of timeline %d", startTimeline, target)
		case branchesBeforeStop(edges[first].SwitchLSN, stopLSN, okLSN):
			offTarget = fmt.Sprintf("timeline %d branched before the backup reached consistency", edges[first].Child)
		}
		legs = legs[:0]
		if offTarget != "" {
			// The backup cannot be restored to the newest timeline; it can be
			// rolled forward on its own, which a restore must then name
			// (recovery_target_timeline).
			path.TargetTimeline = startTimeline
			legs = append(legs, leg{timeline: startTimeline, last: math.MaxUint64})
		} else {
			for _, edge := range edges[first:] {
				if edge.SwitchPosition == 0 {
					return withTarget(unknownPath("timeline switch at the first segment"), path)
				}
				legs = append(legs, leg{timeline: edge.Parent, last: edge.SwitchPosition - 1})
			}
			legs = append(legs, leg{timeline: target, last: math.MaxUint64})
		}
	}
	childEmpty = childEmpty && offTarget == ""
	position := startPosition
	for index, current := range legs {
		path.Timelines = append(path.Timelines, current.timeline)
		if current.last != math.MaxUint64 && position > current.last {
			continue
		}
		run, found := rangeAt(wal.Ranges, current.timeline, position)
		if !found && childEmpty && current.timeline == target {
			// The parent legs reached the switch; the child has no WAL yet.
			return switchInFlight(path, target, position, stopPosition, wal.SegmentSize)
		}
		if !found {
			// On a later leg the frontier stays where the previous leg ended
			// (SwitchPosition-1 on the parent): the child has nothing yet.
			if path.FrontierTimeline == 0 {
				path.FrontierTimeline = current.timeline
				if position > 0 {
					path.FrontierPosition = position - 1
					path.FrontierWAL, _ = WALName(current.timeline, position-1, wal.SegmentSize)
				}
			}
			path.State = missingState(wal.Gaps, current.timeline, position)
			path.Stop = gapStop(path.State)
			path.Reason = fmt.Sprintf("segment %s is not archived", mustWALName(current.timeline, position, wal.SegmentSize))
			path.Consistent = position > stopPosition
			return inconsistentPath(path)
		}
		path.FrontierTimeline, path.FrontierPosition, path.FrontierWAL, path.FrontierReceipt = current.timeline, run.End, run.Last, run.EndReceipt
		if current.last == math.MaxUint64 {
			break
		}
		if run.End < current.last && childEmpty && index == len(legs)-2 && run.End >= stopPosition && !rangeAfter(wal.Ranges, current.timeline, run.End) {
			// The parent's tail before the switch is not archived yet and the
			// child has nothing: nothing proves the parent closed, so its
			// last segments may still be on their way.
			return switchInFlight(path, target, run.End+1, stopPosition, wal.SegmentSize)
		}
		if run.End < current.last {
			path.State = missingState(wal.Gaps, current.timeline, run.End+1)
			path.Stop = gapStop(path.State)
			path.Reason = fmt.Sprintf("segment %s is not archived; the backup recovers only to %s on timeline %d", mustWALName(current.timeline, run.End+1, wal.SegmentSize), run.Last, current.timeline)
			path.Consistent = run.End >= stopPosition
			return inconsistentPath(path)
		}
		// Recovery leaves this timeline at the switch segment: whatever the
		// parent holds from S on (a .partial, or another branch) is not read.
		path.FrontierPosition, path.FrontierReceipt = current.last, time.Time{}
		if run.End == current.last {
			path.FrontierReceipt = run.EndReceipt
		}
		path.FrontierWAL, _ = WALName(current.timeline, current.last, wal.SegmentSize)
		position = current.last + 1
	}
	path.Consistent = path.FrontierPosition >= stopPosition
	if !path.Consistent {
		path.Stop, path.State, path.Reason = CoverageGapLimited, evidence.Unhealthy, "WAL to consistency is not archived"
		return path
	}
	path.Stop = CoverageFrontier
	path.State, path.Reason = evidence.Healthy, "WAL is contiguous from the backup to the newest segment of the target timeline"
	if gap, ok := gapAfter(wal.Gaps, path.FrontierTimeline, path.FrontierPosition); ok {
		path.Reason = fmt.Sprintf("WAL is contiguous to %s; a %s gap follows", path.FrontierWAL, gap.Status)
		path.State = evidence.Warning
		if gap.Status == GapConfirmed {
			path.State = evidence.Unhealthy
		}
		path.Stop = gapStop(path.State)
	}
	if offTarget != "" {
		path.OffTarget = true
		path.Reason = offTarget + "; the backup recovers only on its own timeline, which a restore must name"
		if path.State == evidence.Healthy {
			path.State = evidence.Warning
		}
	}
	return path
}

// branchesBeforeStop reports whether a timeline that branched at switchLSN
// left a backup that stopped at stopLSN behind (see evaluatePath); an
// unknown stop LSN branches, which keeps the backup off the child.
func branchesBeforeStop(switchLSN, stopLSN uint64, stopKnown bool) bool {
	return !stopKnown || stopLSN > switchLSN
}

// switchInFlight ends a walk whose next position, missing, is the switch
// segment of a target timeline that has no archived WAL yet, or the parent's
// tail just before it. The frontier stays where the walk was.
func switchInFlight(path RecoveryPath, target uint32, missing, stopPosition uint64, segmentSize int64) RecoveryPath {
	path.SwitchInFlight = true
	path.Stop, path.State = CoverageUnknownLimited, evidence.Warning
	path.Consistent = path.FrontierTimeline != 0 && path.FrontierPosition >= stopPosition && missing > stopPosition
	if path.Consistent {
		path.Reason = fmt.Sprintf("timeline %d's history is archived but its WAL is not yet (the switch may be in flight); the backup recovers to %s on timeline %d", target, path.FrontierWAL, path.FrontierTimeline)
	} else {
		path.Reason = fmt.Sprintf("timeline %d's history is archived but its WAL is not yet (the switch may be in flight); segment %s, which the backup needs to reach consistency on it, is not archived yet", target, mustWALName(target, missing, segmentSize))
	}
	return path
}

// rangeAfter reports whether the timeline has archived WAL after position.
func rangeAfter(ranges []WALRange, timeline uint32, position uint64) bool {
	return slices.ContainsFunc(ranges, func(value WALRange) bool { return value.Timeline == timeline && value.End > position })
}

// inconsistentPath makes a walk that stopped before the backup's
// archive-stop unhealthy whatever the gap: pgBackRest finishes a backup only
// once that WAL is archived, so it is lost, not in flight.
func inconsistentPath(path RecoveryPath) RecoveryPath {
	if !path.Consistent {
		path.State, path.Stop = evidence.Unhealthy, CoverageGapLimited
		path.Reason = "WAL the backup needs to reach consistency is missing: " + path.Reason
		if index := strings.Index(path.Reason, "; the backup recovers only"); index >= 0 {
			path.Reason = path.Reason[:index]
		}
	}
	return path
}

// gapStop is the stop of a walk that ended at a missing position of the
// given state: a candidate gap (warning) may still fill, as the wire
// contract's candidate-limited says; anything else is a confirmed gap or
// proven loss.
func gapStop(state evidence.State) CoverageStop {
	if state == evidence.Warning {
		return CoverageCandidateLimited
	}
	return CoverageGapLimited
}

// missingState is the state of a walk that stopped at a missing position: a
// candidate gap may still fill (warning); a confirmed gap, or WAL that ends
// before a timeline switch it must reach, is unhealthy.
func missingState(gaps []WALGap, timeline uint32, position uint64) evidence.State {
	for _, gap := range gaps {
		if gap.Timeline == timeline && gap.Start <= position && position <= gap.End {
			if gap.Status == GapCandidate {
				return evidence.Warning
			}
			return evidence.Unhealthy
		}
	}
	return evidence.Unhealthy
}

func withTarget(path, template RecoveryPath) RecoveryPath {
	path.TargetTimeline, path.StartWAL, path.ConsistentWAL = template.TargetTimeline, template.StartWAL, template.ConsistentWAL
	return path
}

func mustWALName(timeline uint32, position uint64, segmentSize int64) string {
	name, err := WALName(timeline, position, segmentSize)
	if err != nil {
		return "beyond the WAL name range"
	}
	return name
}

func rangeAt(ranges []WALRange, timeline uint32, position uint64) (WALRange, bool) {
	for _, value := range ranges {
		if value.Timeline == timeline && value.Start <= position && position <= value.End {
			return value, true
		}
	}
	return WALRange{}, false
}

func gapAfter(gaps []WALGap, timeline uint32, position uint64) (WALGap, bool) {
	for _, gap := range gaps {
		if gap.Timeline == timeline && position != math.MaxUint64 && gap.Start == position+1 {
			return gap, true
		}
	}
	return WALGap{}, false
}

// recoveryWindow takes the newest usable backup's frontier as the end of the
// window, and the oldest usable backup that reaches the same frontier as its
// start: backups behind a gap recover only up to the gap.
func recoveryWindow(backups []BackupReport, segmentSize int64, source SegmentSizeSource) *RecoveryWindow {
	var usable []BackupReport
	for _, backup := range backups {
		if backup.Usable {
			usable = append(usable, backup)
		}
	}
	if len(usable) == 0 {
		return nil
	}
	newest := usable[0]
	for _, backup := range usable[1:] {
		if backup.Backup.TimestampStop.After(newest.Backup.TimestampStop) || (backup.Backup.TimestampStop.Equal(newest.Backup.TimestampStop) && backup.Backup.Label > newest.Backup.Label) {
			newest = backup
		}
	}
	window := &RecoveryWindow{
		NewestBackup: newest.Backup.Label, NewestStop: newest.Backup.TimestampStop,
		EndTimeline: newest.Path.FrontierTimeline, EndWAL: newest.Path.FrontierWAL, EndReceipt: newest.Path.FrontierReceipt,
		OldestBackup: newest.Backup.Label, Start: newest.Backup.TimestampStop,
	}
	// #nosec G115 -- segmentSize is a valid WAL segment size here (a usable backup has a path).
	endLSN := (newest.Path.FrontierPosition + 1) * uint64(segmentSize)
	window.EndLSN = fmt.Sprintf("%X/%X", endLSN>>32, endLSN&0xFFFFFFFF)
	for _, backup := range usable {
		if backup.Path.FrontierTimeline != newest.Path.FrontierTimeline || backup.Path.FrontierPosition != newest.Path.FrontierPosition {
			continue
		}
		window.UsableBackups++
		if backup.Backup.TimestampStop.Before(window.Start) || (backup.Backup.TimestampStop.Equal(window.Start) && backup.Backup.Label < window.OldestBackup) {
			window.OldestBackup, window.Start = backup.Backup.Label, backup.Backup.TimestampStop
		}
	}
	window.Assumptions = []string{"the window ends at the end of the last contiguous segment; its time is the segment's upload time, not a commit time"}
	if source == SegmentSizeAssumed {
		window.Assumptions = append(window.Assumptions, "the WAL segment size is assumed to be the 16 MiB default: the backups' LSNs do not decide it")
	}
	return window
}

func rollup(report Report) (evidence.State, string) {
	if len(report.Backups) == 0 {
		return evidence.Unhealthy, "backup.info lists no backup"
	}
	if report.SegmentSize == 0 {
		return evidence.Unknown, "WAL segment size cannot be established"
	}
	if report.Window == nil {
		for _, backup := range report.Backups {
			if backup.Path.State == evidence.Unknown || backup.Manifest.State == evidence.Unknown {
				return evidence.Unknown, "no backup is proven usable, and some evidence is unknown"
			}
		}
		return evidence.Unhealthy, "no backup has a complete chain, manifests and WAL to consistency"
	}
	for _, finding := range report.Findings {
		if finding.Severity == SeverityError {
			return evidence.Warning, "a recovery window exists; some backups or files have errors"
		}
	}
	if len(report.Findings) > 0 {
		return evidence.Warning, "a recovery window exists; see the warnings"
	}
	for _, backup := range report.Backups {
		if backup.Usable && backup.Path.SwitchInFlight {
			return evidence.Warning, "a recovery window exists; a timeline switch is not archived yet"
		}
	}
	return evidence.Healthy, "a recovery window exists and every check passed"
}

type findingSet struct {
	values []Finding
	seen   map[string]bool
}

func (s *findingSet) add(code FindingCode, severity Severity, subject, message string) {
	if s.seen == nil {
		s.seen = make(map[string]bool)
	}
	identity := string(code) + "\x00" + subject + "\x00" + message
	if s.seen[identity] {
		return
	}
	s.seen[identity] = true
	s.values = append(s.values, Finding{Code: code, Severity: severity, Subject: subject, Message: message})
}

func (s *findingSet) sorted() []Finding {
	result := slices.Clone(s.values)
	sort.Slice(result, func(i, j int) bool {
		if result[i].Code != result[j].Code {
			return result[i].Code < result[j].Code
		}
		if result[i].Subject != result[j].Subject {
			return result[i].Subject < result[j].Subject
		}
		return result[i].Message < result[j].Message
	})
	return result
}
