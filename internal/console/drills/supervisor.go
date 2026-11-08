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

package drills

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"

	"github.com/fyannk/pgToolBox/internal/console/ops"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

// Interval is the supervisor's cadence: the drill's duration (start →
// healthy) is measured to within one interval.
const Interval = 15 * time.Second

// cacheLag is how long a just-created drill may be missing from the
// informer cache before the supervisor believes it is gone.
const cacheLag = 2 * time.Minute

// Teardown retries back off from Interval, doubling, to maxBackoff: a
// missing Role is not fixed in seconds, and every attempt is an API call
// and a log line.
const maxBackoff = 10 * time.Minute

// maxUnsealed bounds the set of labelled-but-unsealed Clusters the
// supervisor remembers having warned about.
const maxUnsealed = 1024

// leftInPlaceText is what a drill that stopped verifying says.
const leftInPlaceText = "left in place: the Cluster no longer verifies as a drill of this console (its seal, owner or labels changed); the console will not delete it"

// Observer reads drill Clusters from the console's read model.
type Observer interface {
	// DrillClusters lists the Clusters labelled as drills of owner in
	// the watched namespaces (cache reads).
	DrillClusters(ctx context.Context, owner string) ([]*cnpgv1.Cluster, error)
	// DrillObservation reads one drill Cluster's phase, its instance's
	// /pg/status report and its exporter's database size.
	DrillObservation(ctx context.Context, namespace, name string) (Observation, error)
	// DrillCluster reads one Cluster by name from the cache: nil without
	// error when it does not exist, an error when the namespace is not
	// readable (then nothing is known).
	DrillCluster(ctx context.Context, namespace, name string) (*cnpgv1.Cluster, error)
}

// Reaper verifies and deletes drills (ops.DrillReaper).
type Reaper interface {
	Owner() string
	Verify(c *cnpgv1.Cluster) (ops.DrillMeta, error)
	Teardown(ctx context.Context, c *cnpgv1.Cluster) error
}

// Supervisor judges the running drills and tears down the decided ones.
// Every replica runs one: judging is a pure function of what each reads,
// a delete is idempotent and preconditioned on the UID, so two replicas
// agree without coordinating.
type Supervisor struct {
	store    *Store
	observer Observer
	reaper   Reaper
	clock    observe.Clock
	logger   *slog.Logger

	mu       sync.Mutex
	unsealed map[Key]bool
	retry    map[Key]backoff
}

type backoff struct {
	next  time.Time
	delay time.Duration
}

