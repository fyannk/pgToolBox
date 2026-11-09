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
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/golang/snappy"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
	"github.com/ulikunitz/xz"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
	"github.com/fyannk/pgToolBox/internal/evidence/store/storetest"
)

// validHistory is the history of timeline 3 after two promotions, each in
// the middle of a segment (switch segments 2 and 4), as CNPG failovers are.
const validHistory = "1\t0/020000A0\tfirst branch\n2\t0/040000A0\tsecond branch\n"

func TestTimelineHistoryParserUsesExactPostgreSQLSwitchpoints(t *testing.T) {
	t.Parallel()
	edges, err := ParseHistory([]byte(validHistory), 3, 16<<20)
	if err != nil || len(edges) != 2 || edges[0].Parent != 1 || edges[0].Child != 2 || edges[0].SwitchPosition != 2 || edges[1].Parent != 2 || edges[1].Child != 3 || edges[1].SwitchPosition != 4 {
		t.Fatalf("ParseHistory() = %#v, %v", edges, err)
	}
	for _, malformed := range []string{"", "# only\n", "1 0/2 reason\n", "1\tbad\treason\n", "2\t0/2\ta\n1\t0/4\tb\n", "1\t0/4\ta\n2\t0/2\tb\n", "3\t0/2\tcycle\n"} {
		if _, err := ParseHistory([]byte(malformed), 3, 16<<20); err == nil {
			t.Errorf("ParseHistory(%q) error = nil", malformed)
		}
	}
	cycle := map[uint32]HistoryEdge{2: {Parent: 3, Child: 2}, 3: {Parent: 2, Child: 3}}
	if !cycleInEdges(cycle) {
		t.Fatal("cycle was not detected")
	}
}

func TestTimelineHistoryReaderSupportsEveryBarmanCloudCodec(t *testing.T) {
	t.Parallel()
	plain := []byte("1\t0/2000000\tbranch\n")
	encoders := map[string]func(*testing.T) []byte{
		"none": func(*testing.T) []byte { return plain },
		"gzip": func(t *testing.T) []byte {
			return encodeWithCloser(t, plain, func(buffer *bytes.Buffer) io.WriteCloser { return gzip.NewWriter(buffer) })
		},
		"bzip2": func(t *testing.T) []byte {
			value, err := base64.StdEncoding.DecodeString("QlpoOTFBWSZTWSY7m4EAAAVZgBAwAADwADhBEAAgACIAA0IBoAeF1idCqaEAcPF3JFOFCQJjubgQ")
			if err != nil {
				t.Fatal(err)
			}
			return value
		},
		"snappy": func(t *testing.T) []byte {
			return encodeWithCloser(t, plain, func(buffer *bytes.Buffer) io.WriteCloser { return snappy.NewBufferedWriter(buffer) })
		},
		"lz4": func(t *testing.T) []byte {
			return encodeWithCloser(t, plain, func(buffer *bytes.Buffer) io.WriteCloser { return lz4.NewWriter(buffer) })
		},
		"xz": func(t *testing.T) []byte {
			return encodeWithErrorCloser(t, plain, func(buffer *bytes.Buffer) (io.WriteCloser, error) { return xz.NewWriter(buffer) })
		},
		"zstd": func(t *testing.T) []byte {
			return encodeWithErrorCloser(t, plain, func(buffer *bytes.Buffer) (io.WriteCloser, error) { return zstd.NewWriter(buffer) })
		},
	}
	for compression, encode := range encoders {
		t.Run(compression, func(t *testing.T) {
			t.Parallel()
			reader, closer, err := historyReader(bytes.NewReader(encode(t)), compression)
			if err != nil {
				t.Fatal(err)
			}
			if closer != nil {
				defer closer.Close()
			}
			decoded, err := io.ReadAll(reader)
			if err != nil || !bytes.Equal(decoded, plain) {
				t.Fatalf("decoded = %q, %v", decoded, err)
			}
		})
	}
}

