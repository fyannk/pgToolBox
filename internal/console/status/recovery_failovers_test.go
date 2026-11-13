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

package status

import (
	"math/rand/v2"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/evidence"
	"github.com/fyannk/pgToolBox/internal/console/sweep"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// failoverRings are the readyWalFiles and timeline rings of one
// instance: each reading is a primary sweep at an instant on a timeline,
// with readyWalFiles files (read one second later); a gap is a sweep that
// read the instance as a standby.
type failoverReading struct {
	at       time.Time
	files    float64
	timeline float64
	gap      bool
}

func failoverGathered(primary string, promoted time.Time, rings map[string][]failoverReading) *gathered {
	c := inTreeCluster()
	c.Status.CurrentPrimary = primary
	c.Status.CurrentPrimaryTimestamp = promoted.Format(time.RFC3339Nano)
	g := &gathered{
		cluster: c, swept: sweep.Snapshot{Instances: map[string]sweep.Instance{}},
		readyRings: map[string][]viewmodel.Point{}, timelineRings: map[string][]viewmodel.Point{},
	}
	for name, readings := range rings {
		for _, r := range readings {
			if r.gap {
				g.readyRings[name] = append(g.readyRings[name], viewmodel.Point{At: r.at})
				g.timelineRings[name] = append(g.timelineRings[name], viewmodel.Point{At: r.at})
				continue
			}
			files, tl := r.files, r.timeline
			g.readyRings[name] = append(g.readyRings[name], viewmodel.Point{At: r.at, Value: &files, ReadBy: r.at.Add(time.Second)})
			g.timelineRings[name] = append(g.timelineRings[name], viewmodel.Point{At: r.at, Value: &tl, ReadBy: r.at.Add(time.Second)})
		}
	}
	return g
}

// failoverFacts are the recovery facts of g's failover readings, as
// recoveryFactsOf derives the former archiver, the promotion, the live
// timeline and the instances that rejoined.
func failoverFacts(g *gathered) recoveryFacts {
	promoted := promotedAt(g)
	return recoveryFacts{
		now: now, archiveTimeout: defaultArchiveTimeout, sweepInterval: 30 * time.Second,
		former:   formerArchiverOf(g, promoted, 30*time.Second),
		promoted: promoted, liveTimeline: liveTimelineOf(g, promoted), rejoined: rejoinedSince(g, promoted),
	}
}

func failoverTimeline(g *gathered, paths ...evidence.RecoveryPath) viewmodel.Backups {
	facts := failoverFacts(g)
	ev := timelineFixture{paths: paths}.evidence()
	tl := (&Service{}).recoveryTimeline(ev, viewmodel.BackupSummary{}, nil, facts)
	return viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}
}

func assertNotCovered(t *testing.T, b viewmodel.Backups, end time.Time, offsets ...time.Duration) {
	t.Helper()
	for _, d := range offsets {
		if got := restoreCheck(b, end.Add(-d)); got.Answer == "covered" {
			t.Errorf("end-%s answers covered: %+v", d, got)
		}
	}
}

func bandOn(b viewmodel.Backups, timeline string) *viewmodel.RecoverySegment {
	for i := range b.RecoveryTimeline.Segments {
		s := &b.RecoveryTimeline.Segments[i]
		if s.Params["timeline"] == timeline {
			return s
		}
	}
	return nil
}

