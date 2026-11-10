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
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// Bounds applied to WAL arithmetic, to the compact WAL catalog and to the
// timeline history files that are read.
const (
	// MinWALSegmentSize is the smallest WAL segment size PostgreSQL accepts (1 MiB).
	MinWALSegmentSize int64 = 1 << 20
	// MaxWALSegmentSize is the largest WAL segment size PostgreSQL accepts (1 GiB).
	MaxWALSegmentSize int64 = 1 << 30
	// DefaultWALSegmentSize is initdb's default, assumed only when the
	// backups' LSNs do not decide the size (see InferSegmentSize).
	DefaultWALSegmentSize int64 = 16 << 20
	// MaxWALRanges caps the contiguous ranges kept per archive id; past it
	// the archive's WAL state becomes unknown.
	MaxWALRanges = 10_000
	// MaxWALDiagnostics caps the anomalies listed per archive id; every
	// anomaly is still counted.
	MaxWALDiagnostics = 200
	// MaxHistoryFiles caps the timeline history files retained across all
	// archive ids for later reading.
	MaxHistoryFiles = 1_000
	// MaxHistoryBytes caps one decrypted timeline history file.
	MaxHistoryBytes int64 = 256 * 1024
	// MaxHistoryEntries caps the switchpoint lines accepted in one history file.
	MaxHistoryEntries = 128
)

// WALClass is how one object under archive/<stanza>/<archive id>/ was classified.
type WALClass string

const (
	// WALSegment is a complete WAL segment: <24 hex>-<sha1>[.<compression>].
	WALSegment WALClass = "segment"
	// WALPartial is <24 hex>.partial-<sha1>[.<compression>]: the incomplete
	// last segment of a timeline, pushed at promotion. It never fills continuity.
	WALPartial WALClass = "partial"
	// WALHistory is a timeline history file, <8 hex>.history, directly under
	// the archive id directory.
	WALHistory WALClass = "history"
	// WALBackupHistory is a <24 hex>.<8 hex>.backup label file, one per backup.
	WALBackupHistory WALClass = "backup-history"
	// WALUnknown is an object that could not be recognized, or a segment the
	// listing returned out of order.
	WALUnknown WALClass = "unknown"
	// WALDuplicate is a second object for one WAL position, such as the same
	// segment with two checksums. pgBackRest's archive-get refuses such a
	// position, so the reader does not choose either.
	WALDuplicate WALClass = "duplicate"
)

// The names pgBackRest gives archived files (src/command/archive/common.h,
// src/common/compress/helper.h), with strict upper-case hexadecimal where
// pgBackRest's [0-F] class is looser.
var (
	segmentFilePattern = regexp.MustCompile(`^([0-9A-F]{24})(\.partial)?-([0-9a-f]{40})(\.gz|\.lz4|\.zst|\.xz|\.bz2)?$`)
	backupFilePattern  = regexp.MustCompile(`^([0-9A-F]{24})\.[0-9A-F]{8}\.backup$`)
	historyFilePattern = regexp.MustCompile(`^([0-9A-F]{8})\.history$`)
	segmentDirPattern  = regexp.MustCompile(`^[0-9A-F]{16}$`)
	archiveIDPattern   = regexp.MustCompile(`^([1-9][0-9]?(?:\.[0-9])?)-([1-9][0-9]{0,9})$`)
)

var compressionNames = map[string]string{"": "none", ".gz": "gzip", ".lz4": "lz4", ".zst": "zstd", ".xz": "xz", ".bz2": "bzip2"}

// ClassifiedWAL is the allowlisted result of interpreting one archive object key.
type ClassifiedWAL struct {
	Key, Stanza, ArchiveID, Name string
	Compression, Checksum        string
	Class                        WALClass
	Timeline, Log, Segment       uint32
	Size                         int64
	LastModified                 time.Time
	Reason                       string
}

