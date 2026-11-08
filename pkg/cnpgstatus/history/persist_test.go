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
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"
)

// fakeJournal is a persister with real journal semantics: mutations
// mutate its state, Load returns what a restart would find. Tests
// simulate a restart by building a second store on the same journal.
type fakeJournal struct {
	revs    map[uint64]Revision
	objects map[string]ObjectRecord
	seq     uint64
	evicted bool
	loadErr error
	updates int
}

func newFakeJournal() *fakeJournal {
	return &fakeJournal{revs: map[uint64]Revision{}, objects: map[string]ObjectRecord{}}
}

func (j *fakeJournal) Load() (Contents, error) {
	if j.loadErr != nil {
		return Contents{}, j.loadErr
	}
	contents := Contents{Objects: map[string]ObjectRecord{}, Seq: j.seq, Evicted: j.evicted}
	seqs := make([]uint64, 0, len(j.revs))
	for seq := range j.revs {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(a, b int) bool { return seqs[a] < seqs[b] })
	for _, seq := range seqs {
		contents.Revisions = append(contents.Revisions, j.revs[seq])
	}
	for uid, rec := range j.objects {
		contents.Objects[uid] = rec
	}
	return contents, nil
}

func (j *fakeJournal) Append(rev Revision) {
	j.revs[rev.Seq] = rev
	if rev.Seq > j.seq {
		j.seq = rev.Seq
	}
}

func (j *fakeJournal) Update(rev Revision) {
	j.revs[rev.Seq] = rev
	j.updates++
}

func (j *fakeJournal) Evict(seq uint64) { delete(j.revs, seq) }

func (j *fakeJournal) PutObject(uid string, rec ObjectRecord) { j.objects[uid] = rec }

func (j *fakeJournal) DeleteObject(uid string) { delete(j.objects, uid) }

func (j *fakeJournal) MarkEvicted() { j.evicted = true }

// restart builds a second store over the same journal, as a new process
// life would.
func restart(t *testing.T, j *fakeJournal, clock *fakeClock) *Store {
	t.Helper()
	s, err := NewPersistedStore(Limits{PerObject: 10, MaxRevisions: 100, MaxBytes: 1 << 20, CoalesceWindow: time.Minute}, clock, j)
	if err != nil {
		t.Fatalf("NewPersistedStore: %v", err)
	}
	return s
}

func TestPersistedStoreResumesAcrossRestart(t *testing.T) {
	journal := newFakeJournal()
	clock := &fakeClock{now: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)}
	before := restart(t, journal, clock)
	before.Seed("pods", []Observation{obs("a", "s1", "t1"), obs("b", "s1", "t1")})
	lastSeen := clock.now

	clock.advance(10 * time.Minute)
	after := restart(t, journal, clock)

	snap, ok := after.Snapshot()
	if !ok || len(snap.Entries) != 2 {
		t.Fatalf("restarted store retained %d entries, want 2", len(snap.Entries))
	}
	rev, ok := after.Revision(snap.Entries[0].Seq)
	if !ok || rev.Manifest == nil {
		t.Fatal("restarted store lost a manifest")
	}

	// The first seed after restart reconciles against the previous
	// process life: a changed spec is an after-gap change, a missing
	// object an unobserved deletion — never a fresh first observation.
	after.Seed("pods", []Observation{obs("a", "s2", "t1")})
	entries := entriesOf(t, after)
	if len(entries) != 4 {
		t.Fatalf("entries = %d, want 4", len(entries))
	}
	change := entries[0]
	if change.UID != "a" || change.Change != ChangeSpec || !change.AfterGap {
		t.Fatalf("newest = %+v, want after-gap spec change of a", change)
	}
	if change.PrevObservedAt != lastSeen {
		t.Fatalf("gap opens at %v, want the pre-restart observation %v", change.PrevObservedAt, lastSeen)
	}
	deletion := entries[1]
	if deletion.UID != "b" || deletion.Change != ChangeDeletedUnobserved {
		t.Fatalf("second = %+v, want unobserved deletion of b", deletion)
	}
	if deletion.Kind != "Pod" || deletion.Name != "cluster-b" {
		t.Fatalf("deletion identity = %s/%s, want Pod/cluster-b from the persisted object state", deletion.Kind, deletion.Name)
	}
}

// entriesOf snapshots a store and returns the timeline newest first.
func entriesOf(t *testing.T, s *Store) []Entry {
	t.Helper()
	snap, _ := s.Snapshot()
	return snap.Entries
}

func TestPersistedStoreDeduplicatesAcrossRestart(t *testing.T) {
	journal := newFakeJournal()
	clock := &fakeClock{now: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)}
	restart(t, journal, clock).Seed("pods", []Observation{obs("a", "s1", "t1")})

	clock.advance(10 * time.Minute)
	after := restart(t, journal, clock)
	after.Seed("pods", []Observation{obs("a", "s1", "t1")})

	if entries := entriesOf(t, after); len(entries) != 1 {
		t.Fatalf("entries = %d, want 1: an unchanged object across a restart is not news", len(entries))
	}
}

