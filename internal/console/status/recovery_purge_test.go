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
	"strconv"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/evidence"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// TestPurgeBandTakesAPgBackRestChain (round 14, PG P3-A, the reviewer's
// probe): pgBackRest expires a full backup with every incremental taken
// against it. Weekly fulls and daily incrementals, the paths kept stale
// from a scan 2 h ago (a missed scan): the oldest full's whole chain may
// be gone, so nothing before the next full is covered; a Barman
// repository with the same backups loses only the oldest one.
func TestPurgeBandTakesAPgBackRestChain(t *testing.T) {
	facts := recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout, sweepInterval: 30 * time.Second}
	consistent := true
	var paths []evidence.RecoveryPath
	for d := 14; d >= 1; d-- {
		p := tlPath("b"+strconv.Itoa(d), time.Duration(d)*24*time.Hour, time.Minute, evidencev1alpha1.CoverageFrontier)
		p.Consistent = &consistent
		paths = append(paths, p)
	}
	for _, pgBackRest := range []bool{true, false} {
		ev := timelineFixture{paths: paths, scannedAt: now.Add(-2 * time.Hour), stale: true}.evidence()
		if pgBackRest {
			ev.result.Snapshot.Identity.Repository.Format = evidencev1alpha1.FormatPGBackRest
			for i := range ev.result.Backups {
				typ := "incr"
				if id := ev.result.Backups[i].ID; id == "b14" || id == "b7" {
					typ = "full"
				}
				ev.result.Backups[i].PgBackRest = &evidence.PgBackRestItem{Type: typ, Usable: true}
			}
		}
		tl := (&Service{}).recoveryTimeline(ev, viewmodel.BackupSummary{}, nil, facts)
		b := viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}
		for _, d := range []time.Duration{12 * 24, 10 * 24, 7*24 + 1} {
			c := restoreCheck(b, now.Add(-d*time.Hour))
			chain := c.Answer != "covered" && c.Code == "recovery.segment.purgeEdge"
			if pgBackRest && !chain {
				t.Errorf("pgBackRest, now-%dh: %s by %s (%s), want the purge band: the oldest full's chain may be gone", d, c.Answer, c.BackupID, c.Code)
			}
			if !pgBackRest && chain {
				t.Errorf("Barman, now-%dh: %s (%s), want covered: only the oldest backup may be gone", d, c.Answer, c.Code)
			}
		}
		if c := restoreCheck(b, now.Add(-6*24*time.Hour)); c.Answer != "covered" {
			t.Errorf("pgBackRest %v, now-6d: %s (%s), want covered by the next full's chain", pgBackRest, c.Answer, c.Code)
		}
	}
}

// TestPurgeBandCountsBackupsWithNoPath (round 14, PG P3-A): a Barman
// backup the repository lists but no recovery path draws is a retention
// anchor too. Drawn backups old (7 d ago) and d (6 d ago), undrawn u (5 d
// ago), drawn new (2 d ago), read 2 d ago: the window's start may have
// passed u, so old and d may be gone and u, with no span, is the first
// base kept: nothing before new's lower bound is covered. A failed
// backup is no anchor.
func TestPurgeBandCountsBackupsWithNoPath(t *testing.T) {
	first, last := now.Add(-7*24*time.Hour), now.Add(-time.Minute)
	day := 24 * time.Hour
	spans := []interval{
		{from: first, to: last, backupID: "old"}, {from: first.Add(day), to: last, backupID: "d"}, {from: first.Add(5 * day), to: last, backupID: "new"},
	}
	item := func(id string, at time.Time, failed bool) evidence.BackupItem {
		return evidence.BackupItem{ID: id, EndAt: &at, Failed: failed}
	}
	scanned := now.Add(-2 * day)
	in := timelineInput{facts: recoveryFacts{now: now}, scannedAt: &scanned, refresh: 5 * time.Minute}
	in.backups = []evidence.BackupItem{item("old", first, false), item("d", first.Add(day), false), item("u", first.Add(2*day), false), item("new", first.Add(5*day), false)}
	if got := purgeBand(in, spans, first, last); got != 5*day {
		t.Errorf("an undrawn backup newer than the drawn one before reach: band %s, want %s (to new)", got, 5*day)
	}
	in.backups[2].Failed = true
	if got := purgeBand(in, spans, first, last); got != 2*day+5*time.Minute {
		t.Errorf("the undrawn backup failed: band %s, want the elapsed band %s", got, 2*day+5*time.Minute)
	}
}

