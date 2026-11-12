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
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/evidence"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// fullsInput is a pgBackRest repository listing one full per stop, each
// drawn from its stop to last, read at read, with ends the CNPG Backups
// that ended since.
func fullsInput(read time.Time, stops []time.Time, last time.Time, ends ...time.Time) (timelineInput, []interval) {
	in := timelineInput{facts: recoveryFacts{now: now}, scannedAt: &read, refresh: time.Hour, pgBackRest: true}
	spans := make([]interval, 0, len(stops))
	for i, stop := range stops {
		id := fmt.Sprintf("f%d", i+1)
		end := stop
		in.backups = append(in.backups, evidence.BackupItem{ID: id, EndAt: &end, PgBackRest: &evidence.PgBackRestItem{Type: "full", Usable: true}})
		spans = append(spans, interval{from: stop, to: last, backupID: id})
	}
	for i := range ends {
		in.facts.backupEnds = append(in.facts.backupEnds, &ends[i])
	}
	return in, spans
}

// TestR17CountBurstPastAMissedScan (round 17, PG P3-1, the reviewer's
// probe made a pgBackRest repository): repo-retention-full=4 with four
// weekly fulls, the agent missed scans for 6 h, and on-demand fulls were
// taken since the read. Each expired the oldest listed full: four expire
// them all, and one only the oldest.
func TestR17CountBurstPastAMissedScan(t *testing.T) {
	day := 24 * time.Hour
	read := now.Add(-6 * time.Hour)
	first, last := read.Add(-21*day), read
	stops := []time.Time{first, first.Add(7 * day), first.Add(14 * day), first.Add(21*day - time.Hour)}
	window := last.Sub(first)
	since := func(n int) []time.Time {
		out := make([]time.Time, 0, n)
		for i := range n {
			out = append(out, read.Add(time.Duration(i+1)*time.Hour))
		}
		return out
	}
	for _, c := range []struct {
		name   string
		n      int
		policy pgbackrestPolicy
		want   time.Duration
	}{
		{"four fulls since: every listed one may be gone", 4, pgbackrestPolicy{fulls: 4}, window},
		{"one full since: the oldest goes", 1, pgbackrestPolicy{fulls: 4}, 7 * day},
		{"three since: the newest listed is left", 3, pgbackrestPolicy{fulls: 4}, 21*day - time.Hour},
		{"one since, six kept: nothing listed goes", 1, pgbackrestPolicy{fulls: 6}, 0},
		{"one since, the policy not reported", 1, pgbackrestPolicy{unknown: true}, window},
		// P3-2: archive=1 keeps WAL only from the newest full: one taken
		// since leaves every listed one its consistency range alone.
		{"archive 1, one since", 1, pgbackrestPolicy{fulls: 4, archive: 1, archiveTypes: map[string]bool{"full": true}}, window},
		{"archive 2, one since: from the newest listed", 1, pgbackrestPolicy{fulls: 4, archive: 2, archiveTypes: map[string]bool{"full": true}}, 21*day - time.Hour},
	} {
		in, spans := fullsInput(read, stops, last, since(c.n)...)
		in.policy = c.policy
		got := purgeBand(in, spans, first, last)
		if c.want == 0 {
			if listed := listedPurgeBand(in, spans, first, last); got != listed {
				t.Errorf("%s: band %s, want the listed band %s", c.name, got, listed)
			}
			continue
		}
		if got != c.want {
			t.Errorf("%s: band %s of %s, want %s", c.name, got, window, c.want)
		}
	}
	// The Backups not listed: how many ran expire is not known.
	in, spans := fullsInput(read, stops, last)
	in.policy, in.facts.backupsUnread = pgbackrestPolicy{fulls: 4}, true
	if got := purgeBand(in, spans, first, last); got != window {
		t.Errorf("Backups unread: band %s, want the whole window %s", got, window)
	}
	// A listed full with no time: its place, which decides what expire
	// removed, is not known.
	in, spans = fullsInput(read, stops, last, read.Add(time.Hour))
	in.policy = pgbackrestPolicy{fulls: 4}
	in.backups = append(in.backups, evidence.BackupItem{ID: "f0", PgBackRest: &evidence.PgBackRestItem{Type: "full"}})
	if got := purgeBand(in, spans, first, last); got != window {
		t.Errorf("a full with no time: band %s, want the whole window %s", got, window)
	}
	// None since: expire has not run since the read, the band is the
	// listing's.
	in, spans = fullsInput(read, stops, last, read.Add(-time.Hour))
	in.policy = pgbackrestPolicy{fulls: 4}
	if got, listed := purgeBand(in, spans, first, last), listedPurgeBand(in, spans, first, last); got != listed {
		t.Errorf("no backup since: band %s, want the listed band %s", got, listed)
	}
}

