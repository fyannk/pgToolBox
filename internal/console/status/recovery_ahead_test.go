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

	"github.com/fyannk/pgToolBox/internal/console/evidence"
	"github.com/fyannk/pgToolBox/internal/console/sweep"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// aheadPath is a path from a backup on timeline start, from -from,
// toward timeline 3, its frontier on timeline 3 three minutes ago.
func aheadPath(id string, from time.Duration, start uint64) evidence.RecoveryPath {
	p := tlPath(id, from, 3*time.Minute, evidencev1alpha1.CoverageFrontier)
	p.TargetTimeline, p.StartTimeline, p.FrontierTimeline = 3, pu64(start), pu64(3)
	return p
}

// TestRepositoryTimelineAheadOfTheLiveOne (round 12, PG P3-2, the
// reviewer's probe): the repository's newest timeline is 3 and the live
// primary writes timeline 2: another cluster archives here (a cluster
// restored by PITR, or a promoted replica cluster, with the empty-archive
// check skipped). A default restore follows timeline 3, so the restore
// points past the fork are not covered: from the newest lower bound of a
// backup on the live line (its on-target path ends it before the fork),
// and the bar is a warning that names both timelines.
func TestRepositoryTimelineAheadOfTheLiveOne(t *testing.T) {
	g := failoverGathered("db-1", time.Time{}, map[string][]failoverReading{})
	live := func(wal string) {
		g.swept = sweep.Snapshot{SweptAt: now, Interval: 30 * time.Second, Instances: map[string]sweep.Instance{
			"db-1": {State: viewmodel.StatusReadOK, Reading: &instancestatus.Reading{IsPrimary: true, CurrentWAL: wal, ObservedAt: now}},
		}}
	}
	paths := []evidence.RecoveryPath{aheadPath("b0", 72*time.Hour, 2), aheadPath("b1", 48*time.Hour, 2), aheadPath("b2", 10*time.Hour, 3)}

	live("000000020000000000000007")
	b := failoverTimeline(g, paths...)
	tl := b.RecoveryTimeline
	if tl.State != string(evidencev1alpha1.Warning) || len(tl.Reasons) == 0 || tl.Reasons[0].Code != "recovery.reason.repositoryAheadOfLive" ||
		tl.Reasons[0].Params["timeline"] != "3" || tl.Reasons[0].Params["liveTimeline"] != "2" {
		t.Fatalf("state %s, reasons %+v, want a warning that timeline 3 is ahead of 2", tl.State, tl.Reasons)
	}
	if c := restoreCheck(b, now.Add(-60*time.Hour)); c.Answer != "covered" || c.BackupID != "b0" {
		t.Errorf("before b1's end, on the shared history: %s (%s), want covered by b0", c.Answer, c.Code)
	}
	for _, d := range []time.Duration{47 * time.Hour, 25 * time.Hour, 5 * time.Hour, 30 * time.Minute} {
		if c := restoreCheck(b, now.Add(-d)); c.Answer == "covered" || c.Code != "recovery.segment.repositoryAheadOfLive" {
			t.Errorf("now-%s, maybe past the fork: %s (%s), want unknown", d, c.Answer, c.Code)
		}
	}
	if tl.NewestCoveredTarget != nil && tl.NewestCoveredTarget.After(now.Add(-48*time.Hour)) {
		t.Errorf("newest covered target %s past the newest live-line backup", tl.NewestCoveredTarget)
	}

	// No backup on the live line: nothing is placed before the fork.
	b = failoverTimeline(g, aheadPath("b2", 10*time.Hour, 3))
	if c := restoreCheck(b, now.Add(-9*time.Hour)); c.Answer == "covered" {
		t.Errorf("only a timeline-3 backup: %+v", c)
	}

	// The live primary writes the target timeline, or the live one is
	// not known: nothing is said.
	for _, wal := range []string{"000000030000000000000007", ""} {
		live(wal)
		if wal == "" {
			g.swept = sweep.Snapshot{}
		}
		tl := failoverTimeline(g, paths...).RecoveryTimeline
		for _, r := range tl.Reasons {
			if r.Code == "recovery.reason.repositoryAheadOfLive" {
				t.Errorf("live WAL %q: %+v", wal, tl.Reasons)
			}
		}
		if segmentCoded(tl, "recovery.segment.repositoryAheadOfLive") != nil {
			t.Errorf("live WAL %q: segments marked ahead", wal)
		}
	}
}

