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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/barmancloud"
	pgtimeline "github.com/fyannk/pgToolBox/internal/evidence/formats/timeline"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
	"github.com/fyannk/pgToolBox/internal/evidence/store/storetest"
)

const crossSegment = 16 << 20

// historyFault is how a history file of a timelineScenario fails.
type historyFault int

const (
	historyOK historyFault = iota
	// historyUnreadable: the object store answers an error (a 503).
	historyUnreadable
	// historyMalformed: the content is not a history file.
	historyMalformed
	// historyOversize: the object exceeds the history size bound.
	historyOversize
)

// timelineScenario is one archive read both as Barman Cloud and as
// pgBackRest: one backup on timeline 1, the archived segments of each
// timeline, and the history files.
type timelineScenario struct {
	begin, end uint64
	endLSN     string
	segments   map[uint32][]uint64
	histories  map[uint32]string
	faults     map[uint32]historyFault
	truncated  bool
	// gapStatus, when set, records every hole between two runs of a
	// timeline as a gap of that status (both formats); unset, the archive
	// has no gaps recorded.
	gapStatus GapStatus
}

func (s timelineScenario) clone() timelineScenario {
	out := s
	out.segments = make(map[uint32][]uint64, len(s.segments))
	for timeline, values := range s.segments {
		out.segments[timeline] = slices.Clone(values)
	}
	out.histories = maps.Clone(s.histories)
	out.faults = maps.Clone(s.faults)
	return out
}

func crossName(t testing.TB, timeline uint32, position uint64) string {
	t.Helper()
	name, err := WALName(timeline, position, crossSegment)
	if err != nil {
		t.Fatal(err)
	}
	return name
}

// ranges are the contiguous runs of the scenario's segments, sorted.
func (s timelineScenario) ranges(t testing.TB) []WALRange {
	t.Helper()
	var out []WALRange
	for _, timeline := range slices.Sorted(maps.Keys(s.segments)) {
		positions := slices.Sorted(slices.Values(s.segments[timeline]))
		for index := 0; index < len(positions); {
			last := index
			for last+1 < len(positions) && positions[last+1] == positions[last]+1 {
				last++
			}
			receipt := time.Unix(int64(positions[last]), 0).UTC()
			out = append(out, WALRange{
				Timeline: timeline, Start: positions[index], End: positions[last], Count: positions[last] - positions[index] + 1,
				First: crossName(t, timeline, positions[index]), Last: crossName(t, timeline, positions[last]), EndReceipt: receipt, LatestReceipt: receipt,
			})
			index = last + 1
		}
	}
	return out
}

// gaps are the holes between consecutive runs of each timeline, recorded as
// gapStatus; none when gapStatus is unset.
func (s timelineScenario) gaps(t testing.TB) []WALGap {
	t.Helper()
	if s.gapStatus == "" {
		return nil
	}
	var out []WALGap
	ranges := s.ranges(t)
	for index := 0; index+1 < len(ranges); index++ {
		left, right := ranges[index], ranges[index+1]
		if left.Timeline != right.Timeline {
			continue
		}
		first, last := left.End+1, right.Start-1
		gap := WALGap{
			Timeline: left.Timeline, Start: first, End: last, Count: last - first + 1,
			First: crossName(t, left.Timeline, first), Last: crossName(t, left.Timeline, last), Status: s.gapStatus,
			FirstObservedGeneration: 1, LastObservedGeneration: 2,
		}
		if s.gapStatus == GapCandidate {
			gap.LastObservedGeneration = 1
		}
		out = append(out, gap)
	}
	return out
}

func (s timelineScenario) newestVisible() uint32 {
	var newest uint32
	for timeline, values := range s.segments {
		if len(values) > 0 {
			newest = max(newest, timeline)
		}
	}
	for timeline := range s.histories {
		newest = max(newest, timeline)
	}
	return newest
}

// pgbackrest evaluates the backup's path as pgBackRest reads the archive.
func (s timelineScenario) pgbackrest(t testing.TB) RecoveryPath {
	t.Helper()
	wal := ArchiveWAL{State: evidence.Healthy, SegmentSize: crossSegment, Ranges: s.ranges(t), Gaps: s.gaps(t)}
	histories := make([]TimelineHistory, 0, len(s.histories))
	for _, timeline := range slices.Sorted(maps.Keys(s.histories)) {
		history := TimelineHistory{ArchiveID: "17-1", Key: fmt.Sprintf("archive/demo/17-1/%08X.history", timeline), Timeline: timeline}
		switch s.faults[timeline] {
		case historyUnreadable, historyOversize:
			history.State, history.Reason = evidence.Unknown, "timeline history could not be read"
		case historyMalformed:
			history.State, history.Reason = evidence.Unhealthy, "timeline history is malformed"
		default:
			edges, err := ParseHistory([]byte(s.histories[timeline]), timeline, crossSegment)
			if err != nil {
				history.State, history.Reason = evidence.Unhealthy, "timeline history is malformed"
			} else {
				history.State, history.Reason, history.Edges = evidence.Healthy, "timeline ancestry parsed", edges
			}
		}
		histories = append(histories, history)
	}
	backup := Backup{Label: "b", ArchiveStart: crossName(t, 1, s.begin), ArchiveStop: crossName(t, 1, s.end), LSNStop: s.endLSN}
	return evaluatePath(backup, wal, histories, s.truncated)
}