// ClassifyWALObject recognizes archive/<stanza>/<archive id>/... keys. The
// boolean reports whether the object belongs to an archive id directory; the
// info files and anything else directly under archive/<stanza>/ do not.
// Only the name is read: nothing is downloaded.
func ClassifyWALObject(object store.Object) (ClassifiedWAL, bool) {
	parts := strings.Split(object.Key, "/")
	if len(parts) < 4 || parts[0] != "archive" || !evidencev1alpha1.ValidPGBackRestStanza(parts[1]) || !archiveIDPattern.MatchString(parts[2]) {
		return ClassifiedWAL{}, false
	}
	result := ClassifiedWAL{
		Key: object.Key, Stanza: parts[1], ArchiveID: parts[2], Class: WALUnknown, Size: object.Size,
		LastModified: object.LastModified.UTC(), Reason: "unrecognized pgBackRest archive object",
	}
	name := parts[len(parts)-1]
	switch len(parts) {
	case 4:
		match := historyFilePattern.FindStringSubmatch(name)
		if match == nil {
			return result, true
		}
		timeline := parseHex32(match[1])
		if timeline <= 1 {
			result.Reason = "malformed timeline history name"
			return result, true
		}
		result.Name, result.Class, result.Timeline, result.Compression, result.Reason = name, WALHistory, timeline, "none", "timeline history metadata"
		return result, true
	case 5:
	default:
		return result, true
	}
	directory := parts[3]
	if !segmentDirPattern.MatchString(directory) {
		result.Reason = "WAL object is outside a segment directory"
		return result, true
	}
	var segmentName string
	if match := segmentFilePattern.FindStringSubmatch(name); match != nil {
		segmentName = match[1]
		result.Checksum, result.Compression = match[3], compressionNames[match[4]]
		if match[2] == "" {
			result.Class, result.Reason = WALSegment, "complete WAL segment name"
		} else {
			result.Class, result.Reason = WALPartial, "partial WAL does not fill continuity"
		}
	} else if match := backupFilePattern.FindStringSubmatch(name); match != nil {
		segmentName = match[1]
		result.Class, result.Compression, result.Reason = WALBackupHistory, "none", "backup history metadata"
	} else {
		result.Reason = "malformed WAL archive object name"
		return result, true
	}
	if segmentName[:16] != directory {
		result.Class, result.Reason = WALUnknown, "WAL object is outside its segment directory"
		return result, true
	}
	timeline := parseHex32(segmentName[:8])
	log := parseHex32(segmentName[8:16])
	segment := parseHex32(segmentName[16:24])
	if timeline == 0 {
		result.Class, result.Reason = WALUnknown, "malformed WAL segment name"
		return result, true
	}
	result.Name, result.Timeline, result.Log, result.Segment = segmentName, timeline, log, segment
	return result, true
}

// GapStatus is how many complete scans have observed a WAL gap.
type GapStatus string

const (
	// GapCandidate is a gap seen in the current scan only; it may still be
	// filled by an upload in flight (archive-push-queue and process-max push
	// out of order).
	GapCandidate GapStatus = "candidate"
	// GapConfirmed is a gap that was already present in an earlier complete scan.
	GapConfirmed GapStatus = "confirmed"
)

// WALCounts counts the objects of one archive id per class.
type WALCounts struct {
	Segments, Partials, History, BackupHistory, Unknown, Duplicates int64
}

// WALRange is a run of consecutive WAL segments on one timeline. Start and
// End are absolute segment positions (see WALPosition), First and Last the
// matching file names; LatestReceipt is the newest upload time in the run and
// EndReceipt the upload time of its last segment.
type WALRange struct {
	Timeline      uint32
	Start, End    uint64
	Count         uint64
	First, Last   string
	LatestReceipt time.Time
	EndReceipt    time.Time
}

// WALGap is a run of missing segment positions between two ranges of the
// same timeline, with the scan generations that first and last observed it.
type WALGap struct {
	Timeline                                        uint32
	Start, End, Count                               uint64
	First, Last                                     string
	Status                                          GapStatus
	FirstObservedGeneration, LastObservedGeneration uint64
}

// WALDiagnostic is one anomalous archive object kept for display, with the
// reason it was not counted as a segment.
type WALDiagnostic struct {
	Key, Name, Reason string
	Class             WALClass
	Timeline          uint32
	LastModified      time.Time
}

// HistoryObject is the bounded reference needed to read one timeline history
// file after the listing. It contains no content.
type HistoryObject struct {
	Key, Stanza, ArchiveID string
	Timeline               uint32
	Size                   int64
}

// ArchiveWAL is the compact WAL evidence of one archive id of one stanza.
type ArchiveWAL struct {
	Stanza, ArchiveID    string
	State                evidence.State
	Reason               string
	SegmentSize          int64
	Counts               WALCounts
	Ranges               []WALRange
	Gaps                 []WALGap
	Diagnostics          []WALDiagnostic
	DiagnosticsTruncated bool
	RangesTruncated      bool
	LatestArchiveReceipt time.Time
	// SegmentSizeUnproven is the stanza's SegmentSizeInference.Unproven: a
	// crossing to the next log id, or a timeline switch position, holds
	// only at the inferred size.
	SegmentSizeUnproven bool `json:",omitempty"`
}

