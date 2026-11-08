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

package cache

import (
	"sort"
	"sync"
)

// ClusterKey names one Cluster. An empty Name means "every Cluster of
// the namespace": an object the console could not attribute (an event
// about an unknown pod), or a source that changed state as a whole.
type ClusterKey struct {
	Namespace string
	Name      string
}

// Subscription receives the keys of Clusters whose inputs changed,
// coalesced: a burst of a thousand pod updates is one key, and no change
// is ever dropped, because the pending set is unbounded only in the
// number of distinct Clusters (bounded by the watched namespaces).
type Subscription struct {
	mu      sync.Mutex
	pending map[ClusterKey]struct{}
	ready   chan struct{}
}

func newSubscription() *Subscription {
	return &Subscription{pending: map[ClusterKey]struct{}{}, ready: make(chan struct{}, 1)}
}

// Ready is signalled when at least one key is pending.
func (s *Subscription) Ready() <-chan struct{} { return s.ready }

// Drain returns the pending keys, sorted, and clears them.
func (s *Subscription) Drain() []ClusterKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ClusterKey, 0, len(s.pending))
	for key := range s.pending {
		out = append(out, key)
	}
	clear(s.pending)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (s *Subscription) notify(key ClusterKey) {
	s.mu.Lock()
	s.pending[key] = struct{}{}
	s.mu.Unlock()
	select {
	case s.ready <- struct{}{}:
	default:
	}
}

// hub fans changes out to subscriptions, and stamps every key it
// publishes with the next value of one counter, so a reader can tell
// whether anything about a Cluster changed since it last looked.
type hub struct {
	mu   sync.RWMutex
	subs []*Subscription

	genMu sync.Mutex
	seq   uint64
	gens  map[ClusterKey]uint64
}

func (h *hub) subscribe() *Subscription {
	s := newSubscription()
	h.mu.Lock()
	h.subs = append(h.subs, s)
	h.mu.Unlock()
	return s
}

func (h *hub) unsubscribe(target *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, s := range h.subs {
		if s == target {
			h.subs = append(h.subs[:i], h.subs[i+1:]...)
			return
		}
	}
}

func (h *hub) publish(key ClusterKey) {
	h.genMu.Lock()
	if h.gens == nil {
		h.gens = map[ClusterKey]uint64{}
	}
	h.seq++
	h.gens[key] = h.seq
	h.genMu.Unlock()

	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, s := range h.subs {
		s.notify(key)
	}
}

// generation is the stamp of the latest change to the Cluster or to its
// whole namespace; 0 when neither ever changed. Stamps never repeat, so
// equal generations mean nothing was published in between. The map
// keeps one entry per key ever published, bounded like Subscription's.
func (h *hub) generation(key ClusterKey) uint64 {
	h.genMu.Lock()
	defer h.genMu.Unlock()
	return max(h.gens[key], h.gens[ClusterKey{Namespace: key.Namespace}])
}