// barman analyzes the same archive as a Barman Cloud server.
func (s timelineScenario) barman(t testing.TB) barmancloud.ServerRecovery {
	t.Helper()
	server := barmancloud.ServerWAL{Server: "demo", State: evidence.Healthy, PostgreSQLVersion: 170000, SegmentSize: crossSegment}
	for _, value := range s.ranges(t) {
		server.Ranges = append(server.Ranges, barmancloud.WALRange(value))
	}
	for _, value := range s.gaps(t) {
		gap := barmancloud.WALGap{
			Timeline: value.Timeline, Start: value.Start, End: value.End, Count: value.Count, First: value.First, Last: value.Last,
			Status: barmancloud.GapStatus(value.Status), FirstObservedGeneration: value.FirstObservedGeneration, LastObservedGeneration: value.LastObservedGeneration,
		}
		server.Gaps = append(server.Gaps, gap)
	}
	backup := barmancloud.Backup{
		Server: "demo", ID: "b", State: evidence.Healthy, BeginWAL: crossName(t, 1, s.begin), EndWAL: crossName(t, 1, s.end),
		EndLSN: s.endLSN, SegmentSize: crossSegment, PostgreSQLVersion: 170000, Timeline: 1, EndAt: time.Unix(1, 0).UTC(),
	}
	contents := make(map[string]string)
	objects := make([]barmancloud.HistoryObject, 0, len(s.histories))
	for _, timeline := range slices.Sorted(maps.Keys(s.histories)) {
		name := fmt.Sprintf("%08X.history", timeline)
		key := "demo/wals/" + name
		objects = append(objects, barmancloud.HistoryObject{Key: key, Server: "demo", Name: name, Compression: "none", Timeline: timeline})
		switch s.faults[timeline] {
		case historyUnreadable:
			// Absent from contents: the fake store answers an error.
		case historyMalformed:
			contents[key] = "not a history file"
		case historyOversize:
			contents[key] = strings.Repeat("#", int(barmancloud.MaxHistoryObjectBytes)+1)
		default:
			contents[key] = s.histories[timeline]
		}
	}
	reader := &storetest.Fake{OpenFunc: func(_ context.Context, request store.OpenRequest) (io.ReadCloser, error) {
		content, ok := contents[request.Key]
		if !ok {
			return nil, errors.New("503 service unavailable")
		}
		return io.NopCloser(bytes.NewReader([]byte(content))), nil
	}}
	catalog := barmancloud.AnalyzeRecovery(context.Background(), reader, []barmancloud.Backup{backup}, barmancloud.WALCatalog{Servers: []barmancloud.ServerWAL{server}}, objects, s.truncated, barmancloud.RecoveryOptions{})
	if err := catalog.Validate(); err != nil {
		t.Fatalf("Barman catalog does not validate: %v", err)
	}
	if len(catalog.Servers) != 1 {
		t.Fatalf("Barman servers %+v", catalog.Servers)
	}
	return catalog.Servers[0]
}

// The reviewer's inputs (round 3, P1-3 and P1-4).
const (
	history3 = "1\t0/2000050\tfirst\n2\t0/4000050\tsecond\n"
	history2 = "1\t0/2000050\tfirst\n"
	// offTargetHistory2: timeline 2 branched one byte before the backup end
	// (end LSN 0/3000051).
	offTargetHistory2 = "1\t0/3000050\tpromoted\n"
)

func middleGapScenario(withHistory2 bool) timelineScenario {
	s := timelineScenario{
		begin: 1, end: 1, endLSN: "0/1000100",
		segments:  map[uint32][]uint64{1: {1}, 2: {2, 3}, 3: {4, 5, 6}},
		histories: map[uint32]string{3: history3},
		faults:    map[uint32]historyFault{},
	}
	if withHistory2 {
		s.histories[2] = history2
	}
	return s
}

func newestUnreadableScenario(fault historyFault, truncated bool) timelineScenario {
	return timelineScenario{
		begin: 1, end: 3, endLSN: "0/3000051",
		segments:  map[uint32][]uint64{1: {1, 2, 3}},
		histories: map[uint32]string{2: offTargetHistory2},
		faults:    map[uint32]historyFault{2: fault},
		truncated: truncated,
	}
}

