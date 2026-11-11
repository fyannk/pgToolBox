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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
)

func heldBy(s *Store, who string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.signedOut {
		if e.Subject == who {
			n++
		}
	}
	return n
}

// 48 identities at their 32 entries fill the document (any namespace
// editor can mint ServiceAccounts, and signing in needs no SAR): every
// later sign-out used to be refused for up to 12 h (round 9). A newcomer
// now takes the place of an entry of a person holding the most, and
// keeps getting in while the identities replace their own.
func TestFairShareAdmitsANewcomerToAFullDocument(t *testing.T) {
	clock := &testClock{now: t0}
	s := synced(t, Options{Name: "console-state", Now: clock.Now})
	ids := MaxSignOuts / MaxSignOutsPerSubject
	for i := range ids {
		for j := range MaxSignOutsPerSubject {
			if !s.RecordSignOut(fmt.Sprintf("a-%d-%d", i, j), tag(fmt.Sprint("sa-", i)), t0.Add(12*time.Hour)) {
				t.Fatalf("identity %d's sign-out %d refused", i, j)
			}
		}
	}
	before := othersOf(s, tag("victim"))
	evicted := testutil.ToFloat64(signOutsRefused.WithLabelValues(refusedEvicted))
	if !s.RecordSignOut("victim-session", tag("victim"), t0.Add(12*time.Hour)) || !s.SignedOut("victim-session") {
		t.Fatal("a full document refused a newcomer's sign-out")
	}
	if got := testutil.ToFloat64(signOutsRefused.WithLabelValues(refusedEvicted)) - evicted; got != 1 {
		t.Errorf("evictions counted: %v, want 1", got)
	}
	after := othersOf(s, tag("victim"))
	if len(after) != len(before)-1 || len(s.signedOut) != MaxSignOuts {
		t.Fatalf("%d entries left for the newcomer, %d held", len(before)-len(after), len(s.signedOut))
	}
	for h, e := range before {
		if _, ok := after[h]; !ok && heldBy(s, e.Subject) != MaxSignOutsPerSubject-1 {
			t.Errorf("the entry taken belongs to a person holding %d", heldBy(s, e.Subject))
		}
	}
	// The identities keep the document full, each replacing its own; the
	// newcomer's later sign-out still gets in.
	clock.Add(11 * time.Hour)
	for i := range ids {
		for j := range 4 {
			if !s.RecordSignOut(fmt.Sprintf("b-%d-%d", i, j), tag(fmt.Sprint("sa-", i)), clock.Now().Add(12*time.Hour)) {
				t.Fatalf("identity %d's own replacement refused", i)
			}
		}
	}
	if !s.RecordSignOut("victim-session-2", tag("victim"), clock.Now().Add(12*time.Hour)) ||
		!s.SignedOut("victim-session-2") || !s.SignedOut("victim-session") {
		t.Fatal("the newcomer's sign-outs are not both held")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := planSignOutCaps(s.signedOut, s.latestRecordedLocked()); len(d) != 0 {
		t.Errorf("the bounds drop %d entries from a document RecordSignOut made", len(d))
	}
}

// tagOrder breaks fairShares' ties by tag.
func tagOrder(a, b string, _ int) bool { return a < b }

// fairShares takes from the tags holding the most, never from one
// holding fewer than another that keeps more than it.
func TestFairShares(t *testing.T) {
	for _, tc := range []struct {
		name   string
		counts map[string]int
		n      int
		want   map[string]int
	}{
		{"fits", map[string]int{"a": 3, "b": 2}, 5, map[string]int{"a": 3, "b": 2}},
		{"the largest loses", map[string]int{"a": 5, "b": 2}, 6, map[string]int{"a": 4, "b": 2}},
		{"level", map[string]int{"a": 5, "b": 5, "c": 1}, 7, map[string]int{"a": 3, "b": 3, "c": 1}},
		{"extras in the order given", map[string]int{"b": 5, "a": 5, "c": 1}, 8, map[string]int{"a": 4, "b": 3, "c": 1}},
		{"nobody's last", map[string]int{"": 5, "a": 5}, 9, map[string]int{"": 4, "a": 5}},
		{"level zero", map[string]int{"a": 1, "b": 1, "c": 1}, 2, map[string]int{"a": 1, "b": 1, "c": 0}},
	} {
		got := fairShares(tc.counts, tc.n, tagOrder)
		if !maps.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	r := rand.New(rand.NewPCG(1, 2))
	for range 500 {
		counts := map[string]int{}
		total := 0
		for i := range 1 + r.IntN(20) {
			c := 1 + r.IntN(40)
			counts[fmt.Sprint("t", i)] = c
			total += c
		}
		n := r.IntN(total + 5)
		got := fairShares(counts, n, tagOrder)
		sum := 0
		for t1, c1 := range counts {
			sum += got[t1]
			if got[t1] > c1 || got[t1] < 0 {
				t.Fatalf("share %d of %d", got[t1], c1)
			}
			for t2, c2 := range counts {
				// t1 lost an entry while t2 keeps more than t1's share + 1.
				if got[t1] < c1 && got[t2] > got[t1]+1 {
					t.Fatalf("%v of %v (n %d): %s keeps %d while %s holding %d keeps %d", got, counts, n, t2, got[t2], t1, c1, got[t1])
				}
				_ = c2
			}
		}
		if sum != min(n, total) {
			t.Fatalf("shares sum to %d, want %d", sum, min(n, total))
		}
	}
}

// Property, on the store: a newcomer never takes the place of an entry
// of a person holding as many entries as the newcomer, or fewer, and
// RecordSignOut leaves a document the bounds keep as it is.
func TestFairShareNeverTakesFromSomeoneHoldingLess(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 8))
	clock := &testClock{now: t0}
	s := synced(t, Options{Name: "console-state", Now: clock.Now})
	people := make([]string, 1, 71)
	for i := range 70 {
		people = append(people, tag(fmt.Sprint("p", i)))
	}
	evictions, ops := 0, 6000
	if testing.Short() || raceEnabled {
		// Enough to fill the document (MaxSignOuts) and run past it.
		ops = MaxSignOuts + 800
	}
	for n := range ops {
		if n%20 == 0 {
			clock.Add(time.Second)
		}
		who := people[r.IntN(len(people))]
		counts := map[string]int{}
		s.mu.Lock()
		before := maps.Clone(s.signedOut)
		s.mu.Unlock()
		for _, e := range before {
			counts[e.Subject]++
		}
		s.RecordSignOut(fmt.Sprint("s-", n), who, clock.Now().Add(time.Duration(1+r.IntN(40))*time.Minute))
		s.mu.Lock()
		for h, e := range before {
			if _, ok := s.signedOut[h]; !ok && e.Subject != who && e.Expiry > clock.Now().Unix() {
				evictions++
				if counts[e.Subject] <= counts[who] {
					t.Fatalf("op %d: a newcomer holding %d took an entry of a person holding %d", n, counts[who], counts[e.Subject])
				}
			}
		}
		if n%50 == 0 {
			if d := planSignOutCaps(s.signedOut, s.latestRecordedLocked()); len(d) != 0 {
				t.Fatalf("op %d: the bounds drop %v from a document RecordSignOut made", n, d)
			}
		}
		s.mu.Unlock()
	}
	if evictions == 0 {
		t.Fatal("the document never filled")
	}
}

