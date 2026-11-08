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
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
)

const sha = "4b5b556790a587cd942b152bd2d8cf00875fbd20"

func TestClassifyWALObjectRecognizesPgBackRestNames(t *testing.T) {
	t.Parallel()
	const dir = "archive/demo/17-1/0000000100000000/"
	tests := []struct {
		key         string
		belongs     bool
		class       WALClass
		compression string
		segment     uint32
	}{
		{key: dir + "000000010000000000000003-" + sha, belongs: true, class: WALSegment, compression: "none", segment: 3},
		{key: dir + "000000010000000000000003-" + sha + ".gz", belongs: true, class: WALSegment, compression: "gzip", segment: 3},
		{key: dir + "000000010000000000000003-" + sha + ".lz4", belongs: true, class: WALSegment, compression: "lz4", segment: 3},
		{key: dir + "000000010000000000000003-" + sha + ".zst", belongs: true, class: WALSegment, compression: "zstd", segment: 3},
		{key: dir + "000000010000000000000003-" + sha + ".xz", belongs: true, class: WALSegment, compression: "xz", segment: 3},
		{key: dir + "000000010000000000000003-" + sha + ".bz2", belongs: true, class: WALSegment, compression: "bzip2", segment: 3},
		{key: dir + "0000000100000000000000FF-" + sha + ".gz", belongs: true, class: WALSegment, compression: "gzip", segment: 255},
		{key: dir + "000000010000000000000003.partial-" + sha + ".gz", belongs: true, class: WALPartial, compression: "gzip", segment: 3},
		{key: dir + "000000010000000000000003.00000028.backup", belongs: true, class: WALBackupHistory, compression: "none", segment: 3},
		{key: "archive/demo/17-1/00000002.history", belongs: true, class: WALHistory, compression: "none"},
		{key: "archive/demo/17-1/00000001.history", belongs: true, class: WALUnknown},
		{key: "archive/demo/17-1/00000002.history.gz", belongs: true, class: WALUnknown},
		{key: dir + "000000010000000000000003-" + sha + ".snappy", belongs: true, class: WALUnknown},
		{key: dir + "000000010000000000000003", belongs: true, class: WALUnknown},
		{key: dir + "00000001000000000000000a-" + sha, belongs: true, class: WALUnknown},
		{key: dir + "000000010000000000000003-" + strings.ToUpper(sha), belongs: true, class: WALUnknown},
		{key: dir + "000000010000000000000003-" + sha[:39], belongs: true, class: WALUnknown},
		{key: "archive/demo/17-1/0000000100000001/000000010000000000000003-" + sha, belongs: true, class: WALUnknown},
		{key: "archive/demo/17-1/000000010000000000000003-" + sha, belongs: true, class: WALUnknown},
		{key: dir + "extra/000000010000000000000003-" + sha, belongs: true, class: WALUnknown},
		{key: "archive/demo/17-1/0000000000000000/000000000000000000000003-" + sha, belongs: true, class: WALUnknown},
		{key: "archive/demo/archive.info"},
		{key: "archive/demo/archive.info.copy"},
		{key: "archive/demo/stray"},
		{key: "archive/demo/not-an-id/0000000100000000/000000010000000000000003-" + sha},
		{key: "backup/demo/17-1/x/y"},
		{key: "archive/../17-1/0000000100000000/000000010000000000000003-" + sha},
	}
	for _, test := range tests {
		classified, belongs := ClassifyWALObject(store.Object{Key: test.key})
		if belongs != test.belongs || classified.Class != test.class || (test.compression != "" && classified.Compression != test.compression) || (test.class == WALSegment && classified.Segment != test.segment) {
			t.Errorf("ClassifyWALObject(%s) = %+v, %v", test.key, classified, belongs)
		}
	}
}

func TestArchiveIDPattern(t *testing.T) {
	t.Parallel()
	for id, want := range map[string]bool{"17-1": true, "9.6-2": true, "10-12": true, "17-0": false, "17": false, "17-01": false, "x-1": false, "17-1-1": false, "017-1": false} {
		if archiveIDPattern.MatchString(id) != want {
			t.Errorf("archive id %q: %v", id, !want)
		}
	}
}

