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
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/evidence"
	"github.com/fyannk/pgToolBox/internal/console/sweep"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// parentTailPath is a path toward timeline 2 that stops at a confirmed
// gap on timeline 1, receipt 3 h ago: after a crash failover, the former
// primary's unarchived tail.
func parentTailPath(timeline uint64) evidence.RecoveryPath {
	consistent := true
	p := tlPath("b1", 48*time.Hour, 3*time.Hour, evidencev1alpha1.CoverageGapLimited)
	p.StartTimeline, p.FrontierTimeline, p.Consistent = pu64(1), pu64(timeline), &consistent
	return p
}

// TestFormerPrimaryBacklogBoundsTheParentTail (round 8, PG P2-2, probe
// B): the paths stop on the parent timeline at the receipt R of the last
// archived segment. The WAL before R was archived by the former primary,
// whose backlog the current primary's readings do not show: without a
// reading of it from before the switch, a day before R is uncertain; with
// one that read nothing waiting, from that sweep less one
// archive_timeout; with only readings of WAL waiting, a day. A gap on the
// current timeline is the current primary's and is not widened.
func TestFormerPrimaryBacklogBoundsTheParentTail(t *testing.T) {
	svc := &Service{}
	receipt := now.Add(-3 * time.Hour)
	base := recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout, sweepInterval: 30 * time.Second}
	quietAt := func(at time.Time) formerArchiver {
		return formerArchiver{read: true, quiet: []quietSweep{{at: at, by: at.Add(time.Second)}}}
	}
	for _, tc := range []struct {
		name     string
		timeline uint64
		former   formerArchiver
		code     string
		from     time.Time
		// covered and not are offsets before R a restore check answers
		// covered or not.
		covered, not []time.Duration
	}{
		{
			name: "probe B: no reading of the former primary", timeline: 1,
			code: "recovery.segment.formerArchiverUnknown", from: receipt.Add(-failureLookback),
			not: []time.Duration{20 * time.Minute, time.Hour, 23 * time.Hour},
		},
		{
			name: "the former primary had WAL waiting in every reading", timeline: 1, former: formerArchiver{read: true},
			code: "recovery.segment.formerArchiverBacklog", from: receipt.Add(-failureLookback),
			not: []time.Duration{20 * time.Minute, time.Hour},
		},
		{
			name: "the former primary last had nothing waiting 2 h before R", timeline: 1, former: quietAt(receipt.Add(-2 * time.Hour)),
			code: "recovery.segment.formerArchiverSince", from: receipt.Add(-2*time.Hour - defaultArchiveTimeout),
			not: []time.Duration{20 * time.Minute, time.Hour, 2 * time.Hour}, covered: []time.Duration{3 * time.Hour},
		},
		{
			name: "a bound from the former primary's positions", timeline: 1,
			former: formerArchiver{read: true, bounds: []archivedBound{{readAt: receipt.Add(-time.Hour), before: receipt.Add(-90 * time.Minute)}}},
			code:   "recovery.segment.formerArchiverBound", from: receipt.Add(-90 * time.Minute),
			not: []time.Duration{20 * time.Minute, time.Hour}, covered: []time.Duration{2 * time.Hour},
		},
		{
			name: "a quiet read that returned after the scan does not count", timeline: 1,
			former: formerArchiver{read: true, quiet: []quietSweep{{at: now.Add(-10 * time.Second), by: now.Add(time.Second)}}},
			code:   "recovery.segment.formerArchiverBacklog", from: receipt.Add(-failureLookback),
			not: []time.Duration{20 * time.Minute, time.Hour},
		},
		{
			name: "a gap on the current timeline is the current primary's", timeline: 2,
			covered: []time.Duration{20 * time.Minute, time.Hour},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := base
			facts.former = tc.former
			ev := timelineFixture{paths: []evidence.RecoveryPath{parentTailPath(tc.timeline)}}.evidence()
			tl := svc.recoveryTimeline(ev, viewmodel.BackupSummary{}, nil, facts)
			band := edgeBand(tl, "recovery.segment.formerArchiverUnknown", "recovery.segment.formerArchiverBacklog",
				"recovery.segment.formerArchiverSince", "recovery.segment.formerArchiverBound")
			switch {
			case tc.code == "" && band != nil:
				t.Fatalf("a former-primary band on the current timeline: %+v", band)
			case tc.code != "" && (band == nil || band.Code != tc.code || !band.From.Equal(tc.from) || band.Params["timeline"] != "1"):
				t.Fatalf("band %+v, want %s from %s", band, tc.code, tc.from)
			}
			b := viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}
			for _, d := range tc.not {
				if got := restoreCheck(b, receipt.Add(-d)); got.Answer == "covered" {
					t.Errorf("R-%s answers covered: %+v", d, got)
				}
			}
			for _, d := range tc.covered {
				if got := restoreCheck(b, receipt.Add(-d)); got.Answer != "covered" {
					t.Errorf("R-%s answers %s (%s), want covered", d, got.Answer, got.Text)
				}
			}
		})
	}
}