func TestTimelineHistoryBoundsFailUnknown(t *testing.T) {
	t.Parallel()
	oversized := bytes.Repeat([]byte("x"), int(MaxHistoryObjectBytes)+1)
	reader := &storetest.Fake{OpenFunc: func(_ context.Context, request store.OpenRequest) (io.ReadCloser, error) {
		if request.MaxBytes != MaxHistoryObjectBytes+1 {
			t.Fatalf("Open MaxBytes = %d", request.MaxBytes)
		}
		return io.NopCloser(bytes.NewReader(oversized)), nil
	}}
	if _, err := readHistory(context.Background(), reader, HistoryObject{Key: "alpha/wals/00000002.history", Compression: "none"}); err == nil {
		t.Fatal("oversized compressed history was accepted")
	}

	decompressed := bytes.Repeat([]byte("x"), int(MaxHistoryDecompressedBytes)+1)
	compressed := encodeWithCloser(t, decompressed, func(buffer *bytes.Buffer) io.WriteCloser { return gzip.NewWriter(buffer) })
	reader = &storetest.Fake{OpenFunc: func(context.Context, store.OpenRequest) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(compressed)), nil
	}}
	if _, err := readHistory(context.Background(), reader, HistoryObject{Key: "alpha/wals/00000002.history.gz", Compression: "gzip"}); err == nil {
		t.Fatal("oversized decompressed history was accepted")
	}

	var entries strings.Builder
	for index := 1; index <= MaxHistoryEntries+1; index++ {
		_, _ = fmt.Fprintf(&entries, "%d\t0/%08X\tbranch\n", index, index)
	}
	if _, err := ParseHistory([]byte(entries.String()), MaxHistoryEntries+2, 16<<20); err == nil {
		t.Fatal("history entry ceiling was not enforced")
	}

	backup := recoveryBackup(time.Now().UTC(), 1, 1)
	catalog := AnalyzeRecovery(context.Background(), &storetest.Fake{}, []Backup{backup}, WALCatalog{Servers: []ServerWAL{recoveryWAL()}}, nil, true, RecoveryOptions{})
	if catalog.Servers[0].TimelineState != evidence.Unknown {
		t.Fatalf("truncated history catalog = %#v", catalog.Servers[0])
	}
}

// TestOversizedMetadataIsRejectedNotTruncated reads through the shared
// fake, which cuts each body at MaxBytes as the S3, Azure and GCS adapters
// do: a history file or backup.info one byte over its limit must be
// rejected, not parsed from a truncated prefix.
func TestOversizedMetadataIsRejectedNotTruncated(t *testing.T) {
	t.Parallel()
	serve := func(content string) store.Reader {
		return &storetest.Fake{OpenFunc: func(context.Context, store.OpenRequest) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(content)), nil
		}}
	}
	pad := func(prefix string, size int64) string {
		return prefix + strings.Repeat("#", int(size)-len(prefix)-1) + "\n"
	}
	history := "1\t0/30000A0\tbranch\n"
	if _, err := readHistory(context.Background(), serve(pad(history, MaxHistoryObjectBytes)), HistoryObject{Key: "alpha/wals/00000002.history", Compression: "none"}); err != nil {
		t.Fatalf("history at the limit rejected: %v", err)
	}
	if _, err := readHistory(context.Background(), serve(pad(history, MaxHistoryObjectBytes+1)), HistoryObject{Key: "alpha/wals/00000002.history", Compression: "none"}); err == nil {
		t.Fatal("history one byte over the limit was accepted after truncation")
	}
	info := "status=DONE\n"
	if backup := readBackup(context.Background(), serve(pad(info, MaxBackupInfoBytes)), "alpha/base/b1/backup.info", "alpha", "b1"); backup.Status != "DONE" {
		t.Fatalf("backup.info at the limit = %s %q", backup.State, backup.Reason)
	}
	if backup := readBackup(context.Background(), serve(pad(info, MaxBackupInfoBytes+1)), "alpha/base/b1/backup.info", "alpha", "b1"); backup.State != evidence.Unknown || backup.Reason != "backup metadata is unreadable" {
		t.Fatalf("backup.info one byte over the limit = %s %q", backup.State, backup.Reason)
	}
}

