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
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/golang/snappy"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
	"github.com/ulikunitz/xz"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
)

// Bounds applied while reading timeline history files and building recovery paths.
const (
	// MaxHistoryObjectBytes caps the stored (possibly compressed) size of one
	// history file that is read.
	MaxHistoryObjectBytes int64 = 256 * 1024
	// MaxHistoryDecompressedBytes caps the decompressed size of one history file.
	MaxHistoryDecompressedBytes int64 = 256 * 1024
	// MaxHistoryEntries caps the switchpoint lines accepted in one history file.
	MaxHistoryEntries = 128
	// MaxRecoveryPaths caps the recovery paths evaluated per server.
	MaxRecoveryPaths = 10_000
)

// CoverageStop says why the walk along a recovery path's WAL stopped.
type CoverageStop string

const (
	// CoverageFrontier means the path reached the newest contiguous segment
	// archived on its target timeline.
	CoverageFrontier CoverageStop = "frontier"
	// CoverageCandidateLimited means the path stopped at a candidate gap.
	CoverageCandidateLimited CoverageStop = "candidate-limited"
	// CoverageGapLimited means the path stopped at a confirmed gap.
	CoverageGapLimited CoverageStop = "gap-limited"
	// CoverageUnknownLimited means the path stopped on missing, unclassified or
	// unknown evidence.
	CoverageUnknownLimited CoverageStop = "unknown-limited"
)

// HistoryEdge is one timeline switch read from a history file: Child forked
// from Parent at SwitchLSN, which falls in segment SwitchWAL (absolute
// position SwitchPosition).
type HistoryEdge struct {
	Parent, Child  uint32
	SwitchLSN      uint64
	SwitchPosition uint64
	SwitchWAL      string
}

// TimelineHistory is the parsed ancestry of one timeline, or the reason it
// could not be established (missing, unreadable, malformed or contradictory).
type TimelineHistory struct {
	Server, Key string
	Timeline    uint32
	State       evidence.State
	Reason      string
	Edges       []HistoryEdge
}

// RecoveryPath is how far the archived WAL lets one backup be rolled forward
// on one target timeline: from its start segment to the frontier where
// coverage stops, with the earliest recoverable time (LowerBound, the backup's
// end) and the assumptions the evaluation relied on.
type RecoveryPath struct {
	Server, BackupID                string
	TargetTimeline                  uint32
	State                           evidence.State
	Reason                          string
	Stop                            CoverageStop
	LowerBound                      time.Time
	StartTimeline, FrontierTimeline uint32
	StartPosition, FrontierPosition uint64
	StartWAL, FrontierWAL           string
	FrontierReceipt                 time.Time
	Assumptions                     []string
}

// RetentionSummary describes the visible backups of one server and, when an
// expectation is configured, whether enough of them are structurally usable.
type RetentionSummary struct {
	VisibleBackups, StructurallyUsable int
	OldestCompletion, NewestCompletion time.Time
	MinimumConfigured                  bool
	MinimumRedundancy                  int
	PolicyConfigured                   bool
	State                              evidence.State
	Reason                             string
}

// ServerRecovery is the timeline, recovery-coverage and retention evidence of
// one Barman server.
type ServerRecovery struct {
	Server         string
	TimelineState  evidence.State
	TimelineReason string
	CoverageState  evidence.State
	CoverageReason string
	Histories      []TimelineHistory
	Paths          []RecoveryPath
	Retention      RetentionSummary
}

// RecoveryCatalog is the recovery evidence of one repository, sorted by
// server name.
type RecoveryCatalog struct {
	Servers []ServerRecovery
}

// RecoveryOptions carries the retention expectations configured for the
// repository; a zero value means none.
type RecoveryOptions struct {
	ExpectedRetentionPolicy   string
	ExpectedMinimumRedundancy *int
}

