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

package status

import (
	"strings"
	"sync"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/sweep"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// rowMaxAge bounds how long a list row is reused even when nothing it
// was built from changed: its ages ("failed 3 h ago") and observed-at
// stamps are relative to when it was built.
const rowMaxAge = 5 * time.Second

// Generations is implemented by a cache that stamps every change it
// publishes (cache.Manager); a cache without it gets no row reuse.
type Generations interface {
	Generation(cache.ClusterKey) uint64
}

// rowStamp is everything a list row is derived from, reduced to what
// changes when the input changes. Two equal stamps mean the row built
// for the first is the row of the second.
type rowStamp struct {
	generation uint64
	sweptAt    time.Time
	hasSweep   bool
	runAt      time.Time
	runGen     uint64
	// sources is the namespace's source states and the runners'
	// states, which no Cluster change announces.
	sources string
}

type memoRow struct {
	stamp rowStamp
	built time.Time
	row   viewmodel.ClusterRow
}

// rowMemo keeps the latest list row of every Cluster. The list is the
// same for every caller (authorization filters it afterwards), so one
// memo serves all of them; at fleet scale it turns a list request from
// a cache read and deep copy of every Cluster's objects into a read of
// the Clusters that changed. Each row carries its JSON encoding
// (viewmodel.ClusterRow.WithJSON), built with the row and dropped with
// it, so the encoding has the row's invalidation and its bound: one
// entry per listed Cluster, removed by keep when the Cluster goes.
type rowMemo struct {
	mu   sync.Mutex
	rows map[cache.ClusterKey]memoRow
}

// stamp reads the row's inputs; ok is false when the cache cannot say
// whether the Cluster changed, and the row is then always rebuilt.
func (s *Service) rowStamp(key cache.ClusterKey, sources string) (rowStamp, bool) {
	gens, ok := s.cache.(Generations)
	if !ok {
		return rowStamp{}, false
	}
	st := rowStamp{generation: gens.Generation(key), sources: sources}
	if sw := s.sweeper(); sw != nil {
		snap, has := sw.Snapshot(sweep.Key{Namespace: key.Namespace, Name: key.Name})
		st.sweptAt, st.hasSweep = snap.SweptAt, has
	}
	if run := s.latestRun(key); run != nil {
		st.runAt, st.runGen = run.At, run.Generation
	}
	return st, true
}

// namespaceSources fingerprints the source states a row reads.
func (s *Service) namespaceSources(namespace string) string {
	var b strings.Builder
	for _, state := range s.cache.Sources(namespace) {
		b.WriteString(state.Source + "=" + string(state.State) + ":" + state.Reason + ";")
	}
	if sw := s.sweeper(); sw != nil {
		state := sw.State()
		b.WriteString("sweep=" + string(state.State) + ":" + state.Reason + ";")
	}
	if d := s.diagnostics(); d != nil {
		state := d.State()
		b.WriteString("diag=" + string(state.State) + ":" + state.Reason + ";")
	}
	return b.String()
}

func (m *rowMemo) get(key cache.ClusterKey, stamp rowStamp, now time.Time) (viewmodel.ClusterRow, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[key]
	if !ok || r.stamp != stamp || now.Sub(r.built) >= rowMaxAge || now.Before(r.built) {
		return viewmodel.ClusterRow{}, false
	}
	return r.row, true
}

func (m *rowMemo) put(key cache.ClusterKey, stamp rowStamp, now time.Time, row viewmodel.ClusterRow) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = map[cache.ClusterKey]memoRow{}
	}
	m.rows[key] = memoRow{stamp: stamp, built: now, row: row}
}

// keep drops the rows of Clusters no longer listed.
func (m *rowMemo) keep(keys []cache.ClusterKey) {
	listed := make(map[cache.ClusterKey]bool, len(keys))
	for _, key := range keys {
		listed[key] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.rows {
		if !listed[key] {
			delete(m.rows, key)
		}
	}
}