// TestPostFailoverLagWithNoPrimaryToAct (round 12, CNPG P3-1, the
// reviewer's probe): after a failover the repository holds no WAL of the
// live timeline, and the Cluster is hibernated or its primary was not
// read (fenced, unreachable). The bar does not advise pg_switch_wal() on
// a primary that is not running or not read: hibernated says the
// hibernation (unknown); an unread primary keeps the lag warning without
// the advice.
func TestPostFailoverLagWithNoPrimaryToAct(t *testing.T) {
	p1 := now.Add(-10 * time.Minute)
	g := failoverGathered("db-2", p1, map[string][]failoverReading{
		"db-1": {{at: p1.Add(-20 * time.Minute), files: 0, timeline: 1}},
		"db-2": {{at: p1.Add(time.Minute), timeline: 2}},
	})
	receipt := p1.Add(-2 * time.Minute)
	p := tlPath("b1", 48*time.Hour, 0, evidencev1alpha1.CoverageFrontier)
	p.TargetTimeline, p.StartTimeline, p.FrontierTimeline, p.FrontierReceiptAt = 1, pu64(1), pu64(1), &receipt
	ev := timelineFixture{paths: []evidence.RecoveryPath{p}}.evidence()
	for _, c := range []struct {
		name               string
		hibernated, unread bool
		state, code        string
	}{
		{"read", false, false, string(evidencev1alpha1.Warning), "recovery.reason.liveTimelineNotArchived"},
		{"hibernated", true, true, string(evidencev1alpha1.Unknown), "recovery.reason.archiverHibernated"},
		{"primary not read", false, true, string(evidencev1alpha1.Warning), "recovery.reason.liveTimelineNotArchivedPrimaryUnread"},
	} {
		facts := failoverFacts(g)
		if facts.liveTimeline != 2 {
			t.Fatalf("live timeline %d", facts.liveTimeline)
		}
		facts.hibernated, facts.archiverUnread = c.hibernated, c.unread
		tl := (&Service{}).recoveryTimeline(ev, viewmodel.BackupSummary{}, nil, facts)
		if tl.State != c.state || len(tl.Reasons) == 0 || tl.Reasons[0].Code != c.code {
			t.Errorf("%s: state %s, reasons %+v, want %s %s", c.name, tl.State, tl.Reasons, c.state, c.code)
		}
		if advises := strings.Contains(tl.Reason, "pg_switch_wal()"); advises != (c.name == "read") {
			t.Errorf("%s: advises pg_switch_wal() %v: %q", c.name, advises, tl.Reason)
		}
	}
}

// TestPurgeBandSpansTheOldestBackup (round 12, PG P3-3): retention drops
// a whole backup, with the WAL only it needs. Once the scan is older than
// one refresh interval, the band reaches the next backup's lower bound;
// a fresh scan, or a single backup, keeps the elapsed-time band.
func TestPurgeBandSpansTheOldestBackup(t *testing.T) {
	first, last := now.Add(-7*24*time.Hour), now.Add(-time.Minute)
	second := now.Add(-24 * time.Hour)
	two := []interval{{from: first, to: last, backupID: "old"}, {from: second, to: last, backupID: "new"}}
	one := two[:1]
	at := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }
	for _, c := range []struct {
		name    string
		spans   []interval
		scanned *time.Time
		want    time.Duration
	}{
		{"fresh: one refresh plus the age", two, at(2 * time.Minute), 7 * time.Minute},
		{"a refresh old: unchanged", two, at(5 * time.Minute), 10 * time.Minute},
		{"older than a refresh: the oldest backup's own span", two, at(6 * time.Minute), second.Sub(first)},
		{"a single backup is not purged by retention", one, at(2 * time.Hour), 2*time.Hour + 5*time.Minute},
		{"the elapsed band is longer: kept", []interval{{from: first, to: last, backupID: "old"}, {from: first.Add(time.Hour), to: last, backupID: "new"}}, at(3 * time.Hour), 3*time.Hour + 5*time.Minute},
		{"never past the window", []interval{{from: first, to: last, backupID: "old"}, {from: last.Add(time.Hour), to: last.Add(2 * time.Hour), backupID: "new"}}, at(time.Hour), last.Sub(first)},
	} {
		in := timelineInput{facts: recoveryFacts{now: now}, scannedAt: c.scanned, refresh: 5 * time.Minute}
		if got := purgeBand(in, c.spans, first, last); got != c.want {
			t.Errorf("%s: band %s, want %s", c.name, got, c.want)
		}
	}
}

// TestDroppedKeptPathsAreSaid (round 12, DBA): when the last good paths
// are dropped beside a failed read, the bar says why with a coded reason
// the SPA translates.
func TestDroppedKeptPathsAreSaid(t *testing.T) {
	snap := evidencev1alpha1.RepositoryEvidenceSnapshot{}
	for dropped, code := range map[string]string{
		evidence.KeptDroppedTooOld:            "recovery.reason.keptTooOld",
		evidence.KeptDroppedRepositoryChanged: "recovery.reason.keptRepositoryChanged",
		"":                                    "",
	} {
		ev := clusterEvidence{result: evidence.Result{Agent: "a", Snapshot: &snap, Recovery: evidence.Recovery{State: evidence.RecoveryUnavailable, Reason: "timeout", Dropped: dropped}}}
		tl := (&Service{}).recoveryTimeline(ev, viewmodel.BackupSummary{}, nil, recoveryFacts{now: now})
		var codes []string
		for _, r := range tl.Reasons {
			codes = append(codes, r.Code)
		}
		want := []string{"recovery.reason.pathsNotCoverage"}
		if code != "" {
			want = append(want, code)
		}
		if strings.Join(codes, ",") != strings.Join(want, ",") {
			t.Errorf("dropped %q: reasons %v, want %v", dropped, codes, want)
		}
	}
}
