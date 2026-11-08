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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
)

func TestClassifyWALObject(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, key, compression string
		class                  WALClass
		belongs                bool
	}{
		{name: "plain segment", key: "alpha/wals/0000000100000000/000000010000000000000001", class: WALSegment, compression: "none", belongs: true},
		{name: "gzip segment", key: "alpha/wals/0000000100000000/000000010000000000000001.gz", class: WALSegment, compression: "gzip", belongs: true},
		{name: "bzip2 segment", key: "alpha/wals/0000000100000000/000000010000000000000001.bz2", class: WALSegment, compression: "bzip2", belongs: true},
		{name: "snappy segment", key: "alpha/wals/0000000100000000/000000010000000000000001.snappy", class: WALSegment, compression: "snappy", belongs: true},
		{name: "lz4 segment", key: "alpha/wals/0000000100000000/000000010000000000000001.lz4", class: WALSegment, compression: "lz4", belongs: true},
		{name: "xz segment", key: "alpha/wals/0000000100000000/000000010000000000000001.xz", class: WALSegment, compression: "xz", belongs: true},
		{name: "zstd segment", key: "alpha/wals/0000000100000000/000000010000000000000001.zst", class: WALSegment, compression: "zstd", belongs: true},
		{name: "partial", key: "alpha/wals/0000000100000000/000000010000000000000001.partial.gz", class: WALPartial, compression: "gzip", belongs: true},
		{name: "timeline history", key: "alpha/wals/00000002.history.gz", class: WALHistory, compression: "gzip", belongs: true},
		{name: "backup history", key: "alpha/wals/0000000100000000/000000010000000000000001.00000028.backup.gz", class: WALBackupHistory, compression: "gzip", belongs: true},
		{name: "wrong hash", key: "alpha/wals/0000000100000001/000000010000000000000001", class: WALUnknown, belongs: true},
		{name: "segment at root", key: "alpha/wals/000000010000000000000001", class: WALUnknown, belongs: true},
		{name: "history under hash", key: "alpha/wals/0000000100000000/00000002.history", class: WALUnknown, belongs: true},
		{name: "malformed", key: "alpha/wals/0000000100000000/not-a-wal", class: WALUnknown, belongs: true},
		{name: "unsupported compression", key: "alpha/wals/0000000100000000/000000010000000000000001.zip", class: WALUnknown, belongs: true},
		{name: "backup object", key: "alpha/base/id/data.tar", belongs: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			classified, belongs := ClassifyWALObject(store.Object{Key: test.key})
			if belongs != test.belongs || (belongs && classified.Class != test.class) || (test.compression != "" && classified.Compression != test.compression) {
				t.Fatalf("ClassifyWALObject() = %#v, %t", classified, belongs)
			}
		})
	}
}

func TestWALArithmeticMatchesPostgreSQLSegmentRollover(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		segmentSize  int64
		log, segment uint32
		wantPosition uint64
		wantName     string
	}{
		{name: "one MiB", segmentSize: 1 << 20, log: 2, segment: 4095, wantPosition: 12287, wantName: "000000010000000200000FFF"},
		{name: "sixteen MiB", segmentSize: 16 << 20, log: 2, segment: 255, wantPosition: 767, wantName: "0000000100000002000000FF"},
		{name: "one GiB", segmentSize: 1 << 30, log: 2, segment: 3, wantPosition: 11, wantName: "000000010000000200000003"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			position, err := WALPosition(1, test.log, test.segment, test.segmentSize)
			if err != nil || position != test.wantPosition {
				t.Fatalf("WALPosition() = %d, %v; want %d", position, err, test.wantPosition)
			}
			name, err := WALName(1, position, test.segmentSize)
			if err != nil || name != test.wantName {
				t.Fatalf("WALName() = %q, %v; want %q", name, err, test.wantName)
			}
			next, err := WALName(1, position+1, test.segmentSize)
			if err != nil || next != "000000010000000300000000" {
				t.Fatalf("rollover WALName() = %q, %v", next, err)
			}
		})
	}
	for _, invalid := range []int64{0, 512 << 10, 3 << 20, 2 << 30} {
		if ValidWALSegmentSize(invalid) {
			t.Errorf("ValidWALSegmentSize(%d) = true", invalid)
		}
	}
	if _, err := WALPosition(1, 0, 4, 1<<30); err == nil {
		t.Fatal("one-GiB segment component 4 was accepted")
	}
}