// ParseHistory parses PostgreSQL's tab-separated ancestor/switchpoint format.
// Human-readable reason text is validated for boundedness and discarded.
func ParseHistory(data []byte, target uint32, segmentSize int64) ([]HistoryEdge, error) {
	if len(data) == 0 || int64(len(data)) > MaxHistoryDecompressedBytes || target <= 1 || !ValidWALSegmentSize(segmentSize) {
		return nil, errors.New("invalid bounded timeline history")
	}
	var entries []struct {
		parent uint32
		lsn    uint64
	}
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
		parentValue, err := strconv.ParseUint(strings.TrimSpace(fields[0]), 10, 32)
		if err != nil || parentValue == 0 {
			return nil, errors.New("malformed timeline parent")
		}
		lsn, err := ParseLSN(strings.TrimSpace(fields[1]))
		if err != nil || lsn == 0 {
			return nil, errors.New("malformed timeline switchpoint")
		}
		entries = append(entries, struct {
			parent uint32
			lsn    uint64
		}{uint32(parentValue), lsn})
		if len(entries) > MaxHistoryEntries {
			return nil, errors.New("timeline history entry limit reached")
		}
	}
	if err := scanner.Err(); err != nil || len(entries) == 0 {
		return nil, errors.New("malformed timeline history")
	}
	edges := make([]HistoryEdge, len(entries))
	for index, entry := range entries {
		if entry.parent >= target || (index > 0 && (entry.parent <= entries[index-1].parent || entry.lsn <= entries[index-1].lsn)) {
			return nil, errors.New("impossible timeline ancestry")
		}
		child := target
		if index+1 < len(entries) {
			child = entries[index+1].parent
		}
		// #nosec G115 -- ParseHistory rejects any segmentSize failing
		// ValidWALSegmentSize, so it is a power of two from 1 MiB to 1 GiB.
		position := entry.lsn / uint64(segmentSize)
		name, err := WALName(child, position, segmentSize)
		if err != nil {
			return nil, errors.New("timeline switchpoint exceeds WAL range")
		}
		edges[index] = HistoryEdge{Parent: entry.parent, Child: child, SwitchLSN: entry.lsn, SwitchPosition: position, SwitchWAL: name}
	}
	return edges, nil
}

// ParseLSN parses a PostgreSQL LSN written as two hexadecimal halves ("X/Y").
func ParseLSN(value string) (uint64, error) {
	high, low, ok := strings.Cut(value, "/")
	if !ok || high == "" || low == "" || len(high) > 8 || len(low) > 8 || !isHex(high) || !isHex(low) {
		return 0, errors.New("invalid LSN")
	}
	hi, errHigh := strconv.ParseUint(high, 16, 32)
	lo, errLow := strconv.ParseUint(low, 16, 32)
	if errHigh != nil || errLow != nil {
		return 0, errors.New("invalid LSN")
	}
	return hi<<32 | lo, nil
}

// AnalyzeRecovery reads the listed timeline history files and combines them
// with the backups and WAL catalog of a complete scan into per-server
// timeline, recovery-path and retention evidence.
func AnalyzeRecovery(ctx context.Context, reader store.Reader, backups []Backup, wal WALCatalog, historyObjects []HistoryObject, historyTruncated bool, options RecoveryOptions) RecoveryCatalog {
	serverNames := make(map[string]struct{})
	for _, server := range wal.Servers {
		serverNames[server.Server] = struct{}{}
	}
	for _, backup := range backups {
		serverNames[backup.Server] = struct{}{}
	}
	for _, object := range historyObjects {
		serverNames[object.Server] = struct{}{}
	}
	result := RecoveryCatalog{Servers: make([]ServerRecovery, 0, len(serverNames))}
	for serverName := range serverNames {
		serverWAL := previousServer(wal, serverName)
		if serverWAL.Server == "" {
			serverWAL.Server = serverName
		}
		serverBackups := backupsForServer(backups, serverName)
		serverObjects := historiesForServer(historyObjects, serverName)
		server := ServerRecovery{Server: serverName}
		server.Histories, server.TimelineState, server.TimelineReason = analyzeHistories(ctx, reader, serverWAL, serverObjects, historyTruncated)
		server.Paths = recoveryPaths(serverBackups, serverWAL, server.Histories)
		server.CoverageState, server.CoverageReason = coverageRollup(server.Paths)
		server.Retention = retentionSummary(serverBackups, options)
		result.Servers = append(result.Servers, server)
	}
	sort.Slice(result.Servers, func(i, j int) bool { return result.Servers[i].Server < result.Servers[j].Server })
	return result
}