// TestFormerEvidenceIsNotPooledAcrossFailovers (round 9, P2, the CNPG
// and PostgreSQL reviewers' probe): A (db-1) wrote timeline 1 with WAL
// waiting in every reading until P1; B (db-2) was primary on timeline 2
// from P1 to P2 and read quiet; C (db-3) is current since P2. The path
// stops on timeline 1 (A's tail). B's quiet sweeps say nothing of A's
// backlog: the end is uncertain for a day, never covered just before it.
func TestFormerEvidenceIsNotPooledAcrossFailovers(t *testing.T) {
	p1, p2 := now.Add(-4*time.Hour), now.Add(-time.Hour)
	g := failoverGathered("db-3", p2, map[string][]failoverReading{
		"db-1": {{at: p1.Add(-20 * time.Minute), files: 5, timeline: 1}, {at: p1.Add(-10 * time.Minute), files: 6, timeline: 1}, {at: p1.Add(time.Minute), gap: true}},
		"db-2": {{at: p1.Add(10 * time.Minute), timeline: 2}, {at: p2.Add(-30 * time.Minute), timeline: 2}, {at: p2.Add(time.Minute), gap: true}},
	})
	receipt := p1.Add(-5 * time.Minute)
	path := parentTailPath(1)
	path.FrontierReceiptAt = &receipt
	b := failoverTimeline(g, path)
	assertNotCovered(t, b, receipt, 2*time.Minute, 10*time.Minute, 20*time.Minute, 2*time.Hour)
	if band := bandOn(b, "1"); band == nil || band.Code != "recovery.segment.formerArchiverUnknown" || !band.From.Equal(receipt.Add(-failureLookback)) {
		t.Errorf("timeline 1's band %+v, want A's backlog from a day before the end", band)
	}
}

// TestFormerEvidenceAfterAToBToA (round 9, P2): A (db-1) wrote timeline
// 1 with WAL waiting, B (db-2) was promoted on timeline 2 and read quiet,
// then A was promoted again (timeline 3) and is current. A's readings of
// its earlier term bound timeline 1; B's never do.
func TestFormerEvidenceAfterAToBToA(t *testing.T) {
	p1, p2 := now.Add(-4*time.Hour), now.Add(-time.Hour)
	receipt := p1.Add(-5 * time.Minute)
	path := parentTailPath(1)
	path.FrontierReceiptAt = &receipt
	b2 := []failoverReading{{at: p1.Add(10 * time.Minute), timeline: 2}, {at: p2.Add(-30 * time.Minute), timeline: 2}, {at: p2.Add(time.Minute), gap: true}}

	t.Run("A's earlier term had WAL waiting", func(t *testing.T) {
		g := failoverGathered("db-1", p2, map[string][]failoverReading{
			"db-1": {{at: p1.Add(-20 * time.Minute), files: 5, timeline: 1}, {at: p1.Add(-10 * time.Minute), files: 6, timeline: 1}, {at: p1.Add(time.Minute), gap: true}, {at: p2.Add(time.Minute), timeline: 3}},
			"db-2": b2,
		})
		b := failoverTimeline(g, path)
		assertNotCovered(t, b, receipt, 2*time.Minute, 10*time.Minute, 20*time.Minute)
		if band := bandOn(b, "1"); band == nil || band.Code != "recovery.segment.formerArchiverUnknown" {
			t.Errorf("timeline 1's band %+v, want A's backlog", band)
		}
	})
	t.Run("A's earlier term read quiet", func(t *testing.T) {
		quietAt := p1.Add(-2 * time.Hour)
		g := failoverGathered("db-1", p2, map[string][]failoverReading{
			"db-1": {{at: quietAt, timeline: 1}, {at: p1.Add(-10 * time.Minute), files: 6, timeline: 1}, {at: p1.Add(time.Minute), gap: true}, {at: p2.Add(time.Minute), timeline: 3}},
			"db-2": b2,
		})
		b := failoverTimeline(g, path)
		if band := bandOn(b, "1"); band == nil || band.Code != "recovery.segment.formerArchiverSince" || !band.From.Equal(quietAt.Add(-defaultArchiveTimeout)) {
			t.Errorf("timeline 1's band %+v, want A's own quiet sweep", band)
		}
		assertNotCovered(t, b, receipt, 2*time.Minute, time.Hour, 100*time.Minute)
	})
}

