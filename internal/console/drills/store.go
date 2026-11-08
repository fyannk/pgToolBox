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

// Package drills keeps and judges the console's restore drills: the record of each drill this console created, the check that
// decides it without SQL, and the supervisor that tears it down. The
// drill's start is an operation (internal/console/operations, as the
// user); its deletion is internal/console/ops' DrillReaper (the console's
// ServiceAccount, sealed drills only). This package writes nothing to the
// API server itself.
package drills

import (
	"encoding/json"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// Key names one drill Cluster.
type Key struct{ Namespace, Name string }

// SourceKey names one source Cluster.
type SourceKey struct{ Namespace, Name string }

// Record is the console's record of one drill: the creation record the
// drill carries (sealed) plus what the console observed and decided.
type Record struct {
	Drill           Key       `json:"drill"`
	Source          SourceKey `json:"source"`
	UID             string    `json:"uid,omitempty"`
	Backup          string    `json:"backup,omitempty"`
	BackupLabel     string    `json:"backupLabel,omitempty"`
	BackupStoppedAt time.Time `json:"backupStoppedAt,omitzero"`
	StartedAt       time.Time `json:"startedAt"`
	Deadline        time.Time `json:"deadline"`
	SourceBytes     *int64    `json:"sourceBytes,omitempty"`
	// SeenAt is when the console first saw the drill Cluster in its cache.
	SeenAt        time.Time              `json:"seenAt,omitzero"`
	HealthyAt     time.Time              `json:"healthyAt,omitzero"`
	PromotedAt    time.Time              `json:"promotedAt,omitzero"`
	FinishedAt    time.Time              `json:"finishedAt,omitzero"`
	TornDownAt    time.Time              `json:"tornDownAt,omitzero"`
	RestoredBytes *int64                 `json:"restoredBytes,omitempty"`
	Timeline      *int                   `json:"timeline,omitempty"`
	Result        viewmodel.DrillResult  `json:"result"`
	Reason        string                 `json:"reason,omitempty"`
	Checks        []viewmodel.DrillCheck `json:"checks,omitempty"`
	TeardownError string                 `json:"teardownError,omitempty"`
	// LeftInPlace marks a drill the console stopped supervising because
	// its Cluster no longer verifies as this console's drill (seal, owner
	// or labels changed): it was not deleted, and never will be by the
	// console. TeardownError says so.
	LeftInPlace bool `json:"leftInPlace,omitempty"`
}

// Finished reports a decided result.
func (r Record) Finished() bool { return r.Result != "" && r.Result != viewmodel.DrillRunning }

// Persister mirrors records to a durable medium (the history journal
// when history is persistent). Put must not block on disk for long; Load
// is called once, before any Put.
type Persister interface {
	LoadDrills() ([][]byte, error)
	// DrillsSince is when the medium started keeping drill records,
	// recording now on the first call.
	DrillsSince(now time.Time) (time.Time, error)
	PutDrill(key string, value []byte) error
	DeleteDrill(key string) error
}

// Bounds of the store: per source and in total. Drills are rare (a
// person starts each), so these hold years of manual drills.
const (
	MaxPerSource = 50
	maxTotal     = 2000
)

// Store holds the records, bounded, newest kept.
type Store struct {
	mu      sync.RWMutex
	records map[Key]Record
	persist Persister
	logger  *slog.Logger
	// since is when the record became complete (RecordedSince).
	since time.Time
}

// NewStore returns a store, primed from persist when one is given. A
// record that does not decode is skipped (logged by category), never a
// start failure: the drill record is evidence, not configuration. now is
// the console's start: an in-memory record is complete from then on, and
// so is a persistent one whose records could not all be read.
func NewStore(persist Persister, logger *slog.Logger, now time.Time) *Store {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	s := &Store{records: map[Key]Record{}, persist: persist, logger: logger, since: now.UTC()}
	if persist == nil {
		return s
	}
	raw, err := persist.LoadDrills()
	if err != nil {
		logger.Warn("restore drill records not loaded", slog.String("category", "unavailable"))
		return s
	}
	complete := true
	for _, b := range raw {
		var r Record
		if json.Unmarshal(b, &r) != nil || r.Drill.Name == "" {
			logger.Warn("restore drill record skipped", slog.String("category", "malformed"))
			complete = false
			continue
		}
		s.records[r.Drill] = r
	}
	since, err := persist.DrillsSince(now)
	switch {
	case err != nil:
		logger.Warn("restore drill record start not read", slog.String("category", "unavailable"))
	case complete && !since.IsZero() && since.Before(now):
		s.since = since.UTC()
	}
	return s
}

// RecordedSince is when the record became complete: every drill this
// console ran from then on is in it, or a newer passed drill of the same
// source is (the bounds never drop a source's newest pass). Before it,
// the record says nothing.
func (s *Store) RecordedSince() time.Time { return s.since }

// Persistent reports whether records survive a restart.
func (s *Store) Persistent() bool { return s.persist != nil }

func storeKey(k Key) string { return k.Namespace + "/" + k.Name }

// Put records r (replacing the record of the same drill) and applies the
// bounds.
func (s *Store) Put(r Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[r.Drill] = r
	s.mirror(r)
	s.enforce(r.Source)
}

// Get returns the record of one drill.
func (s *Store) Get(k Key) (Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.records[k]
	return r, ok
}

// Source returns the records of one source, newest first.
func (s *Store) Source(src SourceKey) []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Record
	for _, r := range s.records {
		if r.Source == src {
			out = append(out, r)
		}
	}
	sortNewest(out)
	return out
}