func analyzeHistories(ctx context.Context, reader store.Reader, wal ServerWAL, objects []HistoryObject, truncated bool) ([]TimelineHistory, evidence.State, string) {
	if truncated {
		return nil, evidence.Unknown, "timeline history file limit reached"
	}
	byTimeline := make(map[uint32][]HistoryObject)
	for _, object := range objects {
		byTimeline[object.Timeline] = append(byTimeline[object.Timeline], object)
	}
	observed := observedTimelines(wal)
	for _, object := range objects {
		observed[object.Timeline] = struct{}{}
	}
	result := make([]TimelineHistory, 0, len(byTimeline))
	allEdges := make(map[uint32]HistoryEdge)
	knownChildren := make(map[uint32]bool)
	state, reason := evidence.Healthy, "all required timeline histories parsed"
	timelines := make([]uint32, 0, len(byTimeline))
	for timeline := range byTimeline {
		timelines = append(timelines, timeline)
	}
	slices.Sort(timelines)
	for _, timeline := range timelines {
		listed := byTimeline[timeline]
		history := TimelineHistory{Server: wal.Server, Timeline: timeline, State: evidence.Unknown}
		if len(listed) != 1 {
			history.Reason = "duplicate timeline history objects"
		} else {
			history.Key = listed[0].Key
			data, readErr := readHistory(ctx, reader, listed[0])
			var parseErr error
			if readErr == nil {
				history.Edges, parseErr = ParseHistory(data, timeline, wal.SegmentSize)
			}
			switch {
			case readErr != nil:
				history.Reason = "timeline history could not be read"
			case parseErr != nil:
				history.Reason = "timeline history is malformed"
			case !historyEdgesAgree(history.Edges, allEdges):
				history.Reason = "timeline histories contradict one another"
			case cycleInEdges(allEdges):
				history.Reason = "timeline ancestry is cyclic"
			case !childRangesAgree(history.Edges, wal.Ranges):
				history.Reason = "child WAL range disagrees with timeline switchpoint"
			default:
				history.State, history.Reason = evidence.Healthy, "timeline ancestry parsed"
				for _, edge := range history.Edges {
					knownChildren[edge.Child] = true
				}
			}
		}
		if history.State != evidence.Healthy {
			state, reason = evidence.Unknown, "timeline ancestry contains missing or invalid evidence"
		}
		result = append(result, history)
	}
	for timeline := range observed {
		if timeline > 1 && len(byTimeline[timeline]) == 0 && !knownChildren[timeline] {
			result = append(result, TimelineHistory{Server: wal.Server, Timeline: timeline, State: evidence.Unknown, Reason: "required timeline history is missing"})
			state, reason = evidence.Unknown, "timeline ancestry contains missing or invalid evidence"
		}
	}
	if len(observed) == 0 {
		state, reason = evidence.Unknown, "no WAL timelines observed"
	} else if len(result) == 0 {
		reason = "timeline 1 requires no history file"
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Timeline < result[j].Timeline })
	return result, state, reason
}

func readHistory(ctx context.Context, reader store.Reader, object HistoryObject) ([]byte, error) {
	// Adapters cut the body at MaxBytes, so ask for one byte more than the
	// limit: an object that exceeds it must be seen, not silently truncated.
	stream, err := reader.Open(ctx, store.OpenRequest{Key: object.Key, MaxBytes: MaxHistoryObjectBytes + 1})
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()
	compressed, err := io.ReadAll(io.LimitReader(stream, MaxHistoryObjectBytes+1))
	if err != nil || int64(len(compressed)) > MaxHistoryObjectBytes {
		return nil, errors.New("history object exceeds limit")
	}
	readerValue, closeValue, err := historyReader(bytes.NewReader(compressed), object.Compression)
	if err != nil {
		return nil, err
	}
	if closeValue != nil {
		defer func() { _ = closeValue.Close() }()
	}
	data, err := io.ReadAll(io.LimitReader(readerValue, MaxHistoryDecompressedBytes+1))
	if err != nil || int64(len(data)) > MaxHistoryDecompressedBytes {
		return nil, errors.New("decompressed history exceeds limit")
	}
	return data, nil
}

func historyReader(source io.Reader, compression string) (io.Reader, io.Closer, error) {
	switch compression {
	case "none":
		return source, nil, nil
	case "gzip":
		reader, err := gzip.NewReader(source)
		return reader, reader, err
	case "bzip2":
		return bzip2.NewReader(source), nil, nil
	case "snappy":
		return snappy.NewReader(source), nil, nil
	case "lz4":
		return lz4.NewReader(source), nil, nil
	case "xz":
		reader, err := xz.NewReader(source)
		return reader, nil, err
	case "zstd":
		reader, err := zstd.NewReader(source, zstd.WithDecoderMaxMemory(uint64(MaxHistoryDecompressedBytes*4)))
		if err != nil {
			return nil, nil, err
		}
		closer := reader.IOReadCloser()
		return closer, closer, nil
	default:
		return nil, nil, errors.New("unsupported history compression")
	}
}

