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
	"math"
	"slices"
	"strings"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// This file compares what a stanza holds with the retention its plugin
// configures (repo-retention-*), as pgBackRest 2.60.0's expire command
// applies it (src/command/expire/expire.c, src/config/load.c):
//
//   - expire runs at the end of every backup, never on its own (neither
//     plugin schedules it), so the stanza is judged as of the stop time of
//     its newest backup, not as of the scan;
//   - count-based full retention keeps the newest repo-retention-full full
//     backups; time-based keeps every full backup that stopped within the
//     window and the newest one that stopped before it, with their
//     dependents;
//   - repo-retention-diff keeps the newest that many full-or-differential
//     backups (a full counts as a differential) and expires the older
//     differentials with their incrementals;
//   - WAL is kept, from the archive start of the archive-retention backup
//     (the repo-retention-archive-th newest backup of repo-retention-archive-
//     type) onwards, and, for each older backup, from its archive start to
//     its archive stop; repo-retention-archive defaults to
//     repo-retention-full (count), to the full backups kept (time), or to
//     repo-retention-diff (archive type diff); WAL is never expired when it
//     resolves to nothing, or while fewer backups exist than it names;
//   - without repo-retention-full nothing is expired and pgBackRest warns
//     on every command that the repository may run out of space.
//
// The agent never runs expire: it reads what expire left behind.

// The retention finding codes (stable; extend, never rename).
const (
	// FindingRetentionNotConfigured: repo-retention-full is not set, so
	// pgBackRest never expires a full backup.
	FindingRetentionNotConfigured FindingCode = "RetentionNotConfigured"
	// FindingRetentionNotApplied: the stanza holds what the retention would
	// have expired at its newest backup (a full or differential backup, or
	// WAL before the archive-retention backup): expire is not running, or
	// not with this retention.
	FindingRetentionNotApplied FindingCode = "RetentionNotApplied"
	// FindingRetentionShortfall: the stanza lacks what the retention keeps
	// (a full backup an earlier scan saw, or the WAL of a backup from its
	// archive start): something other than this retention removed it.
	FindingRetentionShortfall FindingCode = "RetentionShortfall"
)

// RetentionFullType is repo-retention-full-type.
type RetentionFullType string

// The full retention types.
const (
	RetentionCount RetentionFullType = "count"
	RetentionTime  RetentionFullType = "time"
)

// MaxRetention is the upper bound pgBackRest (and both plugins' CRDs) put
// on repo-retention-full, -diff and -archive: the contract's.
const MaxRetention = evidencev1alpha1.MaxPGBackRestRetention

// maxLostBackups bounds the full backups a stanza remembers as lost.
const maxLostBackups = 64

// Retention is the expectation: the repo-retention-* options of the
// repository the agent reads. A zero count is an unset option; an empty
// type is pgBackRest's default (count, full).
type Retention struct {
	// Full is repo-retention-full: a count of full backups, or days when
	// FullType is time.
	Full        int
	FullType    RetentionFullType
	Diff        int
	Archive     int
	ArchiveType BackupType
}

// Validate checks the expectation against pgBackRest's own bounds (the
// contract's ValidatePGBackRestRetention, shared with the operator and the
// agent's settings).
func (r Retention) Validate() error {
	return evidencev1alpha1.ValidatePGBackRestRetention(r.Full, string(r.FullType), r.Diff, r.Archive, string(r.ArchiveType))
}

// withDefaults fills the types pgBackRest defaults.
func (r Retention) withDefaults() Retention {
	if r.FullType == "" {
		r.FullType = RetentionCount
	}
	if r.ArchiveType == "" {
		r.ArchiveType = BackupFull
	}
	return r
}

// LostBackup is a full backup an earlier scan saw that the retention keeps
// and the stanza no longer holds.
type LostBackup struct {
	Label string
	Stop  time.Time
}