type rawRange struct {
	timeline, log, start, end uint32
	latestReceipt             time.Time
	endReceipt                time.Time
}

type walBuilder struct {
	counts               WALCounts
	ranges               []rawRange
	diagnostics          []WALDiagnostic
	diagnosticsTruncated bool
	rangesTruncated      bool
	outOfOrder           bool
}

// WALCollector incrementally compacts a lexicographically ordered listing:
// within an archive id, segment directories sort by timeline then log, and
// names within a directory by segment, so a regression to out-of-order
// segments fails continuity to unknown instead of being re-sorted (which
// would need every name in memory).
type WALCollector struct {
	archives         map[string]*walBuilder
	historyObjects   []HistoryObject
	historyTruncated bool
}

// NewWALCollector returns an empty collector ready for Add.
func NewWALCollector() *WALCollector {
	return &WALCollector{archives: make(map[string]*walBuilder)}
}

func archiveKey(stanza, archiveID string) string { return stanza + "\x00" + archiveID }

// Add classifies one listed object and folds it into its archive id's
// counts, ranges and diagnostics.
func (c *WALCollector) Add(object store.Object) {
	classified, belongs := ClassifyWALObject(object)
	if !belongs {
		return
	}
	key := archiveKey(classified.Stanza, classified.ArchiveID)
	builder := c.archives[key]
	if builder == nil {
		builder = &walBuilder{}
		c.archives[key] = builder
	}
	switch classified.Class {
	case WALSegment:
		builder.counts.Segments++
		builder.addSegment(classified)
	// Partial, history and backup-history objects are expected in every
	// healthy archive. They are counted, never listed: only anomalies fill
	// the bounded diagnostic list.
	case WALPartial:
		builder.counts.Partials++
	case WALHistory:
		builder.counts.History++
		if len(c.historyObjects) < MaxHistoryFiles {
			c.historyObjects = append(c.historyObjects, HistoryObject{Key: classified.Key, Stanza: classified.Stanza, ArchiveID: classified.ArchiveID, Timeline: classified.Timeline, Size: classified.Size})
		} else {
			c.historyTruncated = true
		}
	case WALBackupHistory:
		builder.counts.BackupHistory++
	default:
		builder.counts.Unknown++
		builder.addDiagnostic(classified)
	}
}

func (b *walBuilder) addSegment(object ClassifiedWAL) {
	if len(b.ranges) == 0 {
		b.appendRange(object)
		return
	}
	last := &b.ranges[len(b.ranges)-1]
	if last.timeline == object.Timeline && last.log == object.Log && last.end == object.Segment {
		b.counts.Segments--
		b.counts.Duplicates++
		duplicate := object
		duplicate.Class = WALDuplicate
		duplicate.Reason = "duplicate representation of one WAL position"
		b.addDiagnostic(duplicate)
		if object.LastModified.After(last.latestReceipt) {
			last.latestReceipt = object.LastModified
		}
		return
	}
	if last.timeline == object.Timeline && last.log == object.Log && last.end != math.MaxUint32 && last.end+1 == object.Segment {
		last.end = object.Segment
		last.endReceipt = object.LastModified
		if object.LastModified.After(last.latestReceipt) {
			last.latestReceipt = object.LastModified
		}
		return
	}
	if object.Timeline < last.timeline || (object.Timeline == last.timeline && (object.Log < last.log || (object.Log == last.log && object.Segment < last.end))) {
		b.outOfOrder = true
		outOfOrder := object
		outOfOrder.Class = WALUnknown
		outOfOrder.Reason = "provider listing returned WAL positions out of order"
		b.addDiagnostic(outOfOrder)
		return
	}
	b.appendRange(object)
}

func (b *walBuilder) appendRange(object ClassifiedWAL) {
	if len(b.ranges) >= MaxWALRanges {
		b.rangesTruncated = true
		return
	}
	b.ranges = append(b.ranges, rawRange{
		timeline: object.Timeline, log: object.Log, start: object.Segment, end: object.Segment,
		latestReceipt: object.LastModified, endReceipt: object.LastModified,
	})
}

// addDiagnostic records one anomaly for display. The cap bounds memory and
// rendering only: every anomaly is also counted or flagged, and each already
// makes the archive unknown, so truncating the list never changes the state.
func (b *walBuilder) addDiagnostic(object ClassifiedWAL) {
	if len(b.diagnostics) >= MaxWALDiagnostics {
		b.diagnosticsTruncated = true
		return
	}
	b.diagnostics = append(b.diagnostics, WALDiagnostic{
		Key: object.Key, Name: object.Name, Reason: object.Reason, Class: object.Class,
		Timeline: object.Timeline, LastModified: object.LastModified.UTC(),
	})
}