// TestTimelineLatestAgreesAcrossFormats: both formats resolve
// recovery_target_timeline 'latest' with PostgreSQL's probe of
// consecutive history files (findNewestTimeLine), so a missing middle
// history file (P1-3) or an unreadable newest one (P1-4) never reads as
// healthy coverage in either, and the two agree with the valid archives.
func TestTimelineLatestAgreesAcrossFormats(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		scenario  timelineScenario
		state     evidence.State // of the pgBackRest path and the Barman own-timeline path
		offTarget bool
		healthy   bool // whether any Barman path and the Barman timeline state may be healthy
		reason    string
	}{
		{name: "middle history missing", scenario: middleGapScenario(false), state: evidence.Unknown, reason: "history 2 is missing"},
		{name: "every history present", scenario: middleGapScenario(true), state: evidence.Healthy, healthy: true},
		{name: "newest history valid, backup left behind", scenario: newestUnreadableScenario(historyOK, false), state: evidence.Warning, offTarget: true, healthy: true},
		{name: "newest history unreadable (503)", scenario: newestUnreadableScenario(historyUnreadable, false), state: evidence.Unknown, reason: "history"},
		{name: "newest history malformed", scenario: newestUnreadableScenario(historyMalformed, false), state: evidence.Unknown, reason: "history"},
		{name: "newest history over the size bound", scenario: newestUnreadableScenario(historyOversize, false), state: evidence.Unknown, reason: "history"},
		{name: "history listing truncated", scenario: newestUnreadableScenario(historyOK, true), state: evidence.Unknown, reason: "not all read"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			pg := test.scenario.pgbackrest(t)
			server := test.scenario.barman(t)
			var own barmancloud.RecoveryPath
			for _, path := range server.Paths {
				if path.TargetTimeline == 1 {
					own = path
				}
				if !test.healthy && path.State == evidence.Healthy {
					t.Errorf("Barman path to timeline %d is healthy: %q", path.TargetTimeline, path.Reason)
				}
			}
			if own.BackupID == "" {
				t.Fatalf("no Barman own-timeline path: %+v", server.Paths)
			}
			if test.healthy && pg.State == evidence.Healthy {
				// The pgBackRest path follows the newest timeline; Barman's
				// path to the same timeline must agree.
				for _, path := range server.Paths {
					if path.TargetTimeline == pg.TargetTimeline && path.State != pg.State {
						t.Errorf("Barman path to timeline %d is %s, pgBackRest's %s", path.TargetTimeline, path.State, pg.State)
					}
				}
			} else if own.State != test.state {
				t.Errorf("Barman own-timeline path %s %q, want %s", own.State, own.Reason, test.state)
			}
			if pg.State != test.state || pg.OffTarget != test.offTarget {
				t.Errorf("pgBackRest path %s off-target %v %q, want %s off-target %v", pg.State, pg.OffTarget, pg.Reason, test.state, test.offTarget)
			}
			if !test.healthy && (server.TimelineState == evidence.Healthy || server.CoverageState == evidence.Healthy) {
				t.Errorf("Barman timeline %s %q, coverage %s %q", server.TimelineState, server.TimelineReason, server.CoverageState, server.CoverageReason)
			}
			if test.reason != "" && (!strings.Contains(pg.Reason, test.reason) || !strings.Contains(own.Reason, test.reason)) {
				t.Errorf("reasons do not say %q: pgBackRest %q, Barman %q", test.reason, pg.Reason, own.Reason)
			}
		})
	}
}

// ancestryScenario is a backup on timeline 1 (segments 1 to 3, end LSN
// 0/3000100) and two newer timelines whose histories decide the line a
// default restore follows.
func ancestryScenario(history2, history3 string, segments map[uint32][]uint64) timelineScenario {
	return timelineScenario{
		begin: 1, end: 3, endLSN: "0/3000100", segments: segments,
		histories: map[uint32]string{2: history2, 3: history3},
		faults:    map[uint32]historyFault{},
	}
}

// The round-4 layouts (P1-6): timeline 2 forked from timeline 1 after the
// backup ended; timeline 3 is linear (from 2), a sibling forked from 1
// after the backup ended, or a sibling forked from 1 before it ended.
func linearScenario() timelineScenario {
	return ancestryScenario("1\t0/5000050\tfailover\n", "1\t0/5000050\tfailover\n2\t0/7000050\tfailover\n",
		map[uint32][]uint64{1: {1, 2, 3, 4, 5}, 2: {5, 6, 7}, 3: {7, 8, 9}})
}

func siblingAfterEndScenario() timelineScenario {
	return ancestryScenario("1\t0/5000050\tfailover\n", "1\t0/4000050\tpitr\n",
		map[uint32][]uint64{1: {1, 2, 3, 4, 5}, 2: {5, 6, 7}, 3: {4, 5, 6}})
}