// RetentionReport is the outcome of the comparison.
type RetentionReport struct {
	// Expectation is the retention compared against, defaults filled.
	Expectation Retention
	State       evidence.State
	Reason      string
	// The backups backup.info lists, by type.
	FullBackups, DifferentialBackups, IncrementalBackups int
	// EvaluatedAt is the stop time of the newest backup: the last time
	// pgBackRest applied the retention. Zero without a backup.
	EvaluatedAt time.Time
	// WindowStart is EvaluatedAt minus Full days (time-based only).
	WindowStart time.Time
	// ArchiveRetention is the repo-retention-archive expire applies, in
	// backups of ArchiveRetentionType; 0 when WAL is never expired.
	ArchiveRetention     int
	ArchiveRetentionType BackupType
	// Lost are the full backups the retention keeps that are gone (only
	// tracked while repo-retention-full is set; forgotten on restart).
	Lost     []LostBackup
	Findings []Finding
}

// ApplyRetention compares the report with want and records the result
// (Report.Retention, and its findings in Report.Findings). previous is the
// report of the last complete scan of the stanza, if any: the full backups
// it held tell a lost backup from one that never existed.
func (r *Report) ApplyRetention(want Retention, previous *Report) {
	result := EvaluateRetention(*r, want, previous)
	for _, finding := range result.Findings {
		r.AddFinding(finding)
	}
	r.Retention = &result
}

type retentionEvaluation struct {
	report   Report
	want     Retention
	result   RetentionReport
	findings *findingSet
	unknown  string
	// walSteps counts the steps of the WAL walk of walBeyond, so a test
	// can pin that it is bounded by spans, not by segments.
	walSteps int
}

// EvaluateRetention compares what report shows with want. It reads nothing
// and does not modify report.
func EvaluateRetention(report Report, want Retention, previous *Report) RetentionReport {
	return evaluateRetention(report, want, previous).result
}

func evaluateRetention(report Report, want Retention, previous *Report) *retentionEvaluation {
	want = want.withDefaults()
	e := &retentionEvaluation{report: report, want: want, findings: &findingSet{}}
	e.result = RetentionReport{Expectation: want, ArchiveRetentionType: want.ArchiveType}
	if want.Full == 0 {
		e.findings.add(FindingRetentionNotConfigured, SeverityWarning, "backup/"+report.Stanza+"/",
			"repo-retention-full is not set: pgBackRest never expires a full backup, and the repository grows until it is full")
	}
	switch {
	case !report.infoReadable():
		e.unknown = "backup.info cannot be used"
	case len(report.Backups) == 0:
		e.unknown = "backup.info lists no backup"
	default:
		e.evaluate(previous)
	}
	e.result.Findings = e.findings.sorted()
	e.result.State, e.result.Reason = e.rollup()
	return e
}

func (e *retentionEvaluation) evaluate(previous *Report) {
	backups := slices.Clone(e.report.Backups)
	slices.SortFunc(backups, func(left, right BackupReport) int { return strings.Compare(left.Backup.Label, right.Backup.Label) })
	var fulls []Backup
	for _, backup := range backups {
		switch backup.Backup.Type {
		case BackupFull:
			e.result.FullBackups++
			fulls = append(fulls, backup.Backup)
		case BackupDifferential:
			e.result.DifferentialBackups++
		case BackupIncremental:
			e.result.IncrementalBackups++
		}
		if backup.Backup.TimestampStop.After(e.result.EvaluatedAt) {
			e.result.EvaluatedAt = backup.Backup.TimestampStop.UTC()
		}
	}
	if e.want.Full > 0 && e.want.FullType == RetentionTime {
		e.result.WindowStart = e.result.EvaluatedAt.Add(-time.Duration(e.want.Full) * 24 * time.Hour)
	}
	expired := e.expiredFulls(fulls)
	expiredSet := make(map[string]bool, len(expired))
	for _, label := range expired {
		expiredSet[label] = true
		e.findings.add(FindingRetentionNotApplied, SeverityWarning, label, e.fullNotAppliedMessage())
	}
	notApplied := len(expired) > 0
	if e.want.Diff > 0 {
		var list []Backup
		for _, backup := range backups {
			if backup.Backup.Type == BackupFull || backup.Backup.Type == BackupDifferential {
				list = append(list, backup.Backup)
			}
		}
		for index := 0; index < len(list)-e.want.Diff; index++ {
			_, full, _ := LabelType(list[index].Label)
			if list[index].Type != BackupDifferential || expiredSet[full] {
				continue
			}
			notApplied = true
			e.findings.add(FindingRetentionNotApplied, SeverityWarning, list[index].Label,
				fmt.Sprintf("differential backup older than the newest %d full or differential backups (repo-retention-diff): expire would have removed it", e.want.Diff))
		}
	}
	e.lostFulls(fulls, previous)
	e.archive(backups, fulls, expired, notApplied)
}

