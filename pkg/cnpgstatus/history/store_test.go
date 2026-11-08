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

package history

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// fakeClock is a hand-advanced clock.
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// newTestStore returns a store with generous bounds and its clock.
func newTestStore() (*Store, *fakeClock) {
	clock := &fakeClock{now: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)}
	return NewStore(Limits{PerObject: 10, MaxRevisions: 100, MaxBytes: 1 << 20, CoalesceWindow: time.Minute}, clock), clock
}

// obs builds a pod-shaped observation.
func obs(uid, specHash, statusHash string) Observation {
	return Observation{
		Scope:      "pods",
		Version:    "v1",
		Kind:       "Pod",
		Namespace:  "db",
		Name:       "cluster-" + uid,
		UID:        uid,
		Manifest:   []byte(`{"kind":"Pod","spec":"` + specHash + `","status":"` + statusHash + `"}`),
		SpecHash:   specHash,
		StatusHash: statusHash,
		Actor:      Actor{Manager: "kubelet", Operation: "Update"},
	}
}

// deletion builds the watch-delivered deletion of obs(uid, ...).
func deletion(uid string) Observation {
	o := obs(uid, "", "")
	o.Manifest = nil
	o.Deleted = true
	return o
}

// entries snapshots the store and returns the timeline newest first.
func entries(t *testing.T, s *Store) []Entry {
	t.Helper()
	snap, _ := s.Snapshot()
	return snap.Entries
}

func TestSeedRecordsFirstObserved(t *testing.T) {
	s, _ := newTestStore()
	s.Seed("pods", []Observation{obs("a", "s1", "t1")})

	got := entries(t, s)
	if len(got) != 1 {
		t.Fatalf("entries = %d, want 1", len(got))
	}
	if got[0].Change != ChangeFirstObserved {
		t.Fatalf("change = %q, want %q", got[0].Change, ChangeFirstObserved)
	}
	if got[0].Actor.Manager != "kubelet" {
		t.Fatalf("actor = %q, want kubelet", got[0].Actor.Manager)
	}
}

func TestWatchAddRecordsCreated(t *testing.T) {
	s, _ := newTestStore()
	s.Observe(obs("a", "s1", "t1"))

	got := entries(t, s)
	if len(got) != 1 || got[0].Change != ChangeCreated {
		t.Fatalf("got %+v, want one created entry", got)
	}
}

func TestSpecAndStatusChangesClassified(t *testing.T) {
	s, clock := newTestStore()
	s.Observe(obs("a", "s1", "t1"))
	clock.advance(2 * time.Minute)
	s.Observe(obs("a", "s2", "t1"))
	clock.advance(2 * time.Minute)
	s.Observe(obs("a", "s2", "t2"))

	got := entries(t, s)
	if len(got) != 3 {
		t.Fatalf("entries = %d, want 3", len(got))
	}
	if got[0].Change != ChangeStatus {
		t.Fatalf("newest = %q, want %q", got[0].Change, ChangeStatus)
	}
	if got[1].Change != ChangeSpec {
		t.Fatalf("middle = %q, want %q", got[1].Change, ChangeSpec)
	}
	if got[1].PrevObservedAt.IsZero() {
		t.Fatal("spec change carries no previous observation time")
	}
}

func TestIdenticalObservationDeduplicates(t *testing.T) {
	s, _ := newTestStore()
	s.Observe(obs("a", "s1", "t1"))
	s.Observe(obs("a", "s1", "t1"))
	s.Seed("pods", []Observation{obs("a", "s1", "t1")})

	if got := entries(t, s); len(got) != 1 {
		t.Fatalf("entries = %d, want 1: an unchanged re-observation is not news", len(got))
	}
}

func TestStatusChurnCoalesces(t *testing.T) {
	s, clock := newTestStore()
	s.Observe(obs("a", "s1", "t1"))
	clock.advance(2 * time.Minute)
	s.Observe(obs("a", "s1", "t2"))
	clock.advance(10 * time.Second)
	s.Observe(obs("a", "s1", "t3"))
	clock.advance(10 * time.Second)
	s.Observe(obs("a", "s1", "t4"))

	got := entries(t, s)
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2: churn inside the window folds into one", len(got))
	}
	if got[0].Coalesced != 2 {
		t.Fatalf("coalesced = %d, want 2", got[0].Coalesced)
	}
	if got[0].ObservedAt != clock.now {
		t.Fatal("coalesced entry does not carry the latest observation time")
	}
	rev, ok := s.Revision(got[0].Seq)
	if !ok || rev.StatusHash != "t4" {
		t.Fatalf("coalesced revision hash = %q, want t4", rev.StatusHash)
	}
}