func TestPersistedStoreSequencesContinueAcrossRestart(t *testing.T) {
	journal := newFakeJournal()
	clock := &fakeClock{now: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)}
	restart(t, journal, clock).Observe(obs("a", "s1", "t1"))

	after := restart(t, journal, clock)
	clock.advance(2 * time.Minute)
	after.Observe(obs("a", "s2", "t1"))

	entries := entriesOf(t, after)
	if entries[0].Seq <= entries[1].Seq {
		t.Fatalf("sequence did not continue: %d after %d", entries[0].Seq, entries[1].Seq)
	}
}

func TestPrimeTrimsToShrunkenLimits(t *testing.T) {
	journal := newFakeJournal()
	clock := &fakeClock{now: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)}
	big := restart(t, journal, clock)
	for _, uid := range []string{"u1", "u2", "u3", "u4", "u5"} {
		big.Observe(obs(uid, "s1", "t1"))
	}

	small, err := NewPersistedStore(Limits{MaxRevisions: 3}, clock, journal)
	if err != nil {
		t.Fatalf("NewPersistedStore: %v", err)
	}
	snap, _ := small.Snapshot()
	if len(snap.Entries) != 3 || !snap.Evicted {
		t.Fatalf("entries = %d evicted = %v, want the shrunken bound applied", len(snap.Entries), snap.Evicted)
	}
	if len(journal.revs) != 3 || !journal.evicted {
		t.Fatalf("journal holds %d revisions, want the trim mirrored", len(journal.revs))
	}
}

func TestPersistedStoreMirrorsLifecycle(t *testing.T) {
	journal := newFakeJournal()
	clock := &fakeClock{now: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)}
	s := restart(t, journal, clock)

	s.Observe(obs("a", "s1", "t1"))
	if len(journal.revs) != 1 || len(journal.objects) != 1 {
		t.Fatalf("journal revs=%d objects=%d after creation", len(journal.revs), len(journal.objects))
	}

	// A coalesced flap rewrites the same journal revision in place.
	clock.advance(2 * time.Minute)
	s.Observe(obs("a", "s1", "t2"))
	clock.advance(time.Second)
	s.Observe(obs("a", "s1", "t3"))
	if journal.updates == 0 {
		t.Fatal("coalescing was not mirrored as an update")
	}
	if len(journal.revs) != 2 {
		t.Fatalf("journal revs = %d, want 2 after coalescing", len(journal.revs))
	}

	s.Observe(deletion("a"))
	if len(journal.objects) != 0 {
		t.Fatal("deletion did not clear the journal's object state")
	}
	if len(journal.revs) != 3 {
		t.Fatalf("journal revs = %d, want the deletion appended", len(journal.revs))
	}
}

func TestPersistedStoreLoadFailureIsFatal(t *testing.T) {
	journal := newFakeJournal()
	journal.loadErr = errors.New("torn journal")
	clock := &fakeClock{now: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)}
	if _, err := NewPersistedStore(Limits{}, clock, journal); err == nil {
		t.Fatal("a journal that cannot be loaded must fail the store, not start it empty")
	}
}

// TestPrimeEnforcesALoweredPerObjectCap: a journal written under a larger
// per-object cap is trimmed to the new one on restart, whatever the
// excess, and the trim is mirrored.
func TestPrimeEnforcesALoweredPerObjectCap(t *testing.T) {
	journal := newFakeJournal()
	clock := &fakeClock{now: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)}
	before, err := NewPersistedStore(Limits{PerObject: 100, MaxRevisions: 5000, MaxBytes: 1 << 24}, clock, journal)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		clock.advance(time.Minute)
		before.Observe(obs("a", fmt.Sprintf("s%d", i), "t1"))
	}
	if got := len(entries(t, before)); got != 100 {
		t.Fatalf("first life retained %d, want 100", got)
	}

	after, err := NewPersistedStore(Limits{PerObject: 20, MaxRevisions: 1000, MaxBytes: 1 << 24}, clock, journal)
	if err != nil {
		t.Fatal(err)
	}
	got := entries(t, after)
	if len(got) != 20 {
		t.Fatalf("after restart retained %d revisions of one object, want the cap of 20", len(got))
	}
	if got[0].Seq != 100 || got[19].Seq != 81 {
		t.Errorf("kept seqs %d..%d, want the newest 81..100", got[19].Seq, got[0].Seq)
	}
	if len(journal.revs) != 20 {
		t.Errorf("journal holds %d revisions after the trim, want 20", len(journal.revs))
	}
	// The coalescing target and the count survived the trim: one more
	// revision evicts exactly one.
	clock.advance(time.Minute)
	after.Observe(obs("a", "s-next", "t1"))
	if got := len(entries(t, after)); got != 20 {
		t.Errorf("after one more revision: %d, want 20", got)
	}
}