// TestFormerEvidenceOverTwoFailovers (round 9, P2): timelines 1, 2 and 3
// (A, then B, then C). The repository's WAL stops on timeline 1 (a hole
// before the first switch) and on timeline 2 (B's tail): each end is
// bounded by its own timeline's archiver only.
func TestFormerEvidenceOverTwoFailovers(t *testing.T) {
	p1, p2 := now.Add(-5*time.Hour-30*time.Minute), now.Add(-90*time.Minute)
	bQuiet := now.Add(-3 * time.Hour)
	g := failoverGathered("db-3", p2, map[string][]failoverReading{
		"db-1": {{at: p1.Add(-20 * time.Minute), files: 5, timeline: 1}, {at: p1.Add(-10 * time.Minute), files: 6, timeline: 1}, {at: p1.Add(time.Minute), gap: true}},
		"db-2": {{at: p1.Add(10 * time.Minute), files: 4, timeline: 2}, {at: bQuiet, timeline: 2}, {at: p2.Add(-10 * time.Minute), files: 3, timeline: 2}, {at: p2.Add(time.Minute), gap: true}},
		"db-3": {{at: p2.Add(time.Minute), timeline: 3}},
	})
	consistent := true
	first := tlPath("b1", 48*time.Hour, 6*time.Hour, evidencev1alpha1.CoverageGapLimited)
	first.TargetTimeline, first.StartTimeline, first.FrontierTimeline, first.Consistent = 3, pu64(1), pu64(1), &consistent
	second := tlPath("b2", 5*time.Hour, 2*time.Hour, evidencev1alpha1.CoverageGapLimited)
	second.TargetTimeline, second.StartTimeline, second.FrontierTimeline, second.Consistent = 3, pu64(2), pu64(2), &consistent
	b := failoverTimeline(g, first, second)
	end1, end2 := now.Add(-6*time.Hour), now.Add(-2*time.Hour)
	assertNotCovered(t, b, end1, 2*time.Minute, 20*time.Minute, 2*time.Hour)
	assertNotCovered(t, b, end2, 2*time.Minute, 30*time.Minute)
	if band := bandOn(b, "1"); band == nil || band.Code != "recovery.segment.formerArchiverUnknown" {
		t.Errorf("timeline 1's band %+v, want A's backlog (B's quiet sweep is timeline 2's)", band)
	}
	if band := bandOn(b, "2"); band == nil || band.Code != "recovery.segment.formerArchiverSince" || !band.From.Equal(bQuiet.Add(-defaultArchiveTimeout)) {
		t.Errorf("timeline 2's band %+v, want B's quiet sweep", band)
	}
	if got := restoreCheck(b, now.Add(-4*time.Hour)); got.Answer != "covered" {
		t.Errorf("before B's quiet sweep on timeline 2: %s (%s), want covered", got.Answer, got.Text)
	}
}

