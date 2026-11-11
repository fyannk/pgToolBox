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
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/barmancloud"
	pgtimeline "github.com/fyannk/pgToolBox/internal/evidence/formats/timeline"
)

// The monotonicity fuzz targets judge recovery paths in both repository
// formats over a small archive: up to three timelines of segments 1 to 12,
// timeline 2 forked from 1, timeline 3 forked from 2 or (a sibling branch)
// from 1, switches on a segment boundary or inside a segment, holes
// recorded as confirmed gaps, candidate gaps or not at all, and a backup on
// timeline 1 that ends in segment 1, 2 or 3. Taking evidence away from an
// archive never makes a restore reach further: that is the invariant.

const monotoneSegments = 12

// monotoneInput is one fuzzed archive.
type monotoneInput struct {
	masks    [3]uint16 // bit n: segment n of timeline 1, 2, 3 is archived
	switch2  uint64    // the segment timeline 2 forks from 1 in
	switch3  uint64    // the segment timeline 3 forks in
	sibling  bool      // timeline 3 forks from 1, not from 2
	mid      bool      // the switches fall inside their segment
	gaps     uint8     // 0 none recorded, 1 confirmed, 2 candidate
	end      uint64    // the backup's end segment on timeline 1
	timeline uint8     // 1: timeline 2 only; 2: timelines 2 and 3
}

func newMonotoneInput(m1, m2, m3 uint16, sw2, sw3, shape, end uint8) monotoneInput {
	in := monotoneInput{
		switch2:  2 + uint64(sw2)%(monotoneSegments-1),
		sibling:  shape&1 != 0,
		mid:      shape&2 != 0,
		gaps:     (shape >> 2) % 3,
		end:      1 + uint64(end)%3,
		timeline: 1 + (shape>>4)%2,
	}
	in.switch3 = in.switch2 + uint64(sw3)%(monotoneSegments-in.switch2+1)
	if in.sibling {
		in.switch3 = 2 + uint64(sw3)%(monotoneSegments-1)
	}
	const valid = (1<<(monotoneSegments+1) - 1) &^ 1
	in.masks[0] = (m1 | 1<<1) & valid // the backup's first segment
	in.masks[1] = m2 & valid &^ (1<<in.switch2 - 1)
	in.masks[2] = m3 & valid &^ (1<<in.switch3 - 1)
	if in.timeline < 2 {
		in.masks[2] = 0
	}
	return in
}

func (in monotoneInput) switchLSN(segment uint64) string {
	if in.mid {
		return fmt.Sprintf("0/%X000050", segment)
	}
	return fmt.Sprintf("0/%X000000", segment)
}

func (in monotoneInput) scenario() timelineScenario {
	s := timelineScenario{
		begin: 1, end: in.end, endLSN: fmt.Sprintf("0/%X000100", in.end),
		segments:  map[uint32][]uint64{},
		histories: map[uint32]string{2: "1\t" + in.switchLSN(in.switch2) + "\tfailover\n"},
		faults:    map[uint32]historyFault{},
	}
	switch in.gaps {
	case 1:
		s.gapStatus = GapConfirmed
	case 2:
		s.gapStatus = GapCandidate
	}
	if in.timeline >= 2 {
		if in.sibling {
			s.histories[3] = "1\t" + in.switchLSN(in.switch3) + "\tfailover\n"
		} else {
			s.histories[3] = s.histories[2] + "2\t" + in.switchLSN(in.switch3) + "\tfailover\n"
		}
	}
	for index, mask := range in.masks {
		for segment := uint64(1); segment <= monotoneSegments; segment++ {
			if mask&(1<<segment) != 0 {
				s.segments[uint32(index+1)] = append(s.segments[uint32(index+1)], segment)
			}
		}
	}
	return s
}

// monotonePaths are the paths of one archive: Barman's by target timeline,
// and pgBackRest's one path.
type monotonePaths struct {
	barman     map[uint32]barmancloud.RecoveryPath
	pgbackrest RecoveryPath
}

func analyzeMonotone(t *testing.T, s timelineScenario) monotonePaths {
	t.Helper()
	out := monotonePaths{barman: map[uint32]barmancloud.RecoveryPath{}, pgbackrest: s.pgbackrest(t)}
	for _, path := range s.barman(t).Paths {
		out.barman[path.TargetTimeline] = path
	}
	if len(out.pgbackrest.Reason) > pgtimeline.MaxReasonBytes {
		t.Fatalf("pgBackRest reason of %d bytes: %q", len(out.pgbackrest.Reason), out.pgbackrest.Reason)
	}
	return out
}

// assertNotFurther fails when after reaches past before: a path's frontier
// (its position; a timeline of 0 means none) moved forward although the
// archive only lost evidence.
func assertNotFurther(t *testing.T, label string, beforeTimeline uint32, beforePosition uint64, afterTimeline uint32, afterPosition uint64, reason string) {
	t.Helper()
	if afterTimeline != 0 && (beforeTimeline == 0 || afterPosition > beforePosition) {
		t.Errorf("%s: frontier moved forward from %d/%d to %d/%d (%q)", label, beforeTimeline, beforePosition, afterTimeline, afterPosition, reason)
	}
}

