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

	corev1 "k8s.io/api/core/v1"
)

// Sign-outs naming nobody (a session sealed without its person's tag)
// used to skip the per-person bound, and the document's bound kept the
// entries that end first: one person's 768 anonymous sign-outs (two
// entries each) pushed bob's, already shared, out of the document on
// every replica (round-7 repro). They now share one bound of their own,
// and nothing the document holds is dropped for a newer entry.
func TestAnonymousSignOutsDoNotEvictAStoredOne(t *testing.T) {
	clock := &testClock{now: t0.Add(30 * time.Minute)}
	s := New(Options{Name: "console-state", Now: clock.Now})
	if !s.RecordSignOut("victim-session", tag("bob"), t0.Add(12*time.Hour+20*time.Minute)) {
		t.Fatal("bob's sign-out was refused")
	}
	shared := 0
	for i := range MaxSignOuts / 2 {
		end := t0.Add(12*time.Hour + time.Duration(i)*time.Second/2)
		for _, sid := range []string{fmt.Sprintf("session-%d", i), fmt.Sprintf("bearer-%d", i)} {
			if s.RecordSignOut(sid, "", end) {
				shared++
			}
		}
	}
	if !s.SignedOut("victim-session") {
		t.Errorf("bob's sign-out is gone from the shared document after %d anonymous sign-outs", MaxSignOuts)
	}
	if shared != MaxUnnamedSignOuts {
		t.Errorf("%d anonymous sign-outs shared, want %d", shared, MaxUnnamedSignOuts)
	}
	// And the document still has room for everyone else.
	if !s.RecordSignOut("alice-session", tag("alice"), t0.Add(13*time.Hour)) {
		t.Error("another person's later sign-out was refused")
	}
}

// The same document filled by 1,536 sign-outs naming nobody (round-7
// probe): a person's next sign-out is still shared.
func TestUnnamedSignOutsCannotFillTheDocument(t *testing.T) {
	clock := &testClock{now: t0}
	s := New(Options{Name: "console-state", Now: clock.Now})
	for i := range MaxSignOuts {
		s.RecordSignOut(fmt.Sprintf("old-%d", i), "", t0.Add(12*time.Hour))
	}
	clock.now = t0.Add(time.Minute)
	if !s.RecordSignOut("victim", tag("bob"), t0.Add(time.Minute+12*time.Hour)) {
		t.Fatal("unnamed sign-outs filled the document: bob's sign-out was refused")
	}
}

// 48 people, 16 sign-outs each (two entries a sign-out), fill the
// document: what it held before stays, and the next ones are refused.
func TestManySubjectsDoNotEvictAStoredOne(t *testing.T) {
	clock := &testClock{now: t0.Add(30 * time.Minute)}
	s := New(Options{Name: "console-state", Now: clock.Now})
	s.RecordSignOut("victim-session", tag("bob"), t0.Add(12*time.Hour+20*time.Minute))
	for sa := range 48 {
		for i := range 16 {
			end := t0.Add(12*time.Hour + time.Duration(sa*16+i)*time.Second)
			who := tag(fmt.Sprintf("system:serviceaccount:team:sa%d", sa))
			s.RecordSignOut(fmt.Sprintf("s-%d-%d", sa, i), who, end)
			s.RecordSignOut(fmt.Sprintf("b-%d-%d", sa, i), who, end)
		}
	}
	if !s.SignedOut("victim-session") {
		t.Error("bob's sign-out is gone after 48 service accounts' sign-outs")
	}
	s.mu.Lock()
	n := len(s.signedOut)
	s.mu.Unlock()
	if n != MaxSignOuts {
		t.Errorf("%d sign-outs held, want the document full at %d", n, MaxSignOuts)
	}
}