func siblingBeforeEndScenario() timelineScenario {
	return ancestryScenario("1\t0/5000050\tfailover\n", "1\t0/2000050\tpitr\n",
		map[uint32][]uint64{1: {1, 2, 3, 4, 5}, 2: {5, 6, 7}, 3: {2, 3, 4}})
}

// TestTimelineAncestryAgreesAcrossFormats: recovery reads the history of
// the timeline 'latest' resolves to and follows only the ancestry it
// lists. A sibling branch with a lower number is not on it: Barman's path
// toward it is off target, at most a warning, and names the targetTLI a
// restore must give. pgBackRest evaluates only toward the newest timeline,
// so it is unaffected; both agree on that path. The reproduction (sibling
// forked before the backup ended): only targetTLI 2 restores the backup,
// and no path may read as healthy.
func TestTimelineAncestryAgreesAcrossFormats(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		scenario timelineScenario
		// states of the Barman paths by target timeline (absent: no path)
		paths     map[uint32]evidence.State
		elsewhere []uint32 // Barman paths off target because a sibling
		pgTarget  uint32
		pgState   evidence.State
		pgOff     bool
		coverage  evidence.State
	}{
		{name: "linear", scenario: linearScenario(), paths: map[uint32]evidence.State{1: evidence.Healthy, 2: evidence.Healthy, 3: evidence.Healthy}, pgTarget: 3, pgState: evidence.Healthy, coverage: evidence.Healthy},
		{name: "sibling forked after the backup", scenario: siblingAfterEndScenario(), paths: map[uint32]evidence.State{1: evidence.Healthy, 2: evidence.Warning, 3: evidence.Healthy}, elsewhere: []uint32{2}, pgTarget: 3, pgState: evidence.Healthy, coverage: evidence.Healthy},
		{name: "sibling forked before the backup ended", scenario: siblingBeforeEndScenario(), paths: map[uint32]evidence.State{1: evidence.Warning, 2: evidence.Warning}, elsewhere: []uint32{2}, pgTarget: 1, pgState: evidence.Warning, pgOff: true, coverage: evidence.Warning},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := test.scenario.barman(t)
			if server.TimelineState != evidence.Healthy {
				t.Fatalf("Barman timeline %s %q", server.TimelineState, server.TimelineReason)
			}
			got := make(map[uint32]evidence.State)
			for _, path := range server.Paths {
				got[path.TargetTimeline] = path.State
				sibling := slices.Contains(test.elsewhere, path.TargetTimeline)
				if sibling && (!path.OffTarget || !strings.Contains(path.Reason, fmt.Sprintf("needs targetTLI %d", path.TargetTimeline))) {
					t.Errorf("Barman path to sibling timeline %d: off-target %v %q", path.TargetTimeline, path.OffTarget, path.Reason)
				}
				if !sibling && path.TargetTimeline != 1 && path.OffTarget {
					t.Errorf("Barman path to timeline %d off target: %q", path.TargetTimeline, path.Reason)
				}
			}
			if !maps.Equal(got, test.paths) {
				t.Fatalf("Barman paths %v, want %v: %+v", got, test.paths, server.Paths)
			}
			if server.CoverageState != test.coverage {
				t.Fatalf("Barman coverage %s %q, want %s", server.CoverageState, server.CoverageReason, test.coverage)
			}
			pg := test.scenario.pgbackrest(t)
			if pg.TargetTimeline != test.pgTarget || pg.State != test.pgState || pg.OffTarget != test.pgOff {
				t.Fatalf("pgBackRest path to %d %s off-target %v %q, want %d %s %v", pg.TargetTimeline, pg.State, pg.OffTarget, pg.Reason, test.pgTarget, test.pgState, test.pgOff)
			}
			if got[pg.TargetTimeline] != pg.State {
				t.Fatalf("Barman path to timeline %d is %s, pgBackRest's %s", pg.TargetTimeline, got[pg.TargetTimeline], pg.State)
			}
		})
	}
}

// claimRank orders what a path state claims about coverage: healthy
// claims the most, warning less, unknown and unhealthy claim none.
func claimRank(state evidence.State) int {
	switch state {
	case evidence.Healthy:
		return 2
	case evidence.Warning:
		return 1
	default:
		return 0
	}
}

