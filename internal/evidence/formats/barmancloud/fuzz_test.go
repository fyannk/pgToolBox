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
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
)

func FuzzParseBackupInfo(f *testing.F) {
	f.Add([]byte("status=DONE\nbegin_time=2026-07-27 10:00:00 +0000\nend_time=2026-07-27 10:01:00 +0000\nbegin_wal=000000010000000000000001\nend_wal=000000010000000000000002\nbegin_lsn=0/01000000\nend_lsn=0/02000000\ntimeline=1\nsystemid=7612345678901234567\nxlog_segment_size=16777216\nversion=180000\nsize=42\ndeduplicated_size=21\ncompression=gzip\ntablespaces=[('ts', 16384, '/synthetic')]\n"))
	f.Add([]byte("status=STARTED\nbackup_type=postgres\nencryption=none\n"))
	f.Add([]byte("malformed metadata"))

	f.Fuzz(func(t *testing.T, data []byte) {
		const server, id = "fuzz-server", "fuzz-backup"
		backup, err := ParseBackupInfo(data, server, id)
		if err != nil {
			return
		}
		again, err := ParseBackupInfo(data, server, id)
		if err != nil || !reflect.DeepEqual(backup, again) {
			t.Fatal("accepted backup metadata did not parse deterministically")
		}
		if backup.Server != server || backup.ID != id || (backup.Status == "" && !slices.Contains(backup.WithheldFields, "status")) || backup.State != evidence.Unknown {
			t.Fatal("accepted backup metadata violated parser-owned identity or state")
		}
		if again := withholdInvalidOptionalFacts(&backup); len(again) != 0 {
			t.Fatalf("accepted backup metadata retained values outside the evidence constraints: %v", again)
		}
		if backup.SegmentSize < 0 || backup.PostgreSQLVersion < 0 || backup.LogicalBytes < 0 || (backup.DeduplicatedBytes != nil && *backup.DeduplicatedBytes < 0) {
			t.Fatal("accepted backup metadata retained a negative numeric fact")
		}
		if (!backup.BeginAt.IsZero() && backup.BeginAt.Location() != time.UTC) || (!backup.EndAt.IsZero() && backup.EndAt.Location() != time.UTC) {
			t.Fatal("accepted backup metadata retained a non-UTC timestamp")
		}
		if !slices.IsSorted(backup.TablespaceOIDs) {
			t.Fatal("accepted backup metadata retained unsorted tablespace OIDs")
		}
		for index := 1; index < len(backup.TablespaceOIDs); index++ {
			if backup.TablespaceOIDs[index-1] == backup.TablespaceOIDs[index] {
				t.Fatal("accepted backup metadata retained duplicate tablespace OIDs")
			}
		}
	})
}

func FuzzParseTimelineHistory(f *testing.F) {
	f.Add([]byte("1\t0/02000000\tfirst branch\n2\t0/04000000\tsecond branch\n"), uint32(3), uint8(4))
	f.Add([]byte("1\t0/01000000\tsynthetic branch\n"), uint32(2), uint8(4))
	f.Add([]byte("not a history file"), uint32(2), uint8(0))

	f.Fuzz(func(t *testing.T, data []byte, targetInput uint32, sizeCode uint8) {
		segmentSize := fuzzSegmentSize(sizeCode)
		target := uint32(uint64(targetInput)%1_000_000 + 2)
		edges, err := ParseHistory(data, target, segmentSize)
		if err != nil {
			return
		}
		again, err := ParseHistory(data, target, segmentSize)
		if err != nil || !reflect.DeepEqual(edges, again) {
			t.Fatal("accepted timeline history did not parse deterministically")
		}
		if len(edges) == 0 || len(edges) > MaxHistoryEntries || edges[len(edges)-1].Child != target {
			t.Fatal("accepted timeline history violated its bounded ancestry")
		}
		for index, edge := range edges {
			if edge.Parent == 0 || edge.Parent >= edge.Child || edge.Child > target || edge.SwitchLSN == 0 || edge.SwitchPosition != edge.SwitchLSN/uint64(segmentSize) {
				t.Fatal("accepted timeline history retained an impossible edge")
			}
			timeline, position, err := ParseWALName(edge.SwitchWAL, segmentSize)
			if err != nil || timeline != edge.Child || position != edge.SwitchPosition {
				t.Fatal("accepted timeline history retained an inconsistent switch WAL")
			}
			if index > 0 && (edges[index-1].Parent >= edge.Parent || edges[index-1].SwitchLSN >= edge.SwitchLSN) {
				t.Fatal("accepted timeline history retained unordered edges")
			}
		}
	})
}

func FuzzParseWALName(f *testing.F) {
	f.Add("000000010000000000000001", uint8(4))
	f.Add("000000010000000200000FFF", uint8(0))
	f.Add("000000010000000200000003", uint8(10))
	f.Add("not-a-wal", uint8(4))

	f.Fuzz(func(t *testing.T, name string, sizeCode uint8) {
		segmentSize := fuzzSegmentSize(sizeCode)
		timeline, position, err := ParseWALName(name, segmentSize)
		if err != nil {
			return
		}
		canonical, err := WALName(timeline, position, segmentSize)
		if err != nil || !strings.EqualFold(canonical, name) {
			t.Fatal("accepted WAL name did not round-trip canonically")
		}
		againTimeline, againPosition, err := ParseWALName(canonical, segmentSize)
		if err != nil || againTimeline != timeline || againPosition != position {
			t.Fatal("canonical WAL name did not preserve its arithmetic identity")
		}
	})
}

func fuzzSegmentSize(code uint8) int64 {
	return MinWALSegmentSize << (code % 11)
}