// All returns every record, newest first.
func (s *Store) All() []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Record, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, r)
	}
	sortNewest(out)
	return out
}

// LastPassed is when the newest passed drill of src was judged.
func (s *Store) LastPassed(src SourceKey) (time.Time, bool) {
	var last time.Time
	found := false
	for _, r := range s.Source(src) {
		if r.Result == viewmodel.DrillPassed && (!found || r.FinishedAt.After(last)) {
			last, found = r.FinishedAt, true
		}
	}
	return last, found
}

func sortNewest(rs []Record) {
	sort.Slice(rs, func(i, j int) bool {
		if !rs[i].StartedAt.Equal(rs[j].StartedAt) {
			return rs[i].StartedAt.After(rs[j].StartedAt)
		}
		return storeKey(rs[i].Drill) < storeKey(rs[j].Drill)
	})
}

// enforce drops the oldest finished records beyond the bounds; a running
// drill is never dropped (its teardown depends on nothing here, but its
// record is what the screen shows).
func (s *Store) enforce(src SourceKey) {
	var bySource, all []Record
	for _, r := range s.records {
		all = append(all, r)
		if r.Source == src {
			bySource = append(bySource, r)
		}
	}
	// A source's newest pass is kept whatever the bounds: dropping it
	// would turn "passed 40 days ago" into "no pass recorded".
	newestPass := map[SourceKey]Key{}
	for _, r := range all {
		if r.Result != viewmodel.DrillPassed {
			continue
		}
		if k, ok := newestPass[r.Source]; !ok || r.FinishedAt.After(s.records[k].FinishedAt) {
			newestPass[r.Source] = r.Drill
		}
	}
	drop := func(rs []Record, keep int) {
		sortNewest(rs)
		for _, r := range rs[min(keep, len(rs)):] {
			if newestPass[r.Source] == r.Drill {
				continue
			}
			if r.Finished() {
				delete(s.records, r.Drill)
				if s.persist != nil {
					if err := s.persist.DeleteDrill(storeKey(r.Drill)); err != nil {
						s.logger.Warn("restore drill record not deleted", slog.String("category", "unavailable"))
					}
				}
			}
		}
	}
	drop(bySource, MaxPerSource)
	if len(s.records) > maxTotal {
		all = all[:0]
		for _, r := range s.records {
			all = append(all, r)
		}
		drop(all, maxTotal)
	}
}

func (s *Store) mirror(r Record) {
	if s.persist == nil {
		return
	}
	b, err := json.Marshal(r)
	if err == nil {
		err = s.persist.PutDrill(storeKey(r.Drill), b)
	}
	if err != nil {
		s.logger.Warn("restore drill record not persisted", slog.String("category", "unavailable"))
	}
}

// View converts a record to its view model.
func View(r Record) viewmodel.RestoreDrill {
	at := func(t time.Time) *time.Time {
		if t.IsZero() {
			return nil
		}
		u := t.UTC()
		return &u
	}
	v := viewmodel.RestoreDrill{
		Drill:  viewmodel.ClusterRef{Namespace: r.Drill.Namespace, Name: r.Drill.Name, UID: r.UID},
		Source: viewmodel.ClusterRef{Namespace: r.Source.Namespace, Name: r.Source.Name},
		Backup: r.Backup, BackupLabel: r.BackupLabel, BackupStoppedAt: at(r.BackupStoppedAt),
		StartedAt: r.StartedAt.UTC(), Deadline: r.Deadline.UTC(),
		HealthyAt: at(r.HealthyAt), FinishedAt: at(r.FinishedAt), TornDownAt: at(r.TornDownAt),
		RestoredBytes: r.RestoredBytes, SourceBytes: r.SourceBytes, Timeline: r.Timeline,
		Result: r.Result, Health: viewmodel.DrillHealth(r.Result), Reason: r.Reason,
		Checks: slices.Clone(r.Checks), TeardownError: r.TeardownError,
	}
	if v.Result == "" {
		v.Result = viewmodel.DrillRunning
		v.Health = viewmodel.DrillHealth(v.Result)
	}
	if v.Checks == nil {
		v.Checks = []viewmodel.DrillCheck{}
	}
	if !r.HealthyAt.IsZero() {
		d := int64(r.HealthyAt.Sub(r.StartedAt).Round(time.Second) / time.Second)
		v.DurationSeconds = &d
	}
	return v
}