func recoveryPaths(backups []Backup, wal ServerWAL, histories []TimelineHistory) []RecoveryPath {
	result := make([]RecoveryPath, 0)
	if conflictingSystemIDs(backups) {
		for _, backup := range backups {
			if backup.State != evidence.Healthy {
				continue
			}
			result = append(result, unknownPath(backup, "backup metadata has conflicting PostgreSQL system identifiers"))
		}
		sortRecoveryPaths(result)
		return result
	}
	for _, backup := range backups {
		if backup.State != evidence.Healthy {
			continue
		}
		startTimeline, start, end, err := backupRange(backup)
		if err != nil || wal.SegmentSize != backup.SegmentSize {
			result = append(result, unknownPath(backup, "backup WAL anchors or arithmetic context are invalid"))
			continue
		}
		own := evaluatePath(backup, wal, startTimeline, start, end, startTimeline, nil)
		result = append(result, own)
		seenTargets := map[uint32]bool{startTimeline: true}
		notApplicableTargets := make(map[uint32]bool)
		for _, history := range histories {
			if history.State != evidence.Healthy || history.Timeline == startTimeline {
				continue
			}
			edges, ok := descendantEdges(history.Edges, startTimeline)
			if !ok || len(edges) == 0 {
				continue
			}
			if branchBeforeBackup(edges[0], backup, end) {
				for _, edge := range edges {
					notApplicableTargets[edge.Child] = true
				}
				continue
			}
			for edgeIndex, edge := range edges {
				if seenTargets[edge.Child] {
					continue
				}
				seenTargets[edge.Child] = true
				result = append(result, evaluatePath(backup, wal, startTimeline, start, end, edge.Child, edges[:edgeIndex+1]))
				if len(result) >= MaxRecoveryPaths {
					limitedPath(result, backup)
					sortRecoveryPaths(result)
					return result
				}
			}
		}
		observedTargets := make([]uint32, 0)
		for timeline := range observedTimelines(wal) {
			if timeline > startTimeline && !seenTargets[timeline] && !notApplicableTargets[timeline] {
				observedTargets = append(observedTargets, timeline)
			}
		}
		slices.Sort(observedTargets)
		for _, target := range observedTargets {
			path := unknownPath(backup, "observed target timeline is not connected by valid history evidence")
			path.TargetTimeline = target
			result = append(result, path)
			if len(result) >= MaxRecoveryPaths {
				limitedPath(result, backup)
				sortRecoveryPaths(result)
				return result
			}
		}
	}
	sortRecoveryPaths(result)
	return result
}

func sortRecoveryPaths(result []RecoveryPath) {
	sort.Slice(result, func(i, j int) bool {
		if result[i].BackupID != result[j].BackupID {
			return result[i].BackupID < result[j].BackupID
		}
		return result[i].TargetTimeline < result[j].TargetTimeline
	})
}

func evaluatePath(backup Backup, wal ServerWAL, timeline uint32, start, requiredEnd uint64, target uint32, edges []HistoryEdge) RecoveryPath {
	path := RecoveryPath{Server: backup.Server, BackupID: backup.ID, TargetTimeline: target, LowerBound: backup.EndAt.UTC(), StartTimeline: timeline, StartPosition: start, StartWAL: backup.BeginWAL, Assumptions: recoveryAssumptions(backup)}
	if wal.State == evidence.Unknown {
		path.State, path.Stop, path.Reason = evidence.Unknown, CoverageUnknownLimited, "WAL continuity contains unknown evidence"
		return path
	}
	// Recovery reads each segment from the newest timeline of the path whose
	// start segment is not after it (XLogFileReadAnyTLI). A promotion at
	// switchpoint L copies the parent's WAL up to L into segment L/segsize of
	// the child (XLogInitNewTimeline) and archives the parent's own copy of
	// that segment only as .partial, which recovery never reads
	// (CleanupAfterArchiveRecovery). So the parent is needed through
	// SwitchPosition-1 and the child from SwitchPosition, whether L falls
	// mid-segment or exactly on a segment boundary.
	current, position := timeline, start
	var (
		frontier uint64
		receipt  time.Time
		state    evidence.State
		stop     CoverageStop
		reason   string
	)
	for _, edge := range edges {
		if edge.SwitchPosition < position {
			path.State, path.Stop, path.Reason = evidence.Unknown, CoverageUnknownLimited, "timeline switch precedes the recovery position"
			return path
		}
		if edge.SwitchPosition > position {
			frontier, receipt, state, stop, reason = reachPosition(wal, current, position, edge.SwitchPosition-1, childHasWAL(wal, edge))
			path.FrontierTimeline, path.FrontierPosition, path.FrontierReceipt = current, frontier, receipt
			path.FrontierWAL, _ = WALName(current, frontier, wal.SegmentSize)
			if state != evidence.Healthy {
				path.State, path.Stop, path.Reason = state, stop, reason
				return path
			}
		}
		if !rangeContains(wal.Ranges, edge.Child, edge.SwitchPosition) {
			path.State, path.Stop, path.Reason = evidence.Unknown, CoverageUnknownLimited, "timeline switch segment is not observed on the child timeline"
			return path
		}
		current, position = edge.Child, edge.SwitchPosition
	}
	// The backup's end segment is read from the timeline current at that
	// segment: the backup's own timeline, or the child when the backup
	// ended inside the switch segment.
	if position <= requiredEnd {
		frontier, receipt, state, stop, reason = reachPosition(wal, current, position, requiredEnd, false)
		path.FrontierTimeline, path.FrontierPosition, path.FrontierReceipt = current, frontier, receipt
		path.FrontierWAL, _ = WALName(current, frontier, wal.SegmentSize)
		if state != evidence.Healthy {
			path.State, path.Stop, path.Reason = state, stop, reason
			return path
		}
		position = requiredEnd
	}
	frontier, receipt, state, stop, reason = contiguousFrontier(wal, current, position)
	path.FrontierTimeline, path.FrontierPosition, path.FrontierReceipt = current, frontier, receipt
	path.FrontierWAL, _ = WALName(current, frontier, wal.SegmentSize)
	path.State, path.Stop, path.Reason = state, stop, reason
	return path
}