// HistoryObjects returns a copy of the bounded history references and
// whether the history-file ceiling was exceeded.
func (c *WALCollector) HistoryObjects() ([]HistoryObject, bool) {
	return slices.Clone(c.historyObjects), c.historyTruncated
}

// Finish converts the archive id of stanza to absolute positions with
// segmentSize (0 when it is not known), merges ranges across log boundaries
// and applies the gap lifecycle against the previous complete scan. An
// archive id with no listed object still gets a result.
func (c *WALCollector) Finish(stanza, archiveID string, segmentSize int64, previous *ArchiveWAL, generation uint64) ArchiveWAL {
	builder := c.archives[archiveKey(stanza, archiveID)]
	if builder == nil {
		builder = &walBuilder{}
	}
	value := ArchiveWAL{
		Stanza: stanza, ArchiveID: archiveID, Counts: builder.counts, Diagnostics: slices.Clone(builder.diagnostics),
		DiagnosticsTruncated: builder.diagnosticsTruncated, RangesTruncated: builder.rangesTruncated,
	}
	if !ValidWALSegmentSize(segmentSize) {
		value.State, value.Reason = evidence.Unknown, "WAL segment size is not known"
		return value
	}
	value.SegmentSize = segmentSize
	invalidPosition := false
	for _, raw := range builder.ranges {
		start, errStart := WALPosition(raw.timeline, raw.log, raw.start, segmentSize)
		end, errEnd := WALPosition(raw.timeline, raw.log, raw.end, segmentSize)
		if errStart != nil || errEnd != nil {
			invalidPosition = true
			continue
		}
		first, _ := WALName(raw.timeline, start, segmentSize)
		last, _ := WALName(raw.timeline, end, segmentSize)
		next := WALRange{Timeline: raw.timeline, Start: start, End: end, Count: end - start + 1, First: first, Last: last, LatestReceipt: raw.latestReceipt.UTC(), EndReceipt: raw.endReceipt.UTC()}
		if len(value.Ranges) > 0 {
			prior := &value.Ranges[len(value.Ranges)-1]
			if prior.Timeline == next.Timeline && prior.End != math.MaxUint64 && prior.End+1 == next.Start {
				prior.End, prior.Last, prior.Count, prior.EndReceipt = next.End, next.Last, next.End-prior.Start+1, next.EndReceipt
				if next.LatestReceipt.After(prior.LatestReceipt) {
					prior.LatestReceipt = next.LatestReceipt
				}
				continue
			}
		}
		value.Ranges = append(value.Ranges, next)
	}
	for _, rangeValue := range value.Ranges {
		if rangeValue.LatestReceipt.After(value.LatestArchiveReceipt) {
			value.LatestArchiveReceipt = rangeValue.LatestReceipt
		}
	}
	value.Gaps = gapsForRanges(value.Ranges, previous, segmentSize, generation)
	switch {
	case builder.rangesTruncated:
		value.State, value.Reason = evidence.Unknown, "WAL range safety limit reached"
	case builder.outOfOrder:
		value.State, value.Reason = evidence.Unknown, "WAL listing order is incompatible"
	case invalidPosition:
		value.State, value.Reason = evidence.Unknown, "WAL name is invalid for the segment size"
	case builder.counts.Duplicates > 0:
		value.State, value.Reason = evidence.Unknown, "duplicate WAL representations require operator review"
	case builder.counts.Unknown > 0:
		value.State, value.Reason = evidence.Unknown, "unrecognized archive objects require operator review"
	case len(value.Ranges) == 0:
		value.State, value.Reason = evidence.Unknown, "no complete WAL segments observed"
	case hasGapStatus(value.Gaps, GapConfirmed):
		value.State, value.Reason = evidence.Unhealthy, "confirmed segment-name gaps observed"
	case hasGapStatus(value.Gaps, GapCandidate):
		value.State, value.Reason = evidence.Warning, "candidate segment-name gaps observed"
	default:
		value.State, value.Reason = evidence.Healthy, "no segment-name gaps observed within compact ranges"
	}
	return value
}