// mutations are every archive one object less or one history corrupted:
// each WAL segment removed, each history file removed, each history file
// made unreadable or malformed. A removal that hides the newest timeline
// from the archive altogether is left out: nothing then shows it existed,
// which is what PostgreSQL's own 'latest' sees too.
func (s timelineScenario) mutations() map[string]timelineScenario {
	out := make(map[string]timelineScenario)
	for timeline, values := range s.segments {
		for index, position := range values {
			next := s.clone()
			next.segments[timeline] = slices.Delete(next.segments[timeline], index, index+1)
			out[fmt.Sprintf("without segment %d of timeline %d", position, timeline)] = next
		}
	}
	for timeline := range s.histories {
		next := s.clone()
		delete(next.histories, timeline)
		delete(next.faults, timeline)
		out[fmt.Sprintf("without history %d", timeline)] = next
		for _, fault := range []historyFault{historyUnreadable, historyMalformed, historyOversize} {
			next := s.clone()
			next.faults[timeline] = fault
			out[fmt.Sprintf("history %d fault %d", timeline, fault)] = next
		}
	}
	next := s.clone()
	next.truncated = true
	out["history listing truncated"] = next
	for name, value := range out {
		if value.newestVisible() < s.newestVisible() {
			delete(out, name)
		}
	}
	return out
}

// TestRemovingEvidenceNeverStrengthensAClaim is the monotonicity property
// of the recovery analysis, for both formats: removing or corrupting one
// object of an archive never makes a path's state stronger, never moves a
// claimed frontier further, and never makes the Barman timeline or
// coverage roll-up stronger; a path that appears only once evidence is
// gone claims nothing.
func TestRemovingEvidenceNeverStrengthensAClaim(t *testing.T) {
	t.Parallel()
	bases := map[string]timelineScenario{
		"three timelines, every history":       middleGapScenario(true),
		"three timelines, middle history gone": middleGapScenario(false),
		"backup left behind by timeline 2":     newestUnreadableScenario(historyOK, false),
		"backup left behind, timeline 2 has WAL": func() timelineScenario {
			s := newestUnreadableScenario(historyOK, false)
			s.segments[2] = []uint64{3, 4, 5}
			return s
		}(),
		"linear ancestry":                linearScenario(),
		"ancestor runs past the switch":  ancestorLeftScenario(),
		"parent tail lost before switch": parentTailLostScenario(),
		"parent tail present before switch": func() timelineScenario {
			s := parentTailLostScenario()
			s.segments[1] = []uint64{1, 2, 3, 4}
			return s
		}(),
		"sibling forked after the backup":  siblingAfterEndScenario(),
		"sibling forked before the backup": siblingBeforeEndScenario(),
		"backup follows timeline 2": func() timelineScenario {
			s := newestUnreadableScenario(historyOK, false)
			s.endLSN = "0/3000050"
			s.segments[1] = []uint64{1, 2}
			s.segments[2] = []uint64{3, 4, 5}
			return s
		}(),
	}
	for baseName, base := range bases {
		pgBase, barmanBase := base.pgbackrest(t), base.barman(t)
		for name, mutated := range base.mutations() {
			t.Run(baseName+"/"+name, func(t *testing.T) {
				t.Parallel()
				pg := mutated.pgbackrest(t)
				if claimRank(pg.State) > claimRank(pgBase.State) {
					t.Errorf("pgBackRest path %s %q became stronger than %s %q", pg.State, pg.Reason, pgBase.State, pgBase.Reason)
				}
				if claimRank(pg.State) > 0 && pg.FrontierTimeline == pgBase.FrontierTimeline && pg.FrontierPosition > pgBase.FrontierPosition {
					t.Errorf("pgBackRest frontier moved from %d to %d", pgBase.FrontierPosition, pg.FrontierPosition)
				}
				barman := mutated.barman(t)
				if claimRank(barman.TimelineState) > claimRank(barmanBase.TimelineState) || claimRank(barman.CoverageState) > claimRank(barmanBase.CoverageState) {
					t.Errorf("Barman roll-ups timeline %s, coverage %s became stronger than %s, %s", barman.TimelineState, barman.CoverageState, barmanBase.TimelineState, barmanBase.CoverageState)
				}
				before := make(map[uint32]barmancloud.RecoveryPath)
				for _, path := range barmanBase.Paths {
					before[path.TargetTimeline] = path
				}
				// PostgreSQL's own non-monotone case: removing a history file
				// can settle 'latest' on a lower timeline, which a default
				// restore then really follows (without 00000003.history a
				// sibling timeline 2 is the latest). The Barman timeline
				// evidence is then not healthy (a history the probe needs is
				// missing), so the console takes no path as coverage.
				lowered := strings.HasPrefix(name, "without history") && barman.TimelineState != evidence.Healthy
				for _, path := range barman.Paths {
					prior, ok := before[path.TargetTimeline]
					switch {
					case lowered:
					case !ok && claimRank(path.State) > 0:
						t.Errorf("new Barman path to timeline %d claims %s: %q", path.TargetTimeline, path.State, path.Reason)
					case ok && claimRank(path.State) > claimRank(prior.State):
						t.Errorf("Barman path to timeline %d became %s %q, was %s %q", path.TargetTimeline, path.State, path.Reason, prior.State, prior.Reason)
					case ok && claimRank(path.State) > 0 && path.FrontierTimeline == prior.FrontierTimeline && path.FrontierPosition > prior.FrontierPosition:
						t.Errorf("Barman frontier to timeline %d moved from %d to %d", path.TargetTimeline, prior.FrontierPosition, path.FrontierPosition)
					}
				}
			})
		}
	}
}