// childHasWAL reports whether the child of a timeline switch has archived WAL
// from its switch segment on. Its history and that WAL prove the parent
// closed: no server writes the parent's segments before the switch any more,
// so one that is missing is lost, not in flight. Without child WAL the
// promotion may be minutes old and the parent's last segments still on
// their way, which stays unknown.
func childHasWAL(wal ServerWAL, edge HistoryEdge) bool {
	return slices.ContainsFunc(wal.Ranges, func(value WALRange) bool {
		return value.Timeline == edge.Child && value.End >= edge.SwitchPosition
	})
}

// reachPosition walks timeline from start to required. closed says the
// timeline can receive no more WAL up to required (childHasWAL), so WAL
// missing at its end is lost.
func reachPosition(wal ServerWAL, timeline uint32, start, required uint64, closed bool) (uint64, time.Time, evidence.State, CoverageStop, string) {
	ranges := rangesForTimeline(wal.Ranges, timeline)
	for index, value := range ranges {
		if start < value.Start {
			return start, time.Time{}, evidence.Unknown, CoverageUnknownLimited, "required starting WAL segment was not observed"
		}
		if start > value.End {
			continue
		}
		if value.End >= required {
			receipt := time.Time{}
			if value.End == required {
				receipt = value.EndReceipt
			}
			return required, receipt, evidence.Healthy, CoverageFrontier, ""
		}
		if index+1 < len(ranges) {
			gap := gapAfter(wal.Gaps, timeline, value.End)
			if gap.Status == GapConfirmed {
				return value.End, value.EndReceipt, evidence.Unhealthy, CoverageGapLimited, "confirmed required WAL gap stops coverage"
			}
			if gap.Status == GapCandidate {
				return value.End, value.EndReceipt, evidence.Warning, CoverageCandidateLimited, "candidate required WAL gap stops coverage"
			}
			return value.End, value.EndReceipt, evidence.Unknown, CoverageUnknownLimited, "unclassified WAL discontinuity stops coverage"
		}
		if closed {
			return value.End, value.EndReceipt, evidence.Unhealthy, CoverageGapLimited, "WAL before the timeline switch is missing and the child timeline has WAL: it is lost"
		}
		return value.End, value.EndReceipt, evidence.Unknown, CoverageUnknownLimited, "required backup consistency WAL was not observed"
	}
	return start, time.Time{}, evidence.Unknown, CoverageUnknownLimited, "required starting WAL segment was not observed"
}

func contiguousFrontier(wal ServerWAL, timeline uint32, start uint64) (uint64, time.Time, evidence.State, CoverageStop, string) {
	ranges := rangesForTimeline(wal.Ranges, timeline)
	for index, value := range ranges {
		if start < value.Start {
			return start, time.Time{}, evidence.Unknown, CoverageUnknownLimited, "required starting WAL segment was not observed"
		}
		if start > value.End {
			continue
		}
		if index+1 < len(ranges) {
			gap := gapAfter(wal.Gaps, timeline, value.End)
			if gap.Status == GapConfirmed {
				return value.End, value.EndReceipt, evidence.Unhealthy, CoverageGapLimited, "confirmed required WAL gap stops coverage"
			}
			if gap.Status == GapCandidate {
				return value.End, value.EndReceipt, evidence.Warning, CoverageCandidateLimited, "candidate required WAL gap stops coverage"
			}
			return value.End, value.EndReceipt, evidence.Unknown, CoverageUnknownLimited, "unclassified WAL discontinuity stops coverage"
		}
		return value.End, value.EndReceipt, evidence.Healthy, CoverageFrontier, "observed contiguous segment-name coverage reaches the current archive frontier"
	}
	return start, time.Time{}, evidence.Unknown, CoverageUnknownLimited, "required starting WAL segment was not observed"
}