// expiredFulls returns the full backups the retention would have expired at
// the newest backup, oldest first.
func (e *retentionEvaluation) expiredFulls(fulls []Backup) []string {
	if e.want.Full == 0 {
		return nil
	}
	keep := e.keptFrom(fulls)
	labels := make([]string, 0, keep)
	for _, backup := range fulls[:keep] {
		labels = append(labels, backup.Label)
	}
	return labels
}

// keptFrom is the index of the oldest full backup the retention keeps in
// fulls (sorted oldest first); len(fulls) keeps none, 0 keeps all.
func (e *retentionEvaluation) keptFrom(fulls []Backup) int {
	if e.want.Full == 0 {
		return 0
	}
	if e.want.FullType == RetentionCount {
		return max(len(fulls)-e.want.Full, 0)
	}
	// Time: from the newest full backup that stopped before the window.
	for index := len(fulls) - 1; index >= 0; index-- {
		if fulls[index].TimestampStop.Before(e.result.WindowStart) {
			return index
		}
	}
	return 0
}

func (e *retentionEvaluation) fullNotAppliedMessage() string {
	if e.want.FullType == RetentionTime {
		return fmt.Sprintf("full backup older than the %d-day window and than the newest full backup before it (repo-retention-full-type=time): expire would have removed it", e.want.Full)
	}
	return fmt.Sprintf("full backup beyond the newest %d (repo-retention-full): expire would have removed it", e.want.Full)
}

// lostFulls carries the lost full backups forward: those of the previous
// scan that are gone now, plus those the previous scan had already lost,
// as long as the retention still keeps them.
func (e *retentionEvaluation) lostFulls(fulls []Backup, previous *Report) {
	if e.want.Full == 0 || previous == nil || !previous.infoReadable() {
		return
	}
	present := make(map[string]bool, len(fulls))
	for _, backup := range fulls {
		present[backup.Label] = true
	}
	candidates := make(map[string]LostBackup)
	for _, backup := range previous.Backups {
		if backup.Backup.Type == BackupFull && !present[backup.Backup.Label] {
			candidates[backup.Backup.Label] = LostBackup{Label: backup.Backup.Label, Stop: backup.Backup.TimestampStop.UTC()}
		}
	}
	if previous.Retention != nil {
		for _, lost := range previous.Retention.Lost {
			if !present[lost.Label] {
				candidates[lost.Label] = lost
			}
		}
	}
	if len(candidates) == 0 {
		return
	}
	combined := slices.Clone(fulls)
	for _, lost := range candidates {
		combined = append(combined, Backup{Label: lost.Label, Type: BackupFull, TimestampStop: lost.Stop})
	}
	slices.SortFunc(combined, func(left, right Backup) int { return strings.Compare(left.Label, right.Label) })
	for _, backup := range combined[e.keptFrom(combined):] {
		lost, ok := candidates[backup.Label]
		if !ok {
			continue
		}
		e.result.Lost = append(e.result.Lost, lost)
	}
	if len(e.result.Lost) > maxLostBackups {
		e.result.Lost = e.result.Lost[len(e.result.Lost)-maxLostBackups:]
	}
	for _, lost := range e.result.Lost {
		e.findings.add(FindingRetentionShortfall, SeverityError, lost.Label, e.lostMessage())
	}
}

func (e *retentionEvaluation) lostMessage() string {
	if e.want.FullType == RetentionTime {
		return fmt.Sprintf("full backup seen by an earlier scan is gone, although the %d-day retention keeps it", e.want.Full)
	}
	return fmt.Sprintf("full backup seen by an earlier scan is gone, although repo-retention-full=%d keeps it", e.want.Full)
}