func TestRecoveryCoverageTraversesValidHistoryAndStopsAtRelevantGaps(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	backup := recoveryBackup(now, 1, 1)
	wal := recoveryWAL()
	reader := historyFake(validHistory)
	catalog := AnalyzeRecovery(context.Background(), reader, []Backup{backup}, WALCatalog{Servers: []ServerWAL{wal}}, []HistoryObject{{Key: "alpha/wals/00000003.history", Server: "alpha", Name: "00000003.history", Compression: "none", Timeline: 3}}, false, RecoveryOptions{})
	server := catalog.Servers[0]
	if server.TimelineState != evidence.Healthy || server.CoverageState != evidence.Warning || len(server.Paths) != 3 {
		t.Fatalf("valid graph = %#v", server)
	}
	path := findPath(t, server.Paths, 1)
	if path.State != evidence.Healthy || path.FrontierTimeline != 1 || path.FrontierPosition != 1 || path.Stop != CoverageFrontier {
		t.Fatalf("own timeline path = %#v", path)
	}
	path = findPath(t, server.Paths, 2)
	if path.State != evidence.Healthy || path.FrontierTimeline != 2 || path.FrontierPosition != 3 || path.Stop != CoverageFrontier {
		t.Fatalf("first failover path = %#v", path)
	}
	path = findPath(t, server.Paths, 3)
	if path.State != evidence.Warning || path.FrontierTimeline != 3 || path.FrontierPosition != 6 || path.Stop != CoverageCandidateLimited {
		t.Fatalf("candidate path = %#v", path)
	}
	wal.Gaps[0].Status = GapConfirmed
	catalog = AnalyzeRecovery(context.Background(), reader, []Backup{backup}, WALCatalog{Servers: []ServerWAL{wal}}, []HistoryObject{{Key: "alpha/wals/00000003.history", Server: "alpha", Name: "00000003.history", Compression: "none", Timeline: 3}}, false, RecoveryOptions{})
	path = findPath(t, catalog.Servers[0].Paths, 3)
	if path.State != evidence.Unhealthy || path.Stop != CoverageGapLimited {
		t.Fatalf("confirmed path = %#v", path)
	}
}

