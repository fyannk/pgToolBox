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

	"github.com/fyannk/pgToolBox/internal/console/evidence"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// expireAt is pgBackRest's expire run at run over list (oldest first):
// count-based, the chains of the K newest fulls are kept; time-based, the
// chains from the newest full stopped before run minus the retention.
func expireAt(list []chainBackup, run time.Time, k int, retained time.Duration) []chainBackup {
	keepFrom := -1
	if retained > 0 {
		cut := run.Add(-retained)
		for _, b := range list {
			if b.typ == "full" && b.stop.Before(cut) {
				keepFrom = b.full
			}
		}
	} else {
		var fulls []int
		for _, b := range list {
			if b.typ == "full" {
				fulls = append(fulls, b.full)
			}
		}
		if len(fulls) > k {
			keepFrom = fulls[len(fulls)-k]
		}
	}
	if keepFrom < 0 {
		return list
	}
	var out []chainBackup
	for _, b := range list {
		if b.full >= keepFrom {
			out = append(out, b)
		}
	}
	return out
}

// randomChain is n backups stopped before newest, oldest first, the first
// a full, each later one a full, a diff or an incremental.
func randomChain(rng *rand.Rand, n int, newest time.Time) []chainBackup {
	types := []string{"full", "diff", "incr"}
	stops := make([]time.Time, n)
	at := newest
	for i := n - 1; i >= 0; i-- {
		stops[i] = at
		at = at.Add(-time.Duration(1 + rng.Int64N(int64(48*time.Hour))))
	}
	seq := make([]chainBackup, 0, n)
	lastFull := -1
	for i := range n {
		typ := "full"
		if i > 0 {
			typ = types[rng.IntN(3)]
		}
		if typ == "full" {
			lastFull = i
		}
		seq = append(seq, chainBackup{id: fmt.Sprintf("b%d", i), typ: typ, stop: stops[i], full: lastFull})
	}
	return seq
}

// TestR19SteadyStateNoPendingBand (round 19, PG probe): the listing is what
// expire left after every backup, and no Backup ended since the read. The
// pending rule keeps no band: a healthy cluster is not shown uncertain.
// Count-based (with an archive retention) and time-based policies.
func TestR19SteadyStateNoPendingBand(t *testing.T) {
	rng := rand.New(rand.NewPCG(19, 1))
	day := 24 * time.Hour
	types := []string{"full", "diff", "incr"}
	fired := 0
	for iter := range 50000 {
		read := now.Add(-time.Duration(rng.Int64N(int64(4 * time.Minute))))
		seq := randomChain(rng, 1+rng.IntN(14), read.Add(-2*scanSlack-time.Duration(rng.Int64N(int64(day)))))
		k := 1 + rng.IntN(4)
		atype := types[rng.IntN(3)]
		inT := func(typ string) bool {
			switch atype {
			case "full":
				return typ == "full"
			case "diff":
				return typ != "incr"
			}
			return true
		}
		a := 1 + rng.IntN(5)
		timeBased := rng.IntN(3) == 0
		var retained time.Duration
		if timeBased {
			retained = time.Duration(1+rng.IntN(10)) * day
		}
		var alive []chainBackup
		walFrom := time.Time{}
		for _, b := range seq {
			alive = expireAt(append(alive, b), b.stop, k, retained)
			if timeBased {
				continue
			}
			cnt := 0
			for i := len(alive) - 1; i >= 0; i-- {
				if inT(alive[i].typ) {
					if cnt++; cnt == a {
						if alive[i].stop.After(walFrom) {
							walFrom = alive[i].stop
						}
						break
					}
				}
			}
		}
		first, last := alive[0].stop, read
		r := &evidencev1alpha1.PGBackRestRetention{Configured: true, FullType: "count", Full: new(uint64(k)), Archive: new(uint64(a)), ArchiveType: atype, EffectiveArchive: new(uint64(a))}
		if timeBased {
			r = &evidencev1alpha1.PGBackRestRetention{Configured: true, FullType: "time", Full: new(uint64(retained / day))}
		}
		snap := &evidencev1alpha1.RepositoryEvidenceSnapshot{Details: evidencev1alpha1.Details{Type: "pgbackrest", PGBackRest: &evidencev1alpha1.PGBackRestSummary{Retention: r}}}
		in := timelineInput{facts: recoveryFacts{now: now}, scannedAt: &read, refresh: 5 * time.Minute, pgBackRest: true}
		in.policy = pgbackrestPolicyOf(snap)
		in.retainedFor = retainedFor(snap)
		spans := make([]interval, 0, len(alive))
		for _, b := range alive {
			end := b.stop
			in.backups = append(in.backups, evidence.BackupItem{ID: b.id, EndAt: &end, PgBackRest: &evidence.PgBackRestItem{Type: b.typ, Usable: true}})
			to := last
			if b.stop.Before(walFrom) {
				to = b.stop
			}
			spans = append(spans, interval{from: b.stop, to: to, backupID: b.id})
		}
		for i := range seq {
			e := seq[i].stop
			in.facts.backupEnds = append(in.facts.backupEnds, &e)
		}
		if band, ok := sinceReadBand(in, spans, first, last); ok {
			if fired++; fired < 5 {
				t.Errorf("iteration %d: steady state keeps a band of %s over %s: time=%v k=%d a=%d type=%s alive=%v wal from %s",
					iter, band, last.Sub(first), timeBased, k, a, atype, alive, walFrom)
			}
		}
	}
	if fired > 0 {
		t.Errorf("%d steady states kept a band", fired)
	}
}