func TestStatusOutsideWindowAppends(t *testing.T) {
	s, clock := newTestStore()
	s.Observe(obs("a", "s1", "t1"))
	clock.advance(2 * time.Minute)
	s.Observe(obs("a", "s1", "t2"))
	clock.advance(2 * time.Minute)
	s.Observe(obs("a", "s1", "t3"))

	if got := entries(t, s); len(got) != 3 {
		t.Fatalf("entries = %d, want 3: transitions apart stay distinct", len(got))
	}
}

func TestSpecChangeNeverCoalesces(t *testing.T) {
	s, clock := newTestStore()
	s.Observe(obs("a", "s1", "t1"))
	clock.advance(time.Second)
	s.Observe(obs("a", "s1", "t2"))
	clock.advance(time.Second)
	s.Observe(obs("a", "s2", "t2"))

	got := entries(t, s)
	if len(got) != 3 || got[0].Change != ChangeSpec {
		t.Fatalf("got %d entries, newest %q; a spec change must stand alone", len(got), got[0].Change)
	}
}

func TestDeletionRecordedOnceAndUnknownIgnored(t *testing.T) {
	s, _ := newTestStore()
	s.Observe(obs("a", "s1", "t1"))
	s.Observe(deletion("a"))
	s.Observe(deletion("a"))
	s.Observe(deletion("never-seen"))

	got := entries(t, s)
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2", len(got))
	}
	if got[0].Change != ChangeDeleted || got[0].HasManifest {
		t.Fatalf("newest = %+v, want manifest-free deletion", got[0])
	}
}

func TestSeedDetectsUnobservedDeletion(t *testing.T) {
	s, clock := newTestStore()
	s.Seed("pods", []Observation{obs("a", "s1", "t1"), obs("b", "s1", "t1")})
	lastSeen := clock.now
	clock.advance(5 * time.Minute)
	s.Seed("pods", []Observation{obs("b", "s1", "t1")})

	got := entries(t, s)
	if len(got) != 3 {
		t.Fatalf("entries = %d, want 3", len(got))
	}
	newest := got[0]
	if newest.Change != ChangeDeletedUnobserved || newest.UID != "a" {
		t.Fatalf("newest = %+v, want unobserved deletion of a", newest)
	}
	if !newest.AfterGap || newest.PrevObservedAt != lastSeen || newest.ObservedAt != clock.now {
		t.Fatalf("window = [%v, %v], want [%v, %v]", newest.PrevObservedAt, newest.ObservedAt, lastSeen, clock.now)
	}
	if newest.Kind != "Pod" || newest.Name != "cluster-a" {
		t.Fatalf("identity = %s/%s, want Pod/cluster-a", newest.Kind, newest.Name)
	}
}

func TestSeedOfOneScopeLeavesOthersAlone(t *testing.T) {
	s, _ := newTestStore()
	podA := obs("a", "s1", "t1")
	poolerPod := obs("p", "s1", "t1")
	poolerPod.Scope = "pooler pods"
	s.Seed("pods", []Observation{podA})
	s.Seed("pooler pods", []Observation{poolerPod})

	// A pods re-seed says nothing about pooler pods.
	s.Seed("pods", []Observation{podA})

	for _, e := range entries(t, s) {
		if e.Change == ChangeDeletedUnobserved {
			t.Fatalf("scope crosstalk: %+v", e)
		}
	}
}

func TestDeletionThroughAnotherScopeLeavesNoPhantom(t *testing.T) {
	s, clock := newTestStore()
	poolerPod := obs("p", "s1", "t1")
	poolerPod.Scope = "pooler pods"
	s.Seed("pooler pods", []Observation{poolerPod})

	// The same pod matches the instance-pod watch's selector, so its
	// deletion can arrive through the "pods" scope.
	gone := deletion("p")
	gone.Scope = "pods"
	s.Observe(gone)

	clock.advance(5 * time.Minute)
	s.Seed("pooler pods", nil)

	got := entries(t, s)
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2: the re-seed must not synthesize a second deletion", len(got))
	}
	if got[0].Change != ChangeDeleted {
		t.Fatalf("newest = %q, want %q", got[0].Change, ChangeDeleted)
	}
}

func TestSeedChangeAcrossGapFlagsAfterGap(t *testing.T) {
	s, clock := newTestStore()
	s.Seed("pods", []Observation{obs("a", "s1", "t1")})
	clock.advance(5 * time.Minute)
	s.Seed("pods", []Observation{obs("a", "s2", "t1")})

	got := entries(t, s)
	if got[0].Change != ChangeSpec || !got[0].AfterGap {
		t.Fatalf("newest = %+v, want spec change flagged after-gap", got[0])
	}
}

