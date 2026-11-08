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
	"sort"
	"sync"
	"time"
)

// objectState is the classification state of one live object
// incarnation. It outlives the object's retained revisions: dedup,
// classification and the naming of an unobserved deletion need the last
// observation even after the revisions carrying it were evicted.
type objectState struct {
	scope                string
	group, version, kind string
	namespace, name      string
	specHash, statusHash string
	lastObservedAt       time.Time
	// count is the number of retained revisions of this incarnation.
	count int
	// last points at the newest retained revision, the coalescing
	// target. Nil when that revision was evicted.
	last *Revision
}

// record is the persisted form of this state.
func (o *objectState) record() ObjectRecord {
	return ObjectRecord{
		Scope:          o.scope,
		Group:          o.group,
		Version:        o.version,
		Kind:           o.kind,
		Namespace:      o.namespace,
		Name:           o.name,
		SpecHash:       o.specHash,
		StatusHash:     o.statusHash,
		LastObservedAt: o.lastObservedAt,
	}
}

// Store folds observations into the bounded revision timeline. Writers
// are the kube boundary's capture taps, which run on several collector
// and watch goroutines; readers receive value copies.
//
// It intentionally has no collector loop of its own: capture piggybacks
// on the existing per-resource watches, so the store's liveness is
// exactly the collectors' liveness and there is no second set of watch
// connections to keep honest.
//
// Retained state lives in memory either way — the bounds guarantee it
// fits — and an optional persister mirrors every mutation so a restart
// resumes instead of forgetting. Reads never touch the persister.
type Store struct {
	mu      sync.Mutex
	limits  Limits
	clock   Clock
	persist Persister

	seq        uint64
	generation uint64
	// entries is the retained timeline in observation order. Pointers,
	// so the coalescing target stays addressable across evictions.
	entries    []*Revision
	objects    map[string]*objectState
	scopes     map[string]map[string]bool
	totalBytes int
	evicted    bool
}

// The store is the Recorder the kube boundary consumes.
var _ Recorder = (*Store)(nil)

// NewStore returns an empty store bounded by limits, retained in memory
// only.
func NewStore(limits Limits, clock Clock) *Store {
	return &Store{
		limits:  limits,
		clock:   clock,
		objects: map[string]*objectState{},
		scopes:  map[string]map[string]bool{},
	}
}

// NewPersistedStore returns a store primed from the persister's
// contents and mirroring every further mutation into it. Priming
// re-enforces the configured limits, so a persisted timeline larger
// than the current bounds is trimmed — and the trim is mirrored — before
// the first observation arrives.
//
// A load failure is returned, not degraded around: a deployment that
// mounted a journal expects history to survive restarts, and silently
// starting empty would violate exactly the promise the mount makes.
func NewPersistedStore(limits Limits, clock Clock, persist Persister) (*Store, error) {
	contents, err := persist.Load()
	if err != nil {
		return nil, err
	}
	s := NewStore(limits, clock)
	s.persist = persist
	s.prime(contents)
	return s, nil
}

// prime rebuilds the retained state from persisted contents. The next
// seeds then classify against the previous process life's state, which
// is what turns "changed while this console was down" into an explicit
// after-gap revision instead of a silent first observation.
func (s *Store) prime(contents Contents) {
	for i := range contents.Revisions {
		rev := contents.Revisions[i]
		s.entries = append(s.entries, &rev)
		s.totalBytes += revisionSize(&rev)
	}
	s.seq = contents.Seq
	// Generation resumes at the sequence: monotonic across the restart
	// without persisting a second counter.
	s.generation = contents.Seq
	s.evicted = contents.Evicted

	for uid, rec := range contents.Objects {
		s.objects[uid] = &objectState{
			scope:          rec.Scope,
			group:          rec.Group,
			version:        rec.Version,
			kind:           rec.Kind,
			namespace:      rec.Namespace,
			name:           rec.Name,
			specHash:       rec.SpecHash,
			statusHash:     rec.StatusHash,
			lastObservedAt: rec.LastObservedAt,
		}
		s.scopeSet(rec.Scope)[uid] = true
	}
	// Counts and coalescing targets are derived from the revisions
	// rather than persisted: the newest retained revision of a live UID
	// is its coalescing target by definition.
	for _, rev := range s.entries {
		if state := s.objects[rev.UID]; state != nil {
			state.count++
			state.last = rev
		}
	}
	for uid, state := range s.objects {
		s.enforcePerObject(uid, state)
	}
	s.enforceBounds()
}