// Two replicas each record one sign-out of a person at their bound
// before syncing: both replace the same entry, so both take its slot at
// the same generation, and merging keeps one. That drop used to be taken
// for a replacement reaching the replica and was not counted (round 9):
// it is now counted under merge.
func TestConcurrentReplacementsOfOneEntryAreCounted(t *testing.T) {
	clock := &testClock{now: t0}
	a := synced(t, Options{Name: "console-state", Now: clock.Now})
	b := synced(t, Options{Name: "console-state", Now: clock.Now})
	for j := range MaxSignOutsPerSubject {
		a.RecordSignOut(fmt.Sprint("s-", j), tag("mallory"), t0.Add(time.Duration(j+1)*time.Hour))
	}
	exchange(b, a)
	clock.Add(time.Second)
	before := testutil.ToFloat64(signOutsRefused.WithLabelValues(refusedMerge))
	if !a.RecordSignOut("new-on-a", tag("mallory"), clock.Now().Add(12*time.Hour)) ||
		!b.RecordSignOut("new-on-b", tag("mallory"), clock.Now().Add(12*time.Hour)) {
		t.Fatal("a replacement was refused")
	}
	exchange(b, a)
	exchange(a, b)
	if got := testutil.ToFloat64(signOutsRefused.WithLabelValues(refusedMerge)) - before; got < 1 {
		t.Errorf("the dropped replacement was not counted (merge +%v)", got)
	}
	if !maps.Equal(a.signedOut, b.signedOut) || heldBy(a, tag("mallory")) != MaxSignOutsPerSubject {
		t.Fatalf("replicas hold %d and %d entries, differently", len(a.signedOut), len(b.signedOut))
	}
	if a.SignedOut("new-on-a") == a.SignedOut("new-on-b") {
		t.Error("want exactly one of the two replacements held")
	}
}

