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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fyannk/pgToolBox/internal/console/evidence"
	"github.com/fyannk/pgToolBox/internal/console/sweep"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// segmentCoded is the first segment carrying code; nil when none does.
func segmentCoded(tl viewmodel.RecoveryTimeline, code string) *viewmodel.RecoverySegment {
	for i := range tl.Segments {
		if tl.Segments[i].Code == code {
			return &tl.Segments[i]
		}
	}
	return nil
}

// TestRepositoryTimelineLagsTheLiveOne (round 10, CNPG P2-1 and PG
// P2-a, both reviewers' probes): B (db-2) was promoted to timeline 2; A
// (db-1) wrote timeline 1 with WAL waiting in every reading kept. Until
// 00000002.history reaches the repository and the agent reads it again,
// every path targets timeline 1 and stops at A's last receipt. That end
// is A's tail: bounded by A's backlog, never covered just before it, and
// the tail says the repository has no WAL of timeline 2 yet.
func TestRepositoryTimelineLagsTheLiveOne(t *testing.T) {
	t.Run("CNPG probe: just promoted, every path still targets the parent", func(t *testing.T) {
		p1 := now.Add(-10 * time.Minute)
		g := failoverGathered("db-2", p1, map[string][]failoverReading{
			"db-1": {{at: p1.Add(-20 * time.Minute), files: 40, timeline: 1}, {at: p1.Add(-10 * time.Minute), files: 60, timeline: 1}, {at: p1.Add(time.Minute), gap: true}},
			"db-2": {{at: p1.Add(-20 * time.Minute), gap: true}, {at: p1.Add(time.Minute), timeline: 2}, {at: p1.Add(5 * time.Minute), timeline: 2}},
		})
		receipt := p1.Add(-2 * time.Minute)
		p := tlPath("b1", 48*time.Hour, 0, evidencev1alpha1.CoverageFrontier)
		p.TargetTimeline = 1
		p.StartTimeline, p.FrontierTimeline = pu64(1), pu64(1)
		p.FrontierReceiptAt = &receipt
		facts := failoverFacts(g)
		if facts.liveTimeline != 2 {
			t.Fatalf("live timeline %d, want 2 (db-2's sweeps since its promotion)", facts.liveTimeline)
		}
		facts.quiet = []quietSweep{{at: p1.Add(time.Minute), by: p1.Add(time.Minute + time.Second), timeline: 2}, {at: p1.Add(5 * time.Minute), by: p1.Add(5*time.Minute + time.Second), timeline: 2}}
		tl := (&Service{}).recoveryTimeline(timelineFixture{paths: []evidence.RecoveryPath{p}}.evidence(), viewmodel.BackupSummary{}, nil, facts)
		b := viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}
		assertNotCovered(t, b, receipt, 10*time.Minute, 20*time.Minute, 30*time.Minute, 3*time.Hour)
		if band := bandOn(b, "1"); band == nil || band.Code != "recovery.segment.formerArchiverBacklog" || !band.From.Equal(receipt.Add(-failureLookback)) {
			t.Errorf("timeline 1's band %+v, want A's backlog from a day before the end", band)
		}
		tail := segmentCoded(tl, "recovery.segment.liveTimelineNotArchived")
		if tail == nil || tail.Params["timeline"] != "2" || !tail.From.Equal(receipt) || !strings.Contains(tail.Text, "no WAL of timeline 2") {
			t.Errorf("tail %+v, want it to say the repository has no WAL of timeline 2 yet", tail)
		}
		// Round 11, DBA P2: the chip is not healthy with no reason.
		if tl.State != string(evidencev1alpha1.Warning) || len(tl.Reasons) == 0 || tl.Reasons[0].Code != "recovery.reason.liveTimelineNotArchived" ||
			tl.Reasons[0].Params["timeline"] != "2" || !strings.Contains(tl.Reason, "pg_switch_wal()") {
			t.Errorf("state %s, reasons %+v, want a warning that the repository has no WAL of timeline 2", tl.State, tl.Reasons)
		}

		// Round 11, CNPG P3: the history file is archived (the path
		// targets timeline 2) but no segment of timeline 2 yet.
		p.TargetTimeline = 2
		tl = (&Service{}).recoveryTimeline(timelineFixture{paths: []evidence.RecoveryPath{p}}.evidence(), viewmodel.BackupSummary{}, nil, facts)
		if segmentCoded(tl, "recovery.segment.liveTimelineNotArchived") != nil {
			t.Errorf("the tail still says the history file may not be archived")
		}
		tail = segmentCoded(tl, "recovery.segment.liveTimelineNoSegment")
		if tail == nil || tail.Params["timeline"] != "2" || !strings.Contains(tail.Text, "no WAL segment of it yet") {
			t.Errorf("tail %+v, want it to say no segment of timeline 2 yet", tail)
		}
		if tl.State != string(evidencev1alpha1.Warning) || len(tl.Reasons) == 0 || tl.Reasons[0].Code != "recovery.reason.liveTimelineNotArchived" {
			t.Errorf("target 2: state %s, reasons %+v", tl.State, tl.Reasons)
		}
	})

	t.Run("PG probe: the target lags, or the target moved on and the parent is gap-limited", func(t *testing.T) {
		p1 := now.Add(-20 * time.Minute)
		g := failoverGathered("db-2", p1, map[string][]failoverReading{
			"db-1": {{at: p1.Add(-90 * time.Minute), files: 40, timeline: 1}, {at: p1.Add(-60 * time.Minute), files: 50, timeline: 1}, {at: p1.Add(-2 * time.Minute), files: 60, timeline: 1}, {at: p1.Add(time.Minute), gap: true}},
			"db-2": {{at: p1.Add(-10 * time.Minute), gap: true}, {at: p1.Add(time.Minute), timeline: 2}, {at: p1.Add(10 * time.Minute), timeline: 2}},
		})
		receipt := p1.Add(-time.Minute)
		for _, target := range []uint64{1, 2} {
			consistent := true
			p := tlPath("b1", 48*time.Hour, 0, evidencev1alpha1.CoverageFrontier)
			p.FrontierReceiptAt = &receipt
			p.TargetTimeline, p.StartTimeline, p.FrontierTimeline, p.Consistent = target, pu64(1), pu64(1), &consistent
			if target == 2 {
				p.Stop, p.State = evidencev1alpha1.CoverageGapLimited, evidencev1alpha1.Unhealthy
			}
			b := failoverTimeline(g, p)
			assertNotCovered(t, b, receipt, 10*time.Minute, 20*time.Minute, 30*time.Minute, time.Hour, 3*time.Hour)
			if band := bandOn(b, "1"); band == nil || band.Code != "recovery.segment.formerArchiverBacklog" {
				t.Errorf("target %d: timeline 1's band %+v, want A's backlog", target, band)
			}
			lag := segmentCoded(b.RecoveryTimeline, "recovery.segment.liveTimelineNotArchived")
			if (target == 1) != (lag != nil) {
				t.Errorf("target %d: lag tail %+v (a gap-limited tail says the gap instead)", target, lag)
			}
		}
	})

	t.Run("the live timeline from the current reading's WAL file", func(t *testing.T) {
		p1 := now.Add(-10 * time.Minute)
		g := failoverGathered("db-2", p1, map[string][]failoverReading{
			"db-1": {{at: p1.Add(-20 * time.Minute), files: 40, timeline: 1}},
		})
		g.swept = sweep.Snapshot{SweptAt: now, Interval: 30 * time.Second, Instances: map[string]sweep.Instance{
			"db-2": {State: viewmodel.StatusReadOK, Reading: &instancestatus.Reading{IsPrimary: true, CurrentWAL: "000000020000000000000007", CurrentLSN: "0/7000100", ObservedAt: now}},
		}}
		if got := liveTimelineOf(g, promotedAt(g)); got != 2 {
			t.Fatalf("live timeline %d, want 2 from currentWAL", got)
		}
		g.swept.Instances["db-2"].Reading.CurrentWAL = "not a wal file name"
		if got := liveTimelineOf(g, promotedAt(g)); got != 0 {
			t.Fatalf("live timeline %d from an unparsable name and no ring, want 0", got)
		}
	})

	t.Run("live timeline unknown: an end before the promotion is a former primary's", func(t *testing.T) {
		p1 := now.Add(-30 * time.Minute)
		g := failoverGathered("db-2", p1, map[string][]failoverReading{})
		end := func(receipt time.Time) evidence.RecoveryPath {
			p := tlPath("b1", 48*time.Hour, 0, evidencev1alpha1.CoverageFrontier)
			p.TargetTimeline, p.StartTimeline, p.FrontierTimeline = 1, pu64(1), pu64(1)
			p.FrontierReceiptAt = &receipt
			return p
		}
		before := p1.Add(-2 * time.Minute)
		b := failoverTimeline(g, end(before))
		assertNotCovered(t, b, before, 20*time.Minute, 3*time.Hour)
		if band := bandOn(b, "1"); band == nil || band.Code != "recovery.segment.formerArchiverUnknown" {
			t.Errorf("band %+v, want the unknown band of a former archiver", band)
		}
		// After the promotion the current primary archived it: its own
		// facts bound the end, as before.
		after := now.Add(-time.Minute)
		b = failoverTimeline(g, end(after))
		if band := bandOn(b, "1"); band != nil {
			t.Errorf("an end after the promotion drew a former archiver's band %+v", band)
		}
		if got := restoreCheck(b, after.Add(-time.Hour)); got.Answer != "covered" {
			t.Errorf("an hour before an end archived by the current primary: %s (%s), want covered", got.Answer, got.Code)
		}
	})
}