func TestInferSegmentSizeFromBackupLSNs(t *testing.T) {
	t.Parallel()
	backup := func(start, lsnStart, stop, lsnStop string) Backup {
		return Backup{ArchiveStart: start, LSNStart: lsnStart, ArchiveStop: stop, LSNStop: lsnStop}
	}
	tests := []struct {
		name    string
		backups []Backup
		size    int64
		source  SegmentSizeSource
	}{
		{name: "16 MiB, as the fixtures", backups: []Backup{backup("000000010000000000000003", "0/3000028", "000000010000000000000004", "0/400B7E8")}, size: 16 << 20, source: SegmentSizeFromLSN},
		{name: "64 MiB", backups: []Backup{backup("000000010000000000000003", "0/C000028", "000000010000000000000003", "0/C100000")}, size: 64 << 20, source: SegmentSizeFromLSN},
		{name: "1 GiB on a later log", backups: []Backup{backup("000000010000000500000002", "5/80000028", "000000010000000500000002", "5/80000100")}, size: 1 << 30, source: SegmentSizeFromLSN},
		{name: "no backup", size: 16 << 20, source: SegmentSizeAssumed},
		{name: "backups without LSN", backups: []Backup{{}}, size: 16 << 20, source: SegmentSizeAssumed},
		{name: "first segment only", backups: []Backup{backup("000000010000000000000000", "0/28", "000000010000000000000000", "0/100")}, size: 16 << 20, source: SegmentSizeAssumed},
		{name: "backups that disagree", backups: []Backup{
			backup("000000010000000000000003", "0/3000028", "000000010000000000000004", "0/400B7E8"),
			backup("000000010000000000000003", "0/C000028", "000000010000000000000003", "0/C100000"),
		}},
	}
	for _, test := range tests {
		size, source := InferSegmentSize(test.backups)
		if size != test.size || source != test.source {
			t.Errorf("%s: InferSegmentSize() = %d, %q", test.name, size, source)
		}
	}
}

func TestWALArithmeticRoundTrips(t *testing.T) {
	t.Parallel()
	for _, size := range []int64{1 << 20, 16 << 20, 1 << 30} {
		for _, name := range []string{"000000010000000000000000", "0000000200000003000000FF", "000000010000000000000003"} {
			timeline, position, err := ParseWALName(name, size)
			if size == 1<<30 && name[16:] == "000000FF" {
				if err == nil {
					t.Fatalf("%s accepted with 1 GiB segments", name)
				}
				continue
			}
			if err != nil {
				t.Fatalf("ParseWALName(%s, %d) = %v", name, size, err)
			}
			back, err := WALName(timeline, position, size)
			if err != nil || back != name {
				t.Fatalf("WALName() = %s, %v; want %s", back, err, name)
			}
		}
	}
	if _, _, err := ParseWALName("000000010000000000000100", 16<<20); err == nil {
		t.Fatal("segment past the 16 MiB rollover accepted")
	}
	if _, err := WALPosition(1, 0, 0, 3<<20); err == nil {
		t.Fatal("a segment size that is not a power of two accepted")
	}
}

func TestParseHistoryReadsPostgreSQLFormat(t *testing.T) {
	t.Parallel()
	edges, err := ParseHistory([]byte("1\t0/E000050\treached consistency\n2\t0/12000000\tno recovery target specified\n"), 3, 16<<20)
	if err != nil || len(edges) != 2 || edges[0].Parent != 1 || edges[0].Child != 2 || edges[0].SwitchPosition != 0xE || edges[1].Parent != 2 || edges[1].Child != 3 || edges[1].SwitchPosition != 0x12 {
		t.Fatalf("ParseHistory() = %+v, %v", edges, err)
	}
	for _, malformed := range []string{"", "# only\n", "1 0/2 reason\n", "1\tbad\treason\n", "1\t0/2\t\n", "2\t0/2\ta\n1\t0/4\tb\n", "1\t0/4\ta\n2\t0/2\tb\n", "3\t0/2\tcycle\n", "1\t0/0\tzero\n"} {
		if _, err := ParseHistory([]byte(malformed), 3, 16<<20); err == nil {
			t.Errorf("ParseHistory(%q) error = nil", malformed)
		}
	}
	if _, err := ParseHistory([]byte("1\t0/2\tx\n"), 1, 16<<20); err == nil {
		t.Error("history for timeline 1 accepted")
	}
}

