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

package sharedstate

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// The review's repro (round 10): with the document full of identities
// holding two entries (one sign-out each), a victim holding four is the
// person holding the most; a newcomer's sign-out took the place of the
// victim's NEWEST sign-out, the one a cookie stolen since needs, and the
// other replicas accepted the stolen cookie again. Each person now keeps
// their latest sign-outs; the one recorded longest ago goes.
func TestAFullDocumentKeepsAPersonsLatestSignOuts(t *testing.T) {
	clock := &testClock{now: t0}
	s := synced(t, Options{Name: "console-state", Now: clock.Now})
	for i := range (MaxSignOuts - 4) / 2 {
		s.RecordSignOut(fmt.Sprintf("a-%d", i), tag(fmt.Sprint("sa-", i)), t0.Add(12*time.Hour))
		s.RecordSignOut(fmt.Sprintf("a-%d-b", i), tag(fmt.Sprint("sa-", i)), t0.Add(12*time.Hour))
	}
	s.RecordSignOut("victim-old", tag("victim"), t0.Add(12*time.Hour))
	s.RecordSignOut("victim-old-b", tag("victim"), t0.Add(12*time.Hour))
	clock.Add(time.Hour)
	s.RecordSignOut("victim-new", tag("victim"), clock.Now().Add(12*time.Hour))
	s.RecordSignOut("victim-new-b", tag("victim"), clock.Now().Add(12*time.Hour))
	if len(s.signedOut) != MaxSignOuts {
		t.Fatalf("the document holds %d entries, want it full at %d", len(s.signedOut), MaxSignOuts)
	}
	clock.Add(time.Minute)
	s.RecordSignOut("attacker", tag("fresh-sa"), clock.Now().Add(12*time.Hour))
	s.RecordSignOut("attacker-b", tag("fresh-sa"), clock.Now().Add(12*time.Hour))
	if !s.SignedOut("victim-new") || !s.SignedOut("victim-new-b") {
		t.Error("the victim's newest sign-out was taken for a newcomer")
	}
	if s.SignedOut("victim-old") || s.SignedOut("victim-old-b") {
		t.Error("the victim's oldest sign-out stayed while a newcomer took a place")
	}
	if !s.SignedOut("attacker") || !s.SignedOut("attacker-b") {
		t.Error("the newcomer was refused")
	}
	// A merge keeps what the eviction kept.
	s.mu.Lock()
	dropped := planSignOutCaps(s.signedOut, s.latestRecordedLocked())
	s.mu.Unlock()
	if len(dropped) != 0 {
		t.Errorf("capping the document again drops %d entries", len(dropped))
	}
}

// Property: the entry a newcomer's sign-out evicts from a full document
// is what planSignOutCaps drops from the document with it added, so a
// merge never undoes an eviction.
func TestEvictionIsWhatThePlannerDrops(t *testing.T) {
	steps := 20000
	if testing.Short() || raceEnabled {
		steps = 4000
	}
	r := rand.New(rand.NewPCG(7, 7))
	clock := &testClock{now: t0}
	s := synced(t, Options{Name: "console-state", Now: clock.Now})
	people := make([]string, 1, 91)
	for i := range 90 {
		people = append(people, tag(fmt.Sprint("q", i)))
	}
	checked := 0
	for n := range steps {
		if n%3 == 0 {
			clock.Add(time.Second)
		}
		who := people[r.IntN(len(people))]
		s.mu.Lock()
		s.pruneSignOutsLocked()
		_, held := s.ownEntriesLocked(who)
		limit := MaxSignOutsPerSubject
		if who == "" {
			limit = MaxUnnamedSignOuts
		}
		if len(s.signedOut) >= MaxSignOuts && held < limit {
			h, e := sessionHash(fmt.Sprint("s-", n)), signOut{Expiry: clock.Now().Add(time.Hour).Unix(), Recorded: clock.Now().Unix(), Subject: who}
			if victim, ok := s.evictionLocked(h, e); ok {
				m := maps.Clone(s.signedOut)
				m[h] = e
				checked++
				if d := planSignOutCaps(m, s.latestRecordedLocked()); len(d) != 1 || d[victim] == "" {
					s.mu.Unlock()
					t.Fatalf("step %d: eviction of %s, the planner drops %v", n, victim, d)
				}
			}
		}
		s.mu.Unlock()
		s.RecordSignOut(fmt.Sprint("s-", n), who, clock.Now().Add(time.Duration(30+r.IntN(3000))*time.Minute))
	}
	if checked == 0 {
		t.Fatal("no eviction was checked")
	}
}