func TestPerObjectCapEvictsThatObjectsOldest(t *testing.T) {
	s, clock := newTestStore()
	s.limits = Limits{PerObject: 2}
	s.Observe(obs("quiet", "s1", "t1"))
	s.Observe(obs("noisy", "s1", "t1"))
	for i := 0; i < 5; i++ {
		clock.advance(2 * time.Minute)
		s.Observe(obs("noisy", "s1", fmt.Sprintf("t%d", i+2)))
	}

	quiet, noisy := 0, 0
	for _, e := range entries(t, s) {
		switch e.UID {
		case "quiet":
			quiet++
		case "noisy":
			noisy++
		}
	}
	if quiet != 1 {
		t.Fatalf("quiet retained %d, want 1: churn must not crowd out quiet objects", quiet)
	}
	if noisy != 2 {
		t.Fatalf("noisy retained %d, want the cap of 2", noisy)
	}
	if snap, _ := s.Snapshot(); !snap.Evicted {
		t.Fatal("eviction not reported")
	}
}

func TestGlobalRevisionCapEvictsOldest(t *testing.T) {
	s, _ := newTestStore()
	s.limits = Limits{MaxRevisions: 3}
	for i := 0; i < 5; i++ {
		s.Observe(obs(fmt.Sprintf("u%d", i), "s1", "t1"))
	}

	got := entries(t, s)
	if len(got) != 3 {
		t.Fatalf("entries = %d, want 3", len(got))
	}
	if got[len(got)-1].UID != "u2" {
		t.Fatalf("oldest = %s, want u2", got[len(got)-1].UID)
	}
}

func TestByteBudgetEvictsOldest(t *testing.T) {
	s, _ := newTestStore()
	one := obs("a", "s1", "t1")
	s.limits = Limits{MaxBytes: 2*len(one.Manifest) + 1}
	s.Observe(one)
	s.Observe(obs("b", "s1", "t1"))
	s.Observe(obs("c", "s1", "t1"))

	got := entries(t, s)
	if len(got) != 2 || got[len(got)-1].UID != "b" {
		t.Fatalf("got %d entries, oldest %s; want 2 with a evicted", len(got), got[len(got)-1].UID)
	}
}

func TestRevisionReturnsManifestCopy(t *testing.T) {
	s, _ := newTestStore()
	s.Observe(obs("a", "s1", "t1"))
	seq := entries(t, s)[0].Seq

	rev, ok := s.Revision(seq)
	if !ok {
		t.Fatal("revision not found")
	}
	rev.Manifest[0] = 'X'
	again, _ := s.Revision(seq)
	if again.Manifest[0] == 'X' {
		t.Fatal("revision manifest is shared mutable state")
	}
	if _, ok := s.Revision(seq + 100); ok {
		t.Fatal("unknown sequence resolved")
	}
}

func TestSnapshotNewestFirstWithGenerations(t *testing.T) {
	s, clock := newTestStore()
	s.Observe(obs("a", "s1", "t1"))
	first, _ := s.Snapshot()
	clock.advance(2 * time.Minute)
	s.Observe(obs("a", "s2", "t1"))
	second, _ := s.Snapshot()

	if second.Generation <= first.Generation {
		t.Fatalf("generation did not advance: %d then %d", first.Generation, second.Generation)
	}
	if len(second.Entries) != 2 || !second.Entries[0].ObservedAt.After(second.Entries[1].ObservedAt) {
		t.Fatalf("entries not newest first: %+v", second.Entries)
	}
	if _, ok := NewStore(Limits{}, clock).Snapshot(); ok {
		t.Fatal("empty store reports a snapshot")
	}
}

func TestConcurrentObserves(t *testing.T) {
	s, _ := newTestStore()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				s.Observe(obs(fmt.Sprintf("u%d", g), "s1", fmt.Sprintf("t%d", i)))
				s.Snapshot()
			}
		}(g)
	}
	wg.Wait()
	if got := entries(t, s); len(got) == 0 {
		t.Fatal("nothing retained")
	}
}

// TestSeedInAccountsOnlyForListedNamespaces: a listing that covered only
// some namespaces synthesizes deletions only there.
func TestSeedInAccountsOnlyForListedNamespaces(t *testing.T) {
	for _, tc := range []struct {
		name       string
		namespaces []string
		want       []string
	}{
		{name: "whole scope", namespaces: nil, want: []string{"a", "b"}},
		{name: "one namespace", namespaces: []string{"db"}, want: []string{"a"}},
		{name: "another namespace", namespaces: []string{"other"}, want: []string{"b"}},
		{name: "none listed", namespaces: []string{}, want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestStore()
			s.Observe(obs("a", "s1", "t1"))
			other := obs("b", "s1", "t1")
			other.Namespace = "other"
			s.Observe(other)
			s.SeedIn("pods", tc.namespaces, nil)
			var gone []string
			for _, e := range entries(t, s) {
				if e.Change == ChangeDeletedUnobserved {
					gone = append(gone, e.UID)
				}
			}
			slices.Sort(gone)
			if !slices.Equal(gone, tc.want) {
				t.Fatalf("deleted unobserved = %v, want %v", gone, tc.want)
			}
		})
	}
}