// TestR19TimePendingAfterRead (round 19, PG P3-A): a time-based expire the
// listing shows pending runs after the read, not at the newest listed end
// E: a full stopped between E minus the retention and the run minus it
// is the base it keeps, and the older one it removes is not covered.
func TestR19TimePendingAfterRead(t *testing.T) {
	day := 24 * time.Hour
	read := now.Add(-time.Minute)
	newest := read.Add(-3 * time.Minute) // the newest full; its CNPG Backup is not counted
	retained := 7 * day
	f0 := newest.Add(-retained - 2*day)
	f1 := newest.Add(-retained + 90*time.Second) // after E-N, before run-N (run >= read)
	stops := []time.Time{f0, f1, newest.Add(-3 * day), newest}
	in, spans := fullsInput(read, stops, read, stops...)
	in.retainedFor = retained
	in.refresh = 5 * time.Minute
	if band := purgeBand(in, spans, f0, read); band < f1.Sub(f0) {
		t.Errorf("band %s: f0, which the pending expire removes, is claimed covered (want at least %s)", band, f1.Sub(f0))
	}
	// The newest end older than one refresh interval: its expire ran
	// before the read, at that end, and the listing is what it left.
	in.refresh = time.Minute
	if band := purgeBand(in, spans, f0, read); band >= f1.Sub(f0) {
		t.Errorf("band %s: an expire that ran before the read is timed at the read", band)
	}
}