// Property, harsher than TestSharedConfigMapConverges: up to five
// replicas, bursts without a write in between, the same person signing
// out on every replica in the same second, and, every other seed, drill
// results and grants at their bounds so that MaxData is reached: once
// quiescent, every replica holds the same document, within MaxData.
func TestReplicasConvergeAtTheCeiling(t *testing.T) {
	seeds, steps := []uint64{0, 1}, 4000
	if testing.Short() || raceEnabled {
		// The ceiling seed only: what the race detector looks at is the
		// merge under bursts, which a few hundred steps already reach.
		seeds, steps = []uint64{0}, 400
	}
	for _, seed := range seeds {
		r := rand.New(rand.NewPCG(seed, 1234))
		clock := &testClock{now: t0}
		n := 2 + r.IntN(4)
		st := make([]*Store, n)
		for i := range st {
			st[i] = synced(t, Options{Name: "console-state", Now: clock.Now})
		}
		ceiling := seed%2 == 0
		if ceiling {
			st[0].mu.Lock()
			fillToTheBounds(st[0], true, drillAtTheBounds)
			st[0].signedOut = map[string]signOut{}
			st[0].mu.Unlock()
		}
		cm := &corev1.ConfigMap{}
		flush := func(i int) bool {
			s := st[i]
			s.mu.Lock()
			defer s.mu.Unlock()
			s.mergeLocked(cm)
			s.pruneLocked()
			d := s.desiredLocked()
			changed := !maps.Equal(d.Data, cm.Data)
			size := 0
			for k, v := range d.Data {
				size += len(k) + len(v)
			}
			if size > MaxData {
				t.Fatalf("seed %d: data %d bytes, past MaxData", seed, size)
			}
			cm = d
			s.base = d.DeepCopy()
			return changed
		}
		people := []string{""}
		for i := range 10 + r.IntN(110) {
			people = append(people, tag(fmt.Sprint("p", i)))
		}
		for k := range steps {
			if k%5 == 0 {
				clock.Add(time.Second)
			}
			switch x := r.IntN(10); {
			case x == 0:
				flush(r.IntN(n))
			case x == 1 && ceiling:
				st[r.IntN(n)].KeepDrill(DrillResult{
					Namespace: fmt.Sprintf("%063d", k), Name: strings.Repeat("n", 240) + strconv.Itoa(k), SourceNamespace: "ns",
					Source: strings.Repeat("c", 253), SourceUID: strings.Repeat("u", 36), Backup: strings.Repeat("b", 253), BackupLabel: strings.Repeat("l", 64),
					Result: "passed", Reason: strings.Repeat("\x01", 512), StartedAt: clock.Now().Unix() - 10, FinishedAt: clock.Now().Unix(),
				})
			default:
				who := people[r.IntN(len(people))]
				until := clock.Now().Add(time.Duration(600+r.IntN(4000)) * time.Second)
				if r.IntN(20) == 0 {
					// The same person on every replica in the same second.
					for i := range st {
						st[i].RecordSignOut(fmt.Sprintf("c%d-%d-%d", seed, k, i), who, until)
					}
					continue
				}
				st[r.IntN(n)].RecordSignOut(fmt.Sprintf("x%d-%d", seed, k), who, until)
			}
		}
		for round := range 20 {
			changed := false
			for i := range st {
				if flush(i) {
					changed = true
				}
			}
			if !changed {
				break
			}
			if round == 19 {
				t.Fatalf("seed %d: still writing after 20 rounds", seed)
			}
		}
		for i := 1; i < n; i++ {
			if !maps.Equal(st[i].signedOut, st[0].signedOut) || !maps.Equal(st[i].facts, st[0].facts) || len(st[i].drills) != len(st[0].drills) {
				t.Fatalf("seed %d: replica %d differs", seed, i)
			}
		}
		if !maps.Equal(capSet(st[0].signedOut), st[0].signedOut) {
			t.Fatalf("seed %d: not capped", seed)
		}
	}
}

// The review's repro (round 11): with everyone at the same number of
// entries, the leftover places went in tag order, so after some 85 fresh
// identities a person's newest sign-out was the one evicted. Ties now go
// to the entries recorded latest: the victim's newest sign-out, and their
// only one, outlive every older entry.
func TestEqualHoldersKeepTheirNewestSignOut(t *testing.T) {
	clock := &testClock{now: t0}
	s := synced(t, Options{Name: "console-state", Now: clock.Now})
	for i := range (MaxSignOuts - 3) / 2 {
		s.RecordSignOut(fmt.Sprintf("a-%d", i), tag(fmt.Sprint("sa-", i)), t0.Add(12*time.Hour))
		s.RecordSignOut(fmt.Sprintf("a-%d-b", i), tag(fmt.Sprint("sa-", i)), t0.Add(12*time.Hour))
	}
	clock.Add(time.Hour)
	s.RecordSignOut("victim-old", tag("victim"), clock.Now().Add(12*time.Hour))
	clock.Add(time.Second)
	s.RecordSignOut("victim-new", tag("victim"), clock.Now().Add(12*time.Hour))
	s.RecordSignOut("only-new", tag("only"), clock.Now().Add(12*time.Hour))
	if !s.SignedOut("victim-new") || !s.SignedOut("only-new") {
		t.Fatal("the victims' sign-outs were not recorded")
	}
	for j := range 1000 {
		clock.Add(time.Second)
		s.RecordSignOut(fmt.Sprint("att-", j), tag(fmt.Sprint("fresh-", j)), clock.Now().Add(12*time.Hour))
		s.RecordSignOut(fmt.Sprint("att-", j, "-b"), tag(fmt.Sprint("fresh-", j)), clock.Now().Add(12*time.Hour))
		if !s.SignedOut("victim-new") || !s.SignedOut("only-new") {
			t.Fatalf("after %d fresh identities: victim-new %v, only-new %v", j+1, s.SignedOut("victim-new"), s.SignedOut("only-new"))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Nobody holds a second entry recorded before the victims'.
	held := map[string]int{}
	for _, e := range s.signedOut {
		if e.Recorded < t0.Add(time.Hour).Unix() {
			if held[e.Subject]++; held[e.Subject] > 1 {
				t.Fatalf("%s kept two entries recorded at t0 while newer ones went", e.Subject)
			}
		}
	}
	if d := planSignOutCaps(s.signedOut, s.latestRecordedLocked()); len(d) != 0 {
		t.Errorf("capping the document again drops %d entries", len(d))
	}
}