func assertNoFurtherReach(t *testing.T, before, after monotonePaths, what string) {
	t.Helper()
	for timeline, a := range after.barman {
		b, ok := before.barman[timeline]
		if !ok {
			if a.State == evidence.Healthy {
				t.Errorf("Barman: removing %s made a healthy path to timeline %d appear (%q)", what, timeline, a.Reason)
			}
			continue
		}
		assertNotFurther(t, fmt.Sprintf("Barman path to %d, removing %s", timeline, what), b.FrontierTimeline, b.FrontierPosition, a.FrontierTimeline, a.FrontierPosition, a.Reason)
	}
	if a, b := after.pgbackrest, before.pgbackrest; a.TargetTimeline == b.TargetTimeline {
		assertNotFurther(t, "pgBackRest, removing "+what, b.FrontierTimeline, b.FrontierPosition, a.FrontierTimeline, a.FrontierPosition, a.Reason)
	}
}

// FuzzRecoveryFrontierMonotoneSegment: removing any one archived segment
// never moves a path's frontier forward, in either format. The state may
// improve at the same frontier, and that is right: removing the segment
// after a hole (or a child's only WAL) removes the proof that the WAL
// before it is missing for good, so it may be on its way.
func FuzzRecoveryFrontierMonotoneSegment(f *testing.F) {
	f.Add(uint16(0b110), uint16(0b11100000), uint16(0), uint8(3), uint8(0), uint8(0), uint8(0), uint8(3))
	f.Add(uint16(0b110), uint16(0b11100000), uint16(0b1100000000), uint8(3), uint8(2), uint8(0b10101), uint8(1), uint8(14))
	f.Add(uint16(0b111111110), uint16(0b111000), uint16(0b111110000), uint8(1), uint8(1), uint8(0b10011), uint8(2), uint8(27))
	f.Add(uint16(0b11011110), uint16(0b1111100000), uint16(0), uint8(4), uint8(0), uint8(0b100), uint8(0), uint8(4))
	f.Fuzz(func(t *testing.T, m1, m2, m3 uint16, sw2, sw3, shape, end, drop uint8) {
		in := newMonotoneInput(m1, m2, m3, sw2, sw3, shape, end)
		index := int(drop) % (3 * monotoneSegments)
		timeline, segment := index/monotoneSegments, uint64(1+index%monotoneSegments)
		if in.masks[timeline]&(1<<segment) == 0 {
			return
		}
		reduced := in
		reduced.masks[timeline] &^= 1 << segment
		before, after := analyzeMonotone(t, in.scenario()), analyzeMonotone(t, reduced.scenario())
		assertNoFurtherReach(t, before, after, fmt.Sprintf("segment %d of timeline %d", segment, timeline+1))
	})
}

// FuzzRecoveryFrontierMonotoneHistory: removing a timeline's history file
// never makes a default restore reach that timeline or one after it. A
// default restore (recovery_target_timeline 'latest') stops probing at the
// first missing history file, so no path to such a timeline may be healthy
// and on target; and the paths that stay on timelines a default restore
// still follows never reach further than before, except where the
// earlier answer left the timeline at a switch: removing a history file
// changes which timeline 'latest' resolves to, and so where a default
// restore leaves an ancestor (that WAL was archived all along and only
// needed a targetTLI).
func FuzzRecoveryFrontierMonotoneHistory(f *testing.F) {
	f.Add(uint16(0b110), uint16(0b11100000), uint16(0), uint8(3), uint8(0), uint8(0), uint8(0), uint8(0))
	f.Add(uint16(0b110), uint16(0b11100000), uint16(0b1100000000), uint8(3), uint8(2), uint8(0b10101), uint8(1), uint8(1))
	f.Add(uint16(0b111111110), uint16(0b111000), uint16(0b111110000), uint8(1), uint8(1), uint8(0b10011), uint8(2), uint8(1))
	f.Fuzz(func(t *testing.T, m1, m2, m3 uint16, sw2, sw3, shape, end, drop uint8) {
		in := newMonotoneInput(m1, m2, m3, sw2, sw3, shape, end)
		full := in.scenario()
		removed := uint32(2 + drop%2)
		if _, ok := full.histories[removed]; !ok {
			return
		}
		reduced := full.clone()
		delete(reduced.histories, removed)
		before, after := analyzeMonotone(t, full), analyzeMonotone(t, reduced)
		for timeline, a := range after.barman {
			if timeline >= removed && a.State == evidence.Healthy && !a.OffTarget {
				t.Errorf("Barman: without history %d the path to timeline %d is healthy on target (%q)", removed, timeline, a.Reason)
			}
			b, ok := before.barman[timeline]
			if ok && timeline < removed && !leftAtSwitch(b.Reason) {
				assertNotFurther(t, fmt.Sprintf("Barman path to %d, removing history %d", timeline, removed), b.FrontierTimeline, b.FrontierPosition, a.FrontierTimeline, a.FrontierPosition, a.Reason)
			}
		}
		if a := after.pgbackrest; a.TargetTimeline >= removed && a.State == evidence.Healthy && !a.OffTarget {
			t.Errorf("pgBackRest: without history %d the path to timeline %d is healthy on target (%q)", removed, a.TargetTimeline, a.Reason)
		}
	})
}

// leftAtSwitch reports whether a Barman reason says a default restore left
// the path's timeline at a switch (leaveReason).
func leftAtSwitch(reason string) bool {
	return strings.Contains(reason, "a default restore leaves timeline ")
}