// Observe records one watch delivery.
func (s *Store) Observe(obs Observation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observe(obs, s.clock.Now(), false)
}

// Seed records one complete listing of a scope: it first accounts for
// every previously-known object the listing no longer contains, then
// folds the listed objects in. An unchanged object deduplicates away, so
// a reconnect's re-seed adds nothing to the timeline.
func (s *Store) Seed(scope string, obs []Observation) {
	s.SeedIn(scope, nil, obs)
}

// SeedIn is Seed for a listing that covered only some namespaces of the
// scope: a previously-known object outside them is not accounted for — it
// is unknown, not gone — so a namespace the console lost access to never
// reads as a mass deletion. Nil namespaces means the whole scope; "" names
// the cluster-scoped objects.
func (s *Store) SeedIn(scope string, namespaces []string, obs []Observation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()
	var covered map[string]bool
	if namespaces != nil {
		covered = make(map[string]bool, len(namespaces))
		for _, ns := range namespaces {
			covered[ns] = true
		}
	}

	listed := make(map[string]bool, len(obs))
	for i := range obs {
		listed[obs[i].UID] = true
	}
	// Missing UIDs are collected before any mutation and deleted in a
	// stable order, so the synthesized revisions do not depend on map
	// iteration.
	var gone []string
	for uid := range s.scopes[scope] {
		if listed[uid] {
			continue
		}
		if state := s.objects[uid]; covered != nil && (state == nil || !covered[state.namespace]) {
			continue
		}
		gone = append(gone, uid)
	}
	sort.Strings(gone)
	for _, uid := range gone {
		s.deleteUnobserved(uid, now)
	}
	for i := range obs {
		s.observe(obs[i], now, true)
	}
}

// observe folds one observation under the lock.
func (s *Store) observe(obs Observation, now time.Time, seed bool) {
	if obs.Deleted {
		s.deleteObserved(obs, now)
		return
	}

	state := s.objects[obs.UID]
	if state == nil {
		change := ChangeCreated
		if seed {
			change = ChangeFirstObserved
		}
		state = &objectState{
			scope:     obs.Scope,
			group:     obs.Group,
			version:   obs.Version,
			kind:      obs.Kind,
			namespace: obs.Namespace,
			name:      obs.Name,
		}
		s.objects[obs.UID] = state
		s.scopeSet(obs.Scope)[obs.UID] = true
		s.append(revisionOf(obs, change, now, time.Time{}, false), state)
		s.commit(obs, state, now)
		return
	}

	if obs.SpecHash == state.specHash && obs.StatusHash == state.statusHash {
		// The definition is exactly what the last revision recorded; the
		// only news is that it was seen again — which still moves the
		// object's gap window, so it is mirrored.
		state.lastObservedAt = now
		if s.persist != nil {
			s.persist.PutObject(obs.UID, state.record())
		}
		return
	}

	change := ChangeStatus
	if obs.SpecHash != state.specHash {
		change = ChangeSpec
	}

	if change == ChangeStatus && !seed && s.coalesce(obs, state, now) {
		return
	}
	s.append(revisionOf(obs, change, now, state.lastObservedAt, seed), state)
	s.commit(obs, state, now)
}

// commit records the observation's hashes and time as the object's
// current classification state and mirrors it.
func (s *Store) commit(obs Observation, state *objectState, now time.Time) {
	state.specHash = obs.SpecHash
	state.statusHash = obs.StatusHash
	state.lastObservedAt = now
	if s.persist != nil {
		s.persist.PutObject(obs.UID, state.record())
	}
}

