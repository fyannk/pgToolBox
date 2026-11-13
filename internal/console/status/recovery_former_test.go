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
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"

	"github.com/fyannk/pgToolBox/internal/console/evidence"
	"github.com/fyannk/pgToolBox/internal/console/sweep"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
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
	tl1 := map[uint64]bool{1: true}
	quietAt := func(at time.Time) formerArchiver {
		return formerArchiver{read: tl1, quiet: []quietSweep{{at: at, by: at.Add(time.Second), timeline: 1}}}
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
			name: "the former primary had WAL waiting in every reading", timeline: 1, former: formerArchiver{read: tl1},
			code: "recovery.segment.formerArchiverUnknown", from: receipt.Add(-failureLookback),
			not: []time.Duration{20 * time.Minute, time.Hour},
		},
		{
			name: "the former primary last had nothing waiting 2 h before R", timeline: 1, former: quietAt(receipt.Add(-2 * time.Hour)),
			code: "recovery.segment.formerArchiverSince", from: receipt.Add(-2*time.Hour - defaultArchiveTimeout),
			not: []time.Duration{20 * time.Minute, time.Hour, 2 * time.Hour}, covered: []time.Duration{3 * time.Hour},
		},
		{
			// Round 9, P2: the primary of timeline 2 read quiet; it says
			// nothing of timeline 1's archiver.
			name: "readings of another timeline's archiver are no evidence", timeline: 1,
			former: formerArchiver{
				read:  map[uint64]bool{2: true},
				quiet: []quietSweep{{at: receipt.Add(time.Hour), by: receipt.Add(time.Hour + time.Second), timeline: 2}},
			},
			code: "recovery.segment.formerArchiverUnknown", from: receipt.Add(-failureLookback),
			not: []time.Duration{2 * time.Minute, 20 * time.Minute, time.Hour},
		},
		{
			// Round 9, P2: a band that does not end before the end (a quiet
			// sweep or a bound after it) is no evidence for that end, never
			// coverage.
			name: "an inverted quiet band is unknown", timeline: 1, former: quietAt(receipt.Add(time.Hour)),
			code: "recovery.segment.formerArchiverUnknown", from: receipt.Add(-failureLookback),
			not: []time.Duration{2 * time.Minute, 20 * time.Minute, time.Hour},
		},
		{
			name: "a quiet read that returned after the scan does not count", timeline: 1,
			former: formerArchiver{read: tl1, quiet: []quietSweep{{at: now.Add(-10 * time.Second), by: now.Add(time.Second), timeline: 1}}},
			code:   "recovery.segment.formerArchiverUnknown", from: receipt.Add(-failureLookback),
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
			band := edgeBand(tl, "recovery.segment.formerArchiverUnknown", "recovery.segment.formerArchiverSince")
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
// currentPrimaryTimestamp) that read an instance as primary on a known
// timeline; a standby's ring (gaps only), a reading after the promotion,
// a reading without a timeline and an unparsed promotion time are none.
func TestFormerArchiverOfReadsOnlyThePreviousTerm(t *testing.T) {
	promoted := now.Add(-time.Hour)
	c := inTreeCluster()
	c.Status.CurrentPrimaryTimestamp = promoted.Format(time.RFC3339Nano)
	point := func(at time.Time, v float64) viewmodel.Point { return viewmodel.Point{At: at, Value: &v} }
	g := &gathered{
		cluster: c, swept: sweep.Snapshot{Instances: map[string]sweep.Instance{}},
		readyRings: map[string][]viewmodel.Point{
			"db-1": {point(promoted.Add(-2*time.Hour), 0), point(promoted.Add(-time.Minute), 4), {At: promoted.Add(time.Minute)}},
			"db-2": {point(promoted.Add(-3*time.Hour), 0), point(now, 0)},
			"db-3": {{At: promoted.Add(-time.Minute)}, point(promoted.Add(time.Minute), 0)},
		},
		timelineRings: map[string][]viewmodel.Point{
			"db-1": {point(promoted.Add(-2*time.Hour), 1), point(promoted.Add(-time.Minute), 1), {At: promoted.Add(time.Minute)}},
			// db-2's reading 3 h before carries no timeline (a ring
			// restored from an older snapshot): no evidence.
			"db-2": {{At: promoted.Add(-3 * time.Hour)}, point(now, 2)},
			"db-3": {{At: promoted.Add(-time.Minute)}, point(promoted.Add(time.Minute), 2)},
		},
	}
	f := formerArchiverOf(g, promotedAt(g), 30*time.Second)
	read, quiet := f.on(1)
	if !read || len(quiet) != 1 || !quiet[0].at.Equal(promoted.Add(-2*time.Hour)) || !quiet[0].by.Equal(promoted.Add(-2*time.Hour+30*time.Second)) {
		t.Fatalf("former archiver %+v, want db-1's quiet sweep 3 h ago on timeline 1, read by one interval later", f)
	}
	if read, _ := f.on(2); read || len(f.quiet) != 1 {
		t.Fatalf("readings after the promotion or without a timeline count: %+v", f)
	}
	c.Status.CurrentPrimaryTimestamp = ""
	if f := formerArchiverOf(g, promotedAt(g), 30*time.Second); len(f.read) != 0 {
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
}

// TestEarlierReadyCarriesTheInstanceClock (CNPG P3-3): each earlier
// readyWalFiles sample carries the last_archived_time the same sweep read
// (the instance's clock), matched by instant, so the steady-stall rule
// compares the instance's clock with itself.
func TestEarlierReadyCarriesTheInstanceClock(t *testing.T) {
	ready := ring(now, 6, 6, 6)
	archived := ring(now, float64(now.Add(time.Hour).UnixMilli()), -1, float64(now.Add(time.Hour).UnixMilli()))
	for i := range archived {
		archived[i].At = archived[i].At.In(time.FixedZone("elsewhere", -7200))
	}
	g := &gathered{cluster: inTreeCluster(), readyRings: map[string][]viewmodel.Point{"db-2": ready}, archivedRings: map[string][]viewmodel.Point{"db-2": archived}}
	got := earlierReady(g, "db-2", time.Time{}, now)
	if len(got) != 3 || got[0].LastArchivedAt == nil || !got[0].LastArchivedAt.Equal(now.Add(time.Hour).Truncate(time.Millisecond)) || got[1].LastArchivedAt != nil {
		t.Fatalf("earlier samples %+v", got)
	}
}

// TestArchivingWalOnlyOnTheArchiver (CNPG P3-2): a replica cluster's
// standbys run with archive_mode always, so their isArchivingWAL reads
// true, but only the designated primary (current or target primary)
// archives: the others' archivingWal is not served.
func TestArchivingWalOnlyOnTheArchiver(t *testing.T) {
	c := inTreeCluster()
	enabled := true
	c.Spec.ReplicaCluster = &cnpgv1.ReplicaClusterConfiguration{Enabled: &enabled, Source: "origin"}
	c.Status.CurrentPrimary, c.Status.TargetPrimary = "dr-1", "dr-1"
	standby := instancestatus.Reading{IsArchivingWAL: true}
	g := &gathered{cluster: c, swept: sweep.Snapshot{Instances: map[string]sweep.Instance{
		"dr-1": {State: viewmodel.StatusReadOK, Reading: &standby},
		"dr-2": {State: viewmodel.StatusReadOK, Reading: &standby},
	}}}
	svc := &Service{sweep: fakeSweep{}}
	if _, live := svc.statusRead(g, "dr-1", ""); live == nil || !live.ArchivingWAL {
		t.Errorf("the designated primary's archiving: %+v", live)
	}
	if _, live := svc.statusRead(g, "dr-2", ""); live == nil || live.ArchivingWAL {
		t.Errorf("a standby of a replica cluster reads as archiving: %+v", live)
	}
	// Round 9, CNPG P3-a: mid-switchover the target primary does not
	// archive yet, and a pod that still reports itself primary but is
	// not status.currentPrimary is refused by the instance manager.
	c.Spec.ReplicaCluster = nil
	c.Status.CurrentPrimary, c.Status.TargetPrimary = "dr-1", "dr-2"
	stale := instancestatus.Reading{IsArchivingWAL: true, IsPrimary: true}
	g.swept.Instances["dr-3"] = sweep.Instance{State: viewmodel.StatusReadOK, Reading: &stale}
	if _, live := svc.statusRead(g, "dr-2", ""); live == nil || live.ArchivingWAL {
		t.Errorf("the target primary of a switchover reads as archiving: %+v", live)
	}
	if _, live := svc.statusRead(g, "dr-3", ""); live == nil || live.ArchivingWAL {
		t.Errorf("a self-reported primary that is not currentPrimary reads as archiving: %+v", live)
	}
	if _, live := svc.statusRead(g, "dr-1", ""); live == nil || !live.ArchivingWAL {
		t.Errorf("currentPrimary's archiving: %+v", live)
	}
}

// TestMissingWALSaysTheFormerPrimaryMayHoldIt (round 8, PG plan 5): when
// the agent's path stops at a segment a former primary may still hold
// (its reason ends with evidencev1alpha1.FormerPrimaryMayHold), the bar's
// missing-WAL segment and its reason say not to delete that instance or
// its volume, for the tail and for a hole; a gap without it says only
// that WAL is missing.
func TestMissingWALSaysTheFormerPrimaryMayHoldIt(t *testing.T) {
	svc := &Service{}
	facts := recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout, sweepInterval: 30 * time.Second}
	held := func(p evidence.RecoveryPath) evidence.RecoveryPath {
		p.Reason = evidencev1alpha1.Reason{Code: "coverage-path-unhealthy", Message: "segment 000000010000000000000042 before the timeline switch is not in the repository; " + evidencev1alpha1.FormerPrimaryMayHold}
		return p
	}
	codes := func(tl viewmodel.RecoveryTimeline) map[string]bool {
		out := map[string]bool{}
		for _, s := range tl.Segments {
			out[s.Code] = true
		}
		for _, r := range tl.Reasons {
			out[r.Code] = true
		}
		return out
	}
	tail := svc.recoveryTimeline(timelineFixture{paths: []evidence.RecoveryPath{held(parentTailPath(1))}}.evidence(), viewmodel.BackupSummary{}, nil, facts)
	if c := codes(tail); !c["recovery.segment.missingAfterLatestFormerPrimary"] || !c["recovery.reason.missingAfterLatestFormerPrimary"] || c["recovery.segment.missingAfterLatest"] {
		t.Errorf("tail codes %v", c)
	}
	consistent := true
	after := tlPath("b2", 2*time.Hour, time.Minute, evidencev1alpha1.CoverageFrontier)
	after.Consistent = &consistent
	hole := svc.recoveryTimeline(timelineFixture{paths: []evidence.RecoveryPath{held(parentTailPath(1)), after}}.evidence(), viewmodel.BackupSummary{}, nil, facts)
	if c := codes(hole); !c["recovery.segment.missingWALFormerPrimary"] || c["recovery.segment.missingWAL"] {
		t.Errorf("hole codes %v", c)
	}
	plain := svc.recoveryTimeline(timelineFixture{paths: []evidence.RecoveryPath{parentTailPath(1)}}.evidence(), viewmodel.BackupSummary{}, nil, facts)
	if c := codes(plain); !c["recovery.segment.missingAfterLatest"] || c["recovery.segment.missingAfterLatestFormerPrimary"] {
		t.Errorf("plain gap codes %v", c)
	}
	for _, s := range tail.Segments {
		if s.Code == "recovery.segment.missingAfterLatestFormerPrimary" && (s.Params["instance"] != "" || !strings.Contains(s.Text, "do not delete that instance")) {
			t.Errorf("an unknown former primary is named: %+v", s)
		}
	}

	// Round 9, DBA: when one instance was read as primary on the timeline
	// the WAL stops on, the advice names it; with two, it names none.
	facts.former = formerArchiver{read: map[uint64]bool{1: true}, holders: map[uint64]map[string]bool{1: {"db-1": true}}}
	named := func(tl viewmodel.RecoveryTimeline, code string) []string {
		var out []string
		for _, s := range tl.Segments {
			if s.Code == code {
				out = append(out, s.Params["instance"]+"|"+s.Text)
			}
		}
		for _, r := range tl.Reasons {
			if r.Code == code {
				out = append(out, r.Params["instance"]+"|"+r.Text)
			}
		}
		return out
	}
	tail = svc.recoveryTimeline(timelineFixture{paths: []evidence.RecoveryPath{held(parentTailPath(1))}}.evidence(), viewmodel.BackupSummary{}, nil, facts)
	for _, code := range []string{"recovery.segment.missingAfterLatestFormerPrimary", "recovery.reason.missingAfterLatestFormerPrimary"} {
		got := named(tail, code)
		if len(got) != 1 || !strings.HasPrefix(got[0], "db-1|") || !strings.Contains(got[0], "do not delete db-1 or its volume") {
			t.Errorf("%s: %q, want db-1 named", code, got)
		}
	}
	hole = svc.recoveryTimeline(timelineFixture{paths: []evidence.RecoveryPath{held(parentTailPath(1)), after}}.evidence(), viewmodel.BackupSummary{}, nil, facts)
	if got := named(hole, "recovery.segment.missingWALFormerPrimary"); len(got) != 1 || !strings.HasPrefix(got[0], "db-1|") {
		t.Errorf("hole: %q, want db-1 named", got)
	}
	facts.former.holders[1]["db-2"] = true
	tail = svc.recoveryTimeline(timelineFixture{paths: []evidence.RecoveryPath{held(parentTailPath(1))}}.evidence(), viewmodel.BackupSummary{}, nil, facts)
	if got := named(tail, "recovery.segment.missingAfterLatestFormerPrimary"); len(got) != 1 || !strings.HasPrefix(got[0], "|") {
		t.Errorf("two instances read on timeline 1: %q, want none named", got)
	}
}