// TestPgBackRestPolicyOf reads the retention block: a count bound for a
// count-based full retention, an archive bound when it may keep less WAL
// than the fulls, and unknown without the block.
func TestPgBackRestPolicyOf(t *testing.T) {
	snap := func(r *evidencev1alpha1.PGBackRestRetention) *evidencev1alpha1.RepositoryEvidenceSnapshot {
		return &evidencev1alpha1.RepositoryEvidenceSnapshot{Details: evidencev1alpha1.Details{Type: "pgbackrest", PGBackRest: &evidencev1alpha1.PGBackRestSummary{Retention: r}}}
	}
	u := func(v uint64) *uint64 { return &v }
	for _, c := range []struct {
		name          string
		snap          *evidencev1alpha1.RepositoryEvidenceSnapshot
		fulls, archiv int
		types         []string
		unknown       bool
	}{
		{"no snapshot", nil, 0, 0, nil, true},
		{"no retention block", snap(nil), 0, 0, nil, true},
		{"count 4, archive defaulted", snap(&evidencev1alpha1.PGBackRestRetention{Configured: true, Full: u(4), FullType: "count", ArchiveType: "full", EffectiveArchive: u(4)}), 4, 4, []string{"full"}, false},
		{"count 4, archive 1", snap(&evidencev1alpha1.PGBackRestRetention{Configured: true, Full: u(4), FullType: "count", Archive: u(1), ArchiveType: "full", EffectiveArchive: u(1)}), 4, 1, []string{"full"}, false},
		{"time 30, archive defaulted", snap(&evidencev1alpha1.PGBackRestRetention{Configured: true, Full: u(30), FullType: "time", ArchiveType: "full", EffectiveArchive: u(5)}), 0, 0, nil, false},
		{"time 30, archive 2", snap(&evidencev1alpha1.PGBackRestRetention{Configured: true, Full: u(30), FullType: "time", Archive: u(2), ArchiveType: "full", EffectiveArchive: u(2)}), 0, 2, []string{"full"}, false},
		{"archive type diff from repo-retention-diff", snap(&evidencev1alpha1.PGBackRestRetention{Configured: true, Full: u(2), FullType: "count", Diff: u(3), ArchiveType: "diff", EffectiveArchive: u(3)}), 2, 3, []string{"diff", "full"}, false},
		{"archive type incr", snap(&evidencev1alpha1.PGBackRestRetention{Configured: true, Full: u(2), FullType: "count", Archive: u(5), ArchiveType: "incr", EffectiveArchive: u(5)}), 2, 5, []string{"diff", "full", "incr"}, false},
		{"full unset: never expires", snap(&evidencev1alpha1.PGBackRestRetention{FullType: "count", ArchiveType: "full"}), 0, 0, nil, false},
	} {
		got := pgbackrestPolicyOf(c.snap)
		var types []string
		for k := range got.archiveTypes {
			types = append(types, k)
		}
		slices.Sort(types)
		if got.unknown != c.unknown || got.fulls != c.fulls || got.archive != c.archiv || !slices.Equal(types, c.types) {
			t.Errorf("%s: %+v (types %v), want fulls %d archive %d types %v unknown %v", c.name, got, types, c.fulls, c.archiv, c.types, c.unknown)
		}
	}
}

// TestBackupsSince counts the Backups that ended after the read, a
// completed one with no stop time included, and none when the Backups
// were not listed.
func TestBackupsSince(t *testing.T) {
	read := now.Add(-time.Hour)
	backup := func(phase string, stopped *time.Time) *cnpgv1.Backup {
		b := &cnpgv1.Backup{Status: cnpgv1.BackupStatus{Phase: cnpgv1.BackupPhase(phase)}}
		if stopped != nil {
			b.Status.StoppedAt = &metav1.Time{Time: *stopped}
		}
		return b
	}
	before, after := read.Add(-10*time.Minute), read.Add(10*time.Minute)
	g := &gathered{readable: map[string]bool{cache.SourceBackups: true}, backups: []*cnpgv1.Backup{
		backup("completed", &before), backup("completed", &after), backup("failed", &after), backup("completed", nil), backup("running", nil),
	}}
	ends, unread := backupEndsOf(g)
	f := recoveryFacts{backupEnds: ends, backupsUnread: unread}
	if n, ok := f.backupsSince(read); !ok || n != 3 {
		t.Errorf("backupsSince = %d, %v; want 3 (two ended after the read, one completed with no stop time)", n, ok)
	}
	g.failed = map[string]bool{cache.SourceBackups: true}
	ends, unread = backupEndsOf(g)
	if _, ok := (recoveryFacts{backupEnds: ends, backupsUnread: unread}).backupsSince(read); ok {
		t.Error("a failed Backups read counted")
	}
}