// TestOffTargetReasonIsKeptBesideAnotherFailure: a backup left behind by
// the newest timeline whose own WAL to consistency is missing is flagged
// off-target in both formats, and both keep the failure's reason beside
// the timeline a restore must name.
func TestOffTargetReasonIsKeptBesideAnotherFailure(t *testing.T) {
	t.Parallel()
	scenario := newestUnreadableScenario(historyOK, false)
	scenario.segments = map[uint32][]uint64{1: {1, 2}, 2: {3, 4, 5}}
	pg := scenario.pgbackrest(t)
	server := scenario.barman(t)
	var own barmancloud.RecoveryPath
	for _, path := range server.Paths {
		if path.TargetTimeline == 1 {
			own = path
		}
	}
	for format, path := range map[string]struct {
		state     evidence.State
		offTarget bool
		reason    string
	}{
		"pgBackRest": {pg.State, pg.OffTarget, pg.Reason},
		"Barman":     {own.State, own.OffTarget, own.Reason},
	} {
		if path.state == evidence.Healthy || path.state == evidence.Warning || !path.offTarget || !strings.Contains(path.reason, "a restore must name timeline 1") || !strings.Contains(path.reason, "branched before the backup reached consistency") {
			t.Errorf("%s path %s off-target %v %q", format, path.state, path.offTarget, path.reason)
		}
	}
}

// TestCoverageSaysNoBackupIsOnTheNewestTimeline: when every backup is left
// behind by the newest timeline, both formats say so rather than report a
// gap or a missing consistency point.
func TestCoverageSaysNoBackupIsOnTheNewestTimeline(t *testing.T) {
	t.Parallel()
	server := newestUnreadableScenario(historyOK, false).barman(t)
	if server.CoverageState != evidence.Warning || server.CoverageReason != barmancloud.NoBackupOnNewestTimeline {
		t.Fatalf("Barman coverage %s %q", server.CoverageState, server.CoverageReason)
	}
	pg := newestUnreadableScenario(historyOK, false).pgbackrest(t)
	report := Report{Backups: []BackupReport{{Path: pg}}}
	if aspect := report.coverageAspect(); aspect.State != evidence.Unknown || aspect.Reason != NoBackupOnNewestTimeline {
		t.Fatalf("pgBackRest coverage %+v", aspect)
	}
	if NoBackupOnNewestTimeline != barmancloud.NoBackupOnNewestTimeline {
		t.Fatal("the two formats word the same fact differently")
	}
}

// ancestorLeftScenario is the round-5 reproduction (P2-1): a backup on
// timeline 1, timeline 2 forked at 0/3000000 and timeline 3 forked from 2
// at 0/5000000. Timeline 2 holds WAL up to segment 9, but a default restore
// (latest: timeline 3) leaves it at 0/5000000: segments 5 to 9 of timeline
// 2 are replayed only by a restore that names targetTLI 2.
func ancestorLeftScenario() timelineScenario {
	return timelineScenario{
		begin: 1, end: 1, endLSN: "0/1000100",
		segments:  map[uint32][]uint64{1: {1, 2}, 2: {3, 4, 5, 6, 7, 8, 9}, 3: {5, 6}},
		histories: map[uint32]string{2: "1\t0/3000000\tfailover\n", 3: "1\t0/3000000\tfailover\n2\t0/5000000\tfailover\n"},
		faults:    map[uint32]historyFault{},
	}
}