// TestFormerEvidenceOfOtherTimelinesChangesNothing is the property behind
// round 9's P2, run on real rings (round 10, PG P3): the former primary's
// readyWalFiles and timeline rings go through formerArchiverOf and
// timelineAt as a sweep would keep them. Readings of
// the archivers of other timelines, however many and wherever they fall
// (another instance's, or the same instance's on a later term), leave the
// bar of a timeline 1 end exactly as that timeline's own readings draw
// it; and the bar matches the rule exactly: covered before the newest
// band start that timeline's readings give, when that start is before the
// end, and nowhere in the day before the end otherwise (a band they would
// invert is never coverage).
func TestFormerEvidenceOfOtherTimelinesChangesNothing(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 2))
	path := parentTailPath(1)
	receipt := *path.FrontierReceiptAt
	promoted := now.Add(-time.Hour)
	svc := &Service{}
	type reading struct {
		at       time.Time
		files    float64
		timeline float64
	}
	// series is a random term's readings on one timeline, oldest first:
	// readyWalFiles 0 a third of the time.
	series := func(timeline float64, taken map[int64]bool) []reading {
		var out []reading
		for range r.IntN(7) {
			at := receipt.Add(-26 * time.Hour).Add(time.Duration(r.Int64N(int64(promoted.Sub(receipt.Add(-26*time.Hour))/time.Second))) * time.Second)
			if taken[at.UnixNano()] {
				continue
			}
			taken[at.UnixNano()] = true
			out = append(out, reading{at: at, timeline: timeline})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
		for i := range out {
			if r.IntN(3) > 0 {
				out[i].files = float64(1 + r.IntN(6))
			}
		}
		return out
	}
	build := func(instances map[string][]reading) *gathered {
		c := inTreeCluster()
		c.Status.CurrentPrimary = "db-2"
		c.Status.CurrentPrimaryTimestamp = promoted.Format(time.RFC3339Nano)
		g := &gathered{
			cluster: c,
			swept: sweep.Snapshot{SweptAt: now, Interval: 30 * time.Second, Instances: map[string]sweep.Instance{
				// The current primary, on timeline 5: its reading gives the
				// live timeline.
				"db-2": {State: viewmodel.StatusReadOK, Reading: &instancestatus.Reading{IsPrimary: true, CurrentWAL: "000000050000000000000007", CurrentLSN: "0/7000100", ObservedAt: now}},
			}},
			readyRings: map[string][]viewmodel.Point{}, timelineRings: map[string][]viewmodel.Point{},
		}
		for name, readings := range instances {
			sorted := append([]reading(nil), readings...)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i].at.Before(sorted[j].at) })
			for _, x := range sorted {
				files, tl := x.files, x.timeline
				by := x.at.Add(time.Second)
				g.readyRings[name] = append(g.readyRings[name], viewmodel.Point{At: x.at, Value: &files, ReadBy: by})
				g.timelineRings[name] = append(g.timelineRings[name], viewmodel.Point{At: x.at, Value: &tl, ReadBy: by})
			}
		}
		return g
	}
	draw := func(g *gathered) viewmodel.RecoveryTimeline {
		return svc.recoveryTimeline(timelineFixture{paths: []evidence.RecoveryPath{path}}.evidence(), viewmodel.BackupSummary{}, nil, failoverFacts(g))
	}
	margin := recoveryFacts{archiveTimeout: defaultArchiveTimeout}.frontierMargin()
	evidenced, inverted := 0, 0
	for i := range 400 {
		taken := map[int64]bool{}
		own := series(1, taken)
		other := map[string][]reading{"db-1": append(append([]reading(nil), own...), series(4, taken)...), "db-3": series(2, taken), "db-4": series(3, taken)}
		ownG, mixedG := build(map[string][]reading{"db-1": own}), build(other)
		want, got := draw(ownG), draw(mixedG)
		if !reflect.DeepEqual(want.Segments, got.Segments) {
			t.Fatalf("case %d: other timelines' readings changed the bar:\n%+v\nwant\n%+v", i, got.Segments, want.Segments)
		}
		// The rule, from timeline 1's own readings: the newest band start
		// a read by the scan gives.
		read, quiet := formerArchiverOf(ownG, promoted, 30*time.Second).on(1)
		var start *time.Time
		later := func(at time.Time) {
			if start == nil || at.After(*start) {
				start = &at
			}
		}
		for _, q := range quiet {
			if !q.by.After(now) {
				later(q.at.Add(-defaultArchiveTimeout))
			}
		}
		if read && start != nil {
			evidenced++
			if !start.Before(receipt) {
				inverted++
			}
		}
		b := viewmodel.Backups{RecoveryTimeline: got, GeneratedAt: now}
		for range 12 {
			at := receipt.Add(-time.Duration(1+r.Int64N(int64(failureLookback/time.Second)-1)) * time.Second)
			covered := restoreCheck(b, at).Answer == "covered"
			expect := at.Before(receipt.Add(-margin)) && read && start != nil && start.Before(receipt) && at.Before(*start)
			if covered != expect {
				t.Fatalf("case %d: %s before the end covered %v, want %v (band start %v, own readings %+v)", i, receipt.Sub(at), covered, expect, start, own)
			}
		}
	}
	if evidenced < 100 || inverted < 20 {
		t.Fatalf("the generator exercised %d cases with evidence, %d of them inverted: too few", evidenced, inverted)
	}
}