// A refused sign-out that pruned ended ones still marks the state
// changed, so the pruning reaches the ConfigMap with the next write.
func TestARefusalThatPrunedMarksTheStateChanged(t *testing.T) {
	clock := &testClock{now: t0}
	s := New(Options{Name: "console-state", Now: clock.Now})
	s.mu.Lock()
	s.signedOut[sessionHash("ended")] = signOut{Expiry: t0.Unix() - 1, Recorded: 1, Subject: tag("bob")}
	for i := range MaxSignOutsPerSubject {
		s.signedOut[sessionHash(fmt.Sprint("s-", i))] = signOut{Expiry: t0.Add(time.Hour).Unix(), Recorded: 2, Subject: tag("mallory")}
	}
	before := s.gen
	s.mu.Unlock()
	if s.RecordSignOut("refused", tag("mallory"), t0.Add(time.Hour)) {
		t.Fatal("a sign-out past the person's share was kept")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, still := s.signedOut[sessionHash("ended")]; still {
		t.Fatal("the ended sign-out was not pruned")
	}
	if s.gen == before {
		t.Error("a refusal that pruned ended sign-outs did not mark the state changed")
	}
}

// Property: the bounds are a total order applied the same way, so
// capping is idempotent and capped documents merged pairwise in any
// order end in the state capping their union gives, and an entry
// recorded before the others is never dropped by them.
func TestSignOutCapsConvergeInAnyOrder(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	people := []string{"", tag("a"), tag("b"), tag("c"), tag("d")}
	capped := func(m map[string]signOut) map[string]signOut {
		out := maps.Clone(m)
		for h := range planSignOutCaps(out) {
			delete(out, h)
		}
		return out
	}
	union := func(a, b map[string]signOut) map[string]signOut {
		out := maps.Clone(a)
		for h, e := range b {
			if prev, ok := out[h]; ok {
				e = mergeSignOut(prev, e)
			}
			out[h] = e
		}
		return out
	}
	for trial := range 40 {
		replicas := make([]map[string]signOut, 2+r.IntN(4))
		all := map[string]signOut{}
		for i := range replicas {
			replicas[i] = map[string]signOut{}
			for range r.IntN(3 * MaxSignOuts / len(replicas)) {
				// A shared hash space, so replicas sometimes hold the same
				// session's sign-out.
				h := fmt.Sprintf("%032x", r.IntN(4*MaxSignOuts))
				e := signOut{Expiry: 1 << 40, Recorded: int64(r.IntN(500)), Subject: people[r.IntN(len(people))]}
				if prev, ok := all[h]; ok {
					e.Subject = prev.Subject // one session, one person
				}
				replicas[i][h] = e
				if prev, ok := all[h]; ok {
					e = mergeSignOut(prev, e)
				}
				all[h] = e
			}
		}
		full := map[string]signOut{}
		for _, m := range replicas {
			full = union(full, m)
		}
		want := capped(full)
		if !maps.Equal(capped(want), want) {
			t.Fatalf("trial %d: capping is not idempotent", trial)
		}
		for order := range 3 {
			state := make([]map[string]signOut, len(replicas))
			for i, m := range replicas {
				state[i] = capped(m)
			}
			// Random pairwise merges, then enough rounds for every pair.
			for range 4 * len(state) * len(state) {
				i, j := r.IntN(len(state)), r.IntN(len(state))
				state[i] = capped(union(state[i], state[j]))
			}
			for range 2 {
				for i := range state {
					for j := range state {
						state[i] = capped(union(state[i], state[j]))
					}
				}
			}
			for i := range state {
				if !maps.Equal(state[i], want) {
					t.Fatalf("trial %d order %d: replica %d holds %d sign-outs, the union caps to %d", trial, order, i, len(state[i]), len(want))
				}
			}
		}
	}
}

// Property, on the store: replicas recording sign-outs at random and
// exchanging the ConfigMap in random orders end identical, and a
// sign-out a replica held before the flood is still held everywhere.
func TestStoresConvergeAndKeepWhatTheyHeld(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 5))
	for trial := range 2 {
		clock := &testClock{now: t0}
		stores := make([]*Store, 3)
		for i := range stores {
			stores[i] = New(Options{Name: "console-state", Now: clock.Now})
			stores[i].mu.Lock()
			stores[i].mergeLocked(&corev1.ConfigMap{})
			stores[i].mu.Unlock()
		}
		stores[0].RecordSignOut("victim", tag("bob"), t0.Add(12*time.Hour))
		exchange := func(i, j int) {
			stores[j].mu.Lock()
			cm := stores[j].desiredLocked()
			stores[j].mu.Unlock()
			stores[i].mu.Lock()
			stores[i].mergeLocked(cm)
			stores[i].mu.Unlock()
		}
		exchange(1, 0)
		exchange(2, 0)
		refused := 0
		for n := range 2000 {
			if n%50 == 0 {
				clock.Add(time.Second)
			}
			who := ""
			if k := r.IntN(60); k > 0 {
				who = tag(fmt.Sprint("p", k))
			}
			if !stores[r.IntN(len(stores))].RecordSignOut(fmt.Sprintf("t%d-%d", trial, n), who, clock.Now().Add(time.Duration(1+r.IntN(12))*time.Hour)) {
				refused++
			}
			if r.IntN(25) == 0 {
				exchange(r.IntN(len(stores)), r.IntN(len(stores)))
			}
		}
		for range 3 {
			for i := range stores {
				for j := range stores {
					exchange(i, j)
				}
			}
		}
		if refused == 0 {
			t.Fatalf("trial %d: no bound was reached", trial)
		}
		for i, s := range stores {
			if !s.SignedOut("victim") {
				t.Fatalf("trial %d: replica %d dropped a sign-out held before the flood", trial, i)
			}
			if i > 0 && !maps.Equal(s.signedOut, stores[0].signedOut) {
				t.Fatalf("trial %d: replica %d holds %d sign-outs, replica 0 %d", trial, i, len(s.signedOut), len(stores[0].signedOut))
			}
			if len(s.signedOut) > MaxSignOuts {
				t.Fatalf("trial %d: %d sign-outs held", trial, len(s.signedOut))
			}
		}
	}
}