// TestFormerPrimaryAdviceSkipsAnInstanceThatRejoined (round 10, CNPG
// P3-1, PG and DBA): the do-not-delete advice names the former primary
// read on the timeline the WAL stops on. When that instance is back in
// the Cluster (the current primary after A, then B, then A again, or a
// Ready standby read since the switch), CloudNativePG archived the WAL
// waiting on it before it started again: the bar says so instead of
// advising to keep it. Gone, or not Ready, the advice stands.
func TestFormerPrimaryAdviceSkipsAnInstanceThatRejoined(t *testing.T) {
	p1 := now.Add(-4 * time.Hour)
	receipt := p1.Add(-5 * time.Minute)
	path := parentTailPath(1)
	path.FrontierReceiptAt = &receipt
	path.Reason = evidencev1alpha1.Reason{Code: "coverage-path-unhealthy", Message: "segment 000000010000000000000042 before the timeline switch is not in the repository; " + evidencev1alpha1.FormerPrimaryMayHold}
	rings := func() map[string][]failoverReading {
		return map[string][]failoverReading{
			"db-1": {{at: p1.Add(-20 * time.Minute), files: 5, timeline: 1}, {at: p1.Add(time.Minute), gap: true}},
			"db-2": {{at: p1.Add(10 * time.Minute), timeline: 2}},
		}
	}
	pod := func(ready bool) *corev1.Pod {
		status := corev1.ConditionFalse
		if ready {
			status = corev1.ConditionTrue
		}
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "db-1", UID: "u1"}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}}}
	}
	standby := func(observed time.Time) sweep.Snapshot {
		return sweep.Snapshot{SweptAt: now, Interval: 30 * time.Second, Instances: map[string]sweep.Instance{
			"db-1": {State: viewmodel.StatusReadOK, Reading: &instancestatus.Reading{PodUID: "u1", IsWalReceiverActive: true, ObservedAt: observed}},
		}}
	}
	cases := []struct {
		name     string
		mutate   func(g *gathered)
		rejoined bool
	}{
		{name: "the holder is gone", mutate: func(*gathered) {}},
		{name: "the holder's pod is not Ready", mutate: func(g *gathered) { g.pods, g.swept = []*corev1.Pod{pod(false)}, standby(now) }},
		{name: "the holder is Ready but not read since the switch", mutate: func(g *gathered) { g.pods, g.swept = []*corev1.Pod{pod(true)}, standby(p1.Add(-time.Minute)) }},
		{name: "a recreated pod of the same name", mutate: func(g *gathered) {
			g.pods, g.swept = []*corev1.Pod{pod(true)}, standby(now)
			g.pods[0].UID = "u2"
		}},
		{name: "the holder rejoined as a Ready standby", mutate: func(g *gathered) { g.pods, g.swept = []*corev1.Pod{pod(true)}, standby(now) }, rejoined: true},
		{name: "the holder is the current primary again", mutate: func(g *gathered) { g.cluster.Status.CurrentPrimary = "db-1" }, rejoined: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := failoverGathered("db-2", p1, rings())
			c.mutate(g)
			b := failoverTimeline(g, path)
			tl := b.RecoveryTimeline
			seg, reason := segmentCoded(tl, "recovery.segment.missingAfterLatestFormerPrimary"), tl.Reasons[0]
			if c.rejoined {
				seg = segmentCoded(tl, "recovery.segment.missingAfterLatestFormerPrimaryRejoined")
			}
			if seg == nil || seg.Params["instance"] != "db-1" {
				t.Fatalf("tail segments %+v, want db-1 named (rejoined %v)", tl.Segments, c.rejoined)
			}
			wantReason := "recovery.reason.missingAfterLatestFormerPrimary"
			if c.rejoined {
				wantReason += "Rejoined"
			}
			if reason.Code != wantReason || reason.Params["instance"] != "db-1" {
				t.Errorf("reason %+v, want %s", reason, wantReason)
			}
			for _, text := range []string{seg.Text, reason.Text} {
				advises := strings.Contains(text, "do not delete db-1")
				if advises == c.rejoined {
					t.Errorf("rejoined %v but the advice to keep db-1 is %v: %q", c.rejoined, advises, text)
				}
				if c.rejoined && !strings.Contains(text, "has rejoined the cluster since") {
					t.Errorf("rejoined, but the sentence does not say so: %q", text)
				}
			}
			if tl.State != string(evidencev1alpha1.Unhealthy) {
				t.Errorf("state %s: WAL is still missing from the repository", tl.State)
			}
		})
	}
}

