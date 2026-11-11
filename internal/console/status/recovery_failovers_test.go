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

func failoverTimeline(g *gathered, paths ...evidence.RecoveryPath) viewmodel.Backups {
	f := formerArchiverOf(g, promotedAt(g), defaultArchiveTimeout, 30*time.Second)
	facts := recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout, sweepInterval: 30 * time.Second, former: f}
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
	if band := bandOn(b, "1"); band == nil || band.Code != "recovery.segment.formerArchiverBacklog" || !band.From.Equal(receipt.Add(-failureLookback)) {
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
		if band := bandOn(b, "1"); band == nil || band.Code != "recovery.segment.formerArchiverBacklog" {
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
	if band := bandOn(b, "1"); band == nil || band.Code != "recovery.segment.formerArchiverBacklog" {
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
// round 9's P2: readings of the archivers of other timelines, however many
// and wherever they fall, leave the bar of a timeline 1 end exactly as
// that timeline's own readings draw it, and a band they would invert is
// never coverage.
func TestFormerEvidenceOfOtherTimelinesChangesNothing(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 2))
	receipt := now.Add(-3 * time.Hour)
	svc := &Service{}
	draw := func(f formerArchiver) viewmodel.RecoveryTimeline {
		facts := recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout, sweepInterval: 30 * time.Second, former: f}
		return svc.recoveryTimeline(timelineFixture{paths: []evidence.RecoveryPath{parentTailPath(1)}}.evidence(), viewmodel.BackupSummary{}, nil, facts)
	}
	at := func() time.Time { return receipt.Add(time.Duration(r.Int64N(int64(30*time.Hour))) - 26*time.Hour) }
	for i := range 300 {
		own := formerArchiver{read: map[uint64]bool{}}
		if r.IntN(3) > 0 {
			own.read[1] = true
			for range r.IntN(3) {
				q := at()
				own.quiet = append(own.quiet, quietSweep{at: q, by: q.Add(time.Second), timeline: 1})
			}
			for range r.IntN(3) {
				q := at()
				own.bounds = append(own.bounds, archivedBound{readAt: q, before: q.Add(-time.Duration(r.Int64N(int64(2 * time.Hour)))), timeline: 1})
			}
		}
		mixed := formerArchiver{read: map[uint64]bool{}, quiet: append([]quietSweep(nil), own.quiet...), bounds: append([]archivedBound(nil), own.bounds...)}
		for tl := range own.read {
			mixed.read[tl] = true
		}
		for range 1 + r.IntN(4) {
			tl := uint64(2 + r.IntN(3))
			mixed.read[tl] = true
			q := at()
			mixed.quiet = append(mixed.quiet, quietSweep{at: q, by: q.Add(time.Second), timeline: tl})
			mixed.bounds = append(mixed.bounds, archivedBound{readAt: q, before: q.Add(-time.Minute), timeline: tl})
		}
		want, got := draw(own), draw(mixed)
		if !reflect.DeepEqual(want.Segments, got.Segments) {
			t.Fatalf("case %d: other timelines' readings changed the bar:\n%+v\nwant\n%+v", i, got.Segments, want.Segments)
		}
		b := viewmodel.Backups{RecoveryTimeline: got, GeneratedAt: now}
		if c := restoreCheck(b, receipt.Add(-time.Minute)); c.Answer == "covered" {
			t.Fatalf("case %d: a minute before the end of timeline 1 answers covered: %+v", i, own)
		}
	}
}

// TestBacklogReadsOnlyTheCurrentTerm (CNPG P3-c): the backlog rule skips
// gaps, so an instance promoted again would join its earlier term's
// readings (WAL waiting before it was demoted) to the current one across
// its time as a standby. Only the sweeps since the promotion count: one
// reading at the threshold since then is unconfirmed, not a backlog.
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
	if b.Behind() || b.Unconfirmed != 9 {
		t.Fatalf("backlog %+v, want the current term's single reading unconfirmed", b)
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