// TestPurgeBandNeverEndsBeforeTheFirstRestorePoint (round 17, PG idea 4):
// a property over random pgBackRest policies (count or time fulls, with
// or without an archive retention below them), random fulls after the
// read and random ages. A simulation of expire, run independently at
// each new backup's end, says which listed restore points still have
// their WAL; every point the drawn spans claim past the band must be one.
func TestPurgeBandNeverEndsBeforeTheFirstRestorePoint(t *testing.T) {
	rng := rand.New(rand.NewPCG(17, 3))
	day := 24 * time.Hour
	for iter := range 4000 {
		age := time.Duration(rng.Int64N(int64(10 * day)))
		read := now.Add(-age)
		listed := 1 + rng.IntN(6)
		stops := make([]time.Time, listed)
		at := read.Add(-time.Duration(rng.Int64N(int64(day))))
		for i := listed - 1; i >= 0; i-- {
			stops[i] = at
			at = at.Add(-time.Duration(1 + rng.Int64N(int64(8*day))))
		}
		first, last := stops[0], read
		var news []time.Time
		for range rng.IntN(6) {
			if age > 0 {
				news = append(news, read.Add(time.Duration(1+rng.Int64N(int64(age)))))
			}
		}
		slices.SortFunc(news, func(a, b time.Time) int { return a.Compare(b) })
		r := &evidencev1alpha1.PGBackRestRetention{Configured: true, ArchiveType: "full"}
		timeBased := rng.IntN(2) == 0
		var k, days, archive int
		if timeBased {
			days = 1 + rng.IntN(30)
			r.FullType, r.Full = "time", new(uint64(days))
		} else {
			k = 1 + rng.IntN(6)
			r.FullType, r.Full, r.EffectiveArchive = "count", new(uint64(k)), new(uint64(k))
			archive = k
		}
		if rng.IntN(2) == 0 {
			archive = 1 + rng.IntN(max(k, 3))
			r.Archive, r.EffectiveArchive = new(uint64(archive)), new(uint64(archive))
		}
		// The truth: expire at each new backup's end.
		alive := slices.Clone(stops)
		for _, end := range news {
			alive = append(alive, end)
			if timeBased {
				cut := end.Add(-time.Duration(days) * day)
				keep := alive[:0:0]
				newestBefore := -1
				for i, s := range alive {
					if s.Before(cut) {
						newestBefore = i
					}
				}
				for i, s := range alive {
					if !s.Before(cut) || i == newestBefore {
						keep = append(keep, s)
					}
				}
				alive = keep
			} else if len(alive) > k {
				alive = alive[len(alive)-k:]
			}
		}
		// Continuous WAL runs from the archive-retention full: the
		// archive-th newest of those alive (of the listed at the read).
		walFrom := first
		if archive > 0 {
			pool := alive
			if len(news) == 0 {
				pool = stops
			}
			if len(pool) >= archive {
				walFrom = pool[len(pool)-archive]
			}
		}
		atRead := first
		if archive > 0 && len(stops) >= archive {
			atRead = stops[len(stops)-archive]
		}
		in := timelineInput{facts: recoveryFacts{now: now}, scannedAt: &read, refresh: time.Duration(1 + rng.Int64N(int64(day))), pgBackRest: true}
		in.retainedFor = retainedFor(&evidencev1alpha1.RepositoryEvidenceSnapshot{Details: evidencev1alpha1.Details{Type: "pgbackrest", PGBackRest: &evidencev1alpha1.PGBackRestSummary{Retention: r}}})
		in.policy = pgbackrestPolicyOf(&evidencev1alpha1.RepositoryEvidenceSnapshot{Details: evidencev1alpha1.Details{Type: "pgbackrest", PGBackRest: &evidencev1alpha1.PGBackRestSummary{Retention: r}}})
		spans := make([]interval, 0, len(stops))
		for i, stop := range stops {
			id := fmt.Sprintf("f%d", i+1)
			end := stop
			in.backups = append(in.backups, evidence.BackupItem{ID: id, EndAt: &end, PgBackRest: &evidence.PgBackRestItem{Type: "full", Usable: true}})
			to := last
			if stop.Before(atRead) {
				// The WAL past an older full's own range was already
				// expired at the read: the agent drew its point alone.
				to = stop
			}
			spans = append(spans, interval{from: stop, to: to, backupID: id})
		}
		for i := range news {
			in.facts.backupEnds = append(in.facts.backupEnds, &news[i])
		}
		band := purgeBand(in, spans, first, last)
		restorable := func(p time.Time) bool {
			for _, s := range alive {
				if s.Equal(p) || !s.After(p) && !s.Before(walFrom) {
					return true
				}
			}
			return false
		}
		if band >= last.Sub(first) {
			continue // the whole window is in the band: nothing claimed
		}
		from := first.Add(band)
		for _, sp := range spans {
			for _, p := range []time.Time{sp.from, sp.from.Add(time.Minute), sp.to} {
				if p.Before(from) || p.After(sp.to) || p.After(last) {
					continue
				}
				if !restorable(p) {
					t.Fatalf("iteration %d: %s after the band (%s of %s) is claimed but gone: policy %+v archive %d, listed %v, since %v, alive %v, wal from %s",
						iter, p, band, last.Sub(first), in.policy, archive, stops, news, alive, walFrom)
				}
			}
		}
	}
}
