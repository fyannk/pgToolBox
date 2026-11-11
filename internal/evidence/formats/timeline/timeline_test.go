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
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// linear is the ancestry of timeline k when every timeline descends from
// the one before it: 1, 2, ..., k-1.
func linear(k uint32) []uint32 {
	result := make([]uint32, 0, k-1)
	for value := uint32(1); value < k; value++ {
		result = append(result, value)
	}
	return result
}

// layout builds an archive: each history's status, with the ancestry of
// parents (linear when a valid history has no entry there).
func layout(truncated bool, observed []uint32, histories map[uint32]Status, parents map[uint32][]uint32) *Archive {
	archive := NewArchive(truncated)
	for _, timeline := range observed {
		archive.Observe(timeline)
	}
	for timeline, status := range histories {
		ancestors := parents[timeline]
		if ancestors == nil {
			ancestors = linear(timeline)
		}
		archive.History(timeline, status, ancestors...)
	}
	return archive
}

func TestLatestProbesConsecutiveHistoryFiles(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		histories map[uint32]Status
		parents   map[uint32][]uint32
		observed  []uint32
		truncated bool
		start     uint32
		latest    uint32
		settled   bool
		known     bool
		complete  bool
		stop      uint32
		gap       string
	}{
		{name: "no newer timeline", observed: []uint32{1}, start: 1, latest: 1, settled: true, known: true, complete: true, stop: 2},
		{name: "every history present", histories: map[uint32]Status{2: Valid, 3: Valid}, observed: []uint32{1, 2, 3}, start: 1, latest: 3, settled: true, known: true, complete: true, stop: 4},
		// findNewestTimeLine stops at the first missing file: timeline 3's
		// history does not make it reachable.
		{name: "middle history missing", histories: map[uint32]Status{3: Valid}, observed: []uint32{1, 2, 3}, start: 1, latest: 1, settled: true, known: true, stop: 2, gap: "history 2 is missing"},
		{name: "newest history missing", histories: map[uint32]Status{2: Valid}, observed: []uint32{1, 2, 3}, start: 1, latest: 2, settled: true, known: true, stop: 3, gap: "history 3 is missing"},
		{name: "newest history unreadable", histories: map[uint32]Status{2: Unreadable}, observed: []uint32{1}, start: 1, latest: 1, known: true, stop: 2, gap: "history 2 is unreadable"},
		// The probe tests existence only: a malformed history counts, but
		// recovery cannot read the ancestry of the timeline it resolves to.
		{name: "newest history invalid", histories: map[uint32]Status{2: Invalid}, observed: []uint32{1}, start: 1, latest: 2, settled: true, stop: 3, gap: "history 2 is unreadable or invalid"},
		{name: "middle history invalid, newest valid", histories: map[uint32]Status{2: Invalid, 3: Valid}, parents: map[uint32][]uint32{3: {1}}, observed: []uint32{1, 2, 3}, start: 1, latest: 3, settled: true, known: true, complete: true, stop: 4},
		{name: "start's own history invalid", histories: map[uint32]Status{2: Invalid}, observed: []uint32{2}, start: 2, latest: 2, settled: true, stop: 3, gap: "history 2 is unreadable or invalid"},
		{name: "start's own history absent", observed: []uint32{2}, start: 2, latest: 2, settled: true, known: true, complete: true, stop: 3},
		{name: "listing truncated", histories: map[uint32]Status{2: Valid}, observed: []uint32{1, 2}, truncated: true, start: 1, latest: 2, known: true, stop: 3, gap: "not all read"},
		{name: "start above the gap", histories: map[uint32]Status{3: Valid}, observed: []uint32{2, 3}, start: 2, latest: 3, settled: true, known: true, complete: true, stop: 4},
		{name: "a timeline number that does not wrap", observed: []uint32{math.MaxUint32}, start: math.MaxUint32, latest: math.MaxUint32, settled: true, known: true, complete: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := layout(test.truncated, test.observed, test.histories, test.parents).Latest(test.start)
			if got.Latest != test.latest || got.Settled != test.settled || got.Known() != test.known || got.Complete() != test.complete || got.Stop != test.stop {
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

// TestReachesFollowsTheAncestryOfLatest is the round-4 reproduction
// (P1-6): timeline 3 forked from timeline 1, so a default restore
// follows 1 -> 3 and never timeline 2, whose number is lower.
func TestReachesFollowsTheAncestryOfLatest(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		parents   map[uint32][]uint32
		start     uint32
		reaches   []uint32
		elsewhere []uint32
	}{
		{name: "linear", parents: map[uint32][]uint32{2: {1}, 3: {1, 2}}, start: 1, reaches: []uint32{1, 2, 3}},
		{name: "siblings from timeline 1", parents: map[uint32][]uint32{2: {1}, 3: {1}}, start: 1, reaches: []uint32{1, 3}, elsewhere: []uint32{2}},
		{name: "start on the abandoned sibling", parents: map[uint32][]uint32{2: {1}, 3: {1}}, start: 2, elsewhere: []uint32{2, 3}},
		{name: "start on the line", parents: map[uint32][]uint32{2: {1}, 3: {1, 2}}, start: 2, reaches: []uint32{2, 3}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := layout(false, []uint32{1, 2, 3}, map[uint32]Status{2: Valid, 3: Valid}, test.parents).Latest(test.start)
			for target := test.start; target <= 3; target++ {
				if want := slices.Contains(test.reaches, target); got.Reaches(target) != want {
					t.Errorf("Reaches(%d) = %v, want %v (%+v)", target, !want, want, got)
				}
				if want := slices.Contains(test.elsewhere, target); got.Elsewhere(target) != want {
					t.Errorf("Elsewhere(%d) = %v, want %v", target, !want, want)
				}
			}
			for _, target := range test.elsewhere {
				if reason := got.NeedsReason(target); !strings.Contains(reason, "needs targetTLI "+strconv.Itoa(int(target))) {
					t.Errorf("NeedsReason(%d) = %q", target, reason)
				}
			}
		})
	}
	// Without the ancestry of Latest nothing is reached beyond the start.
	archive := NewArchive(false)
	archive.History(2, Valid)
	if got := archive.Latest(1); got.Reaches(2) || got.Reaches(1) || got.Complete() || got.Elsewhere(2) {
		t.Fatalf("a valid history without ancestry was followed: %+v", got)
	}
}

func TestNeedsNamesTheHistoryRecoveryReads(t *testing.T) {
	t.Parallel()
	got := layout(false, []uint32{1, 4}, map[uint32]Status{2: Invalid, 3: Valid, 4: Unreadable}, map[uint32][]uint32{3: {1}}).Latest(1)
	for timeline, want := range map[uint32]bool{1: false, 2: false, 3: true, 4: true, 5: false} {
		if got.Needs(timeline) != want {
			t.Errorf("Needs(%d) = %v, want %v (%+v)", timeline, !want, want, got)
		}
	}
}

func TestHistoryKeepsTheWeakerStatus(t *testing.T) {
	t.Parallel()
	archive := NewArchive(false)
	archive.History(2, Unreadable)
	archive.History(2, Valid, 1)
	if got := archive.Latest(1); got.Latest != 1 || got.StopStatus != Unreadable {
		t.Fatalf("a duplicate valid history hid an unreadable one: %+v", got)
	}
	archive.History(3, Absent)
	if archive.Newest() != 3 {
		t.Fatalf("Newest() = %d", archive.Newest())
	}
	// Two valid histories of one timeline that disagree are invalid.
	archive = NewArchive(false)
	archive.History(3, Valid, 1, 2)
	archive.History(3, Valid, 1)
	archive.History(2, Valid, 1)
	if got := archive.Latest(1); got.Known() || got.LatestStatus != Invalid {
		t.Fatalf("contradictory duplicates were believed: %+v", got)
	}
}

func TestBeyondReasonNamesWhatARestoreNeeds(t *testing.T) {
	t.Parallel()
	archive := NewArchive(false)
	archive.History(3, Valid, 1, 2)
	archive.Observe(1)
	resolution := archive.Latest(1)
	if got := resolution.BeyondReason(3); got != "timeline 3 needs an explicit targetTLI; history 2 is missing" {
		t.Fatalf("BeyondReason(3) = %q", got)
	}
	if got := resolution.BeyondReason(2); !strings.Contains(got, "no restore can follow timeline 2") {
		t.Fatalf("BeyondReason(2) = %q", got)
	}
}

// TestWeakeningAHistoryNeverStrengthensTheResolution is the exhaustive
// monotonicity sweep over small layouts: timelines 2 to 4, each history
// Absent, Valid, Invalid or Unreadable, every ancestry tree (linear,
// siblings and mixed), from start timelines 1 and 2, with the archive's
// newest timeline fixed at 4. Weakening one valid history (to Absent,
// Invalid or Unreadable) never raises Latest, never makes the resolution
// complete, and never makes a timeline reached that was not.
//
// One class is excluded, because it is PostgreSQL's behaviour: removing a
// history file can settle the probe below the newest timeline, and the
// default restore then really follows the lower timeline. The documented
// case is 2 Valid, 3 Valid, 4 Unreadable: the probe stops undecided at 4;
// without 3.history it stops, settled, at 3, so timeline 2 is reached.
// Such a resolution is never complete, so the format's timeline evidence
// is not healthy and the console does not take its paths as coverage.
func TestWeakeningAHistoryNeverStrengthensTheResolution(t *testing.T) {
	t.Parallel()
	statuses := []Status{Absent, Valid, Invalid, Unreadable}
	trees := make([]map[uint32][]uint32, 0, 6)
	for parent3 := uint32(1); parent3 <= 2; parent3++ {
		for parent4 := uint32(1); parent4 <= 3; parent4++ {
			ancestry := map[uint32][]uint32{2: {1}}
			ancestry[3] = append(slices.Clone(ancestry[parent3]), parent3)
			ancestry[4] = append(slices.Clone(ancestry[parent4]), parent4)
			trees = append(trees, ancestry)
		}
	}
	build := func(tree map[uint32][]uint32, set [3]Status) *Archive {
		histories := map[uint32]Status{}
		for index, status := range set {
			histories[uint32(index)+2] = status
		}
		return layout(false, []uint32{1, 4}, histories, tree)
	}
	excluded, documented := 0, false
	for _, tree := range trees {
		for _, a := range statuses {
			for _, b := range statuses {
				for _, c := range statuses {
					set := [3]Status{a, b, c}
					for index := range set {
						if set[index] != Valid {
							continue
						}
						for _, weaker := range []Status{Absent, Invalid, Unreadable} {
							weakened := set
							weakened[index] = weaker
							for _, start := range []uint32{1, 2} {
								before, after := build(tree, set).Latest(start), build(tree, weakened).Latest(start)
								if after.Latest > before.Latest || after.Complete() && !before.Complete() {
									t.Fatalf("tree %v, %v -> %v from %d: %+v -> %+v", tree, set, weakened, start, before, after)
								}
								for target := start; target <= 4; target++ {
									if !after.Reaches(target) || before.Reaches(target) {
										continue
									}
									if weaker != Absent || !after.Settled || after.Latest >= after.Newest || after.Complete() {
										t.Fatalf("tree %v, %v -> %v from %d: timeline %d newly reached: %+v -> %+v", tree, set, weakened, start, target, before, after)
									}
									excluded++
									documented = documented || set == [3]Status{Valid, Valid, Unreadable} && index == 1 && target == 2 && start == 1
								}
							}
						}
					}
				}
			}
		}
	}
	if !documented || excluded == 0 {
		t.Fatalf("the documented non-monotone case was not met (%d excluded)", excluded)
	}
}

// TestJoinKeepsTheActionableTail (round 8, PG P3): a later Join (an
// off-target note, capUnknown) used to cut the reason from its end, which
// can drop "do not delete that instance or its volume". The clause stays
// whole at the 256-byte limit; what comes before it is cut instead.
func TestJoinKeepsTheActionableTail(t *testing.T) {
	t.Parallel()
	gap := "segment 000000010000000A000000FF before the timeline switch is not in the repository; " + FormerPrimaryMayHold
	for _, extra := range []string{
		"backup metadata has conflicting PostgreSQL system identifiers",
		"the backup is on timeline 1, behind the newest; a restore must name timeline 1",
		"",
	} {
		reason := gap
		if extra == "" {
			reason = strings.Repeat("x", 120) + "; " + gap
		}
		got := Join(reason, extra)
		if len(got) > MaxReasonBytes || !utf8.ValidString(got) {
			t.Fatalf("Join = %d bytes", len(got))
		}
		if !strings.Contains(got, "do not delete that instance or its volume") {
			t.Errorf("Join(%q, %q) = %q lost the actionable clause", reason, extra, got)
		}
		if extra != "" && !strings.HasSuffix(got, extra) {
			t.Errorf("Join dropped the extra reason: %q", got)
		}
	}
	// Twice, as a capped path's reason is joined again.
	twice := Join(Join(gap, "a restore must name timeline 1"), "not coverage: the timeline evidence is unknown")
	if len(twice) > MaxReasonBytes || !strings.Contains(twice, FormerPrimaryMayHold) {
		t.Errorf("joined twice: %q (%d bytes)", twice, len(twice))
	}
}

// TestJoinCutsExtraBeforeTheClause (round 9, PG P3): Barman's capUnknown
// joins the own-timeline reason (about 125 bytes) to a path's reason that
// ends with FormerPrimaryMayHold; together they exceed the wire bound.
// The clause stays whole and extra is cut after it.
func TestJoinCutsExtraBeforeTheClause(t *testing.T) {
	t.Parallel()
	reason := "segment 000000010000000A000000FF before the timeline switch is not in the repository; " + FormerPrimaryMayHold
	extra := "a default restore (recovery_target_timeline 'latest') stops on timeline 3: timeline 4 history is missing; timeline 5 needs an explicit targetTLI"
	for _, r := range []string{reason, FormerPrimaryMayHold, strings.Repeat("é", 100) + "; " + FormerPrimaryMayHold + "; trailing words"} {
		got := Join(r, extra)
		if len(got) > MaxReasonBytes || !utf8.ValidString(got) {
			t.Fatalf("Join = %d bytes, valid UTF-8 %v", len(got), utf8.ValidString(got))
		}
		if !strings.Contains(got, FormerPrimaryMayHold) {
			t.Errorf("Join(%q, %q) = %q lost the actionable clause", r, extra, got)
		}
		if !strings.Contains(got, extra[:40]) {
			t.Errorf("Join dropped extra entirely: %q", got)
		}
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