// Property: one ConfigMap, read-merge-written by each replica in turn
// (what an update carrying the resourceVersion read gives), random
// sign-outs of many people, enough to fill the document and bring the
// fair share in; then, quiescent, every replica holds the same capped
// document and a further round writes nothing.
func TestSharedConfigMapConverges(t *testing.T) {
	seeds := uint64(6)
	if testing.Short() || raceEnabled {
		seeds = 1
	}
	for seed := range seeds {
		r := rand.New(rand.NewPCG(seed, 99))
		clock := &testClock{now: t0}
		n := 2 + r.IntN(3)
		st := make([]*Store, n)
		for i := range st {
			st[i] = synced(t, Options{Name: "console-state", Now: clock.Now})
		}
		cm := &corev1.ConfigMap{}
		flush := func(i int) bool {
			s := st[i]
			s.mu.Lock()
			defer s.mu.Unlock()
			s.mergeLocked(cm)
			d := s.desiredLocked()
			changed := !maps.Equal(d.Data, cm.Data)
			cm = d
			s.base = d.DeepCopy()
			return changed
		}
		people := make([]string, 1, 61)
		for i := range 60 {
			people = append(people, tag(fmt.Sprint("p", i)))
		}
		full := false
		for k := range 2400 {
			if k%7 == 0 {
				clock.Add(time.Second)
			}
			if r.IntN(6) == 0 {
				flush(r.IntN(n))
				full = full || len(st[0].signedOut) >= MaxSignOuts
				continue
			}
			st[r.IntN(n)].RecordSignOut(fmt.Sprintf("x%d-%d", seed, k), people[r.IntN(len(people))],
				clock.Now().Add(time.Duration(600+r.IntN(4000))*time.Second))
		}
		for round := range 10 {
			changed := false
			for i := range st {
				if flush(i) {
					changed = true
				}
			}
			if !changed {
				break
			}
			if round == 9 {
				t.Fatalf("seed %d: still writing after 10 rounds", seed)
			}
		}
		if !full {
			t.Fatalf("seed %d: the document never filled", seed)
		}
		for i := 1; i < n; i++ {
			if !maps.Equal(st[i].signedOut, st[0].signedOut) {
				t.Fatalf("seed %d: replica %d differs", seed, i)
			}
		}
		if !maps.Equal(capSet(st[0].signedOut), st[0].signedOut) {
			t.Fatalf("seed %d: not capped", seed)
		}
	}
}
