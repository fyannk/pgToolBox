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

package barmancloud

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
)

// Bounds applied to WAL arithmetic and to the compact WAL catalog.
const (
	// MinWALSegmentSize is the smallest WAL segment size PostgreSQL accepts (1 MiB).
	MinWALSegmentSize int64 = 1 << 20
	// MaxWALSegmentSize is the largest WAL segment size PostgreSQL accepts (1 GiB).
	MaxWALSegmentSize int64 = 1 << 30
	// MaxWALRanges caps the contiguous ranges kept per server; past it the
	// server's WAL state becomes unknown.
	MaxWALRanges = 10_000
	// MaxWALDiagnostics caps the anomalies listed per server; every anomaly
	// is still counted.
	MaxWALDiagnostics = 200
	// MaxHistoryFiles caps the timeline history files retained across all
	// servers for later reading.
	MaxHistoryFiles = 1_000
)

// WALClass is how one object under a Barman wals/ directory was classified.
type WALClass string

const (
	// WALSegment is a complete WAL segment file.
	WALSegment WALClass = "segment"
	// WALPartial is a .partial file: the incomplete last segment of a timeline.
	WALPartial WALClass = "partial"
	// WALHistory is a timeline .history file.
	WALHistory WALClass = "history"
	// WALBackupHistory is a .backup label file written for each base backup.
	WALBackupHistory WALClass = "backup-history"
	// WALUnknown is an object that could not be recognized, or a segment the
	// listing returned out of order.
	WALUnknown WALClass = "unknown"
	// WALDuplicate is a second object for a WAL position already seen, such as
	// the same segment stored with two compressions.
	WALDuplicate WALClass = "duplicate"
)

// GapStatus is how many complete scans have observed a WAL gap.
type GapStatus string

const (
	// GapCandidate is a gap seen in the current scan only; it may still be
	// filled by an archive upload in flight.
	GapCandidate GapStatus = "candidate"
	// GapConfirmed is a gap that was already present in an earlier complete scan.
	GapConfirmed GapStatus = "confirmed"
)

// ClassifiedWAL is the allowlisted result of interpreting one Barman wals/
// object key. Unknown keys never contribute to continuity.
type ClassifiedWAL struct {
	Key, Server, Name, Compression string
	Class                          WALClass
	Timeline, Log, Segment         uint32
	LastModified                   time.Time
	Reason                         string
}

// WALCounts counts the objects of one server's wals/ directory per class.
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

// HistoryObject is the bounded allowlisted reference needed to read one
// timeline history file after the complete listing. It contains no content.
type HistoryObject struct {
	Key, Server, Name, Compression string
	Timeline                       uint32
}

// WALGap is a run of missing segment positions between two ranges of the same
// timeline, with the scan generations that first and last observed it.
type WALGap struct {
	Timeline                                        uint32
	Start, End, Count                               uint64
	First, Last                                     string
	Status                                          GapStatus
	FirstObservedGeneration, LastObservedGeneration uint64
}

// WALDiagnostic is one anomalous WAL object (unknown, duplicate or out of
// order) kept for display, with the reason it was not counted as a segment.
type WALDiagnostic struct {
	Key, Name, Reason string
	Class             WALClass
	Timeline          uint32
	LastModified      time.Time
}

// ServerWAL contains compact, Barman-native archive evidence for one server.
// It deliberately does not claim recovery coverage or timeline ancestry.
type ServerWAL struct {
	Server               string
	State                evidence.State
	Reason               string
	PostgreSQLVersion    int64
	SegmentSize          int64
	Counts               WALCounts
	Ranges               []WALRange
	Gaps                 []WALGap
	Diagnostics          []WALDiagnostic
	DiagnosticsTruncated bool
	RangesTruncated      bool
	LatestArchiveReceipt time.Time
}

// WALCatalog is the per-server WAL archive evidence of one repository, sorted
// by server name.
type WALCatalog struct {
	Servers []ServerWAL
}

type rawRange struct {
	timeline, log, start, end uint32
	latestReceipt             time.Time
	endReceipt                time.Time
}

type walServerBuilder struct {
	counts               WALCounts
	ranges               []rawRange
	diagnostics          []WALDiagnostic
	diagnosticsTruncated bool
	rangesTruncated      bool
	outOfOrder           bool
}

