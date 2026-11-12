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
		{"a lone backup past a missed scan: the whole window", lone, at(2 * time.Hour), last.Sub(first)},
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

// TestPurgeBandMissedScanOnAShortWindow (round 15, PG P3-1, the
// reviewer's probe): a missed scan is told from the scan's real age, not
// its age capped at the window. Before, a window shorter than missedScan
// never missed a scan: a lone backup with a 9 min window read 2 h ago at
// a 5 min refresh was fully covered, and two backups 20 h apart read ten
// days ago at a 24 h refresh were capped at the newer one.
func TestPurgeBandMissedScanOnAShortWindow(t *testing.T) {
	read := now.Add(-2 * time.Hour)
	for _, c := range []struct {
		name    string
		window  time.Duration
		refresh time.Duration
		want    time.Duration
	}{
		{"3 min window, 5 min refresh", 3 * time.Minute, 5 * time.Minute, 3 * time.Minute},
		{"9 min window, 5 min refresh", 9 * time.Minute, 5 * time.Minute, 9 * time.Minute},
		{"11 min window, 5 min refresh", 11 * time.Minute, 5 * time.Minute, 11 * time.Minute},
		// Read 2 h ago at a 24 h refresh is on time: the newest (lone)
		// backup is never purged.
		{"9 min window, 24 h refresh, on time", 9 * time.Minute, 24 * time.Hour, 0},
	} {
		last := now.Add(-2 * time.Hour)
		first := last.Add(-c.window)
		lone := []interval{{from: first, to: last, backupID: "old"}}
		in := timelineInput{facts: recoveryFacts{now: now}, scannedAt: &read, refresh: c.refresh}
		if got := purgeBand(in, lone, first, last); got != c.want {
			t.Errorf("%s, read 2 h ago: band %s, want %s", c.name, got, c.want)
		}
	}
	last := now.Add(-10 * 24 * time.Hour)
	first := last.Add(-20 * time.Hour)
	two := []interval{{from: first, to: last, backupID: "a"}, {from: first.Add(10 * time.Hour), to: last, backupID: "b"}}
	in := timelineInput{facts: recoveryFacts{now: now}, scannedAt: &last, refresh: 24 * time.Hour}
	if got := purgeBand(in, two, first, last); got != 20*time.Hour {
		t.Errorf("two backups read ten days ago at a 24 h refresh: band %s, want the whole window %s", got, 20*time.Hour)
	}
}

// TestPurgeBandPastTheScan (round 15, PG P3-2, the reviewer's probe):
// once the retention window's start passes the scan's start, the base may
// be a backup taken since, which the snapshot does not list, and the
// newest listed one goes with its whole chain. Weekly bases 13 and 6 days
// before the read (daily incrementals under pgBackRest), read 7 to 9 days
// ago: two days before the read is not covered. Read 5 days ago the
// window's start has not passed the read, and the next base keeps it.
func TestPurgeBandPastTheScan(t *testing.T) {
	day := 24 * time.Hour
	facts := recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout, sweepInterval: 30 * time.Second}
	consistent := true
	for _, pgBackRest := range []bool{true, false} {
		for _, c := range []struct {
			days    int
			covered bool
		}{{5, true}, {7, false}, {8, false}, {9, false}} {
			age := time.Duration(c.days) * day
			var paths []evidence.RecoveryPath
			for d := 13; d >= 0; d-- {
				if !pgBackRest && d != 13 && d != 6 {
					continue
				}
				p := tlPath("b"+strconv.Itoa(d), age+time.Duration(d)*day, age, evidencev1alpha1.CoverageFrontier)
				p.Consistent = &consistent
				paths = append(paths, p)
			}
			ev := timelineFixture{paths: paths, scannedAt: now.Add(-age), stale: true}.evidence()
			if pgBackRest {
				ev.result.Snapshot.Identity.Repository.Format = evidencev1alpha1.FormatPGBackRest
				for i := range ev.result.Backups {
					typ := "incr"
					if id := ev.result.Backups[i].ID; id == "b13" || id == "b6" {
						typ = "full"
					}
					ev.result.Backups[i].PgBackRest = &evidence.PgBackRestItem{Type: typ, Usable: true}
				}
			}
			tl := (&Service{}).recoveryTimeline(ev, viewmodel.BackupSummary{}, nil, facts)
			got := restoreCheck(viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}, now.Add(-age-2*day))
			if covered := got.Answer == "covered"; covered != c.covered {
				t.Errorf("pgBackRest %v, read %d days ago: two days before the read is %s by %s (%s), want covered %v",
					pgBackRest, c.days, got.Answer, got.BackupID, got.Code, c.covered)
			}
		}
	}
}

// TestPurgeBandKeepsABackupErrorFull (round 15, CNPG P3-1): a pgBackRest
// full marked backup-error (page checksum errors) is still a full that
// expire counts. Read on time, the band stops at the newest full, which is
// that one, not at the older full before it.
func TestPurgeBandKeepsABackupErrorFull(t *testing.T) {
	day := 24 * time.Hour
	first, last := now.Add(-7*day), now.Add(-time.Minute)
	spans := []interval{{from: first, to: last, backupID: "f1"}, {from: first.Add(3 * day), to: last, backupID: "f2"}}
	read := now.Add(-2 * time.Minute)
	in := timelineInput{facts: recoveryFacts{now: now}, scannedAt: &read, refresh: 5 * time.Minute, pgBackRest: true}
	in.backups = []evidence.BackupItem{
		{ID: "f1", PgBackRest: &evidence.PgBackRestItem{Type: "full", Usable: true}},
		{ID: "f2", Failed: true, PgBackRest: &evidence.PgBackRestItem{Type: "full"}},
	}
	if got, want := purgeBand(in, spans, first, last), 7*time.Minute; got != want {
		t.Errorf("newest full marked backup-error, read on time: band %s, want %s (it anchors retention)", got, want)
	}
}