// archiveRetention is the repo-retention-archive expire applies, as
// src/config/load.c and expireTimeBasedBackup default it; 0 never expires.
func (e *retentionEvaluation) archiveRetention(fulls []Backup, expired []string) int {
	if e.want.Archive > 0 {
		return e.want.Archive
	}
	switch e.want.ArchiveType {
	case BackupFull:
		switch {
		case e.want.Full == 0:
			return 0
		case e.want.FullType == RetentionCount:
			return e.want.Full
		case e.keptFrom(fulls) == 0 && (len(fulls) == 0 || !fulls[0].TimestampStop.Before(e.result.WindowStart)):
			// Time-based retention not met: nothing expired, and the
			// archive retention is left unset.
			return 0
		default:
			return len(fulls) - len(expired)
		}
	case BackupDifferential:
		return e.want.Diff
	default:
		return 0
	}
}

// archive checks the WAL against the archive retention: nothing before the
// archive-retention backup outside the older backups' own ranges (when the
// backups themselves were expired as expected), and every backup's WAL from
// its archive start.
func (e *retentionEvaluation) archive(backups []BackupReport, fulls []Backup, expired []string, notApplied bool) {
	retention := e.archiveRetention(fulls, expired)
	e.result.ArchiveRetention = retention
	segmentSize := e.report.SegmentSize
	if segmentSize == 0 {
		e.unknown = "the WAL segment size is not known"
		return
	}
	walByID := make(map[string]ArchiveWAL, len(e.report.WAL))
	for _, wal := range e.report.WAL {
		walByID[wal.ArchiveID] = wal
	}
	var list []BackupReport
	for _, backup := range backups {
		switch {
		case backup.Backup.Type == BackupFull,
			backup.Backup.Type == BackupDifferential && e.want.ArchiveType != BackupFull,
			backup.Backup.Type == BackupIncremental && e.want.ArchiveType == BackupIncremental:
			list = append(list, backup)
		}
	}
	var anchor *BackupReport
	if retention > 0 && len(list) >= retention {
		anchor = &list[len(list)-retention]
	}
	if anchor != nil && !notApplied {
		e.walBeyond(*anchor, backups, walByID, segmentSize)
	}
	for _, backup := range backups {
		if anchor != nil && backup.Backup.Label < anchor.Backup.Label {
			// Older than the archive-retention backup: expire keeps its WAL
			// to consistency only, which the recovery paths already judge.
			continue
		}
		e.walFromStart(backup, walByID, segmentSize)
	}
}

// walUsable reports whether the WAL of an archive id is complete enough to
// decide presence and absence.
func walUsable(wal ArchiveWAL, ok bool) bool {
	return ok && !wal.RangesTruncated && wal.State != evidence.Unknown
}

func (e *retentionEvaluation) walFromStart(backup BackupReport, walByID map[string]ArchiveWAL, segmentSize int64) {
	if backup.ArchiveID == "" || backup.Backup.ArchiveStart == "" {
		return
	}
	timeline, position, err := ParseWALName(backup.Backup.ArchiveStart, segmentSize)
	wal, ok := walByID[backup.ArchiveID]
	if err != nil || !walUsable(wal, ok) {
		e.unknown = "the WAL of archive " + backup.ArchiveID + " cannot be judged"
		return
	}
	if _, found := rangeAt(wal.Ranges, timeline, position); !found {
		e.findings.add(FindingRetentionShortfall, SeverityError, backup.Backup.Label,
			"the WAL from the backup's archive start ("+backup.Backup.ArchiveStart+") is not in the repository, although the retention keeps it")
	}
}