// TestRecoveryCoverageCrossesFailoverWhosePartialSwitchSegmentIsUnused pins
// the PostgreSQL rule for a timeline switch: the parent timeline is read up
// to the segment before the switch segment, the child from the switch
// segment on. The parent's copy of the switch segment is archived as
// .partial, which the WAL collector does not count.
func TestRecoveryCoverageCrossesFailoverWhosePartialSwitchSegmentIsUnused(t *testing.T) {
	t.Parallel()
	const segment = 16 << 20
	tests := []struct {
		name          string
		history       string
		begin, end    uint64
		endLSN        string
		ranges        []WALRange
		state         evidence.State
		frontier      uint64
		frontierTL    uint32
		ownTimelineOK bool
	}{
		{name: "mid-segment switch, parent switch segment only partial", history: "1\t0/30000A0\tpromoted\n", begin: 1, end: 1, ranges: []WALRange{walRange(1, 1, 2), walRange(2, 3, 5)}, state: evidence.Healthy, frontier: 5, frontierTL: 2, ownTimelineOK: true},
		{name: "boundary switch, parent ends complete before the switch segment", history: "1\t0/3000000\tpromoted\n", begin: 1, end: 1, ranges: []WALRange{walRange(1, 1, 2), walRange(2, 3, 5)}, state: evidence.Healthy, frontier: 5, frontierTL: 2, ownTimelineOK: true},
		{name: "backup ends inside the switch segment", history: "1\t0/30000A0\tpromoted\n", begin: 1, end: 3, endLSN: "0/3000050", ranges: []WALRange{walRange(1, 1, 2), walRange(2, 3, 5)}, state: evidence.Healthy, frontier: 5, frontierTL: 2},
		{name: "backup begins and ends inside the switch segment", history: "1\t0/30000A0\tpromoted\n", begin: 3, end: 3, endLSN: "0/3000050", ranges: []WALRange{walRange(1, 1, 2), walRange(2, 3, 5)}, state: evidence.Healthy, frontier: 5, frontierTL: 2},
		{name: "child switch segment missing", history: "1\t0/30000A0\tpromoted\n", begin: 1, end: 1, ranges: []WALRange{walRange(1, 1, 2), walRange(2, 4, 5)}, state: evidence.Unknown, frontier: 2, frontierTL: 1, ownTimelineOK: true},
		// The child's history and WAL from its switch segment on prove the
		// parent closed: its missing segment before the switch is lost.
		{name: "parent segment before the switch missing", history: "1\t0/30000A0\tpromoted\n", begin: 1, end: 1, ranges: []WALRange{walRange(1, 1, 1), walRange(2, 3, 5)}, state: evidence.Unhealthy, frontier: 1, frontierTL: 1, ownTimelineOK: true},
		{name: "parent segment before the switch missing, child WAL only after its switch segment", history: "1\t0/30000A0\tpromoted\n", begin: 1, end: 1, ranges: []WALRange{walRange(1, 1, 1), walRange(2, 4, 5)}, state: evidence.Unhealthy, frontier: 1, frontierTL: 1, ownTimelineOK: true},
		// Without child WAL the promotion may be minutes old and the
		// parent's last segment still on its way: unknown, not lost.
		{name: "parent segment before the switch missing, child has no WAL yet", history: "1\t0/30000A0\tpromoted\n", begin: 1, end: 1, ranges: []WALRange{walRange(1, 1, 1)}, state: evidence.Unknown, frontier: 1, frontierTL: 1, ownTimelineOK: true},
		{name: "history archived, child switch segment not yet", history: "1\t0/30000A0\tpromoted\n", begin: 1, end: 1, ranges: []WALRange{walRange(1, 1, 2)}, state: evidence.Unknown, frontier: 2, frontierTL: 1, ownTimelineOK: true},
		// The backup ends exactly at the switchpoint: every byte it needs is
		// before the child's first byte, so it follows the child.
		{name: "backup ends at the switchpoint", history: "1\t0/3000050\tpromoted\n", begin: 1, end: 3, endLSN: "0/3000050", ranges: []WALRange{walRange(1, 1, 2), walRange(2, 3, 5)}, state: evidence.Healthy, frontier: 5, frontierTL: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			backup := recoveryBackup(time.Now().UTC(), test.begin, test.end)
			backup.EndLSN = test.endLSN
			wal := ServerWAL{Server: "alpha", State: evidence.Healthy, PostgreSQLVersion: 180000, SegmentSize: segment, Ranges: test.ranges}
			objects := []HistoryObject{{Key: "alpha/wals/00000002.history", Server: "alpha", Name: "00000002.history", Compression: "none", Timeline: 2}}
			catalog := AnalyzeRecovery(context.Background(), historyFake(test.history), []Backup{backup}, WALCatalog{Servers: []ServerWAL{wal}}, objects, false, RecoveryOptions{})
			server := catalog.Servers[0]
			if server.TimelineState != evidence.Healthy {
				t.Fatalf("timeline = %s %q", server.TimelineState, server.TimelineReason)
			}
			path := findPath(t, server.Paths, 2)
			if path.State != test.state || path.FrontierTimeline != test.frontierTL || path.FrontierPosition != test.frontier {
				t.Fatalf("failover path = state %s stop %s reason %q frontier TL%d/%d", path.State, path.Stop, path.Reason, path.FrontierTimeline, path.FrontierPosition)
			}
			own := findPath(t, server.Paths, 1)
			if (own.State == evidence.Healthy) != test.ownTimelineOK {
				t.Fatalf("own timeline path = state %s reason %q", own.State, own.Reason)
			}
			if err := catalog.Validate(); err != nil {
				t.Fatalf("Validate() = %v", err)
			}
		})
	}
}

// TestBranchBeforeBackupAtTheSwitchpoint pins the end LSN == switchpoint edge
// to PostgreSQL's rule (see branchBeforeBackup): the backup follows the child.
func TestBranchBeforeBackupAtTheSwitchpoint(t *testing.T) {
	t.Parallel()
	edge := HistoryEdge{Parent: 1, Child: 2, SwitchLSN: 0x3000050, SwitchPosition: 3}
	for _, test := range []struct {
		name     string
		endLSN   string
		end      uint64
		branches bool
	}{
		{name: "ends before the switchpoint", endLSN: "0/300004F", end: 3},
		{name: "ends at the switchpoint", endLSN: "0/3000050", end: 3},
		{name: "ends one byte after the switchpoint", endLSN: "0/3000051", end: 3, branches: true},
		{name: "ends in a later segment", endLSN: "0/4000028", end: 4, branches: true},
		{name: "unparsable end LSN", endLSN: "0/zz", end: 3, branches: true},
		{name: "no end LSN, ends before the switch segment", end: 2},
		{name: "no end LSN, ends in the switch segment", end: 3, branches: true},
	} {
		if got := branchBeforeBackup(edge, Backup{EndLSN: test.endLSN}, test.end); got != test.branches {
			t.Errorf("%s: branchBeforeBackup() = %v", test.name, got)
		}
	}
	// One byte after the switchpoint, the child is not a recovery target.
	backup := recoveryBackup(time.Now().UTC(), 1, 3)
	backup.EndLSN = "0/3000051"
	wal := ServerWAL{Server: "alpha", State: evidence.Healthy, PostgreSQLVersion: 180000, SegmentSize: 16 << 20, Ranges: []WALRange{walRange(1, 1, 3), walRange(2, 3, 5)}}
	objects := []HistoryObject{{Key: "alpha/wals/00000002.history", Server: "alpha", Name: "00000002.history", Compression: "none", Timeline: 2}}
	catalog := AnalyzeRecovery(context.Background(), historyFake("1\t0/3000050\tpromoted\n"), []Backup{backup}, WALCatalog{Servers: []ServerWAL{wal}}, objects, false, RecoveryOptions{})
	for _, path := range catalog.Servers[0].Paths {
		if path.TargetTimeline == 2 {
			t.Fatalf("a backup that ends after the switchpoint has a path to the child: %#v", path)
		}
	}
}