// NewSupervisor wires a supervisor.
func NewSupervisor(store *Store, observer Observer, reaper Reaper, clock observe.Clock, logger *slog.Logger) *Supervisor {
	if clock == nil {
		clock = observe.RealClock{}
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Supervisor{store: store, observer: observer, reaper: reaper, clock: clock, logger: logger, unsealed: map[Key]bool{}, retry: map[Key]backoff{}}
}

// Run supervises until ctx ends.
func (s *Supervisor) Run(ctx context.Context) error {
	for {
		s.Tick(ctx)
		if err := s.clock.Wait(ctx, Interval); err != nil {
			return nil //nolint:nilerr // the clock returns an error only when ctx ends, which is the clean end of Run
		}
	}
}

// FromMeta is the record of a drill known only by its sealed creation
// record (created by another replica, or before a restart without
// persistent history).
func FromMeta(k Key, m ops.DrillMeta) Record {
	return Record{
		Drill: k, Source: SourceKey{Namespace: m.SourceNamespace, Name: m.Source},
		Backup: m.Backup, BackupLabel: m.BackupLabel, BackupStoppedAt: m.BackupStoppedAt, StartedAt: m.StartedAt, Deadline: m.Deadline,
		SourceBytes: m.SourceBytes,
	}
}

// Tick is one supervision pass.
//
//nolint:gocyclo,cyclop,funlen // one pass over the drills, in order.
func (s *Supervisor) Tick(ctx context.Context) {
	clusters, err := s.observer.DrillClusters(ctx, s.reaper.Owner())
	if err != nil {
		s.logger.Warn("restore drills not supervised this pass", slog.String("category", "unavailable"))
		return
	}
	now := s.clock.Now().UTC()
	present := map[Key]bool{}
	unsealed := map[Key]bool{}
	for _, c := range clusters {
		k := Key{Namespace: c.Namespace, Name: c.Name}
		meta, err := s.reaper.Verify(c)
		if err != nil {
			unsealed[k] = true
			continue
		}
		before, ok := s.store.Get(k)
		rec := before
		if !ok {
			rec = FromMeta(k, meta)
		}
		if rec.UID != "" && rec.UID != string(c.UID) {
			// The record names another incarnation: this one is not the
			// Cluster the console created and recorded.
			unsealed[k] = true
			continue
		}
		if rec.LeftInPlace {
			continue
		}
		present[k] = true
		rec.UID = string(c.UID)
		if rec.SeenAt.IsZero() {
			rec.SeenAt = now
		}
		if !rec.Finished() {
			obs, err := s.observer.DrillObservation(ctx, c.Namespace, c.Name)
			if err != nil {
				obs = Observation{}
			}
			rec = Judge(rec, obs, now)
		}
		if rec.Finished() && c.DeletionTimestamp.IsZero() {
			rec = s.teardown(ctx, c, before, rec, now)
		}
		if !ok || changed(before, rec) {
			s.store.Put(rec)
		}
	}
	s.remember(unsealed)
	for _, rec := range s.store.All() {
		if present[rec.Drill] || !rec.TornDownAt.IsZero() || rec.LeftInPlace {
			continue
		}
		if !rec.Finished() && rec.SeenAt.IsZero() && now.Sub(rec.StartedAt) < cacheLag {
			continue
		}
		// Missing from the labelled list: read it by name before
		// concluding anything. Gone is gone; still there but no longer
		// verifying (relabelled, seal or owner changed) is left in place,
		// never reported as deleted; unreadable is unknown.
		c, err := s.observer.DrillCluster(ctx, rec.Drill.Namespace, rec.Drill.Name)
		switch {
		case err != nil:
			continue
		case c == nil || (rec.UID != "" && string(c.UID) != rec.UID):
			s.store.Put(Vanished(rec, now))
			delete(s.retry, rec.Drill)
		default:
			if _, verr := s.reaper.Verify(c); verr == nil {
				continue // the cache list lags the by-name read
			}
			s.store.Put(LeftInPlace(rec, now))
			delete(s.retry, rec.Drill)
			s.logger.Warn("restore drill left in place: it no longer verifies as a drill of this console",
				slog.String("namespace", rec.Drill.Namespace), slog.String("drill", rec.Drill.Name))
		}
	}
}

// teardown deletes a decided drill, backing off after a failure. The
// delete accepted (or the Cluster already gone) is the teardown.
func (s *Supervisor) teardown(ctx context.Context, c *cnpgv1.Cluster, before, rec Record, now time.Time) Record {
	k := rec.Drill
	s.mu.Lock()
	b := s.retry[k]
	s.mu.Unlock()
	if now.Before(b.next) {
		return rec
	}
	if err := s.reaper.Teardown(ctx, c); err != nil {
		text := safeText(err)
		if text != before.TeardownError {
			s.logger.Warn("restore drill not deleted", slog.String("namespace", k.Namespace), slog.String("drill", k.Name),
				slog.String("reason", text))
		}
		rec.TeardownError = text
		b.delay = min(max(2*b.delay, Interval), maxBackoff)
		b.next = now.Add(b.delay)
		s.mu.Lock()
		s.retry[k] = b
		s.mu.Unlock()
		return rec
	}
	s.mu.Lock()
	delete(s.retry, k)
	s.mu.Unlock()
	rec.TeardownError = ""
	rec.TornDownAt = now
	s.logger.Info("restore drill deleted", slog.String("namespace", k.Namespace), slog.String("drill", k.Name),
		slog.String("result", string(rec.Result)))
	return rec
}

// remember keeps the unsealed lookalikes of this pass only (bounded),
// warning once for each new one.
func (s *Supervisor) remember(seen map[Key]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[Key]bool, min(len(seen), maxUnsealed))
	for k := range seen {
		if len(next) >= maxUnsealed {
			break
		}
		if !s.unsealed[k] {
			s.logger.Warn("a Cluster carries this console's drill labels but not a drill it created; it is left alone",
				slog.String("namespace", k.Namespace), slog.String("cluster", k.Name))
		}
		next[k] = true
	}
	s.unsealed = next
}

func changed(a, b Record) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) != string(jb)
}

func safeText(err error) string {
	var s interface{ SafeText() string }
	if errors.As(err, &s) {
		return s.SafeText()
	}
	if errors.Is(err, ops.ErrNotOurDrill) {
		return ops.ErrNotOurDrill.Error()
	}
	return "the Kubernetes API server did not take the delete"
}