// TestAncestorPathStopsWhereTheDefaultRestoreLeaves: a Barman path toward
// an ancestor of the timeline 'latest' resolves to ends at the switch where
// that line leaves it, as pgBackRest's parent legs end at SwitchPosition-1;
// its reason names the targetTLI the later WAL needs. Both formats agree
// on the path toward the newest timeline.
func TestAncestorPathStopsWhereTheDefaultRestoreLeaves(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		scenario timelineScenario
		// frontier position of the Barman path toward each ancestor
		ancestors map[uint32]uint64
		needs     map[uint32]bool // ancestors whose reason names targetTLI
	}{
		{name: "timeline 2 runs past the switch", scenario: ancestorLeftScenario(), ancestors: map[uint32]uint64{1: 2, 2: 4}, needs: map[uint32]bool{2: true}},
		{name: "linear, each parent holds its switch segment", scenario: linearScenario(), ancestors: map[uint32]uint64{1: 4, 2: 6}, needs: map[uint32]bool{1: true, 2: true}},
		{name: "a gap after the switch is not on the path", scenario: func() timelineScenario {
			s := ancestorLeftScenario()
			s.segments[2] = []uint64{3, 4, 7, 8, 9}
			return s
		}(), ancestors: map[uint32]uint64{1: 2, 2: 4}, needs: map[uint32]bool{2: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := test.scenario.barman(t)
			pg := test.scenario.pgbackrest(t)
			if pg.State != evidence.Healthy || pg.TargetTimeline != 3 {
				t.Fatalf("pgBackRest path to %d %s %q", pg.TargetTimeline, pg.State, pg.Reason)
			}
			seen := 0
			for _, path := range server.Paths {
				if path.TargetTimeline == pg.TargetTimeline {
					seen++
					if path.State != pg.State || path.FrontierTimeline != pg.FrontierTimeline || path.FrontierPosition != pg.FrontierPosition {
						t.Errorf("Barman path to %d %s at %d/%d, pgBackRest %s at %d/%d", path.TargetTimeline, path.State, path.FrontierTimeline, path.FrontierPosition, pg.State, pg.FrontierTimeline, pg.FrontierPosition)
					}
					continue
				}
				want, ok := test.ancestors[path.TargetTimeline]
				if !ok {
					t.Errorf("unexpected Barman path to %d: %+v", path.TargetTimeline, path)
					continue
				}
				seen++
				if path.State != evidence.Healthy || path.OffTarget || path.FrontierTimeline != path.TargetTimeline || path.FrontierPosition != want {
					t.Errorf("Barman path to ancestor %d %s off-target %v at %d/%d %q, want healthy at %d/%d", path.TargetTimeline, path.State, path.OffTarget, path.FrontierTimeline, path.FrontierPosition, path.Reason, path.TargetTimeline, want)
				}
				needs := fmt.Sprintf("needs targetTLI %d", path.TargetTimeline)
				if strings.Contains(path.Reason, needs) != test.needs[path.TargetTimeline] {
					t.Errorf("Barman path to ancestor %d reason %q, names %q: want %v", path.TargetTimeline, path.Reason, needs, test.needs[path.TargetTimeline])
				}
			}
			if seen != len(test.ancestors)+1 {
				t.Fatalf("Barman paths %+v", server.Paths)
			}
			if server.CoverageState != evidence.Healthy {
				t.Fatalf("Barman coverage %s %q", server.CoverageState, server.CoverageReason)
			}
		})
	}
}

// parentTailLostScenario is the round-6 reproduction (P2): timeline 2
// forked at 0/5000000 and already has WAL from its switch segment on, but
// timeline 1 is archived only to segment 2. Segments 3 and 4 were written
// by the old primary and are not in the repository; that primary's volume
// may still hold them (CloudNativePG archives them before it rejoins).
func parentTailLostScenario() timelineScenario {
	return timelineScenario{
		begin: 1, end: 1, endLSN: "0/1000100",
		segments:  map[uint32][]uint64{1: {1, 2}, 2: {5, 6, 7}},
		histories: map[uint32]string{2: "1\t0/5000000\tfailover\n"},
		faults:    map[uint32]historyFault{},
	}
}

// assertFormerPrimaryHint fails unless reason says the segment may still be
// on the former primary, within the wire bound, and does not call it lost
// (round 7, P2-1: a DBA who reads "lost" may delete the very volume that
// holds it).
func assertFormerPrimaryHint(t *testing.T, format, reason string) {
	t.Helper()
	if !strings.Contains(reason, "is not in the repository") || !strings.HasSuffix(reason, pgtimeline.FormerPrimaryMayHold) ||
		strings.Contains(reason, "lost") || len(reason) > pgtimeline.MaxReasonBytes {
		t.Errorf("%s reason %q (%d bytes): want the former primary hint whole, no \"lost\"", format, reason, len(reason))
	}
}

// TestLostParentTailAgreesAcrossFormats: a parent timeline's segments
// missing before a switch whose child already has WAL are not in the
// repository. pgBackRest (toward the newest timeline) and every Barman
// path, the ancestor path toward timeline 1 included, read unhealthy, name
// the first missing segment and say the former primary may still hold it;
// none claims the archive frontier.
func TestLostParentTailAgreesAcrossFormats(t *testing.T) {
	t.Parallel()
	scenario := parentTailLostScenario()
	missing := "segment " + crossName(t, 1, 3) + " "
	pg := scenario.pgbackrest(t)
	if pg.State != evidence.Unhealthy || pg.Stop != CoverageGapLimited || !strings.Contains(pg.Reason, missing) {
		t.Fatalf("pgBackRest path to %d %s %s %q", pg.TargetTimeline, pg.State, pg.Stop, pg.Reason)
	}
	assertFormerPrimaryHint(t, "pgBackRest", pg.Reason)
	server := scenario.barman(t)
	if len(server.Paths) != 2 {
		t.Fatalf("Barman paths %+v", server.Paths)
	}
	for _, path := range server.Paths {
		assertFormerPrimaryHint(t, fmt.Sprintf("Barman path to %d", path.TargetTimeline), path.Reason)
		if path.State != pg.State || string(path.Stop) != string(pg.Stop) || !strings.Contains(path.Reason, missing) || strings.Contains(path.Reason, "archive frontier") {
			t.Errorf("Barman path to %d %s %s %q, pgBackRest %s %q", path.TargetTimeline, path.State, path.Stop, path.Reason, pg.State, pg.Reason)
		}
		if path.FrontierTimeline != 1 || path.FrontierPosition != 2 {
			t.Errorf("Barman path to %d frontier %d/%d, want 1/2", path.TargetTimeline, path.FrontierTimeline, path.FrontierPosition)
		}
	}
	if server.CoverageState != evidence.Unhealthy {
		t.Errorf("Barman coverage %s %q", server.CoverageState, server.CoverageReason)
	}
}