func gapsForRanges(ranges []WALRange, previous *ArchiveWAL, segmentSize int64, generation uint64) []WALGap {
	prior := make(map[string]WALGap)
	if previous != nil {
		for _, gap := range previous.Gaps {
			prior[gapIdentity(gap.Timeline, gap.Start, gap.End)] = gap
		}
	}
	result := make([]WALGap, 0)
	for index := 1; index < len(ranges); index++ {
		left, right := ranges[index-1], ranges[index]
		if left.Timeline != right.Timeline || left.End == math.MaxUint64 || left.End+1 >= right.Start {
			continue
		}
		start, end := left.End+1, right.Start-1
		first, _ := WALName(left.Timeline, start, segmentSize)
		last, _ := WALName(left.Timeline, end, segmentSize)
		gap := WALGap{
			Timeline: left.Timeline, Start: start, End: end, Count: end - start + 1,
			First: first, Last: last, Status: GapCandidate,
			FirstObservedGeneration: generation, LastObservedGeneration: generation,
		}
		if old, ok := prior[gapIdentity(gap.Timeline, gap.Start, gap.End)]; ok && old.FirstObservedGeneration < generation {
			gap.FirstObservedGeneration = old.FirstObservedGeneration
			gap.Status = GapConfirmed
		}
		result = append(result, gap)
	}
	return result
}

func gapIdentity(timeline uint32, start, end uint64) string {
	return fmt.Sprintf("%08X:%016X:%016X", timeline, start, end)
}

func hasGapStatus(gaps []WALGap, status GapStatus) bool {
	return slices.ContainsFunc(gaps, func(gap WALGap) bool { return gap.Status == status })
}

// ValidWALSegmentSize reports whether size is a power of two between
// MinWALSegmentSize and MaxWALSegmentSize, as PostgreSQL requires.
func ValidWALSegmentSize(size int64) bool {
	return size >= MinWALSegmentSize && size <= MaxWALSegmentSize && size&(size-1) == 0
}

// WALPosition implements PostgreSQL's XLogSegmentsPerXLogId and
// XLogFromFileName arithmetic (the same as pgBackRest's walSegmentNext for
// PostgreSQL 9.3 and later) without assuming the default 16 MiB segment.
func WALPosition(timeline, log, segment uint32, segmentSize int64) (uint64, error) {
	if timeline == 0 || !ValidWALSegmentSize(segmentSize) {
		return 0, errors.New("invalid WAL arithmetic context")
	}
	// #nosec G115 -- the guard above makes segmentSize a power of two from 1 MiB to 1 GiB.
	segmentsPerID := uint64(1<<32) / uint64(segmentSize)
	if uint64(segment) >= segmentsPerID {
		return 0, errors.New("WAL segment component exceeds segment-size rollover")
	}
	return uint64(log)*segmentsPerID + uint64(segment), nil
}

// WALName is the inverse of WALPosition.
func WALName(timeline uint32, position uint64, segmentSize int64) (string, error) {
	if timeline == 0 || !ValidWALSegmentSize(segmentSize) {
		return "", errors.New("invalid WAL arithmetic context")
	}
	// #nosec G115 -- the guard above makes segmentSize a power of two from 1 MiB to 1 GiB.
	segmentsPerID := uint64(1<<32) / uint64(segmentSize)
	log := position / segmentsPerID
	if log > math.MaxUint32 {
		return "", errors.New("WAL position exceeds filename range")
	}
	// #nosec G115 -- log is range-checked above; the segment is a modulus of segmentsPerID.
	return fmt.Sprintf("%08X%08X%08X", timeline, uint32(log), uint32(position%segmentsPerID)), nil
}

// ParseWALName parses a 24-hex-digit segment name into its timeline and
// absolute position for the given segment size. Only PostgreSQL's
// upper-case spelling is accepted, as pgBackRest writes it.
func ParseWALName(name string, segmentSize int64) (uint32, uint64, error) {
	if !canonicalWALName(name) {
		return 0, 0, errors.New("invalid WAL segment name")
	}
	timeline := parseHex32(name[:8])
	log := parseHex32(name[8:16])
	segment := parseHex32(name[16:24])
	position, err := WALPosition(timeline, log, segment, segmentSize)
	return timeline, position, err
}

// SegmentSizeSource says where a stanza's WAL segment size came from.
type SegmentSizeSource string

const (
	// SegmentSizeFromLSN means exactly one segment size makes every backup's
	// archive-start/stop name the segment of its lsn-start/stop.
	SegmentSizeFromLSN SegmentSizeSource = "backup-lsn"
	// SegmentSizeFromNames means the backups' LSNs leave more than one size
	// and the archive's segment names decide it: the highest segment
	// number seen in a log id rules out every size with fewer segments per
	// log, and the segment number at which each log id rolls over to the
	// next (0xFF for 16 MiB) names one size.
	SegmentSizeFromNames SegmentSizeSource = "wal-names"
	// SegmentSizeAssumed means the backups do not decide it (none has an LSN
	// range past the first segment of a log) and the 16 MiB default is
	// assumed. pgBackRest records the size nowhere in the repository.
	SegmentSizeAssumed SegmentSizeSource = "assumed-default"
)