// TestTimelineAncestrySurvivesRetentionPruningBeforeFailover keeps the
// ancestry Healthy once retention removed the WAL from before a failover:
// the child timeline then starts after its switch segment, which is
// pruning, not a contradiction.
func TestTimelineAncestrySurvivesRetentionPruningBeforeFailover(t *testing.T) {
	t.Parallel()
	const segment = 16 << 20
	backupOn := func(timeline uint32, begin, end uint64) Backup {
		beginName, _ := WALName(timeline, begin, segment)
		endName, _ := WALName(timeline, end, segment)
		return Backup{Server: "alpha", ID: "b2", State: evidence.Healthy, BeginWAL: beginName, EndWAL: endName, SegmentSize: segment, PostgreSQLVersion: 180000, Timeline: timeline, EndAt: time.Now().UTC()}
	}
	tests := []struct {
		name    string
		history string
		object  HistoryObject
		ranges  []WALRange
		target  uint32
		end     uint64
	}{
		{name: "one failover, pre-failover WAL pruned", history: "1\t0/30000A0\tpromoted\n", object: HistoryObject{Key: "alpha/wals/00000002.history", Server: "alpha", Name: "00000002.history", Compression: "none", Timeline: 2}, ranges: []WALRange{walRange(2, 0x50, 0x60)}, target: 2, end: 0x60},
		{name: "next failover carries the pruned 1 to 2 edge", history: "1\t0/30000A0\tpromoted\n2\t0/580000A0\tpromoted again\n", object: HistoryObject{Key: "alpha/wals/00000003.history", Server: "alpha", Name: "00000003.history", Compression: "none", Timeline: 3}, ranges: []WALRange{walRange(2, 0x50, 0x57), walRange(3, 0x58, 0x60)}, target: 3, end: 0x60},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			wal := ServerWAL{Server: "alpha", State: evidence.Healthy, PostgreSQLVersion: 180000, SegmentSize: segment, Ranges: test.ranges}
			catalog := AnalyzeRecovery(context.Background(), historyFake(test.history), []Backup{backupOn(2, 0x50, 0x51)}, WALCatalog{Servers: []ServerWAL{wal}}, []HistoryObject{test.object}, false, RecoveryOptions{})
			server := catalog.Servers[0]
			if server.TimelineState != evidence.Healthy {
				t.Fatalf("timeline = %s %q; histories %#v", server.TimelineState, server.TimelineReason, server.Histories)
			}
			path := findPath(t, server.Paths, test.target)
			if path.State != evidence.Healthy || path.FrontierTimeline != test.target || path.FrontierPosition != test.end {
				t.Fatalf("path = state %s reason %q frontier TL%d/%d", path.State, path.Reason, path.FrontierTimeline, path.FrontierPosition)
			}
		})
	}
}