// coalesce folds a status transition into the previous one when both
// land inside the window. It reports whether it did; a false return
// means the caller appends a fresh revision.
func (s *Store) coalesce(obs Observation, state *objectState, now time.Time) bool {
	last := state.last
	if s.limits.CoalesceWindow <= 0 || last == nil || last.Change != ChangeStatus {
		return false
	}
	if now.Sub(last.ObservedAt) >= s.limits.CoalesceWindow {
		return false
	}
	s.totalBytes -= revisionSize(last)
	last.Manifest = obs.Manifest
	last.StatusHash = obs.StatusHash
	last.Generation = obs.Generation
	last.Actor = obs.Actor
	last.Owners = obs.Owners
	last.ObservedAt = now
	last.Coalesced++
	s.totalBytes += revisionSize(last)
	if s.persist != nil {
		s.persist.Update(*last)
	}
	s.commit(obs, state, now)
	s.generation++
	s.enforceBounds()
	return true
}

// deleteObserved folds a watch-delivered deletion. A deletion of an
// object never retained is silently dropped: it was never part of this
// timeline, and a deletion whose UID does not match the retained
// incarnation must not touch the newer incarnation's history.
func (s *Store) deleteObserved(obs Observation, now time.Time) {
	state := s.objects[obs.UID]
	if state == nil {
		return
	}
	rev := revisionOf(obs, ChangeDeleted, now, state.lastObservedAt, false)
	rev.Manifest = nil
	s.append(rev, state)
	s.forget(obs.UID)
}

// deleteUnobserved synthesizes the deletion of an object a complete
// seed no longer contains. The revision's honest content is the window:
// last seen at PrevObservedAt, gone by ObservedAt. Identity comes from
// the classification state, so the revision stays named even when every
// revision of the object was already evicted.
func (s *Store) deleteUnobserved(uid string, now time.Time) {
	state := s.objects[uid]
	if state == nil {
		return
	}
	rev := &Revision{
		Scope:          state.scope,
		Group:          state.group,
		Version:        state.version,
		Kind:           state.kind,
		Namespace:      state.namespace,
		Name:           state.name,
		UID:            uid,
		Change:         ChangeDeletedUnobserved,
		AfterGap:       true,
		ObservedAt:     now,
		PrevObservedAt: state.lastObservedAt,
	}
	s.append(rev, state)
	s.forget(uid)
}

// forget drops an incarnation's classification state once it is deleted.
// Its retained revisions stay until retention evicts them; only the
// live-object bookkeeping goes, which is what keeps the objects map
// bounded by namespace reality rather than by everything ever seen.
//
// The UID leaves every scope set, not only the delivering one: a pod can
// match two watches' selectors, so its deletion may arrive through a
// different scope than the one that first observed it, and a UID left
// behind would make that scope's next seed synthesize a deletion that
// already happened.
func (s *Store) forget(uid string) {
	delete(s.objects, uid)
	for _, set := range s.scopes {
		delete(set, uid)
	}
	if s.persist != nil {
		s.persist.DeleteObject(uid)
	}
}

// append retains one revision, mirrors it, and enforces the bounds.
func (s *Store) append(rev *Revision, state *objectState) {
	s.seq++
	rev.Seq = s.seq
	s.entries = append(s.entries, rev)
	s.totalBytes += revisionSize(rev)
	state.count++
	state.last = rev
	s.generation++
	if s.persist != nil {
		s.persist.Append(*rev)
	}
	s.enforcePerObject(rev.UID, state)
	s.enforceBounds()
}

// enforcePerObject evicts the object's oldest revisions until the
// incarnation is back under its cap, so a churning object can never crowd
// the quiet ones out of the global budget. On append the excess is one;
// on prime it is whatever a larger cap of a previous life left.
func (s *Store) enforcePerObject(uid string, state *objectState) {
	if s.limits.PerObject <= 0 {
		return
	}
	for i := 0; state.count > s.limits.PerObject && i < len(s.entries); {
		if s.entries[i].UID == uid {
			s.evict(i) // the next entry moved to i
			continue
		}
		i++
	}
}

// enforceBounds evicts oldest-first until the global bounds hold.
func (s *Store) enforceBounds() {
	for len(s.entries) > 0 {
		over := s.limits.MaxRevisions > 0 && len(s.entries) > s.limits.MaxRevisions ||
			s.limits.MaxBytes > 0 && s.totalBytes > s.limits.MaxBytes
		if !over {
			return
		}
		s.evict(0)
	}
}

