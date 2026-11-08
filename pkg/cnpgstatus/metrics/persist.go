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

package metrics

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"sort"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
	"github.com/fyannk/pgToolBox/pkg/redact"
)

// Metrics persistence is a periodic snapshot, not a journal: the whole
// bounded window is written atomically (temp file, fsync, rename) once
// a minute and once at shutdown, and read back at startup. The trade is
// stated: a hard kill loses at most the last minute of samples — which
// the next read then shows as a gap, like any other unswept span. The
// history journal made the opposite trade (a 2s write-behind of
// deltas) because a revision timeline is a record; this window is a
// bounded cache of exporter claims, and one minute of it is worth less
// than the journal machinery it would take to keep.
//
// The counter baselines are deliberately not persisted: after a
// restart the first sweep of a counter series yields no rate, so the
// downtime reads as a gap instead of a rate averaged across it.

// snapshotVersion gates the gob payload. A mismatched snapshot is
// discarded with a logged warning rather than failing startup: unlike
// the history journal this file is a bounded cache, and starting it
// empty is the same honest state as first boot.
const snapshotVersion = 1

// flushEvery is the snapshot cadence.
const flushEvery = time.Minute

// PersistedState is the serialized window.
type PersistedState struct {
	// Version gates the layout.
	Version int
	// SavedAt is when the snapshot was taken, Unix seconds.
	SavedAt int64
	// Instruments are every (instance, series) window.
	Instruments []PersistedInstrument
}

// PersistedInstrument is one (instance, series) window.
type PersistedInstrument struct {
	Instance string
	Series   string
	LastAt   int64
	Raw      []PersistedSample
	Rollups  []PersistedBucket
	Open     PersistedBucket
}

// PersistedSample is one raw sample.
type PersistedSample struct {
	At  int64
	Val float64
}

// PersistedBucket is one rollup aggregate; Count 0 means none.
type PersistedBucket struct {
	Start         int64
	Min, Max, Sum float64
	Count         int
}

func exportBucket(b bucket) PersistedBucket {
	return PersistedBucket{Start: b.start, Min: b.min, Max: b.max, Sum: b.sum, Count: b.count}
}

func importBucket(b PersistedBucket) bucket {
	return bucket{start: b.Start, min: b.Min, max: b.Max, sum: b.Sum, count: b.Count}
}

// Export copies the retained window out of the store.
func (s *Store) Export(at time.Time) PersistedState {
	s.mu.Lock()
	defer s.mu.Unlock()

	state := PersistedState{Version: snapshotVersion, SavedAt: at.Unix()}
	for instance, inst := range s.instances {
		for key, ins := range inst.byKey {
			p := PersistedInstrument{
				Instance: instance, Series: key, LastAt: inst.lastAt,
				Open: exportBucket(ins.open),
			}
			inst.times.indices(func(i int) {
				if v := ins.raw.buf[i]; !math.IsNaN(v) {
					p.Raw = append(p.Raw, PersistedSample{At: inst.times.buf[i], Val: v})
				}
			})
			ins.roll.each(func(b bucket) {
				p.Rollups = append(p.Rollups, exportBucket(b))
			})
			state.Instruments = append(state.Instruments, p)
		}
	}
	sort.Slice(state.Instruments, func(a, b int) bool {
		if state.Instruments[a].Instance != state.Instruments[b].Instance {
			return state.Instruments[a].Instance < state.Instruments[b].Instance
		}
		return state.Instruments[a].Series < state.Instruments[b].Series
	})
	return state
}

// Import rebuilds the window from a snapshot, clamped to the store's
// current bounds: rings keep their configured capacities, series
// outside the catalog are dropped, and when the snapshot carries more
// instances than the cap the most recently observed win. Samples older
// than the windows at now (RawWindow for the raw tier, Retention for
// the rollups) are dropped: a snapshot read after a long outage holds
// nothing the live store would still keep. Counter baselines start
// empty, so the first post-restart sweep of a counter series claims
// nothing: the restart reads as a reset, never as a rate averaged
// across the downtime.
func (s *Store) Import(state PersistedState, now time.Time) {
	state = s.withinWindows(state, now)
	s.mu.Lock()
	defer s.mu.Unlock()

	lastByInstance := map[string]int64{}
	for _, p := range state.Instruments {
		if p.LastAt > lastByInstance[p.Instance] {
			lastByInstance[p.Instance] = p.LastAt
		}
	}
	names := make([]string, 0, len(lastByInstance))
	for name := range lastByInstance {
		names = append(names, name)
	}
	sort.Slice(names, func(a, b int) bool { return lastByInstance[names[a]] > lastByInstance[names[b]] })
	if len(names) > s.limits.MaxInstances {
		names = names[:s.limits.MaxInstances]
	}
	keep := map[string]bool{}
	for _, name := range names {
		keep[name] = true
	}

	// The raw tier shares one time axis per instance: gather each kept
	// instance's instruments first, then lay their samples on the union
	// of their timestamps, newest last, clamped to the ring capacity.
	byInstance := map[string][]PersistedInstrument{}
	for _, p := range state.Instruments {
		if !keep[p.Instance] {
			continue
		}
		if _, ok := s.catalog.SeriesByKey(p.Series); !ok {
			continue
		}
		byInstance[p.Instance] = append(byInstance[p.Instance], p)
	}
	for name, instruments := range byInstance {
		timeSet := map[int64]bool{}
		for _, p := range instruments {
			for _, v := range p.Raw {
				timeSet[v.At] = true
			}
		}
		axis := make([]int64, 0, len(timeSet))
		for at := range timeSet {
			axis = append(axis, at)
		}
		sort.Slice(axis, func(a, b int) bool { return axis[a] < axis[b] })

		inst := &instanceSeries{times: s.newTimes(), byKey: map[string]*instrument{}}
		s.instances[name] = inst
		for _, p := range instruments {
			if p.LastAt > inst.lastAt {
				inst.lastAt = p.LastAt
			}
		}
		if capacity := len(inst.times.buf); len(axis) > capacity {
			axis = axis[len(axis)-capacity:]
		}
		for _, at := range axis {
			inst.times.push(at)
		}
		for _, p := range instruments {
			values := make(map[int64]float64, len(p.Raw))
			for _, v := range p.Raw {
				values[v.At] = v.Val
			}
			ins := &instrument{raw: newRing[float64](len(inst.times.buf)), roll: s.newRollups(), open: importBucket(p.Open)}
			for _, at := range axis {
				v, ok := values[at]
				if !ok {
					v = math.NaN()
				}
				ins.raw.push(v)
			}
			for _, b := range p.Rollups {
				ins.roll.push(importBucket(b))
			}
			inst.byKey[p.Series] = ins
		}
	}
}