func TestRecoveryCoverageIgnoresHistoricalGapBeforeAnchor(t *testing.T) {
	t.Parallel()
	backup := recoveryBackup(time.Now().UTC(), 3, 3)
	wal := ServerWAL{
		Server: "alpha", State: evidence.Warning, PostgreSQLVersion: 180000, SegmentSize: 16 << 20,
		Ranges: []WALRange{walRange(1, 1, 1), walRange(1, 3, 5)},
		Gaps:   []WALGap{{Timeline: 1, Start: 2, End: 2, Count: 1, Status: GapCandidate, FirstObservedGeneration: 1, LastObservedGeneration: 1}},
	}
	catalog := AnalyzeRecovery(context.Background(), &storetest.Fake{}, []Backup{backup}, WALCatalog{Servers: []ServerWAL{wal}}, nil, false, RecoveryOptions{})
	path := catalog.Servers[0].Paths[0]
	if path.State != evidence.Healthy || path.FrontierPosition != 5 {
		t.Fatalf("historical gap invalidated path: %#v", path)
	}
}

func TestTimelineAndCoverageFailUnknownForMissingMalformedAndInconsistentEvidence(t *testing.T) {
	t.Parallel()
	backup := recoveryBackup(time.Now().UTC(), 1, 1)
	wal := recoveryWAL()
	tests := []struct {
		name    string
		objects []HistoryObject
		reader  store.Reader
		mutate  func(*ServerWAL)
	}{
		{name: "missing history", reader: &storetest.Fake{}},
		{name: "malformed history", objects: []HistoryObject{{Key: "alpha/wals/00000003.history", Server: "alpha", Compression: "none", Timeline: 3}}, reader: historyFake("malformed")},
		{name: "child precedes switch", objects: []HistoryObject{{Key: "alpha/wals/00000003.history", Server: "alpha", Compression: "none", Timeline: 3}}, reader: historyFake(validHistory), mutate: func(value *ServerWAL) {
			value.Ranges = append(value.Ranges, walRange(3, 1, 1))
			sortRanges(value.Ranges)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := wal
			value.Ranges = append([]WALRange(nil), wal.Ranges...)
			if test.mutate != nil {
				test.mutate(&value)
			}
			catalog := AnalyzeRecovery(context.Background(), test.reader, []Backup{backup}, WALCatalog{Servers: []ServerWAL{value}}, test.objects, false, RecoveryOptions{})
			if catalog.Servers[0].TimelineState != evidence.Unknown || catalog.Servers[0].CoverageState != evidence.Unknown {
				t.Fatalf("timeline = %#v", catalog.Servers[0])
			}
		})
	}
	missingEnd := recoveryBackup(time.Now().UTC(), 1, 7)
	missingWAL := wal
	missingWAL.Ranges = []WALRange{walRange(1, 1, 1)}
	missingWAL.Gaps = nil
	catalog := AnalyzeRecovery(context.Background(), &storetest.Fake{}, []Backup{missingEnd}, WALCatalog{Servers: []ServerWAL{missingWAL}}, nil, false, RecoveryOptions{})
	if catalog.Servers[0].Paths[0].State != evidence.Unknown {
		t.Fatalf("missing consistency range = %#v", catalog.Servers[0].Paths[0])
	}
}

func TestBarmanRetentionIsDescriptiveUnlessExpectationConfigured(t *testing.T) {
	t.Parallel()
	backups := []Backup{{State: evidence.Healthy, EndAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}, {State: evidence.Unhealthy, EndAt: time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)}}
	value := retentionSummary(backups, RecoveryOptions{})
	if value.State != evidence.Healthy || value.StructurallyUsable != 1 {
		t.Fatalf("descriptive = %#v", value)
	}
	minimum := 2
	value = retentionSummary(backups, RecoveryOptions{ExpectedMinimumRedundancy: &minimum})
	if value.State != evidence.Unhealthy {
		t.Fatalf("minimum = %#v", value)
	}
	value = retentionSummary(backups, RecoveryOptions{ExpectedRetentionPolicy: "RECOVERY WINDOW OF 7 DAYS"})
	if value.State != evidence.Unknown {
		t.Fatalf("uninterpreted policy = %#v", value)
	}
}

func TestRecoveryCoverageDoesNotMergeConflictingSystemIdentifiers(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	first := recoveryBackup(now, 1, 1)
	first.ID, first.SystemID = "first", "7612345678901234567"
	second := recoveryBackup(now, 1, 1)
	second.ID, second.SystemID = "second", "7612345678901234568"
	catalog := AnalyzeRecovery(context.Background(), &storetest.Fake{}, []Backup{first, second}, WALCatalog{Servers: []ServerWAL{recoveryWAL()}}, nil, false, RecoveryOptions{})
	if catalog.Servers[0].CoverageState != evidence.Unknown || len(catalog.Servers[0].Paths) != 2 {
		t.Fatalf("conflicting system identifiers = %#v", catalog.Servers[0])
	}
	for _, path := range catalog.Servers[0].Paths {
		if path.State != evidence.Unknown || !strings.Contains(path.Reason, "system identifiers") {
			t.Fatalf("path = %#v", path)
		}
	}
}