// SegmentNames is what an archive's segment names say about the WAL
// segment size, which they encode only as the number of segments per log
// id (4 GiB / size).
type SegmentNames struct {
	// Seen is true when at least one segment name was listed.
	Seen bool
	// MaxSegment is the highest segment number seen in any log id: a size
	// with no more segments per log than that is impossible.
	MaxSegment uint32
	// Rollovers are the last segment numbers of log ids followed, on the
	// same timeline, by segment 0 of the next log id: with contiguous WAL,
	// each is the number of segments per log minus one.
	Rollovers []uint32
}

// SegmentNames gathers the segment-name evidence of every archive id the
// collector saw.
func (c *WALCollector) SegmentNames() SegmentNames {
	var result SegmentNames
	for _, key := range slices.Sorted(maps.Keys(c.archives)) {
		ranges := c.archives[key].ranges
		for index, value := range ranges {
			result.Seen = true
			result.MaxSegment = max(result.MaxSegment, value.end)
			if index+1 < len(ranges) {
				next := ranges[index+1]
				if next.timeline == value.timeline && value.log != math.MaxUint32 && next.log == value.log+1 && next.start == 0 {
					result.Rollovers = append(result.Rollovers, value.end)
				}
			}
		}
	}
	return result
}

// segmentsPerLog is the number of segments in one log id at size.
func segmentsPerLog(size int64) uint64 {
	// #nosec G115 -- callers pass a power of two from 1 MiB to 1 GiB.
	return (uint64(1) << 32) / uint64(size)
}

// SegmentSizeInference is what InferSegmentSize derived.
type SegmentSizeInference struct {
	// Size is the WAL segment size in bytes; 0 when no size fits.
	Size   int64
	Source SegmentSizeSource
	// Unproven is true when a smaller size is still possible: the backups'
	// LSNs do not rule it out, and segment names only ever rule out larger
	// sizes. Contiguity inside a log id holds at any size, but a crossing
	// from the last segment of a log id to segment 0 of the next is
	// contiguous only at the inferred size: at a smaller one, the
	// segments past the inferred rollover are missing.
	Unproven bool
	// Problem says why Size is 0.
	Problem string
}

// InferSegmentSize derives the WAL segment size from the backups: for every
// candidate size, a backup's archive-start must be the segment of its
// lsn-start, and archive-stop the segment of lsn-stop. pgBackRest does not
// store the size (it reads it from pg_control when it runs), so the backups
// and the archive's segment names are the only evidence the repository
// holds. When the backups leave more than one size, the names narrow them
// (SegmentSizeFromNames); the 16 MiB default is assumed only when it is
// still possible and nothing decides. A rollover the names show at a
// segment number no size ends a log at (segments missing at the end of a
// log id) falls back to the default, or to the largest possible size,
// unproven: the crossing then reads as a gap, never as contiguous WAL. The
// size is 0 when no size fits the backups' LSNs and the names together.
func InferSegmentSize(backups []Backup, names SegmentNames) SegmentSizeInference {
	var candidates []int64
	for size := MinWALSegmentSize; size <= MaxWALSegmentSize; size <<= 1 {
		fits := true
		for _, backup := range backups {
			for _, pair := range [][2]string{{backup.ArchiveStart, backup.LSNStart}, {backup.ArchiveStop, backup.LSNStop}} {
				if pair[0] == "" || pair[1] == "" {
					continue
				}
				if !walNameHoldsLSN(pair[0], pair[1], size) {
					fits = false
				}
			}
		}
		if fits {
			candidates = append(candidates, size)
		}
	}
	// A segment number at or past a size's segments per log is impossible
	// at that size, whatever the backups say.
	possible := slices.DeleteFunc(slices.Clone(candidates), func(size int64) bool {
		return names.Seen && uint64(names.MaxSegment) >= segmentsPerLog(size)
	})
	chosen := func(size int64, source SegmentSizeSource) SegmentSizeInference {
		// possible is sorted ascending: a smaller size is still possible
		// unless size is its first.
		return SegmentSizeInference{Size: size, Source: source, Unproven: possible[0] < size}
	}
	switch {
	case len(possible) == 0 && len(candidates) == 0:
		return SegmentSizeInference{Problem: "no WAL segment size agrees with every backup's LSN range"}
	case len(possible) == 0:
		return SegmentSizeInference{Problem: "no WAL segment size the backups' LSN ranges allow has room for the archive's highest segment number"}
	case len(candidates) == 1:
		return chosen(candidates[0], SegmentSizeFromLSN)
	case len(possible) == 1:
		return chosen(possible[0], SegmentSizeFromNames)
	}
	// Every rollover the names show must agree on one size; segments
	// missing at the end of a log would make a rollover look early, so a
	// disagreement decides nothing.
	if len(names.Rollovers) > 0 && !slices.ContainsFunc(names.Rollovers, func(value uint32) bool { return value != names.Rollovers[0] }) {
		if index := slices.IndexFunc(possible, func(size int64) bool { return segmentsPerLog(size) == uint64(names.Rollovers[0])+1 }); index >= 0 {
			return chosen(possible[index], SegmentSizeFromNames)
		}
		// No size ends a log at that segment: segments are missing at the
		// end of a log id. The default, when possible, shows the crossing
		// as a gap.
		if !slices.Contains(possible, DefaultWALSegmentSize) {
			return SegmentSizeInference{Problem: fmt.Sprintf("the archive's segment names roll over to the next log id after segment %02X, where no possible WAL segment size ends a log (segments are missing at the end of a log id), and rule the 16 MiB default out", names.Rollovers[0])}
		}
	}
	if slices.Contains(possible, DefaultWALSegmentSize) {
		return chosen(DefaultWALSegmentSize, SegmentSizeAssumed)
	}
	return SegmentSizeInference{Problem: "the archive's segment names leave several WAL segment sizes and rule the 16 MiB default out"}
}