// TestBacklogReadsOnlyTheCurrentTerm (CNPG P3-c): the backlog rule skips
// gaps, so an instance promoted again would join its earlier term's
// readings (WAL waiting before it was demoted) to the current one across
// its time as a standby. Only the sweeps since the promotion count: one
// reading at the threshold since then is not a backlog.
func TestBacklogReadsOnlyTheCurrentTerm(t *testing.T) {
	promoted := now.Add(-time.Hour)
	read := now.Add(-10 * time.Second)
	c := inTreeCluster()
	c.Status.CurrentPrimary = "db-1"
	c.Status.CurrentPrimaryTimestamp = promoted.Format(time.RFC3339Nano)
	files := func(v float64) *float64 { return &v }
	g := &gathered{
		cluster: c,
		swept: sweep.Snapshot{SweptAt: read, Interval: 30 * time.Second, Instances: map[string]sweep.Instance{
			"db-1": {State: viewmodel.StatusReadOK, Reading: &instancestatus.Reading{IsPrimary: true, ReadyWALFiles: 9, ObservedAt: read}},
		}},
		readyRings: map[string][]viewmodel.Point{"db-1": {
			{At: promoted.Add(-3 * time.Hour), Value: files(8)},
			{At: promoted.Add(-2 * time.Hour), Value: files(9)},
			{At: promoted.Add(-time.Hour)},
			{At: promoted.Add(-time.Minute)},
		}},
	}
	b := cnpgstatus.ArchiveBacklogOf(g.cluster, primaryArchiver(g))
	if b.Behind() {
		t.Fatalf("backlog %+v, want the current term's single reading not judged a backlog", b)
	}
	c.Status.CurrentPrimaryTimestamp = promoted.Add(-4 * time.Hour).Format(time.RFC3339Nano)
	if b := cnpgstatus.ArchiveBacklogOf(g.cluster, primaryArchiver(g)); !b.Behind() {
		t.Fatalf("readings of the same term at the threshold: %+v, want a backlog", b)
	}
}

// TestEarlierReadyReadsAFirstArchive (CNPG P3-b): a sweep that read the
// primary with nothing archived yet (sweep.ArchivedNone) is carried as
// such, never as a time.
func TestEarlierReadyReadsAFirstArchive(t *testing.T) {
	ready, archived := 6.0, float64(sweep.ArchivedNone)
	at := now.Add(-time.Minute)
	g := &gathered{
		cluster:       inTreeCluster(),
		readyRings:    map[string][]viewmodel.Point{"db-2": {{At: at, Value: &ready}}},
		archivedRings: map[string][]viewmodel.Point{"db-2": {{At: at, Value: &archived}}},
	}
	got := earlierReady(g, "db-2", time.Time{}, now)
	if len(got) != 1 || !got[0].ArchivedNone || got[0].LastArchivedAt != nil {
		t.Fatalf("earlier samples %+v, want one read with nothing archived", got)
	}
}

// TestGoldenFailoverFormerPrimaryAdvice is the failover bar a DBA reads
// after A (db-1, timeline 1) crashed with WAL waiting and B (db-2) was
// promoted: the repository's WAL stops on timeline 1 at a segment the
// agent says the former primary may still hold. The bar names db-1 in
// the advice not to delete it, and A's backlog makes the day before that
// end uncertain.
func TestGoldenFailoverFormerPrimaryAdvice(t *testing.T) {
	p1 := now.Add(-4 * time.Hour)
	g := failoverGathered("db-2", p1, map[string][]failoverReading{
		"db-1": {{at: p1.Add(-20 * time.Minute), files: 5, timeline: 1}, {at: p1.Add(-10 * time.Minute), files: 6, timeline: 1}, {at: p1.Add(time.Minute), gap: true}},
		"db-2": {{at: p1.Add(-10 * time.Minute), gap: true}, {at: p1.Add(10 * time.Minute), timeline: 2}},
	})
	receipt := p1.Add(-5 * time.Minute)
	path := parentTailPath(1)
	path.FrontierReceiptAt = &receipt
	path.Reason = evidencev1alpha1.Reason{Code: "coverage-path-unhealthy", Message: "segment 000000010000000000000042 before the timeline switch is not in the repository; " + evidencev1alpha1.FormerPrimaryMayHold}
	b := failoverTimeline(g, path)
	golden(t, "recovery-failover-former-primary", b.RecoveryTimeline)
}