func backupRange(backup Backup) (uint32, uint64, uint64, error) {
	beginTimeline, begin, err := ParseWALName(backup.BeginWAL, backup.SegmentSize)
	if err != nil {
		return 0, 0, 0, err
	}
	endTimeline, end, err := ParseWALName(backup.EndWAL, backup.SegmentSize)
	if err != nil || beginTimeline != endTimeline || end < begin || (backup.Timeline != 0 && backup.Timeline != beginTimeline) {
		return 0, 0, 0, errors.New("invalid backup WAL range")
	}
	return beginTimeline, begin, end, nil
}

func branchBeforeBackup(edge HistoryEdge, backup Backup, end uint64) bool {
	if backup.EndLSN != "" {
		endLSN, err := ParseLSN(backup.EndLSN)
		return err != nil || edge.SwitchLSN <= endLSN
	}
	return edge.SwitchPosition <= end
}

func descendantEdges(edges []HistoryEdge, start uint32) ([]HistoryEdge, bool) {
	for index, edge := range edges {
		if edge.Parent == start {
			return slices.Clone(edges[index:]), true
		}
	}
	return nil, false
}

func retentionSummary(backups []Backup, options RecoveryOptions) RetentionSummary {
	value := RetentionSummary{VisibleBackups: len(backups), PolicyConfigured: options.ExpectedRetentionPolicy != "", State: evidence.Healthy, Reason: "descriptive backup inventory; no retention expectation configured"}
	for _, backup := range backups {
		if backup.State == evidence.Healthy {
			value.StructurallyUsable++
		}
		if !backup.EndAt.IsZero() && (value.OldestCompletion.IsZero() || backup.EndAt.Before(value.OldestCompletion)) {
			value.OldestCompletion = backup.EndAt.UTC()
		}
		if backup.EndAt.After(value.NewestCompletion) {
			value.NewestCompletion = backup.EndAt.UTC()
		}
	}
	if options.ExpectedMinimumRedundancy != nil {
		value.MinimumConfigured, value.MinimumRedundancy = true, *options.ExpectedMinimumRedundancy
		if value.StructurallyUsable < value.MinimumRedundancy {
			value.State, value.Reason = evidence.Unhealthy, "structurally usable backup count is below configured minimum"
		} else {
			value.Reason = "structurally usable backup count meets configured minimum"
		}
	}
	if value.PolicyConfigured {
		value.State, value.Reason = evidence.Unknown, "retention policy syntax is not interpreted in this slice"
	}
	return value
}

func coverageRollup(paths []RecoveryPath) (evidence.State, string) {
	if len(paths) == 0 {
		return evidence.Unknown, "no structurally usable backup recovery anchors"
	}
	state := evidence.Healthy
	for _, path := range paths {
		state = conservativeRecoveryState(state, path.State)
	}
	switch state {
	case evidence.Unknown:
		return state, "one or more recovery paths contain unknown evidence"
	case evidence.Unhealthy:
		return state, "one or more recovery paths cross a confirmed required gap"
	case evidence.Warning:
		return state, "one or more recovery paths stop at a candidate required gap"
	default:
		return state, "all evaluated recovery paths reach an observed archive frontier"
	}
}

func conservativeRecoveryState(left, right evidence.State) evidence.State {
	if left == evidence.Unknown || right == evidence.Unknown {
		return evidence.Unknown
	}
	if left == evidence.Unhealthy || right == evidence.Unhealthy {
		return evidence.Unhealthy
	}
	if left == evidence.Warning || right == evidence.Warning {
		return evidence.Warning
	}
	return evidence.Healthy
}

func historiesForServer(values []HistoryObject, server string) []HistoryObject {
	result := make([]HistoryObject, 0)
	for _, value := range values {
		if value.Server == server {
			result = append(result, value)
		}
	}
	return result
}