func walNameHoldsLSN(name, lsnText string, segmentSize int64) bool {
	lsn, ok := ParseLSN(lsnText)
	if !ok || !canonicalWALName(name) {
		return false
	}
	timeline := parseHex32(name[:8])
	// #nosec G115 -- callers pass a power of two from 1 MiB to 1 GiB.
	expected, err := WALName(timeline, lsn/uint64(segmentSize), segmentSize)
	return err == nil && expected == name
}

// HistoryEdge is one timeline switch read from a history file: Child forked
// from Parent at SwitchLSN, which falls in segment position SwitchPosition.
// From SwitchPosition on, the child's segment files are the ones replayed.
type HistoryEdge struct {
	Parent, Child  uint32
	SwitchLSN      uint64
	SwitchPosition uint64
}

// ParseHistory parses PostgreSQL's timeline history format (one
// "<parent>\t<switchpoint>\t<reason>" line per ancestor, oldest first) for
// the given target timeline. Reasons are validated for boundedness and
// discarded.
func ParseHistory(data []byte, target uint32, segmentSize int64) ([]HistoryEdge, error) {
	if len(data) == 0 || int64(len(data)) > MaxHistoryBytes || target <= 1 || !ValidWALSegmentSize(segmentSize) {
		return nil, errors.New("invalid bounded timeline history")
	}
	type entry struct {
		parent uint32
		lsn    uint64
	}
	var entries []entry
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 1024), 4096)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 || strings.TrimSpace(fields[2]) == "" || len(fields[2]) > 1024 || strings.IndexFunc(fields[2], unicode.IsControl) >= 0 {
			return nil, errors.New("malformed timeline history record")
		}
		parent, err := strconv.ParseUint(strings.TrimSpace(fields[0]), 10, 32)
		if err != nil || parent == 0 {
			return nil, errors.New("malformed timeline parent")
		}
		lsn, ok := parseHistoryLSN(strings.TrimSpace(fields[1]))
		if !ok || lsn == 0 {
			return nil, errors.New("malformed timeline switchpoint")
		}
		entries = append(entries, entry{parent: uint32(parent), lsn: lsn})
		if len(entries) > MaxHistoryEntries {
			return nil, errors.New("timeline history entry limit reached")
		}
	}
	if err := scanner.Err(); err != nil || len(entries) == 0 {
		return nil, errors.New("malformed timeline history")
	}
	edges := make([]HistoryEdge, len(entries))
	for index, value := range entries {
		if value.parent >= target || (index > 0 && (value.parent <= entries[index-1].parent || value.lsn <= entries[index-1].lsn)) {
			return nil, errors.New("impossible timeline ancestry")
		}
		child := target
		if index+1 < len(entries) {
			child = entries[index+1].parent
		}
		// #nosec G115 -- ValidWALSegmentSize above.
		edges[index] = HistoryEdge{Parent: value.parent, Child: child, SwitchLSN: value.lsn, SwitchPosition: value.lsn / uint64(segmentSize)}
	}
	return edges, nil
}