// TestStaleBarmanPathsOfAnExpiredBackup (round 10, PG P2-b, the
// reviewer's probe): the recovery fetch failed, so the previous paths are
// kept beside a fresh, complete backup collection that no longer lists
// backup old (retention expired it). Its span is drawn, never as covered,
// and the bar says why; the purge band is timed from the kept paths' own
// scan.
func TestStaleBarmanPathsOfAnExpiredBackup(t *testing.T) {
	old := tlPath("old", 72*time.Hour, 10*time.Minute, evidencev1alpha1.CoverageFrontier)
	newer := tlPath("new", 24*time.Hour, 10*time.Minute, evidencev1alpha1.CoverageFrontier)
	scanned := now.Add(-time.Minute)
	evidenceOf := func(ids map[string]bool, items []evidence.BackupItem, pathsScanned *time.Time) clusterEvidence {
		snap := evidencev1alpha1.RepositoryEvidenceSnapshot{CompletedAt: &scanned}
		snap.Identity.Repository.Format = evidencev1alpha1.FormatBarmanCloud
		return clusterEvidence{result: evidence.Result{
			Agent: "a", Snapshot: &snap, BackupsComplete: true, BackupIDs: ids, Backups: items, RefreshInterval: 5 * time.Minute,
			Recovery: evidence.Recovery{
				State: evidence.RecoveryAvailable, Complete: true, Stale: true, StaleReason: "recovery pages failed",
				Paths: []evidence.RecoveryPath{old, newer}, ScannedAt: pathsScanned,
			},
		}}
	}
	item := func(p evidence.RecoveryPath) evidence.BackupItem {
		return evidence.BackupItem{ID: p.BackupID, EndAt: p.LowerBoundAt, EndTimeline: pu64(2), EndPosition: pu64(50)}
	}
	facts := recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout, sweepInterval: 30 * time.Second}
	svc := &Service{}

	ev := evidenceOf(map[string]bool{"new": true}, []evidence.BackupItem{item(newer)}, ago(2*time.Hour))
	tl := svc.recoveryTimeline(ev, viewmodel.BackupSummary{}, nil, facts)
	b := viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}
	for _, d := range []time.Duration{70 * time.Hour, 48 * time.Hour, 30 * time.Hour} {
		if c := restoreCheck(b, now.Add(-d)); c.Answer == "covered" || c.Code != "recovery.segment.backupGone" {
			t.Errorf("now-%s: %s backup=%s (%s), want not covered, backupGone", d, c.Answer, c.BackupID, c.Code)
		}
	}
	if c := restoreCheck(b, now.Add(-3*time.Hour)); c.Answer != "covered" || c.BackupID != "new" {
		t.Errorf("now-3h: %s (%s), want covered by new", c.Answer, c.Code)
	}
	if tl.State != string(evidencev1alpha1.Warning) || len(tl.Reasons) == 0 || tl.Reasons[0].Code != "recovery.reason.backupGone" || tl.Reasons[0].Params["backupId"] != "old" {
		t.Errorf("bar %s, reasons %+v, want a warning naming old", tl.State, tl.Reasons)
	}
	if len(offTargetSpans([]evidence.RecoveryPath{func() evidence.RecoveryPath { p := old; p.OffTarget = true; return p }()}, ev.result.Backups, func(id string) bool { return id == "old" })) != 0 {
		t.Error("an off-target span of a backup the repository no longer lists")
	}

	// Both still listed: covered, but the purge band runs from the kept
	// paths' scan two hours ago, not the fresh snapshot's a minute ago.
	ev = evidenceOf(map[string]bool{"old": true, "new": true}, []evidence.BackupItem{item(old), item(newer)}, ago(2*time.Hour))
	tl = svc.recoveryTimeline(ev, viewmodel.BackupSummary{}, nil, facts)
	b = viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}
	first := now.Add(-72 * time.Hour)
	if c := restoreCheck(b, first.Add(time.Hour)); c.Answer == "covered" || c.Code != "recovery.segment.purgeEdge" {
		t.Errorf("an hour after the oldest point: %s (%s), want the purge band timed from the kept paths' scan", c.Answer, c.Code)
	}
	if c := restoreCheck(b, first.Add(3*time.Hour)); c.Answer != "covered" || c.BackupID != "old" {
		t.Errorf("three hours after the oldest point: %s (%s), want covered by old", c.Answer, c.Code)
	}
}