func recoveryBackup(now time.Time, begin, end uint64) Backup {
	beginName, _ := WALName(1, begin, 16<<20)
	endName, _ := WALName(1, end, 16<<20)
	return Backup{Server: "alpha", ID: "backup", State: evidence.Healthy, BeginWAL: beginName, EndWAL: endName, SegmentSize: 16 << 20, PostgreSQLVersion: 180000, Timeline: 1, EndAt: now}
}

// recoveryWAL is the archive of validHistory: each parent timeline's switch
// segment exists only as .partial (not counted), each child starts at the
// switch segment, and timeline 3 has a candidate gap at segment 7.
func recoveryWAL() ServerWAL {
	return ServerWAL{
		Server: "alpha", State: evidence.Warning, PostgreSQLVersion: 180000, SegmentSize: 16 << 20,
		Ranges: []WALRange{walRange(1, 1, 1), walRange(2, 2, 3), walRange(3, 4, 6), walRange(3, 8, 9)},
		Gaps:   []WALGap{{Timeline: 3, Start: 7, End: 7, Count: 1, First: "000000030000000000000007", Last: "000000030000000000000007", Status: GapCandidate, FirstObservedGeneration: 1, LastObservedGeneration: 1}},
	}
}

func walRange(timeline uint32, start, end uint64) WALRange {
	first, _ := WALName(timeline, start, 16<<20)
	last, _ := WALName(timeline, end, 16<<20)
	return WALRange{Timeline: timeline, Start: start, End: end, Count: end - start + 1, First: first, Last: last, EndReceipt: time.Unix(int64(end), 0).UTC(), LatestReceipt: time.Unix(int64(end), 0).UTC()}
}

func historyFake(content string) store.Reader {
	return &storetest.Fake{OpenFunc: func(context.Context, store.OpenRequest) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader([]byte(content))), nil
	}}
}

func findPath(t *testing.T, paths []RecoveryPath, timeline uint32) RecoveryPath {
	t.Helper()
	for _, path := range paths {
		if path.TargetTimeline == timeline {
			return path
		}
	}
	t.Fatalf("timeline %d path missing: %#v", timeline, paths)
	return RecoveryPath{}
}

func sortRanges(values []WALRange) {
	slices.SortFunc(values, func(a, b WALRange) int {
		if a.Timeline < b.Timeline {
			return -1
		}
		if a.Timeline > b.Timeline {
			return 1
		}
		if a.Start < b.Start {
			return -1
		}
		return 1
	})
}

func encodeWithCloser(t *testing.T, data []byte, create func(*bytes.Buffer) io.WriteCloser) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := create(&buffer)
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func encodeWithErrorCloser(t *testing.T, data []byte, create func(*bytes.Buffer) (io.WriteCloser, error)) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer, err := create(&buffer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestUnknownPathTargetsTheBackupTimeline(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		backup Backup
		want   uint32
	}{
		{name: "backup.info timeline", backup: Backup{Timeline: 3, BeginWAL: "000000020000000000000001"}, want: 3},
		{name: "begin WAL timeline", backup: Backup{BeginWAL: "000000020000000000000001"}, want: 2},
		{name: "malformed begin WAL", backup: Backup{BeginWAL: "0000000Z0000000000000001"}, want: 1},
		{name: "zero begin WAL timeline", backup: Backup{BeginWAL: "000000000000000000000001"}, want: 1},
		{name: "nothing known", want: 1},
	}
	for _, test := range tests {
		if got := unknownPath(test.backup, "reason").TargetTimeline; got != test.want {
			t.Errorf("%s: TargetTimeline = %d, want %d", test.name, got, test.want)
		}
	}
	paths := []RecoveryPath{{TargetTimeline: 1}, {TargetTimeline: 4}}
	limitedPath(paths, Backup{Timeline: 1})
	if paths[1].TargetTimeline != 4 || paths[1].State != evidence.Unknown {
		t.Fatalf("limited path = %#v, want the replaced path's target kept", paths[1])
	}
}