// parseHistoryLSN accepts PostgreSQL's %X/%X switchpoint in either case.
func parseHistoryLSN(value string) (uint64, bool) {
	high, low, ok := strings.Cut(value, "/")
	if !ok || high == "" || low == "" || len(high) > 8 || len(low) > 8 {
		return 0, false
	}
	highValue, errHigh := strconv.ParseUint(high, 16, 32)
	lowValue, errLow := strconv.ParseUint(low, 16, 32)
	if errHigh != nil || errLow != nil {
		return 0, false
	}
	return highValue<<32 | lowValue, true
}

// TimelineHistory is the parsed ancestry of one timeline of one archive id,
// or the reason it could not be established.
type TimelineHistory struct {
	ArchiveID string
	Key       string
	Timeline  uint32
	State     evidence.State
	Reason    string
	Edges     []HistoryEdge
}

// readHistories reads every history file of one archive id, bounded, with
// archive.info's cipher-pass when the archive is encrypted (pgBackRest stores
// history files uncompressed, encrypted like the WAL).
func readHistories(ctx context.Context, reader store.Reader, objects []HistoryObject, stanza, archiveID string, passphrase []byte, segmentSize int64) []TimelineHistory {
	result := make([]TimelineHistory, 0)
	for _, object := range objects {
		if object.Stanza != stanza || object.ArchiveID != archiveID {
			continue
		}
		history := TimelineHistory{ArchiveID: archiveID, Key: object.Key, Timeline: object.Timeline, State: evidence.Unknown}
		data, err := readHistory(ctx, reader, object, passphrase)
		switch {
		case err != nil:
			history.Reason = "timeline history could not be read"
		case !ValidWALSegmentSize(segmentSize):
			history.Reason = "WAL segment size is not known"
		default:
			edges, parseErr := ParseHistory(data, object.Timeline, segmentSize)
			if parseErr != nil {
				history.State, history.Reason = evidence.Unhealthy, "timeline history is malformed"
			} else {
				history.State, history.Reason, history.Edges = evidence.Healthy, "timeline ancestry parsed", edges
			}
		}
		result = append(result, history)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Timeline < result[j].Timeline })
	return result
}

func readHistory(ctx context.Context, reader store.Reader, object HistoryObject, passphrase []byte) ([]byte, error) {
	stored := MaxHistoryBytes + int64(cipherHeaderBytes) + 16
	if object.Size > stored {
		return nil, errTooLarge
	}
	// One byte over the bound, as in readOne: the bounded reader below, not
	// the adapter's cut, decides that a history file is too large.
	stream, err := reader.Open(ctx, store.OpenRequest{Key: object.Key, MaxBytes: stored + 1})
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()
	source, _, err := openEnvelope(&boundedReader{source: stream, remaining: stored}, passphrase)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(&boundedReader{source: source, remaining: MaxHistoryBytes})
	if err != nil {
		return nil, err
	}
	return data, nil
}

// rejectChildWALBeforeSwitch makes a parsed history Unknown when one of its
// child timelines has WAL before its switch segment, which PostgreSQL never
// writes: a promotion at switchpoint L creates the child's first segment at
// L/segsize (XLogInitNewTimeline). A child whose oldest segment is after the
// switch segment is what retention leaves once it expires the WAL from
// before a failover; it is not a contradiction, and the path walk reports
// any segment a recovery needs but cannot find.
func rejectChildWALBeforeSwitch(histories []TimelineHistory, ranges []WALRange) {
	oldest := make(map[uint32]uint64)
	for _, value := range ranges {
		if start, ok := oldest[value.Timeline]; !ok || value.Start < start {
			oldest[value.Timeline] = value.Start
		}
	}
	for index := range histories {
		if histories[index].State != evidence.Healthy {
			continue
		}
		for _, edge := range histories[index].Edges {
			if start, ok := oldest[edge.Child]; ok && start < edge.SwitchPosition {
				histories[index].State, histories[index].Reason = evidence.Unknown, "child WAL range disagrees with timeline switchpoint"
				break
			}
		}
	}
}

// parseHex32 parses eight hexadecimal digits that a pattern already
// matched; anything else is 0.
func parseHex32(value string) uint32 {
	if len(value) != 8 || !upperHex(value) {
		return 0
	}
	parsed, err := strconv.ParseUint(value, 16, 32)
	if err != nil {
		return 0
	}
	return uint32(parsed)
}