// TestStalePathsBesideACutCollection (round 11, PG P3, the reviewer's
// probe): the recovery paths are kept stale from a scan two hours ago and
// the backup collection beside them was cut at a bound, so it cannot say
// whether backup old is still in the repository. Its span is not
// covered; a backup the cut collection lists still covers; fresh paths
// beside a cut collection are not doubted.
func TestStalePathsBesideACutCollection(t *testing.T) {
	old := tlPath("old", 72*time.Hour, 10*time.Minute, evidencev1alpha1.CoverageFrontier)
	newer := tlPath("new", 24*time.Hour, 10*time.Minute, evidencev1alpha1.CoverageFrontier)
	scanned := now.Add(-time.Minute)
	evidenceOf := func(stale bool) clusterEvidence {
		snap := evidencev1alpha1.RepositoryEvidenceSnapshot{CompletedAt: &scanned}
		snap.Identity.Repository.Format = evidencev1alpha1.FormatBarmanCloud
		rec := evidence.Recovery{State: evidence.RecoveryAvailable, Complete: true, Paths: []evidence.RecoveryPath{old, newer}, ScannedAt: ago(2 * time.Hour)}
		if stale {
			rec.Stale, rec.StaleReason = true, "recovery pages failed"
		}
		return clusterEvidence{result: evidence.Result{
			Agent: "a", Snapshot: &snap, BackupsComplete: false, BackupIDs: map[string]bool{"new": true},
			Backups:         []evidence.BackupItem{{ID: "new", EndAt: newer.LowerBoundAt, EndTimeline: pu64(2), EndPosition: pu64(50)}},
			RefreshInterval: 5 * time.Minute, Recovery: rec,
		}}
	}
	facts := recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout, sweepInterval: 30 * time.Second}
	svc := &Service{}

	tl := svc.recoveryTimeline(evidenceOf(true), viewmodel.BackupSummary{}, nil, facts)
	b := viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}
	for _, d := range []time.Duration{70 * time.Hour, 48 * time.Hour, 30 * time.Hour} {
		if c := restoreCheck(b, now.Add(-d)); c.Answer == "covered" || c.Code != "recovery.segment.backupUnconfirmed" {
			t.Errorf("now-%s: %s backup=%s (%s), want not covered, backupUnconfirmed", d, c.Answer, c.BackupID, c.Code)
		}
	}
	if c := restoreCheck(b, now.Add(-3*time.Hour)); c.Answer != "covered" || c.BackupID != "new" {
		t.Errorf("now-3h: %s (%s), want covered by new", c.Answer, c.Code)
	}
	if tl.State != string(evidencev1alpha1.Warning) || len(tl.Reasons) == 0 || tl.Reasons[0].Code != "recovery.reason.backupUnconfirmed" || tl.Reasons[0].Params["backupId"] != "old" {
		t.Errorf("bar %s, reasons %+v, want a warning naming old", tl.State, tl.Reasons)
	}

	tl = svc.recoveryTimeline(evidenceOf(false), viewmodel.BackupSummary{}, nil, facts)
	b = viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}
	if c := restoreCheck(b, now.Add(-48*time.Hour)); c.Answer != "covered" || c.BackupID != "old" {
		t.Errorf("fresh paths beside a cut collection: now-48h %s (%s), want covered by old", c.Answer, c.Code)
	}
}