// TestFormerArchiverOfReadsOnlyThePreviousTerm: the former primaries'
// facts come from the sweeps before the promotion (status
// currentPrimaryTimestamp), from every instance but the current primary
// whose ring holds a primary reading then; a standby's ring (gaps only),
// a reading after the promotion and an unparsed promotion time are none.
func TestFormerArchiverOfReadsOnlyThePreviousTerm(t *testing.T) {
	promoted := now.Add(-time.Hour)
	c := inTreeCluster()
	c.Status.CurrentPrimaryTimestamp = promoted.Format(time.RFC3339Nano)
	point := func(at time.Time, v float64) viewmodel.Point { return viewmodel.Point{At: at, Value: &v} }
	g := &gathered{cluster: c, swept: sweep.Snapshot{Instances: map[string]sweep.Instance{}}, readyRings: map[string][]viewmodel.Point{
		"db-1": {point(promoted.Add(-2*time.Hour), 0), point(promoted.Add(-time.Minute), 4), {At: promoted.Add(time.Minute)}},
		"db-2": {point(promoted.Add(-3*time.Hour), 0), point(now, 0)},
		"db-3": {{At: promoted.Add(-time.Minute)}, point(promoted.Add(time.Minute), 0)},
	}}
	f := formerArchiverOf(g, promotedAt(g), defaultArchiveTimeout, 30*time.Second)
	if !f.read || len(f.quiet) != 1 || !f.quiet[0].at.Equal(promoted.Add(-2*time.Hour)) || !f.quiet[0].by.Equal(promoted.Add(-2*time.Hour+30*time.Second)) {
		t.Fatalf("former archiver %+v, want db-1's quiet sweep 3 h ago, read by one interval later", f)
	}
	c.Status.CurrentPrimaryTimestamp = ""
	if f := formerArchiverOf(g, promotedAt(g), defaultArchiveTimeout, 30*time.Second); f.read {
		t.Fatalf("an unknown promotion time read a former archiver: %+v", f)
	}
}

// TestRingReadTimes (CNPG P3-4, Go M4): a quiet sweep counts by the scan
// only when its read returned by then (the ring's ReadBy, else the
// sweep's start plus one interval), and the position and count rings are
// matched by instant, whatever the time's location.
func TestRingReadTimes(t *testing.T) {
	scan := now.Add(-time.Minute)
	v := 0.0
	ring := []viewmodel.Point{{At: scan.Add(-10 * time.Second), Value: &v, ReadBy: scan.Add(5 * time.Second)}}
	f := recoveryFacts{now: now, quiet: quietSweeps(ring, time.Time{}, time.Time{}, 30*time.Second)}
	if q := f.quietBy(&scan); q != nil {
		t.Fatalf("a read that returned after the scan counts as quiet by it: %+v", q)
	}
	ring[0].ReadBy = time.Time{}
	f.quiet = quietSweeps(ring, time.Time{}, time.Time{}, 30*time.Second)
	if q := f.quietBy(&scan); q != nil {
		t.Fatalf("with no read time, the sweep's start plus one interval is after the scan: %+v", q)
	}
	ring[0].ReadBy = scan.Add(-5 * time.Second)
	f.quiet = quietSweeps(ring, time.Time{}, time.Time{}, 30*time.Second)
	if q := f.quietBy(&scan); q == nil || !q.at.Equal(scan.Add(-10*time.Second)) {
		t.Fatalf("a read that returned before the scan: %+v", q)
	}

	read := now.Add(-30 * time.Second)
	r, ready, lsns := busyPrimary(read, 3)
	zone := time.FixedZone("elsewhere", 3600)
	for i := range lsns {
		lsns[i].At = lsns[i].At.In(zone)
	}
	g := &gathered{
		cluster:    inTreeCluster(),
		swept:      sweep.Snapshot{SweptAt: now, Interval: 30 * time.Second, Instances: map[string]sweep.Instance{"db-2": {State: viewmodel.StatusReadOK, Reading: &r}}},
		readyRings: map[string][]viewmodel.Point{"db-2": ready},
	}
	if b := archivedBounds(g, "db-2", lsns, time.Time{}, time.Time{}, defaultArchiveTimeout, 30*time.Second); len(b) == 0 {
		t.Fatal("rings at the same instants in another location are not matched")
	}
}