// TestFormerPrimaryHintOnEveryParentTail covers the other shapes of a parent
// tail missing before a switch whose child has WAL: the backup's own
// consistency WAL in that tail, and an intermediate timeline with none of
// its WAL. Each stays unhealthy and carries the former primary hint whole;
// a gap the parent's later WAL closes, and a child with no WAL yet, do not.
func TestFormerPrimaryHintOnEveryParentTail(t *testing.T) {
	t.Parallel()
	t.Run("backup consistency WAL in the lost tail", func(t *testing.T) {
		t.Parallel()
		scenario := parentTailLostScenario()
		scenario.end, scenario.endLSN = 3, "0/3000100"
		pg := scenario.pgbackrest(t)
		if pg.State != evidence.Unhealthy || pg.Consistent {
			t.Fatalf("pgBackRest %s consistent %v %q", pg.State, pg.Consistent, pg.Reason)
		}
		assertFormerPrimaryHint(t, "pgBackRest", pg.Reason)
		if !strings.HasPrefix(pg.Reason, "WAL the backup needs to reach consistency is missing: ") || strings.Contains(pg.Reason, "recovers only") {
			t.Errorf("pgBackRest reason %q", pg.Reason)
		}
		for _, path := range scenario.barman(t).Paths {
			if path.State != evidence.Unhealthy {
				t.Errorf("Barman path to %d %s %q", path.TargetTimeline, path.State, path.Reason)
			}
			assertFormerPrimaryHint(t, fmt.Sprintf("Barman path to %d", path.TargetTimeline), path.Reason)
		}
	})
	t.Run("intermediate timeline holds none of its WAL", func(t *testing.T) {
		t.Parallel()
		scenario := timelineScenario{
			begin: 1, end: 1, endLSN: "0/1000100",
			segments: map[uint32][]uint64{1: {1, 2}, 3: {5, 6}},
			histories: map[uint32]string{
				2: "1\t0/3000000\tfailover\n",
				3: "1\t0/3000000\tfailover\n2\t0/5000000\tfailover\n",
			},
			faults: map[uint32]historyFault{},
		}
		pg := scenario.pgbackrest(t)
		if pg.State != evidence.Unhealthy || pg.Stop != CoverageGapLimited || !strings.Contains(pg.Reason, crossName(t, 2, 3)) {
			t.Fatalf("pgBackRest %s %s %q", pg.State, pg.Stop, pg.Reason)
		}
		assertFormerPrimaryHint(t, "pgBackRest", pg.Reason)
	})
	t.Run("a gap the parent's later WAL closes is not a former primary tail", func(t *testing.T) {
		t.Parallel()
		scenario := parentTailLostScenario()
		scenario.segments = map[uint32][]uint64{1: {1, 2, 4}, 2: {5, 6, 7}}
		pg := scenario.pgbackrest(t)
		if pg.State != evidence.Unhealthy || strings.Contains(pg.Reason, pgtimeline.FormerPrimaryMayHold) {
			t.Fatalf("pgBackRest %s %q", pg.State, pg.Reason)
		}
	})
	t.Run("child with no WAL yet stays in flight", func(t *testing.T) {
		t.Parallel()
		scenario := parentTailLostScenario()
		scenario.segments = map[uint32][]uint64{1: {1, 2}}
		pg := scenario.pgbackrest(t)
		if pg.State == evidence.Unhealthy || strings.Contains(pg.Reason, pgtimeline.FormerPrimaryMayHold) {
			t.Fatalf("pgBackRest %s %q", pg.State, pg.Reason)
		}
		for _, path := range scenario.barman(t).Paths {
			if strings.Contains(path.Reason, pgtimeline.FormerPrimaryMayHold) {
				t.Errorf("Barman path to %d %q", path.TargetTimeline, path.Reason)
			}
		}
	})
}
