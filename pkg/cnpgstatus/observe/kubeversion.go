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

package observe

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/fyannk/pgToolBox/pkg/redact"
)

// The Kubernetes server version is the one observed fact that has no
// watch: /version is a plain endpoint, so this is the console's only
// poll against the API server, and it is paced accordingly — the value
// changes once per cluster upgrade.
const (
	// kubeVersionInterval separates successful polls.
	kubeVersionInterval = 5 * time.Minute
	// kubeVersionRetry separates polls after a failure, once the
	// retained snapshot has been marked stale.
	kubeVersionRetry = time.Minute
)

// KubeVersionSnapshot is the API server's own account of its version.
type KubeVersionSnapshot struct {
	// Generation increases by one on every publication.
	Generation uint64
	// ObservedAt is the time of the last successful fetch.
	ObservedAt time.Time
	// Stale reports that the poller has lost contact and GitVersion is
	// the retained last-good observation.
	Stale bool
	// GitVersion is the server's /version report, such as "v1.33.1".
	GitVersion string
}

// KubeVersionSource fetches the server version once.
type KubeVersionSource interface {
	// FetchServerVersion returns the server's reported gitVersion.
	FetchServerVersion(ctx context.Context) (string, error)
}

// KubeVersionStore publishes the latest observation. Same discipline as
// every other store: replacement values, never mutations.
type KubeVersionStore struct {
	mu   sync.RWMutex
	snap KubeVersionSnapshot
	has  bool
}

// NewKubeVersionStore builds an empty store.
func NewKubeVersionStore() *KubeVersionStore {
	return &KubeVersionStore{}
}

// CurrentKubeVersion returns the latest snapshot and whether one was
// ever observed.
func (s *KubeVersionStore) CurrentKubeVersion() (KubeVersionSnapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap, s.has
}

// publish stores a fresh observation.
func (s *KubeVersionStore) publish(gitVersion string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap = KubeVersionSnapshot{
		Generation: s.snap.Generation + 1,
		ObservedAt: at,
		GitVersion: gitVersion,
	}
	s.has = true
}

// markStale flags the retained observation as last-good.
func (s *KubeVersionStore) markStale() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.has || s.snap.Stale {
		return
	}
	s.snap.Generation++
	s.snap.Stale = true
}

// KubeVersionPoller maintains the store from a source.
type KubeVersionPoller struct {
	source KubeVersionSource
	store  *KubeVersionStore
	clock  Clock
	logger *slog.Logger
}

// NewKubeVersionPoller wires a poller.
func NewKubeVersionPoller(source KubeVersionSource, store *KubeVersionStore,
	clock Clock, logger *slog.Logger,
) *KubeVersionPoller {
	return &KubeVersionPoller{source: source, store: store, clock: clock, logger: logger}
}

// Run polls until the context ends. A failure marks the retained
// snapshot stale and shortens the wait; it never erases the last-good
// value, because "the version we last saw" remains a true statement.
func (p *KubeVersionPoller) Run(ctx context.Context) error {
	for {
		wait := kubeVersionInterval
		gitVersion, err := p.source.FetchServerVersion(ctx)
		if err != nil {
			p.store.markStale()
			p.logger.Info("kubernetes version poll failed",
				slog.String("category", redact.Safe(err)))
			wait = kubeVersionRetry
		} else {
			p.store.publish(gitVersion, p.clock.Now())
		}
		if p.clock.Wait(ctx, wait) != nil {
			return nil //nolint:nilerr // the clock returns an error only when ctx ends, which is the clean end of Run
		}
	}
}