func segmentObject(timeline, segment uint32, modified time.Time) store.Object {
	name, _ := WALName(timeline, uint64(segment), 16<<20)
	return store.Object{Key: "archive/demo/17-1/" + name[:16] + "/" + name + "-" + sha + ".gz", Size: 1, LastModified: modified}
}

func TestWALCollectorCompactsAndJudgesConservatively(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 7, 8, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		objects []store.Object
		state   evidence.State
		ranges  int
		gaps    int
	}{
		{name: "contiguous across a log boundary", objects: []store.Object{segmentObject(1, 0xFE, at), segmentObject(1, 0xFF, at), segmentObject(1, 0x100, at)}, state: evidence.Healthy, ranges: 1},
		{name: "a gap", objects: []store.Object{segmentObject(1, 1, at), segmentObject(1, 3, at)}, state: evidence.Warning, ranges: 2, gaps: 1},
		{name: "two timelines are not a gap", objects: []store.Object{segmentObject(1, 1, at), segmentObject(2, 3, at)}, state: evidence.Healthy, ranges: 2},
		{name: "a duplicate", objects: []store.Object{segmentObject(1, 1, at), {Key: strings.Replace(segmentObject(1, 1, at).Key, sha, strings.Repeat("0", 40), 1)}}, state: evidence.Unknown, ranges: 1},
		{name: "out of order", objects: []store.Object{segmentObject(1, 3, at), segmentObject(1, 1, at)}, state: evidence.Unknown, ranges: 1},
		{name: "an unknown object", objects: []store.Object{segmentObject(1, 1, at), {Key: "archive/demo/17-1/0000000100000000/junk"}}, state: evidence.Unknown, ranges: 1},
		{name: "nothing archived", state: evidence.Unknown},
	}
	for _, test := range tests {
		collector := NewWALCollector()
		for _, object := range test.objects {
			collector.Add(object)
		}
		wal := collector.Finish("demo", "17-1", 16<<20, nil, 1)
		if wal.State != test.state || len(wal.Ranges) != test.ranges || len(wal.Gaps) != test.gaps {
			t.Errorf("%s: %s %q, %d ranges, %d gaps", test.name, wal.State, wal.Reason, len(wal.Ranges), len(wal.Gaps))
		}
	}
	collector := NewWALCollector()
	collector.Add(segmentObject(1, 1, at))
	if wal := collector.Finish("demo", "17-1", 0, nil, 1); wal.State != evidence.Unknown {
		t.Fatal("an unknown segment size was judged")
	}
}

func TestWALGapIsConfirmedOnlyByALaterScan(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 7, 8, 0, 0, 0, time.UTC)
	scan := func(previous *ArchiveWAL, generation uint64) ArchiveWAL {
		collector := NewWALCollector()
		collector.Add(segmentObject(1, 1, at))
		collector.Add(segmentObject(1, 4, at))
		return collector.Finish("demo", "17-1", 16<<20, previous, generation)
	}
	first := scan(nil, 1)
	if first.Gaps[0].Status != GapCandidate || first.Gaps[0].Count != 2 || first.Gaps[0].First != "000000010000000000000002" || first.Gaps[0].Last != "000000010000000000000003" {
		t.Fatalf("first scan gap = %+v", first.Gaps)
	}
	if again := scan(&first, 1); again.Gaps[0].Status != GapCandidate {
		t.Fatal("the same generation confirmed its own gap")
	}
	second := scan(&first, 2)
	if second.Gaps[0].Status != GapConfirmed || second.Gaps[0].FirstObservedGeneration != 1 || second.State != evidence.Unhealthy {
		t.Fatalf("second scan gap = %+v, %s", second.Gaps, second.State)
	}
}