// WALCollector incrementally compacts a lexicographically ordered provider
// listing. All supported providers return object names in that order; a
// regression to out-of-order segment facts fails continuity to unknown.
type WALCollector struct {
	servers          map[string]*walServerBuilder
	historyObjects   []HistoryObject
	historyTruncated bool
}

// NewWALCollector returns an empty collector ready for Add.
func NewWALCollector() *WALCollector {
	return &WALCollector{servers: make(map[string]*walServerBuilder)}
}

// Add classifies one listed object and folds it into its server's counts,
// ranges and diagnostics; objects outside a wals/ directory are ignored.
func (c *WALCollector) Add(object store.Object) {
	classified, belongs := ClassifyWALObject(object)
	if !belongs {
		return
	}
	builder := c.servers[classified.Server]
	if builder == nil {
		builder = &walServerBuilder{}
		c.servers[classified.Server] = builder
	}
	switch classified.Class {
	case WALSegment:
		builder.counts.Segments++
		builder.addSegment(classified)
	// Partial, history, and backup-history objects are expected in every
	// healthy archive (one .backup per base backup, one .history per
	// promotion). They are counted, never listed: only anomalies fill the
	// bounded diagnostic list, so a long retention cannot reach its cap.
	case WALPartial:
		builder.counts.Partials++
	case WALHistory:
		builder.counts.History++
		if len(c.historyObjects) < MaxHistoryFiles {
			c.historyObjects = append(c.historyObjects, HistoryObject{Key: classified.Key, Server: classified.Server, Name: classified.Name, Compression: classified.Compression, Timeline: classified.Timeline})
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

func (b *walServerBuilder) addSegment(object ClassifiedWAL) {
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

func (b *walServerBuilder) appendRange(object ClassifiedWAL) {
	if len(b.ranges) >= MaxWALRanges {
		b.rangesTruncated = true
		return
	}
	b.ranges = append(b.ranges, rawRange{
		timeline: object.Timeline, log: object.Log, start: object.Segment, end: object.Segment,
		latestReceipt: object.LastModified, endReceipt: object.LastModified,
	})
}

// addDiagnostic records one anomaly (unknown, duplicate, or out-of-order
// object) for display. The cap bounds memory and rendering only: every
// anomaly is also counted or flagged, and each one already makes the server
// unknown, so truncating the list never changes the state.
func (b *walServerBuilder) addDiagnostic(object ClassifiedWAL) {
	if len(b.diagnostics) >= MaxWALDiagnostics {
		b.diagnosticsTruncated = true
		return
	}
	key, keyEscaped := diagnosticText(object.Key)
	name, nameEscaped := diagnosticText(object.Name)
	reason := object.Reason
	if keyEscaped || nameEscaped {
		reason += "; key has control characters or invalid UTF-8, shown escaped"
	}
	b.diagnostics = append(b.diagnostics, WALDiagnostic{
		Key: key, Name: name, Reason: reason, Class: object.Class,
		Timeline: object.Timeline, LastModified: object.LastModified.UTC(),
	})
}

// diagnosticText returns value unchanged when it is printable UTF-8, else
// an ASCII Go-escaped form cut at store.MaxKeyBytes, and whether it escaped.
// An object store accepts any bytes in a key; one hostile or corrupt key
// must become a readable finding, not a catalog that fails validation and
// leaves the whole publication stale.
func diagnosticText(value string) (string, bool) {
	if utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) < 0 {
		return value, false
	}
	quoted := strconv.QuoteToASCII(value)
	escaped := quoted[1 : len(quoted)-1]
	if len(escaped) > store.MaxKeyBytes {
		escaped = escaped[:store.MaxKeyBytes]
	}
	return escaped, true
}

// Finish applies metadata context and the gap lifecycle only after a complete
// scan. Passing a stale previous catalog preserves its last complete gap
// observations across an interrupted refresh.
func (c *WALCollector) Finish(backups []Backup, previous WALCatalog, generation uint64) WALCatalog {
	for _, backup := range backups {
		if backup.Server != "" && c.servers[backup.Server] == nil {
			c.servers[backup.Server] = &walServerBuilder{}
		}
	}
	result := WALCatalog{Servers: make([]ServerWAL, 0, len(c.servers))}
	for server, builder := range c.servers {
		version, segmentSize, contextReason := walContext(backups, server)
		value := ServerWAL{
			Server: server, PostgreSQLVersion: version, SegmentSize: segmentSize,
			Counts: builder.counts, Diagnostics: slices.Clone(builder.diagnostics),
			DiagnosticsTruncated: builder.diagnosticsTruncated, RangesTruncated: builder.rangesTruncated,
		}
		if contextReason != "" {
			value.State, value.Reason = evidence.Unknown, contextReason
			result.Servers = append(result.Servers, value)
			continue
		}
		invalidPosition := false
		for _, raw := range builder.ranges {
			start, err := WALPosition(raw.timeline, raw.log, raw.start, segmentSize)
			if err != nil {
				invalidPosition = true
				continue
			}
			end, err := WALPosition(raw.timeline, raw.log, raw.end, segmentSize)
			if err != nil {
				invalidPosition = true
				continue
			}
			first, _ := WALName(raw.timeline, start, segmentSize)
			last, _ := WALName(raw.timeline, end, segmentSize)
			rangeValue := WALRange{Timeline: raw.timeline, Start: start, End: end, Count: end - start + 1, First: first, Last: last, LatestReceipt: raw.latestReceipt.UTC(), EndReceipt: raw.endReceipt.UTC()}
			if len(value.Ranges) > 0 {
				prior := &value.Ranges[len(value.Ranges)-1]
				if prior.Timeline == rangeValue.Timeline && prior.End != math.MaxUint64 && prior.End+1 == rangeValue.Start {
					prior.End, prior.Last, prior.Count = rangeValue.End, rangeValue.Last, rangeValue.End-prior.Start+1
					prior.EndReceipt = rangeValue.EndReceipt
					if rangeValue.LatestReceipt.After(prior.LatestReceipt) {
						prior.LatestReceipt = rangeValue.LatestReceipt
					}
					continue
				}
			}
			value.Ranges = append(value.Ranges, rangeValue)
		}
		for _, rangeValue := range value.Ranges {
			if rangeValue.LatestReceipt.After(value.LatestArchiveReceipt) {
				value.LatestArchiveReceipt = rangeValue.LatestReceipt
			}
		}
		value.Gaps = gapsForRanges(value.Ranges, previousServer(previous, server), segmentSize, generation)
		switch {
		case builder.rangesTruncated:
			value.State, value.Reason = evidence.Unknown, "WAL range safety limit reached"
		case builder.outOfOrder:
			value.State, value.Reason = evidence.Unknown, "WAL listing order is incompatible"
		case invalidPosition:
			value.State, value.Reason = evidence.Unknown, "WAL name is invalid for metadata segment size"
		case builder.counts.Duplicates > 0:
			value.State, value.Reason = evidence.Unknown, "duplicate WAL representations require operator review"
		case builder.counts.Unknown > 0:
			value.State, value.Reason = evidence.Unknown, "unrecognized WAL objects require operator review"
		case len(value.Ranges) == 0:
			value.State, value.Reason = evidence.Unknown, "no complete WAL segments observed"
		case hasGapStatus(value.Gaps, GapConfirmed):
			value.State, value.Reason = evidence.Unhealthy, "confirmed segment-name gaps observed"
		case hasGapStatus(value.Gaps, GapCandidate):
			value.State, value.Reason = evidence.Warning, "candidate segment-name gaps observed"
		default:
			value.State, value.Reason = evidence.Healthy, "no segment-name gaps observed within compact ranges"
		}
		result.Servers = append(result.Servers, value)
	}
	sort.Slice(result.Servers, func(i, j int) bool { return result.Servers[i].Server < result.Servers[j].Server })
	return result
}

// HistoryObjects returns a copy of the bounded history references retained by
// the collector and whether the global history-file ceiling was exceeded.
func (c *WALCollector) HistoryObjects() ([]HistoryObject, bool) {
	return slices.Clone(c.historyObjects), c.historyTruncated
}

func gapsForRanges(ranges []WALRange, previous ServerWAL, segmentSize int64, generation uint64) []WALGap {
	prior := make(map[string]WALGap, len(previous.Gaps))
	for _, gap := range previous.Gaps {
		prior[gapIdentity(gap.Timeline, gap.Start, gap.End)] = gap
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
		if old, ok := prior[gapIdentity(gap.Timeline, gap.Start, gap.End)]; ok {
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

func previousServer(catalog WALCatalog, server string) ServerWAL {
	for _, value := range catalog.Servers {
		if value.Server == server {
			return value
		}
	}
	return ServerWAL{}
}

func hasGapStatus(gaps []WALGap, status GapStatus) bool {
	for _, gap := range gaps {
		if gap.Status == status {
			return true
		}
	}
	return false
}

func walContext(backups []Backup, server string) (int64, int64, string) {
	var version, segmentSize int64
	for _, backup := range backups {
		if backup.Server != server || backup.PostgreSQLVersion == 0 || backup.SegmentSize == 0 {
			continue
		}
		if backup.PostgreSQLVersion < 90300 || !ValidWALSegmentSize(backup.SegmentSize) {
			return 0, 0, "backup metadata has unsupported WAL arithmetic context"
		}
		if version == 0 {
			version, segmentSize = backup.PostgreSQLVersion, backup.SegmentSize
			continue
		}
		if version != backup.PostgreSQLVersion || segmentSize != backup.SegmentSize {
			return 0, 0, "backup metadata has conflicting WAL arithmetic context"
		}
	}
	if version == 0 {
		return 0, 0, "WAL continuity requires PostgreSQL version and segment size metadata"
	}
	return version, segmentSize, ""
}

// ValidWALSegmentSize reports whether size is a power of two between
// MinWALSegmentSize and MaxWALSegmentSize, as PostgreSQL requires.
func ValidWALSegmentSize(size int64) bool {
	return size >= MinWALSegmentSize && size <= MaxWALSegmentSize && size&(size-1) == 0
}

// WALPosition implements PostgreSQL's XLogSegmentsPerXLogId and
// XLogFromFileName arithmetic without assuming the default 16 MiB segment.
func WALPosition(timeline, log, segment uint32, segmentSize int64) (uint64, error) {
	if timeline == 0 || !ValidWALSegmentSize(segmentSize) {
		return 0, errors.New("invalid WAL arithmetic context")
	}
	// #nosec G115 -- the guard above rejects any segmentSize failing
	// ValidWALSegmentSize, so it is a power of two from 1 MiB to 1 GiB.
	segmentsPerID := uint64(1<<32) / uint64(segmentSize)
	if uint64(segment) >= segmentsPerID {
		return 0, errors.New("WAL segment component exceeds segment-size rollover")
	}
	return uint64(log)*segmentsPerID + uint64(segment), nil
}

// WALName is the inverse of WALPosition: it renders the 24-hex-digit segment
// file name for an absolute position on a timeline.
func WALName(timeline uint32, position uint64, segmentSize int64) (string, error) {
	if timeline == 0 || !ValidWALSegmentSize(segmentSize) {
		return "", errors.New("invalid WAL arithmetic context")
	}
	// #nosec G115 -- the guard above rejects any segmentSize failing
	// ValidWALSegmentSize, so it is a power of two from 1 MiB to 1 GiB.
	segmentsPerID := uint64(1<<32) / uint64(segmentSize)
	log := position / segmentsPerID
	if log > math.MaxUint32 {
		return "", errors.New("WAL position exceeds filename range")
	}
	segment := position % segmentsPerID
	// #nosec G115 -- log is range-checked against math.MaxUint32 above, and
	// segment is a modulus of segmentsPerID, at most 4096 for a 1 MiB segment.
	return fmt.Sprintf("%08X%08X%08X", timeline, uint32(log), uint32(segment)), nil
}

// ParseWALName parses a 24-hex-digit segment file name into its timeline and
// absolute segment position for the given segment size.
func ParseWALName(name string, segmentSize int64) (uint32, uint64, error) {
	if len(name) != 24 || !isHex(name) {
		return 0, 0, errors.New("invalid WAL segment name")
	}
	timeline, okTimeline := parseHex32(name[:8])
	log, okLog := parseHex32(name[8:16])
	segment, okSegment := parseHex32(name[16:24])
	if !okTimeline || !okLog || !okSegment {
		return 0, 0, errors.New("invalid WAL segment name")
	}
	position, err := WALPosition(timeline, log, segment, segmentSize)
	return timeline, position, err
}

// ClassifyWALObject recognizes only Barman's server/wals layout. The boolean
// reports whether the object belongs to a Barman WAL subtree at all.
func ClassifyWALObject(object store.Object) (ClassifiedWAL, bool) {
	parts := strings.Split(object.Key, "/")
	if len(parts) < 3 || parts[0] == "" || parts[1] != "wals" {
		return ClassifiedWAL{}, false
	}
	result := ClassifiedWAL{Key: object.Key, Server: parts[0], Class: WALUnknown, LastModified: object.LastModified.UTC(), Reason: "unrecognized Barman WAL object"}
	if len(parts) != 3 && len(parts) != 4 {
		return result, true
	}
	name, compression, supported := stripWALCompression(parts[len(parts)-1])
	result.Name, result.Compression = name, compression
	if !supported {
		result.Reason = "unsupported WAL compression suffix"
		return result, true
	}
	if len(name) == 16 && strings.HasSuffix(name, ".history") {
		if len(parts) != 3 {
			result.Reason = "timeline history file is outside the WAL root"
			return result, true
		}
		timeline, ok := parseHex32(name[:8])
		if !ok || timeline == 0 {
			result.Reason = "malformed timeline history name"
			return result, true
		}
		result.Class, result.Timeline, result.Reason = WALHistory, timeline, "timeline history metadata"
		return result, true
	}
	if len(parts) != 4 || len(parts[2]) != 16 || len(name) < 24 || !strings.EqualFold(parts[2], name[:16]) || !isHex(parts[2]) {
		result.Reason = "WAL object is outside its Barman hash directory"
		return result, true
	}
	timeline, okTimeline := parseHex32(name[:8])
	log, okLog := parseHex32(name[8:16])
	segment, okSegment := parseHex32(name[16:24])
	if !okTimeline || !okLog || !okSegment || timeline == 0 {
		result.Reason = "malformed WAL segment name"
		return result, true
	}
	result.Timeline, result.Log, result.Segment = timeline, log, segment
	switch {
	case len(name) == 24:
		result.Class, result.Reason = WALSegment, "complete WAL segment name"
	case len(name) == 32 && strings.HasSuffix(name, ".partial"):
		result.Class, result.Reason = WALPartial, "partial WAL does not fill continuity"
	case len(name) == 40 && name[24] == '.' && strings.HasSuffix(name, ".backup") && isHex(name[25:33]):
		result.Class, result.Reason = WALBackupHistory, "backup history metadata"
	default:
		result.Reason = "malformed WAL archive object name"
	}
	return result, true
}

func stripWALCompression(name string) (string, string, bool) {
	for _, value := range []struct{ suffix, compression string }{
		{suffix: ".snappy", compression: "snappy"},
		{suffix: ".lz4", compression: "lz4"},
		{suffix: ".bz2", compression: "bzip2"},
		{suffix: ".zst", compression: "zstd"},
		{suffix: ".gz", compression: "gzip"},
		{suffix: ".xz", compression: "xz"},
	} {
		if strings.HasSuffix(name, value.suffix) {
			return strings.TrimSuffix(name, value.suffix), value.compression, true
		}
	}
	if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
		tail := name[dot:]
		if tail != ".partial" && tail != ".history" && tail != ".backup" && !(len(tail) == 9 && isHex(tail[1:])) {
			return name, "", false
		}
	}
	return name, "none", true
}

func parseHex32(value string) (uint32, bool) {
	if len(value) != 8 || !isHex(value) {
		return 0, false
	}
	parsed, err := strconv.ParseUint(value, 16, 32)
	return uint32(parsed), err == nil
}

func isHex(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'A' && character <= 'F') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

// Validate checks the catalog's bounds, ordering, arithmetic and allowed
// values, so a catalog that fails it is never published.
func (c WALCatalog) Validate() error {
	if len(c.Servers) > 1_000 {
		return errors.New("server limit exceeded in Barman WAL catalog")
	}
	for index, server := range c.Servers {
		if !validDiscoveredName(server.Server) || (index > 0 && c.Servers[index-1].Server >= server.Server) || !validEvidenceState(server.State) || !boundedWALText(server.Reason) {
			return errors.New("invalid Barman WAL server evidence")
		}
		if server.PostgreSQLVersion < 0 || server.SegmentSize < 0 || (server.PostgreSQLVersion == 0) != (server.SegmentSize == 0) || (server.PostgreSQLVersion != 0 && (server.PostgreSQLVersion < 90300 || !ValidWALSegmentSize(server.SegmentSize))) {
			return errors.New("invalid Barman WAL arithmetic context")
		}
		if len(server.Ranges) > MaxWALRanges || len(server.Gaps) > MaxWALRanges || len(server.Diagnostics) > MaxWALDiagnostics || invalidWALCounts(server.Counts) {
			return errors.New("compact limits exceeded in Barman WAL catalog")
		}
		if (len(server.Ranges) > 0 || len(server.Gaps) > 0 || server.State != evidence.Unknown) && (server.PostgreSQLVersion < 90300 || !ValidWALSegmentSize(server.SegmentSize)) {
			return errors.New("no valid arithmetic context in Barman WAL evidence")
		}
		if (server.RangesTruncated || server.DiagnosticsTruncated || server.Counts.Duplicates > 0 || server.Counts.Unknown > 0) && server.State != evidence.Unknown {
			return errors.New("uncertain Barman WAL evidence must remain unknown")
		}
		for rangeIndex, value := range server.Ranges {
			first, firstErr := WALName(value.Timeline, value.Start, server.SegmentSize)
			last, lastErr := WALName(value.Timeline, value.End, server.SegmentSize)
			if value.Timeline == 0 || value.End == math.MaxUint64 || value.End < value.Start || value.Count != value.End-value.Start+1 || firstErr != nil || lastErr != nil || value.First != first || value.Last != last || !utcOrZero(value.LatestReceipt) || !utcOrZero(value.EndReceipt) {
				return errors.New("invalid Barman WAL range")
			}
			if rangeIndex > 0 {
				prior := server.Ranges[rangeIndex-1]
				if prior.Timeline > value.Timeline || (prior.Timeline == value.Timeline && prior.End >= value.Start) {
					return errors.New("unsorted or overlapping Barman WAL ranges")
				}
			}
		}
		for _, gap := range server.Gaps {
			first, firstErr := WALName(gap.Timeline, gap.Start, server.SegmentSize)
			last, lastErr := WALName(gap.Timeline, gap.End, server.SegmentSize)
			if gap.Timeline == 0 || gap.End == math.MaxUint64 || gap.End < gap.Start || gap.Count != gap.End-gap.Start+1 || firstErr != nil || lastErr != nil || gap.First != first || gap.Last != last || (gap.Status != GapCandidate && gap.Status != GapConfirmed) || gap.FirstObservedGeneration == 0 || gap.LastObservedGeneration < gap.FirstObservedGeneration || (gap.Status == GapCandidate && gap.LastObservedGeneration != gap.FirstObservedGeneration) || (gap.Status == GapConfirmed && gap.LastObservedGeneration == gap.FirstObservedGeneration) {
				return errors.New("invalid Barman WAL gap")
			}
		}
		for _, diagnostic := range server.Diagnostics {
			if diagnostic.Key == "" || len(diagnostic.Key) > store.MaxKeyBytes || strings.IndexFunc(diagnostic.Key, unicode.IsControl) >= 0 || len(diagnostic.Name) > store.MaxKeyBytes || !boundedWALText(diagnostic.Reason) || !validWALClass(diagnostic.Class) || !utcOrZero(diagnostic.LastModified) {
				return errors.New("invalid Barman WAL diagnostic")
			}
		}
		if !utcOrZero(server.LatestArchiveReceipt) {
			return errors.New("invalid Barman WAL archive receipt time")
		}
	}
	return nil
}

func invalidWALCounts(counts WALCounts) bool {
	return counts.Segments < 0 || counts.Partials < 0 || counts.History < 0 || counts.BackupHistory < 0 || counts.Unknown < 0 || counts.Duplicates < 0
}

// maxWALTextBytes bounds every reason and assumption a WAL or recovery
// evidence record carries.
const maxWALTextBytes = 256

func boundedWALText(value string) bool {
	return value != "" && len(value) <= maxWALTextBytes && strings.IndexFunc(value, unicode.IsControl) < 0
}

func validWALClass(class WALClass) bool {
	return class == WALSegment || class == WALPartial || class == WALHistory || class == WALBackupHistory || class == WALUnknown || class == WALDuplicate
}

func utcOrZero(value time.Time) bool {
	return value.IsZero() || value.Location() == time.UTC
}

func validEvidenceState(state evidence.State) bool {
	return state == evidence.Healthy || state == evidence.Warning || state == evidence.Unhealthy || state == evidence.Unknown
}

// CloneWALCatalog returns a copy of catalog that shares no slices with it.
func CloneWALCatalog(catalog WALCatalog) WALCatalog {
	result := WALCatalog{Servers: slices.Clone(catalog.Servers)}
	for index := range result.Servers {
		result.Servers[index].Ranges = slices.Clone(catalog.Servers[index].Ranges)
		result.Servers[index].Gaps = slices.Clone(catalog.Servers[index].Gaps)
		result.Servers[index].Diagnostics = slices.Clone(catalog.Servers[index].Diagnostics)
	}
	return result
}