func TestWALCollectorUsesCompactRangesAndTwoCompleteScanGapLifecycle(t *testing.T) {
	t.Parallel()
	backup := Backup{Server: "alpha", PostgreSQLVersion: 180000, SegmentSize: 16 << 20}
	first := NewWALCollector()
	for _, position := range []uint64{1, 2, 5} {
		first.Add(testWALObject(t, position, backup.SegmentSize, ""))
	}
	firstCatalog := first.Finish([]Backup{backup}, WALCatalog{}, 1)
	server := firstCatalog.Servers[0]
	if server.State != evidence.Warning || len(server.Ranges) != 2 || len(server.Gaps) != 1 || server.Gaps[0].Status != GapCandidate || server.Gaps[0].Count != 2 {
		t.Fatalf("first complete scan = %#v", server)
	}
	second := NewWALCollector()
	for _, position := range []uint64{1, 2, 5} {
		second.Add(testWALObject(t, position, backup.SegmentSize, ""))
	}
	secondCatalog := second.Finish([]Backup{backup}, firstCatalog, 3)
	server = secondCatalog.Servers[0]
	if server.State != evidence.Unhealthy || server.Gaps[0].Status != GapConfirmed || server.Gaps[0].FirstObservedGeneration != 1 || server.Gaps[0].LastObservedGeneration != 3 {
		t.Fatalf("second complete scan = %#v", server)
	}
	cleared := NewWALCollector()
	for _, position := range []uint64{1, 2, 3, 4, 5} {
		cleared.Add(testWALObject(t, position, backup.SegmentSize, ""))
	}
	clearedCatalog := cleared.Finish([]Backup{backup}, secondCatalog, 4)
	server = clearedCatalog.Servers[0]
	if server.State != evidence.Healthy || len(server.Ranges) != 1 || len(server.Gaps) != 0 {
		t.Fatalf("complete scan that filled gap = %#v", server)
	}
}

func TestWALCollectorKeepsHugeGapsCompactAndRejectsDuplicates(t *testing.T) {
	t.Parallel()
	const segmentSize = int64(1 << 20)
	backup := Backup{Server: "alpha", PostgreSQLVersion: 180000, SegmentSize: segmentSize}
	collector := NewWALCollector()
	collector.Add(testWALObject(t, 1, segmentSize, ""))
	collector.Add(testWALObject(t, 1_000_000, segmentSize, ""))
	catalog := collector.Finish([]Backup{backup}, WALCatalog{}, 1)
	server := catalog.Servers[0]
	if len(server.Ranges) != 2 || len(server.Gaps) != 1 || server.Gaps[0].Count != 999_998 {
		t.Fatalf("huge gap was not compact: %#v", server)
	}

	duplicates := NewWALCollector()
	duplicates.Add(testWALObject(t, 7, segmentSize, ""))
	duplicates.Add(testWALObject(t, 7, segmentSize, ".gz"))
	server = duplicates.Finish([]Backup{backup}, WALCatalog{}, 1).Servers[0]
	if server.State != evidence.Unknown || server.Counts.Segments != 1 || server.Counts.Duplicates != 1 || len(server.Diagnostics) != 1 || server.Diagnostics[0].Class != WALDuplicate {
		t.Fatalf("duplicate evidence = %#v", server)
	}

	malformed := NewWALCollector()
	malformed.Add(testWALObject(t, 7, segmentSize, ""))
	malformed.Add(store.Object{Key: "alpha/wals/0000000100000000/not-a-wal"})
	server = malformed.Finish([]Backup{backup}, WALCatalog{}, 1).Servers[0]
	if server.State != evidence.Unknown || server.Counts.Unknown != 1 {
		t.Fatalf("malformed evidence = %#v", server)
	}
}

func TestWALCollectorFailsUnknownWithoutConsistentMetadataContext(t *testing.T) {
	t.Parallel()
	collector := NewWALCollector()
	collector.Add(testWALObject(t, 1, 16<<20, ""))
	tests := []struct {
		name    string
		backups []Backup
	}{
		{name: "missing context", backups: []Backup{{Server: "alpha"}}},
		{name: "invalid segment size", backups: []Backup{{Server: "alpha", PostgreSQLVersion: 180000, SegmentSize: 3 << 20}}},
		{name: "conflicting context", backups: []Backup{{Server: "alpha", PostgreSQLVersion: 170000, SegmentSize: 16 << 20}, {Server: "alpha", PostgreSQLVersion: 180000, SegmentSize: 16 << 20}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := collector.Finish(test.backups, WALCatalog{}, 1).Servers[0]
			if server.State != evidence.Unknown {
				t.Fatalf("server = %#v", server)
			}
		})
	}
}