// walBeyond reports WAL the archive retention would have removed: on the
// anchor's archive id, before its archive start, outside the archive start
// to stop of every older backup of that archive id. Names compare
// timeline first, as expireArchiveId compares them.
func (e *retentionEvaluation) walBeyond(anchor BackupReport, backups []BackupReport, walByID map[string]ArchiveWAL, segmentSize int64) {
	if anchor.ArchiveID == "" || anchor.Backup.ArchiveStart == "" {
		return
	}
	limitTimeline, limitPosition, err := ParseWALName(anchor.Backup.ArchiveStart, segmentSize)
	wal, ok := walByID[anchor.ArchiveID]
	if err != nil || !walUsable(wal, ok) {
		e.unknown = "the WAL of archive " + anchor.ArchiveID + " cannot be judged"
		return
	}
	type span struct {
		startTimeline, stopTimeline uint32
		start, stop                 uint64
	}
	var kept []span
	for _, backup := range backups {
		if backup.ArchiveID != anchor.ArchiveID || backup.Backup.Label >= anchor.Backup.Label {
			continue
		}
		startTimeline, start, errStart := ParseWALName(backup.Backup.ArchiveStart, segmentSize)
		stopTimeline, stop, errStop := ParseWALName(backup.Backup.ArchiveStop, segmentSize)
		if errStart != nil || errStop != nil {
			e.unknown = "a backup's WAL range cannot be parsed"
			return
		}
		kept = append(kept, span{startTimeline, stopTimeline, start, stop})
	}
	// coveredTo returns the last position of timeline a kept range covers
	// from position on, in name order (timeline, then position), and false
	// when none covers position.
	coveredTo := func(timeline uint32, position uint64) (uint64, bool) {
		end, found := uint64(0), false
		for _, value := range kept {
			afterStart := timeline > value.startTimeline || timeline == value.startTimeline && position >= value.start
			beforeStop := timeline < value.stopTimeline || timeline == value.stopTimeline && position <= value.stop
			if !afterStart || !beforeStop {
				continue
			}
			last := uint64(math.MaxUint64)
			if timeline == value.stopTimeline {
				last = value.stop
			}
			end, found = max(end, last), true
		}
		return end, found
	}
	for _, value := range wal.Ranges {
		if value.Timeline > limitTimeline {
			continue
		}
		last := value.End
		if value.Timeline == limitTimeline {
			if value.Start >= limitPosition {
				continue
			}
			last = min(last, limitPosition-1)
		}
		// Walk the range by kept spans, not by segment: a range can hold
		// millions of segments.
		for position := value.Start; position <= last; {
			e.walSteps++
			if end, ok := coveredTo(value.Timeline, position); ok {
				if end >= last {
					break
				}
				position = end + 1
				continue
			}
			name, _ := WALName(value.Timeline, position, segmentSize)
			e.findings.add(FindingRetentionNotApplied, SeverityWarning, "archive/"+e.report.ArchiveStanza+"/"+anchor.ArchiveID+"/"+name,
				fmt.Sprintf("WAL before %s, the archive start of %s (the oldest backup repo-retention-archive=%d keeps for point-in-time recovery), is still in the repository: expire would have removed it",
					anchor.Backup.ArchiveStart, anchor.Backup.Label, e.result.ArchiveRetention))
			return
		}
	}
}

func (e *retentionEvaluation) rollup() (evidence.State, string) {
	state := evidence.Healthy
	for _, finding := range e.result.Findings {
		switch finding.Severity {
		case SeverityError:
			state = Conservative(state, evidence.Unhealthy)
		case SeverityWarning:
			state = Conservative(state, evidence.Warning)
		}
	}
	if e.unknown != "" {
		return Conservative(state, evidence.Unknown), boundedMessage(e.unknown, "retention cannot be evaluated")
	}
	has := func(code FindingCode) bool {
		return slices.ContainsFunc(e.result.Findings, func(finding Finding) bool { return finding.Code == code })
	}
	switch {
	case has(FindingRetentionShortfall):
		return state, "the stanza lacks backups or WAL the retention keeps"
	case has(FindingRetentionNotApplied):
		return state, "the stanza holds backups or WAL the retention expires: expire is not applying it"
	case has(FindingRetentionNotConfigured):
		return state, "no full backup retention is configured: pgBackRest never expires"
	case e.want.FullType == RetentionTime:
		return state, fmt.Sprintf("the backups and WAL match the %d-day retention as of the newest backup", e.want.Full)
	default:
		return state, fmt.Sprintf("the backups and WAL match the retention of %d full backups", e.want.Full)
	}
}

// retentionAspect is the state of the retention comparison: unknown when
// the agent was given no expectation (the viewer, or an agent configured by
// hand without one).
func (r Report) retentionAspect() Aspect {
	if r.Retention == nil {
		return Aspect{evidence.Unknown, "no retention expectation is configured"}
	}
	return Aspect{r.Retention.State, r.Retention.Reason}
}
