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
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzLatest drives the resolver with arbitrary history statuses and
// ancestry trees and checks the probe's invariants: Latest is never below
// the start, every timeline it steps over has a listed, readable history,
// the stop is the first that is not, a reached timeline is on the
// ancestry of Latest, and reaching the newest timeline means complete.
// Then it weakens one valid history (to Absent, Invalid or Unreadable,
// keeping the newest timeline) and checks monotonicity: Latest never
// rises, the resolution never becomes complete, and no timeline becomes
// reached, except PostgreSQL's own case of a removed history settling the
// probe below the newest timeline (TestWeakeningAHistoryNeverStrengthens-
// TheResolution documents it).
func FuzzLatest(f *testing.F) {
	f.Add(uint32(1), []byte{3, 1, 2, 0}, false, uint8(0), uint8(0), "reason", "extra")
	f.Add(uint32(1), []byte{2, 2}, true, uint8(1), uint8(2), "", "")
	f.Add(uint32(1), []byte{1, 1, 3}, false, uint8(1), uint8(0), "", "")
	f.Add(uint32(1), []byte{1, 1 + 4}, false, uint8(1), uint8(1), "", "")
	f.Add(uint32(1), []byte{1}, false, uint8(0), uint8(0), "segment 000000010000000A000000FF is not in the repository; the backup recovers only to 000000010000000A000000FE; "+FormerPrimaryMayHold+"; after", "a restore must name timeline 1")
	f.Fuzz(func(t *testing.T, start uint32, layout []byte, truncated bool, weaken, weaker uint8, reason, extra string) {
		if start == 0 || len(layout) > 64 || start > math.MaxUint32-uint32(len(layout))-1 {
			return
		}
		// Each byte is one timeline above start: its status (low two bits)
		// and its parent among start and the timelines before it.
		newest := start + uint32(len(layout))
		statuses := make([]Status, len(layout))
		ancestry := map[uint32][]uint32{}
		for index, value := range layout {
			statuses[index] = Status(value % 4)
			timeline := start + uint32(index) + 1
			parent := start + uint32(int(value/4)%(index+1))
			ancestry[timeline] = append(slices.Clone(ancestry[parent]), parent)
		}
		build := func(statuses []Status) *Archive {
			archive := NewArchive(truncated)
			archive.Observe(start)
			archive.Observe(newest)
			for index, status := range statuses {
				timeline := start + uint32(index) + 1
				archive.History(timeline, status, ancestry[timeline]...)
			}
			return archive
		}
		archive := build(statuses)
		got := archive.Latest(start)
		if got.Latest < start || got.Newest < got.Latest || got.Newest != newest {
			t.Fatalf("Latest(%d) = %+v", start, got)
		}
		for probe := start + 1; probe <= got.Latest && probe != 0; probe++ {
			if status := archive.histories[probe]; status != Valid && status != Invalid {
				t.Fatalf("stepped over timeline %d without a listed, readable history: %+v", probe, got)
			}
		}
		if got.Stop != 0 && (got.Stop != got.Latest+1 || got.StopStatus == Valid || got.StopStatus == Invalid) {
			t.Fatalf("stopped at a listed, readable history: %+v", got)
		}
		if got.Complete() != (got.Settled && got.Known() && got.Latest >= got.Newest) || got.Reaches(got.Newest) && !got.Complete() {
			t.Fatalf("inconsistent resolution: %+v", got)
		}
		for target := start; target <= newest && target != 0; target++ {
			if got.Reaches(target) && (target != got.Latest && !slices.Contains(ancestry[got.Latest], target) || got.Elsewhere(target)) {
				t.Fatalf("timeline %d reached off the ancestry of %d: %+v", target, got.Latest, got)
			}
		}
		if truncated && got.Settled {
			t.Fatalf("a truncated listing settled the probe: %+v", got)
		}
		if len(statuses) > 0 {
			index := int(weaken) % len(statuses)
			if statuses[index] == Valid {
				weakened := slices.Clone(statuses)
				weakened[index] = []Status{Absent, Invalid, Unreadable}[int(weaker)%3]
				after := build(weakened).Latest(start)
				if after.Latest > got.Latest || after.Complete() && !got.Complete() || after.Newest != got.Newest {
					t.Fatalf("weakening timeline %d strengthened %+v into %+v", start+uint32(index)+1, got, after)
				}
				for target := start; target <= newest && target != 0; target++ {
					if after.Reaches(target) && !got.Reaches(target) && (weakened[index] != Absent || !after.Settled || after.Complete()) {
						t.Fatalf("weakening timeline %d made timeline %d reached: %+v -> %+v", start+uint32(index)+1, target, got, after)
					}
				}
			}
		}
		if !utf8.ValidString(reason) || !utf8.ValidString(extra) {
			return
		}
		joined := Join(reason, extra)
		if len(joined) > MaxReasonBytes || !utf8.ValidString(joined) {
			t.Fatalf("Join = %d bytes, valid UTF-8 %v", len(joined), utf8.ValidString(joined))
		}
		// An actionable clause of reason always survives, whatever extra
		// is (round 9, PG P3): extra is cut before it is.
		if strings.Contains(reason, FormerPrimaryMayHold) && !strings.Contains(joined, FormerPrimaryMayHold) {
			t.Fatalf("Join(%q, %q) = %q lost the actionable clause", reason, extra, joined)
		}
		// And extra survives whole whenever it fits beside the clause.
		if strings.Contains(reason, FormerPrimaryMayHold) && len(FormerPrimaryMayHold)+len("…; ; ")+len(extra) <= MaxReasonBytes &&
			!strings.HasSuffix(joined, extra) {
			t.Fatalf("Join(%q, %q) = %q lost the extra reason", reason, extra, joined)
		}
	})
}