// TestPurgeBandHoldsOverTimeChains is TestPurgeBandHoldsOverChains for a
// time-based full retention (round 19, PG plan 3): random chains, expire
// at each backup's end, the newest listed one's maybe pending (it runs
// during the scan, after the read), backups since the read each expiring
// at its end. Every restore point after the band is still restorable, and
// a steady state (nothing pending, nothing since) keeps no pending band
// unless a full stopped within the scan N days before it (the instant
// the console cannot tell from an expire still to run).
func TestPurgeBandHoldsOverTimeChains(t *testing.T) {
	rng := rand.New(rand.NewPCG(19, 3))
	day := 24 * time.Hour
	partial, pending, steady := 0, 0, 0
	for iter := range 20000 {
		age := time.Duration(1 + rng.Int64N(int64(3*day)))
		read := now.Add(-age)
		took := time.Duration(rng.Int64N(int64(3 * time.Minute)))
		completed := read.Add(took)
		newest := read.Add(-time.Duration(1 + rng.Int64N(int64(day))))
		isPending := rng.IntN(3) == 0
		if isPending || rng.IntN(4) == 0 {
			newest = read.Add(-time.Duration(rng.Int64N(int64(3 * scanSlack))))
		}
		seq := randomChain(rng, 1+rng.IntN(10), newest)
		listedN := len(seq)
		retained := time.Duration(1+rng.IntN(10)) * day
		lastFull := seq[listedN-1].full
		news := make([]time.Time, 0, 4)
		for range rng.IntN(4) {
			news = append(news, read.Add(time.Duration(1+rng.Int64N(int64(age)))))
		}
		types := []string{"full", "diff", "incr"}
		slices.SortFunc(news, func(a, b time.Time) int { return a.Compare(b) })
		for _, e := range news {
			typ := types[rng.IntN(3)]
			if typ == "full" {
				lastFull = len(seq)
			}
			seq = append(seq, chainBackup{id: fmt.Sprintf("b%d", len(seq)), typ: typ, stop: e, full: lastFull})
		}
		// The listing: expire after each listed backup at its end, the
		// newest one's still to run when pending.
		var listed []chainBackup
		for i, b := range seq[:listedN] {
			listed = append(listed, b)
			if i < listedN-1 || !isPending {
				listed = expireAt(listed, b.stop, 0, retained)
			}
		}
		alive := listed
		if isPending {
			run := read.Add(time.Duration(rng.Int64N(int64(took) + 1)))
			alive = expireAt(alive, run, 0, retained)
		}
		for _, b := range seq[listedN:] {
			alive = expireAt(append(alive, b), b.stop, 0, retained)
		}
		first, last := listed[0].stop, read
		snap := &evidencev1alpha1.RepositoryEvidenceSnapshot{Details: evidencev1alpha1.Details{Type: "pgbackrest", PGBackRest: &evidencev1alpha1.PGBackRestSummary{Retention: &evidencev1alpha1.PGBackRestRetention{Configured: true, FullType: "time", Full: new(uint64(retained / day))}}}}
		in := timelineInput{facts: recoveryFacts{now: now}, scannedAt: &completed, scanDuration: took, refresh: 5 * time.Minute, pgBackRest: true}
		in.policy = pgbackrestPolicyOf(snap)
		in.retainedFor = retainedFor(snap)
		spans := make([]interval, 0, len(listed))
		for _, b := range listed {
			end := b.stop
			in.backups = append(in.backups, evidence.BackupItem{ID: b.id, EndAt: &end, PgBackRest: &evidence.PgBackRestItem{Type: b.typ, Usable: true}})
			spans = append(spans, interval{from: b.stop, to: last, backupID: b.id})
		}
		for i := range seq {
			end := seq[i].stop
			in.facts.backupEnds = append(in.facts.backupEnds, &end)
		}
		if !isPending && len(news) == 0 && newest.Before(read.Add(-scanSlack)) {
			// Over-widening is allowed only where the console cannot tell
			// a pending expire: the newest end within one refresh of the
			// read and a full stopped in [end-N, completed-N).
			ambiguous := false
			if !newest.Before(read.Add(-in.refresh)) {
				for _, b := range listed {
					if b.typ == "full" && !b.stop.Before(newest.Add(-retained)) && b.stop.Before(completed.Add(-retained)) {
						ambiguous = true
					}
				}
			}
			if band, ok := sinceReadBand(in, spans, first, last); ok && !ambiguous {
				t.Fatalf("iteration %d: steady state keeps a band of %s over %s: listed=%v retained=%s", iter, band, last.Sub(first), listed, retained)
			}
			steady++
		}
		band := purgeBand(in, spans, first, last)
		if band >= last.Sub(first) {
			continue
		}
		partial++
		if isPending {
			pending++
		}
		from := first.Add(band)
		for _, sp := range spans {
			for _, p := range []time.Time{sp.from, sp.from.Add(time.Minute), sp.to} {
				if p.Before(from) || p.After(last) {
					continue
				}
				if !p.Before(alive[0].stop) {
					continue
				}
				t.Fatalf("iteration %d: %s after the band (%s of %s) is claimed but gone: pending=%v listed=%v since=%v alive=%v retained=%s",
					iter, p, band, last.Sub(first), isPending, listed, news, alive, retained)
			}
		}
	}
	if partial == 0 || pending == 0 || steady == 0 {
		t.Errorf("the property never drew a partial band (%d), one on a pending listing (%d) or a steady state (%d)", partial, pending, steady)
	}
}