// A long retention leaves one .backup file per base backup plus .history
// and .partial files. Those are counted, never listed, so hundreds of them
// cannot reach the diagnostic cap and make a contiguous archive unknown.
func TestWALExpectedClassesAreCountedWithoutReachingTheDiagnosticCap(t *testing.T) {
	t.Parallel()
	backup := Backup{Server: "alpha", PostgreSQLVersion: 180000, SegmentSize: 16 << 20}
	collector := NewWALCollector()
	const retained = 3 * MaxWALDiagnostics
	for position := uint64(1); position <= retained; position++ {
		segment := testWALObject(t, position, backup.SegmentSize, "")
		collector.Add(segment)
		backupHistory := segment
		backupHistory.Key += ".00000028.backup"
		collector.Add(backupHistory)
	}
	collector.Add(testWALObject(t, retained+1, backup.SegmentSize, ".partial"))
	collector.Add(store.Object{Key: "alpha/wals/00000002.history", Size: 42, LastModified: time.Unix(1, 0).UTC()})
	server := collector.Finish([]Backup{backup}, WALCatalog{}, 1).Servers[0]
	if server.State != evidence.Healthy || server.DiagnosticsTruncated || len(server.Diagnostics) != 0 {
		t.Fatalf("expected WAL classes changed state: state=%s truncated=%v diagnostics=%d", server.State, server.DiagnosticsTruncated, len(server.Diagnostics))
	}
	if server.Counts != (WALCounts{Segments: retained, BackupHistory: retained, Partials: 1, History: 1}) {
		t.Fatalf("counts = %#v", server.Counts)
	}
}

// Anomalies fill the bounded list. Past the cap the list is truncated for
// display, but every anomaly is still counted and the state is unknown
// because of the anomalies themselves, not because of the cap.
func TestWALDiagnosticCapTruncatesOnlyTheAnomalyList(t *testing.T) {
	t.Parallel()
	backup := Backup{Server: "alpha", PostgreSQLVersion: 180000, SegmentSize: 16 << 20}
	collector := NewWALCollector()
	collector.Add(testWALObject(t, 1, backup.SegmentSize, ""))
	const anomalies = MaxWALDiagnostics + 25
	for index := range anomalies {
		collector.Add(store.Object{Key: fmt.Sprintf("alpha/wals/unexpected-%04d", index), Size: 1, LastModified: time.Unix(1, 0).UTC()})
	}
	server := collector.Finish([]Backup{backup}, WALCatalog{}, 1).Servers[0]
	if !server.DiagnosticsTruncated || len(server.Diagnostics) != MaxWALDiagnostics || server.Counts.Unknown != anomalies {
		t.Fatalf("anomaly list: truncated=%v listed=%d counted=%d", server.DiagnosticsTruncated, len(server.Diagnostics), server.Counts.Unknown)
	}
	if server.State != evidence.Unknown || server.Reason != "unrecognized WAL objects require operator review" {
		t.Fatalf("state = %s (%s)", server.State, server.Reason)
	}
}

func testWALObject(t *testing.T, position uint64, segmentSize int64, suffix string) store.Object {
	t.Helper()
	name, err := WALName(1, position, segmentSize)
	if err != nil {
		t.Fatal(err)
	}
	return store.Object{
		Key:  "alpha/wals/" + name[:16] + "/" + name + suffix,
		Size: segmentSize, LastModified: time.Unix(int64(position), 0).UTC(),
	}
}

// TestWALCollectorEscapesHostileKeysInsteadOfFailingTheCatalog keeps one
// key with control characters or invalid UTF-8 under <server>/wals/ from
// failing WALCatalog.Validate, which would leave every publication stale.
func TestWALCollectorEscapesHostileKeysInsteadOfFailingTheCatalog(t *testing.T) {
	t.Parallel()
	const segmentSize = 16 << 20
	backup := Backup{Server: "alpha", State: evidence.Healthy, PostgreSQLVersion: 180000, SegmentSize: segmentSize}
	long := "alpha/wals/0000000100000000/" + strings.Repeat("\x01", store.MaxKeyBytes/2)
	tests := []struct {
		name, key, wantKey string
	}{
		{name: "terminal escape", key: "alpha/wals/0000000100000000/\x1b[31mevil", wantKey: `alpha/wals/0000000100000000/\x1b[31mevil`},
		{name: "newline in hash directory", key: "alpha/wals/00000001\n0000000/x", wantKey: `alpha/wals/00000001\n0000000/x`},
		{name: "invalid UTF-8", key: "alpha/wals/0000000100000000/\xff", wantKey: `alpha/wals/0000000100000000/\xff`},
		{name: "escape longer than a key", key: long},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			collector := NewWALCollector()
			collector.Add(testWALObject(t, 1, segmentSize, ""))
			collector.Add(store.Object{Key: test.key})
			catalog := collector.Finish([]Backup{backup}, WALCatalog{}, 1)
			if err := catalog.Validate(); err != nil {
				t.Fatalf("Validate() = %v", err)
			}
			server := catalog.Servers[0]
			if server.Counts.Unknown != 1 || len(server.Diagnostics) != 1 {
				t.Fatalf("server = %#v", server)
			}
			diagnostic := server.Diagnostics[0]
			if test.wantKey != "" && diagnostic.Key != test.wantKey {
				t.Fatalf("diagnostic key = %q, want %q", diagnostic.Key, test.wantKey)
			}
			if len(diagnostic.Key) > store.MaxKeyBytes || !strings.Contains(diagnostic.Reason, "shown escaped") {
				t.Fatalf("diagnostic = %#v", diagnostic)
			}
		})
	}
}