// TestPurgeBandPastAStoppedRetention (round 16, PG P3-1, the reviewer's
// probe TestR16LoneStaleBackup): backups stopped for longer than the
// retention, so expire (pgBackRest runs it only at a backup's end) left
// obsolete backups listed, and the agent missed scans for days. A backup
// taken since the read becomes the base and every listed one goes: the
// listed ones bound nothing, and the whole window is in the band, never
// "covered" for the read's last 19 days.
func TestPurgeBandPastAStoppedRetention(t *testing.T) {
	day := 24 * time.Hour
	read := now.Add(-10 * day)
	first, last := read.Add(-30*day), read
	in := timelineInput{facts: recoveryFacts{now: now}, scannedAt: &read, refresh: day}
	for _, c := range []struct {
		name  string
		spans []interval
	}{
		{"a lone backup 30 d old", []interval{{from: first, to: last, backupID: "old"}}},
		{"two backups 30 d and 28 d old", []interval{{from: first, to: last, backupID: "a"}, {from: first.Add(2 * day), to: last, backupID: "b"}}},
	} {
		if got := purgeBand(in, c.spans, first, last); got != last.Sub(first) {
			t.Errorf("%s, read 10 d ago: band %s, want the whole window %s", c.name, got, last.Sub(first))
		}
	}
}

// TestPurgeBandFromATimeRetention (round 16, PG P3-1): a pgBackRest
// time-based full retention places the window's start from the clock:
// expire keeps the newest full at or before now minus the retention, so
// the band runs to it whatever the listing still holds, and past the read
// to the whole window (the base may be an unlisted full).
func TestPurgeBandFromATimeRetention(t *testing.T) {
	day := 24 * time.Hour
	first, last := now.Add(-40*day), now.Add(-time.Minute)
	spans := []interval{{from: first, to: last, backupID: "a"}, {from: now.Add(-35 * day), to: last, backupID: "b"}, {from: now.Add(-3 * day), to: last, backupID: "c"}}
	read := now.Add(-2 * time.Hour)
	base := timelineInput{facts: recoveryFacts{now: now}, scannedAt: &read, refresh: 5 * time.Minute}
	for _, c := range []struct {
		name     string
		retained time.Duration
		read     time.Time
		want     time.Duration
	}{
		{"30 d, read 2 h ago: to the newest full before now-30d", 30 * day, read, 5 * day},
		{"30 d past a long gap: still to that full", 30 * day, now.Add(-4 * day), 5 * day},
		{"7 d, read 10 d ago: the base may be unlisted", 7 * day, now.Add(-10 * day), last.Sub(first)},
		{"50 d: nothing expired yet, the elapsed band", 50 * day, read, 2*time.Hour + 5*time.Minute},
	} {
		in := base
		in.retainedFor, in.scannedAt = c.retained, &c.read
		if got := purgeBand(in, spans, first, last); got != c.want {
			t.Errorf("%s: band %s, want %s", c.name, got, c.want)
		}
	}
}

// TestRetainedForReadsOnlyATimeRetention: only a configured time-based
// pgBackRest full retention is a clock bound; a count, an unset one or a
// Barman summary is none, and an absurd number of days stays a duration.
func TestRetainedForReadsOnlyATimeRetention(t *testing.T) {
	snap := func(r *evidencev1alpha1.PGBackRestRetention) *evidencev1alpha1.RepositoryEvidenceSnapshot {
		return &evidencev1alpha1.RepositoryEvidenceSnapshot{Details: evidencev1alpha1.Details{Type: "pgbackrest", PGBackRest: &evidencev1alpha1.PGBackRestSummary{Retention: r}}}
	}
	seven, huge := uint64(7), uint64(evidencev1alpha1.MaxPGBackRestRetention)
	for _, c := range []struct {
		name string
		snap *evidencev1alpha1.RepositoryEvidenceSnapshot
		want time.Duration
	}{
		{"time, 7 days", snap(&evidencev1alpha1.PGBackRestRetention{Configured: true, Full: &seven, FullType: evidencev1alpha1.RetentionFullTime}), 7 * 24 * time.Hour},
		{"count", snap(&evidencev1alpha1.PGBackRestRetention{Configured: true, Full: &seven, FullType: evidencev1alpha1.RetentionFullCount}), 0},
		{"not configured", snap(&evidencev1alpha1.PGBackRestRetention{Full: &seven, FullType: evidencev1alpha1.RetentionFullTime}), 0},
		{"no retention", snap(nil), 0},
		{"Barman", &evidencev1alpha1.RepositoryEvidenceSnapshot{}, 0},
		{"absurd days", snap(&evidencev1alpha1.PGBackRestRetention{Configured: true, Full: &huge, FullType: evidencev1alpha1.RetentionFullTime}), maxRetainedDays * 24 * time.Hour},
	} {
		if got := retainedFor(c.snap); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}
