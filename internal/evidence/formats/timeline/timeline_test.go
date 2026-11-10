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

package timeline

import (
	"math"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLatestProbesConsecutiveHistoryFiles(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		histories map[uint32]Status
		observed  []uint32
		truncated bool
		start     uint32
		latest    uint32
		settled   bool
		complete  bool
		stop      uint32
		gap       string
	}{
		{name: "no newer timeline", observed: []uint32{1}, start: 1, latest: 1, settled: true, complete: true, stop: 2},
		{name: "every history present", histories: map[uint32]Status{2: Valid, 3: Valid}, observed: []uint32{1, 2, 3}, start: 1, latest: 3, settled: true, complete: true, stop: 4},
		// findNewestTimeLine stops at the first missing file: timeline 3's
		// history does not make it reachable.
		{name: "middle history missing", histories: map[uint32]Status{3: Valid}, observed: []uint32{1, 2, 3}, start: 1, latest: 1, settled: true, stop: 2, gap: "history 2 is missing"},
		{name: "newest history missing", histories: map[uint32]Status{2: Valid}, observed: []uint32{1, 2, 3}, start: 1, latest: 2, settled: true, stop: 3, gap: "history 3 is missing"},
		{name: "newest history unreadable", histories: map[uint32]Status{2: Invalid}, observed: []uint32{1}, start: 1, latest: 1, stop: 2, gap: "history 2 is unreadable or invalid"},
		{name: "listing truncated", histories: map[uint32]Status{2: Valid}, observed: []uint32{1, 2}, truncated: true, start: 1, latest: 2, stop: 3, gap: "not all read"},
		{name: "start above the gap", histories: map[uint32]Status{3: Valid}, observed: []uint32{2, 3}, start: 2, latest: 3, settled: true, complete: true, stop: 4},
		{name: "a timeline number that does not wrap", observed: []uint32{math.MaxUint32}, start: math.MaxUint32, latest: math.MaxUint32, settled: true, complete: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			archive := NewArchive(test.truncated)
			for _, timeline := range test.observed {
				archive.Observe(timeline)
			}
			for timeline, status := range test.histories {
				archive.History(timeline, status)
			}
			got := archive.Latest(test.start)
			if got.Latest != test.latest || got.Settled != test.settled || got.Complete() != test.complete || got.Stop != test.stop {
				t.Fatalf("Latest(%d) = %+v (complete %v)", test.start, got, got.Complete())
			}
			if gap := got.Gap(); (test.gap == "") != (gap == "") || !strings.Contains(gap, test.gap) {
				t.Fatalf("Gap() = %q, want %q", gap, test.gap)
			}
			if !got.Complete() && (got.Reaches(got.Newest) || got.OwnReason() == "") {
				t.Fatalf("an incomplete resolution reaches the newest timeline or gives no reason: %+v", got)
			}
		})
	}
}

func TestHistoryKeepsTheWeakerStatus(t *testing.T) {
	t.Parallel()
	archive := NewArchive(false)
	archive.History(2, Invalid)
	archive.History(2, Valid)
	if got := archive.Latest(1); got.Latest != 1 || got.StopStatus != Invalid {
		t.Fatalf("a duplicate valid history hid an invalid one: %+v", got)
	}
	archive.History(3, Absent)
	if archive.Newest() != 3 {
		t.Fatalf("Newest() = %d", archive.Newest())
	}
}

func TestBeyondReasonNamesWhatARestoreNeeds(t *testing.T) {
	t.Parallel()
	archive := NewArchive(false)
	archive.History(3, Valid)
	archive.Observe(1)
	resolution := archive.Latest(1)
	if got := resolution.BeyondReason(3); got != "timeline 3 needs an explicit targetTLI; history 2 is missing" {
		t.Fatalf("BeyondReason(3) = %q", got)
	}
	if got := resolution.BeyondReason(2); !strings.Contains(got, "no restore can follow timeline 2") {
		t.Fatalf("BeyondReason(2) = %q", got)
	}
}

func TestJoinKeepsBothReasonsWithinTheWireBound(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", 200)
	for _, test := range []struct{ reason, extra string }{
		{reason: "gap", extra: "a restore must name timeline 1"},
		{reason: long, extra: "a restore must name timeline 1"},
		{reason: "", extra: "only"},
		{reason: "only", extra: ""},
		{reason: "x", extra: long + long},
	} {
		got := Join(test.reason, test.extra)
		if len(got) > MaxReasonBytes || !utf8.ValidString(got) {
			t.Fatalf("Join(%d bytes, %d bytes) = %d bytes, valid UTF-8 %v", len(test.reason), len(test.extra), len(got), utf8.ValidString(got))
		}
		if len(test.extra) <= MaxReasonBytes-8 && !strings.HasSuffix(got, test.extra) {
			t.Fatalf("Join dropped the extra reason: %q", got)
		}
	}
}