// withinWindows drops what the live store would no longer hold at now,
// and the instruments left with nothing.
func (s *Store) withinWindows(state PersistedState, now time.Time) PersistedState {
	rawFrom := now.Add(-s.limits.RawWindow).Unix()
	rollFrom := now.Add(-s.limits.Retention).Unix()
	out := PersistedState{Version: state.Version, SavedAt: state.SavedAt, Instruments: make([]PersistedInstrument, 0, len(state.Instruments))}
	for _, p := range state.Instruments {
		kept := PersistedInstrument{Instance: p.Instance, Series: p.Series, LastAt: p.LastAt}
		for _, v := range p.Raw {
			if v.At >= rawFrom {
				kept.Raw = append(kept.Raw, v)
			}
		}
		for _, b := range p.Rollups {
			if b.Start >= rollFrom {
				kept.Rollups = append(kept.Rollups, b)
			}
		}
		if p.Open.Count > 0 && p.Open.Start >= rollFrom {
			kept.Open = p.Open
		}
		if len(kept.Raw) == 0 && len(kept.Rollups) == 0 && kept.Open.Count == 0 {
			continue
		}
		out.Instruments = append(out.Instruments, kept)
	}
	return out
}

// Persister owns the snapshot file: it primes the store at startup and
// rewrites the file on a fixed cadence plus once at shutdown.
type Persister struct {
	store  *Store
	path   string
	clock  observe.Clock
	logger *slog.Logger
}

// OpenPersister loads any existing snapshot into the store and proves
// the path writable by taking one snapshot immediately. An unreadable
// mount or unwritable path fails here, before the listener: the
// deployment mounted a persistence contract and half of it must not
// half-work. A snapshot that exists but does not decode is different —
// it is discarded with a warning, because an empty window is the same
// honest state as first boot.
func OpenPersister(path string, store *Store, clock observe.Clock, logger *slog.Logger) (*Persister, error) {
	p := &Persister{store: store, path: path, clock: clock, logger: logger}

	raw, err := os.ReadFile(path) //nolint:gosec // the path is the validated METRICS_PATH configuration, same trust as the history journal's.
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// First boot on this volume.
	case err != nil:
		return nil, redact.NewError("metrics snapshot read", redact.CategoryInternal, err)
	default:
		var state PersistedState
		if decodeErr := gob.NewDecoder(bytes.NewReader(raw)).Decode(&state); decodeErr != nil || state.Version != snapshotVersion {
			logger.Warn("metrics snapshot unreadable; starting empty",
				slog.Int("version", state.Version))
		} else {
			store.Import(state, clock.Now())
		}
	}

	if err := p.flush(); err != nil {
		return nil, err
	}
	return p, nil
}

// Run snapshots until the context ends, then takes the final snapshot.
func (p *Persister) Run(ctx context.Context) error {
	for {
		if err := p.clock.Wait(ctx, flushEvery); err != nil {
			if flushErr := p.flush(); flushErr != nil {
				p.logger.Warn("metrics snapshot final flush failed",
					slog.String("category", redact.Safe(flushErr)))
			}
			return err
		}
		if err := p.flush(); err != nil {
			p.logger.Warn("metrics snapshot flush failed",
				slog.String("category", redact.Safe(err)))
		}
	}
}

// flush writes one snapshot atomically: temp file in the same
// directory, fsync, rename over the previous snapshot.
func (p *Persister) flush() error {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(p.store.Export(p.clock.Now())); err != nil {
		return redact.NewError("metrics snapshot encode", redact.CategoryInternal, err)
	}
	tmp := p.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // the path derives from the validated METRICS_PATH configuration.
	if err != nil {
		return redact.NewError("metrics snapshot write", redact.CategoryInternal, err)
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		return redact.NewError("metrics snapshot write", redact.CategoryInternal, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return redact.NewError("metrics snapshot sync", redact.CategoryInternal, err)
	}
	if err := f.Close(); err != nil {
		return redact.NewError("metrics snapshot close", redact.CategoryInternal, err)
	}
	if err := os.Rename(tmp, p.path); err != nil {
		return redact.NewError("metrics snapshot rename", redact.CategoryInternal, err)
	}
	return nil
}