func backupsForServer(values []Backup, server string) []Backup {
	result := make([]Backup, 0)
	for _, value := range values {
		if value.Server == server {
			result = append(result, value)
		}
	}
	return result
}

func conflictingSystemIDs(backups []Backup) bool {
	known := ""
	for _, backup := range backups {
		if backup.State != evidence.Healthy || backup.SystemID == "" {
			continue
		}
		if known == "" {
			known = backup.SystemID
			continue
		}
		if known != backup.SystemID {
			return true
		}
	}
	return false
}

func recoveryAssumptions(backup Backup) []string {
	result := []string{"segment-name presence only", "timeline history metadata only", "WAL bytes and restore execution not verified"}
	if backup.SystemID == "" {
		return append(result, "backup system identifier unavailable")
	}
	return append(result, "backup system identifier retained; WAL objects do not carry it")
}

func observedTimelines(wal ServerWAL) map[uint32]struct{} {
	result := make(map[uint32]struct{})
	for _, value := range wal.Ranges {
		result[value.Timeline] = struct{}{}
	}
	return result
}

func historyEdgesAgree(edges []HistoryEdge, known map[uint32]HistoryEdge) bool {
	for _, edge := range edges {
		if prior, ok := known[edge.Child]; ok && prior != edge {
			return false
		}
	}
	for _, edge := range edges {
		known[edge.Child] = edge
	}
	return true
}

func cycleInEdges(edges map[uint32]HistoryEdge) bool {
	for child := range edges {
		seen := map[uint32]bool{}
		for current := child; current != 0; {
			if seen[current] {
				return true
			}
			seen[current] = true
			edge, ok := edges[current]
			if !ok {
				break
			}
			current = edge.Parent
		}
	}
	return false
}

// childRangesAgree rejects a history only when a child timeline has WAL
// before its switch segment, which PostgreSQL never writes. A child whose
// oldest observed segment is after the switch segment is the normal result
// of retention pruning; the path walk reports any segment a recovery needs
// but cannot find.
func childRangesAgree(edges []HistoryEdge, ranges []WALRange) bool {
	for _, edge := range edges {
		child := rangesForTimeline(ranges, edge.Child)
		if len(child) > 0 && child[0].Start < edge.SwitchPosition {
			return false
		}
	}
	return true
}

func rangesForTimeline(values []WALRange, timeline uint32) []WALRange {
	result := make([]WALRange, 0)
	for _, value := range values {
		if value.Timeline == timeline {
			result = append(result, value)
		}
	}
	return result
}

func rangeContains(values []WALRange, timeline uint32, position uint64) bool {
	for _, value := range values {
		if value.Timeline == timeline && value.Start <= position && position <= value.End {
			return true
		}
	}
	return false
}

func gapAfter(values []WALGap, timeline uint32, position uint64) WALGap {
	for _, value := range values {
		if value.Timeline == timeline && value.Start == position+1 {
			return value
		}
	}
	return WALGap{}
}

// unknownPath is an Unknown path for the backup, targeting the backup's own
// timeline: the published contract drops a path without a target timeline,
// which would leave an Unknown coverage rollup with no path explaining it.
func unknownPath(backup Backup, reason string) RecoveryPath {
	return RecoveryPath{Server: backup.Server, BackupID: backup.ID, TargetTimeline: backupTimeline(backup), State: evidence.Unknown, Stop: CoverageUnknownLimited, Reason: reason, LowerBound: backup.EndAt.UTC(), Assumptions: recoveryAssumptions(backup)}
}

// limitedPath replaces the last path with the safety-limit marker, keeping
// its target timeline so the paths stay sorted and unique.
func limitedPath(result []RecoveryPath, backup Backup) {
	target := result[len(result)-1].TargetTimeline
	result[len(result)-1] = unknownPath(backup, "recovery path safety limit reached")
	result[len(result)-1].TargetTimeline = target
}

// backupTimeline is the backup's timeline from backup.info, else the
// timeline of its begin WAL name, else timeline 1: a path whose timeline
// cannot be read is still published, as Unknown.
func backupTimeline(backup Backup) uint32 {
	if backup.Timeline != 0 {
		return backup.Timeline
	}
	if len(backup.BeginWAL) == 24 && isHex(backup.BeginWAL[:8]) {
		if value, err := strconv.ParseUint(backup.BeginWAL[:8], 16, 32); err == nil && value != 0 {
			return uint32(value)
		}
	}
	return 1
}

