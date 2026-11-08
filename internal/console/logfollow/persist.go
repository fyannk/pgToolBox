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

package logfollow

import (
	"sort"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/logstream"
)

// The log matcher's part of the state snapshot
// (internal/console/statesnap): per Cluster, keyed by its UID, the
// observations still inside LOG_MATCH_MAX_AGE. A log finding of the
// last hours survives a restart instead of vanishing until the line
// repeats. The open/closed state of each stream is not carried: it is a
// fact about this process, and the restarted follower reports every
// container unread until its stream opens.

// SnapshotVersion gates the layout of PersistedState.
const SnapshotVersion = 1

// MaxSnapshotBytes bounds the snapshot: every rule's observation on
// every container of a few hundred Clusters, each with its 2 KiB line.
const MaxSnapshotBytes = 64 << 20

// PersistedState is the follower's snapshot.
type PersistedState struct {
	Clusters []PersistedCluster
}

// PersistedCluster is one Cluster's observations.
type PersistedCluster struct {
	UID, Namespace, Name string
	Observations         []logstream.Observation
}

// Export copies the observations out for a snapshot.
func (f *Follower) Export() PersistedState {
	f.mu.Lock()
	type entry struct {
		key Key
		c   *cluster
	}
	entries := make([]entry, 0, len(f.set))
	for k, c := range f.set {
		entries = append(entries, entry{k, c})
	}
	f.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].key.Namespace != entries[j].key.Namespace {
			return entries[i].key.Namespace < entries[j].key.Namespace
		}
		return entries[i].key.Name < entries[j].key.Name
	})
	out := PersistedState{}
	for _, e := range entries {
		if e.c.uid == "" {
			continue
		}
		if obs := e.c.matcher.Export(); len(obs) > 0 {
			out.Clusters = append(out.Clusters, PersistedCluster{UID: e.c.uid, Namespace: e.key.Namespace, Name: e.key.Name, Observations: obs})
		}
	}
	return out
}

// Import holds a snapshot until its Clusters are followed; it must be
// called before Run. It reports how many Clusters it holds of how many
// the snapshot named.
func (f *Follower) Import(state PersistedState, now time.Time) (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = make(map[Key]PersistedCluster, len(state.Clusters))
	f.pendingAt = now
	for _, c := range state.Clusters {
		k := Key{Namespace: c.Namespace, Name: c.Name}
		if c.UID == "" || k.Namespace == "" || k.Name == "" {
			continue
		}
		if _, dup := f.pending[k]; dup {
			delete(f.pending, k)
			continue
		}
		f.pending[k] = c
	}
	return len(f.pending), len(state.Clusters)
}

// restoreLocked primes a new Cluster's matcher when the Cluster still
// carries the UID it was saved under.
func (f *Follower) restoreLocked(k Key, uid string, matcher *logstream.Matcher) {
	p, ok := f.pending[k]
	if !ok {
		return
	}
	delete(f.pending, k)
	if uid != "" && p.UID == uid {
		matcher.Import(p.Observations, f.opts.Now())
	}
}

// expirePendingLocked forgets restored observations nobody claimed
// within LOG_MATCH_MAX_AGE: they would have expired anyway.
func (f *Follower) expirePendingLocked() {
	if len(f.pending) > 0 && f.opts.MaxAge > 0 && f.opts.Now().Sub(f.pendingAt) > f.opts.MaxAge {
		f.pending = nil
	}
}