// TestPurgeBandArithmetic (round 11, PG P3, the reviewer's probe): the
// band at the oldest restore point is the refresh interval plus the age
// of the scan, from the earlier of the agent's scan time and the
// console's fetch time (an agent clock ahead does not shrink it), and
// never past the window (an absurd scan time does not overflow it away).
func TestPurgeBandArithmetic(t *testing.T) {
	facts := recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout, sweepInterval: 30 * time.Second}
	first := now.Add(-72 * time.Hour)
	band := func(scanned, observed time.Time) *viewmodel.RecoverySegment {
		t.Helper()
		p := tlPath("old", 72*time.Hour, 10*time.Minute, evidencev1alpha1.CoverageFrontier)
		ev := timelineFixture{paths: []evidence.RecoveryPath{p}, scannedAt: scanned}.evidence()
		ev.result.Snapshot.CompletedAt = &scanned
		ev.result.ObservedAt = &observed
		ev.result.RefreshInterval = 5 * time.Minute
		tl := (&Service{}).recoveryTimeline(ev, viewmodel.BackupSummary{}, nil, facts)
		return segmentCoded(tl, "recovery.segment.purgeEdge")
	}
	cases := []struct {
		name               string
		scanned, observed  time.Time
		want               time.Duration
		wholeWindowAtLeast bool
	}{
		{"a scan two hours ago", now.Add(-2 * time.Hour), now.Add(-time.Minute), 2*time.Hour + 5*time.Minute, false},
		{"an agent clock 30 minutes ahead", now.Add(30 * time.Minute), now.Add(-10 * time.Minute), 15 * time.Minute, false},
		{"a scan in year 1", time.Date(1, 1, 1, 0, 0, 1, 0, time.UTC), now.Add(-time.Minute), 0, true},
		{"a scan in 1700", time.Date(1700, 1, 1, 0, 0, 0, 0, time.UTC), now.Add(-time.Minute), 0, true},
	}
	for _, c := range cases {
		seg := band(c.scanned, c.observed)
		if seg == nil {
			t.Errorf("%s: no purge band", c.name)
			continue
		}
		if !seg.From.Equal(first) {
			t.Errorf("%s: band from %s, want the oldest point %s", c.name, seg.From, first)
		}
		got := seg.To.Sub(seg.From)
		switch {
		case c.wholeWindowAtLeast && got < 70*time.Hour:
			t.Errorf("%s: band %s, want it to reach (nearly) the whole window", c.name, got)
		case !c.wholeWindowAtLeast && got != c.want:
			t.Errorf("%s: band %s, want %s", c.name, got, c.want)
		}
	}
}