// Validate checks the catalog's bounds, ordering and allowed values, so a
// catalog that fails it is never published.
func (c RecoveryCatalog) Validate() error {
	if len(c.Servers) > 1_000 {
		return errors.New("server limit exceeded in Barman recovery catalog")
	}
	for index, server := range c.Servers {
		if !validDiscoveredName(server.Server) || (index > 0 && c.Servers[index-1].Server >= server.Server) || !validEvidenceState(server.TimelineState) || !validEvidenceState(server.CoverageState) || !boundedWALText(server.TimelineReason) || !boundedWALText(server.CoverageReason) || len(server.Paths) > MaxRecoveryPaths || len(server.Histories) > MaxHistoryFiles {
			return errors.New("invalid Barman recovery server")
		}
		for historyIndex, history := range server.Histories {
			if history.Server != server.Server || history.Timeline < 2 || !validEvidenceState(history.State) || !boundedWALText(history.Reason) || len(history.Edges) > MaxHistoryEntries {
				return errors.New("invalid Barman timeline history")
			}
			if historyIndex > 0 && server.Histories[historyIndex-1].Timeline >= history.Timeline {
				return errors.New("unsorted or duplicate Barman timeline histories")
			}
			if history.Key != "" && (len(history.Key) > store.MaxKeyBytes || strings.IndexFunc(history.Key, unicode.IsControl) >= 0) {
				return errors.New("invalid Barman timeline history key")
			}
			for _, edge := range history.Edges {
				if edge.Parent == 0 || edge.Child <= edge.Parent || edge.SwitchLSN == 0 || edge.SwitchWAL == "" || len(edge.SwitchWAL) != 24 || !isHex(edge.SwitchWAL) {
					return errors.New("invalid Barman timeline history edge")
				}
			}
		}
		for pathIndex, path := range server.Paths {
			if path.Server != server.Server || path.BackupID == "" || len(path.BackupID) > 256 || strings.IndexFunc(path.BackupID, unicode.IsControl) >= 0 || !validEvidenceState(path.State) || !boundedWALText(path.Reason) || (path.Stop != CoverageFrontier && path.Stop != CoverageCandidateLimited && path.Stop != CoverageGapLimited && path.Stop != CoverageUnknownLimited) || !utcOrZero(path.LowerBound) || !utcOrZero(path.FrontierReceipt) || len(path.Assumptions) > 8 || !optionalWALName(path.StartWAL) || !optionalWALName(path.FrontierWAL) {
				return errors.New("invalid Barman recovery path")
			}
			if pathIndex > 0 && (server.Paths[pathIndex-1].BackupID > path.BackupID || (server.Paths[pathIndex-1].BackupID == path.BackupID && server.Paths[pathIndex-1].TargetTimeline >= path.TargetTimeline)) {
				return errors.New("unsorted or duplicate Barman recovery paths")
			}
			for _, assumption := range path.Assumptions {
				if !boundedWALText(assumption) {
					return errors.New("invalid Barman recovery assumption")
				}
			}
		}
		retention := server.Retention
		if retention.VisibleBackups < 0 || retention.StructurallyUsable < 0 || retention.StructurallyUsable > retention.VisibleBackups || retention.MinimumRedundancy < 0 || (!retention.MinimumConfigured && retention.MinimumRedundancy != 0) || !validEvidenceState(retention.State) || !boundedWALText(retention.Reason) || !utcOrZero(retention.OldestCompletion) || !utcOrZero(retention.NewestCompletion) || (!retention.OldestCompletion.IsZero() && !retention.NewestCompletion.IsZero() && retention.NewestCompletion.Before(retention.OldestCompletion)) {
			return errors.New("invalid Barman retention summary")
		}
	}
	return nil
}

func optionalWALName(value string) bool {
	return value == "" || (len(value) == 24 && isHex(value))
}

// CloneRecoveryCatalog returns a copy of value that shares no slices with it.
func CloneRecoveryCatalog(value RecoveryCatalog) RecoveryCatalog {
	result := RecoveryCatalog{Servers: make([]ServerRecovery, len(value.Servers))}
	for index, server := range value.Servers {
		result.Servers[index] = server
		result.Servers[index].Histories = make([]TimelineHistory, len(server.Histories))
		for historyIndex, history := range server.Histories {
			result.Servers[index].Histories[historyIndex] = history
			result.Servers[index].Histories[historyIndex].Edges = slices.Clone(history.Edges)
		}
		result.Servers[index].Paths = make([]RecoveryPath, len(server.Paths))
		for pathIndex, path := range server.Paths {
			result.Servers[index].Paths[pathIndex] = path
			result.Servers[index].Paths[pathIndex].Assumptions = slices.Clone(path.Assumptions)
		}
	}
	return result
}