// evict removes the retained revision at index i and mirrors the
// removal.
func (s *Store) evict(i int) {
	rev := s.entries[i]
	s.entries = append(s.entries[:i], s.entries[i+1:]...)
	s.totalBytes -= revisionSize(rev)
	if s.persist != nil {
		s.persist.Evict(rev.Seq)
		if !s.evicted {
			s.persist.MarkEvicted()
		}
	}
	s.evicted = true
	if state := s.objects[rev.UID]; state != nil {
		state.count--
		if state.last == rev {
			state.last = nil
		}
	}
}

// Snapshot returns the timeline, newest first, and whether any revision
// is retained.
func (s *Store) Snapshot() (Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := Snapshot{
		Generation: s.generation,
		Evicted:    s.evicted,
		Entries:    make([]Entry, 0, len(s.entries)),
	}
	for i := len(s.entries) - 1; i >= 0; i-- {
		rev := s.entries[i]
		snap.Entries = append(snap.Entries, Entry{
			Seq:            rev.Seq,
			Scope:          rev.Scope,
			Group:          rev.Group,
			Version:        rev.Version,
			Kind:           rev.Kind,
			Namespace:      rev.Namespace,
			Name:           rev.Name,
			UID:            rev.UID,
			Change:         rev.Change,
			AfterGap:       rev.AfterGap,
			Actor:          rev.Actor,
			ObservedAt:     rev.ObservedAt,
			PrevObservedAt: rev.PrevObservedAt,
			Coalesced:      rev.Coalesced,
			HasManifest:    rev.Manifest != nil,
		})
	}
	return snap, len(snap.Entries) > 0
}

// Revision resolves one retained revision by sequence. The manifest is
// copied: a returned revision is the caller's value, never shared
// mutable state.
func (s *Store) Revision(seq uint64) (Revision, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := sort.Search(len(s.entries), func(i int) bool { return s.entries[i].Seq >= seq })
	if i >= len(s.entries) || s.entries[i].Seq != seq {
		return Revision{}, false
	}
	rev := *s.entries[i]
	rev.Manifest = append([]byte(nil), rev.Manifest...)
	rev.Owners = copyOwners(rev.Owners)
	return rev, true
}

// revisionSize is the retention cost of one revision: the manifest plus
// the attribution strings, so the byte budget stays the whole truth.
func revisionSize(rev *Revision) int {
	size := len(rev.Manifest)
	for i := range rev.Owners {
		size += len(rev.Owners[i].Manager)
		for _, path := range rev.Owners[i].Paths {
			size += len(path)
		}
	}
	return size
}

// copyOwners deep-copies an owner set, so a returned revision is the
// caller's value.
func copyOwners(owners []FieldOwner) []FieldOwner {
	if owners == nil {
		return nil
	}
	copied := make([]FieldOwner, len(owners))
	for i := range owners {
		copied[i] = FieldOwner{
			Manager: owners[i].Manager,
			Paths:   append([]string(nil), owners[i].Paths...),
		}
	}
	return copied
}

// revisionOf builds the retained record of one observation.
func revisionOf(obs Observation, change Change, now, prev time.Time, afterGap bool) *Revision {
	return &Revision{
		Scope:          obs.Scope,
		Group:          obs.Group,
		Version:        obs.Version,
		Kind:           obs.Kind,
		Namespace:      obs.Namespace,
		Name:           obs.Name,
		UID:            obs.UID,
		Generation:     obs.Generation,
		Change:         change,
		AfterGap:       afterGap,
		Actor:          obs.Actor,
		ObservedAt:     now,
		PrevObservedAt: prev,
		SpecHash:       obs.SpecHash,
		StatusHash:     obs.StatusHash,
		Owners:         obs.Owners,
		Manifest:       obs.Manifest,
	}
}

// scopeSet returns the live-UID set of one scope, creating it on first
// use.
func (s *Store) scopeSet(scope string) map[string]bool {
	set := s.scopes[scope]
	if set == nil {
		set = map[string]bool{}
		s.scopes[scope] = set
	}
	return set
}