// TestPurgeBandNeverPassesTheNewestBackup (round 14, PG P3-A): retention
// never removes the newest backup, so a scan read on time draws no band
// past the newest one's lower bound (a lone backup none at all). Once a
// scan was missed a backup taken since may have let retention remove it:
// the elapsed band stands.
func TestPurgeBandNeverPassesTheNewestBackup(t *testing.T) {
	first, last := now.Add(-7*24*time.Hour), now.Add(-time.Minute)
	at := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }
	lone := []interval{{from: first, to: last, backupID: "old"}}
	near := []interval{{from: first, to: last, backupID: "old"}, {from: first.Add(3 * time.Minute), to: last, backupID: "new"}}
	for _, c := range []struct {
		name    string
		spans   []interval
		scanned *time.Time
		want    time.Duration
	}{
		{"a lone backup read on time", lone, at(2 * time.Minute), 0},
		{"a lone backup past a missed scan", lone, at(2 * time.Hour), 2*time.Hour + 5*time.Minute},
		{"the newest backup 3 min after the oldest, read on time", near, at(2 * time.Minute), 3 * time.Minute},
	} {
		in := timelineInput{facts: recoveryFacts{now: now}, scannedAt: c.scanned, refresh: 5 * time.Minute}
		if got := purgeBand(in, c.spans, first, last); got != c.want {
			t.Errorf("%s: band %s, want %s", c.name, got, c.want)
		}
	}
}

// TestMissedScanFollowsTheScan (round 14, PG P3-B): a normal cycle lasts
// the longer of the refresh interval and the scan, then the next scan,
// then a console poll. Two intervals held the short band for a day at a
// 24 h refresh, and a 2 min scan at a 30 s refresh passed them every
// cycle (the flap of round 13, PG P3-C).
func TestMissedScanFollowsTheScan(t *testing.T) {
	for _, c := range []struct {
		refresh, scan, want time.Duration
	}{
		{24 * time.Hour, 0, 48 * time.Hour},
		{5 * time.Minute, 0, 10 * time.Minute},
		{24 * time.Hour, time.Minute, 24*time.Hour + 3*time.Minute},
		{30 * time.Second, 2 * time.Minute, 6 * time.Minute},
		{5 * time.Minute, 10 * time.Second, 7*time.Minute + 10*time.Second},
	} {
		if got := missedScan(c.refresh, c.scan); got != c.want {
			t.Errorf("refresh %s, scan %s: %s, want %s", c.refresh, c.scan, got, c.want)
		}
	}

	// End to end, the scan's duration from the snapshot: weekly backups.
	facts := recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout, sweepInterval: 30 * time.Second}
	paths := []evidence.RecoveryPath{
		tlPath("w0", 7*24*time.Hour, 6*time.Minute, evidencev1alpha1.CoverageFrontier),
		tlPath("w1", 2*24*time.Hour, 6*time.Minute, evidencev1alpha1.CoverageFrontier),
	}
	band := func(refresh, scan, completedAgo time.Duration) time.Duration {
		t.Helper()
		ev := timelineFixture{paths: paths, scannedAt: now.Add(-completedAgo)}.evidence()
		started := ev.result.Snapshot.CompletedAt.Add(-scan)
		ev.result.Snapshot.StartedAt = &started
		ev.result.RefreshInterval = refresh
		tl := (&Service{}).recoveryTimeline(ev, viewmodel.BackupSummary{}, nil, facts)
		seg := segmentCoded(tl, "recovery.segment.purgeEdge")
		if seg == nil {
			return 0
		}
		return seg.To.Sub(seg.From)
	}
	// A 24 h refresh, a 1 min scan started 25 h ago: missed, the band is
	// the oldest backup's own span.
	if got := band(24*time.Hour, time.Minute, 25*time.Hour-time.Minute); got != 5*24*time.Hour {
		t.Errorf("24 h refresh, read 25 h ago: band %s, want the oldest backup's span", got)
	}
	// A 30 s refresh, a 2 min scan completed 3 min ago (started 5 min
	// ago): a normal cycle, the elapsed band timed from the scan's start.
	if got := band(30*time.Second, 2*time.Minute, 3*time.Minute); got != 5*time.Minute+30*time.Second {
		t.Errorf("30 s refresh, 2 min scan: band %s, want the elapsed band from the scan's start", got)
	}
}
