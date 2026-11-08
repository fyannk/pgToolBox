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
	"math"
	"sort"
	"sync"
	"time"
)

// Tier selects which resolution a read draws from.
type Tier int

const (
	// TierRaw is the scrape-interval window.
	TierRaw Tier = iota
	// TierRollup is the bucketed retention window.
	TierRollup
)

// bucket is one rollup aggregate.
type bucket struct {
	start int64
	min   float64
	max   float64
	sum   float64
	count int
}

// ring is a fixed-capacity append-only window.
type ring[T any] struct {
	buf  []T
	next int
	full bool
}

func newRing[T any](capacity int) *ring[T] {
	if capacity < 1 {
		capacity = 1
	}
	return &ring[T]{buf: make([]T, capacity)}
}

// newGapRing is a value ring of the given capacity whose every slot is
// a gap (NaN), positioned like times so the two stay index-aligned.
func newGapRing(times *ring[int64]) *ring[float64] {
	r := &ring[float64]{buf: make([]float64, len(times.buf)), next: times.next, full: times.full}
	for i := range r.buf {
		r.buf[i] = math.NaN()
	}
	return r
}

func (r *ring[T]) push(v T) {
	r.buf[r.next] = v
	r.next++
	if r.next == len(r.buf) {
		r.next, r.full = 0, true
	}
}

// each visits the retained entries oldest-first.
func (r *ring[T]) each(fn func(T)) {
	r.indices(func(i int) { fn(r.buf[i]) })
}

// indices visits the slots of the retained entries oldest-first. Two
// rings of one capacity pushed in lockstep share their indices.
func (r *ring[T]) indices(fn func(int)) {
	if r.full {
		for i := r.next; i < len(r.buf); i++ {
			fn(i)
		}
	}
	for i := 0; i < r.next; i++ {
		fn(i)
	}
}

// instrument is one (instance, series) window pair.
type instrument struct {
	// raw holds the values of the instance's raw sweeps, index-aligned
	// with instanceSeries.times; NaN is a sweep that claimed nothing.
	raw  *ring[float64]
	roll *ring[bucket]
	// open is the rollup bucket still being filled; count 0 means none.
	open bucket
	// prev is the previous cumulative value of a counter series, for
	// the ingest-time rate conversion. prevAt 0 means none seen.
	prev   float64
	prevAt int64
}

// instanceSeries is one instance's instruments plus its recency, which
// decides eviction when the instance cap is hit.
type instanceSeries struct {
	// times is the raw tier's time axis, shared by every instrument:
	// one sweep reads all series at one instant, so storing the
	// timestamp once per sweep instead of once per series halves the
	// raw tier (it is most of the console's metrics memory).
	times *ring[int64]
	byKey map[string]*instrument
	// instant holds the latest reading of each Instants key. One sample
	// each: a tile states what the exporter says now, so there is
	// nothing to retain and nothing to roll up.
	instant map[string]Instant
	lastAt  int64
}

// Store is the bounded in-memory metrics window. It is safe for one
// writer (the scraper) and many readers.
type Store struct {
	mu sync.Mutex
	// catalog is the surface this store holds. It is fixed at
	// construction: a store built for the instance exporter can neither
	// record nor answer for a pooler key, which is what keeps two
	// windows of two different things from blurring into one.
	catalog Catalog
	limits  Limits
	// instances maps instance name to its instruments.
	instances map[string]*instanceSeries
}

// NewStore builds a store over one catalog with the given bounds.
func NewStore(catalog Catalog, limits Limits) *Store {
	return &Store{catalog: catalog, limits: limits.withDefaults(), instances: map[string]*instanceSeries{}}
}

// Catalog is the surface this store holds, for the read side to render.
func (s *Store) Catalog() Catalog { return s.catalog }

// Interval is the configured scrape cadence, for the read side to state.
func (s *Store) Interval() time.Duration { return s.limits.Interval }

// Retention is the configured rollup window, for the read side to state.
func (s *Store) Retention() time.Duration { return s.limits.Retention }

// Observe records one instance's sweep: the values the exporter
// reported for the catalog keys it served. Absent keys record nothing,
// which reads back as a gap. Counters are converted to per-second rates
// here; a reset (value went backwards) records nothing for that sweep.
//
// instants carries the point-in-time readings of the same sweep. They
// overwrite rather than accumulate, so an instance that stops reporting
// one keeps its last claim with the timestamp that claim was made — the
// read side shows that age rather than pretending the value is current.
func (s *Store) Observe(instance string, at time.Time, values map[string]float64, instants map[string]float64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	inst := s.instances[instance]
	if inst == nil {
		s.evictForLocked()
		inst = &instanceSeries{times: s.newTimes(), byKey: map[string]*instrument{}, instant: map[string]Instant{}}
		s.instances[instance] = inst
	}
	ts := at.Unix()
	inst.lastAt = ts
	if inst.instant == nil {
		// A store primed from a snapshot rebuilds instruments but not
		// instants, so the first sweep after a restart lands here.
		inst.instant = map[string]Instant{}
	}

	for _, def := range s.catalog.Instants {
		value, ok := instants[def.Key]
		if !ok {
			continue
		}
		inst.instant[def.Key] = Instant{At: ts, Value: value}
	}

	// Every instrument gets exactly one raw slot per sweep, a gap when
	// this sweep claims nothing for it, so all stay aligned with times.
	claims := make(map[*instrument]float64, len(inst.byKey))
	for _, def := range s.catalog.Series {
		value, ok := values[def.Key]
		if !ok {
			continue
		}
		ins := inst.byKey[def.Key]
		if ins == nil {
			ins = &instrument{raw: newGapRing(inst.times), roll: s.newRollups()}
			inst.byKey[def.Key] = ins
		}
		if def.Kind == Counter {
			prev, prevAt := ins.prev, ins.prevAt
			ins.prev, ins.prevAt = value, ts
			if prevAt == 0 || ts <= prevAt || value < prev {
				continue // first sight or reset: no rate to claim
			}
			value = (value - prev) / float64(ts-prevAt)
		}
		if math.IsNaN(value) || math.IsInf(value, 0) {
			continue // not a number a chart can draw: a gap
		}
		claims[ins] = value
	}
	inst.times.push(ts)
	for _, ins := range inst.byKey {
		value, ok := claims[ins]
		if !ok {
			ins.raw.push(math.NaN())
			continue
		}
		ins.append(ts, value, s.limits.RollupEvery)
	}
}

func (s *Store) newTimes() *ring[int64] {
	return newRing[int64](int(s.limits.RawWindow / s.limits.Interval))
}

func (s *Store) newRollups() *ring[bucket] {
	return newRing[bucket](int(s.limits.Retention / s.limits.RollupEvery))
}

// evictForLocked makes room for one more instance by dropping the one
// least recently observed.
func (s *Store) evictForLocked() {
	if len(s.instances) < s.limits.MaxInstances {
		return
	}
	oldest, oldestAt := "", int64(0)
	for name, inst := range s.instances {
		if oldest == "" || inst.lastAt < oldestAt {
			oldest, oldestAt = name, inst.lastAt
		}
	}
	delete(s.instances, oldest)
}

// append pushes a converted sample into both tiers.
func (i *instrument) append(ts int64, value float64, rollupEvery time.Duration) {
	i.raw.push(value)
	start := ts - ts%int64(rollupEvery/time.Second)
	if i.open.count > 0 && i.open.start != start {
		i.roll.push(i.open)
		i.open = bucket{}
	}
	if i.open.count == 0 {
		i.open = bucket{start: start, min: value, max: value}
	} else {
		if value < i.open.min {
			i.open.min = value
		}
		if value > i.open.max {
			i.open.max = value
		}
	}
	i.open.sum += value
	i.open.count++
}

// Instances lists the tracked instance names, sorted.
func (s *Store) Instances() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.instances))
	for name := range s.instances {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// InstantReadings returns every instance's latest point-in-time claims,
// keyed by instance then by Instants key. A key an instance has never
// reported is absent rather than zero: a tile must be able to say "not
// reported" instead of showing a fabricated 0.
func (s *Store) InstantReadings() map[string]map[string]Instant {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]map[string]Instant, len(s.instances))
	for name, inst := range s.instances {
		if len(inst.instant) == 0 {
			continue
		}
		copied := make(map[string]Instant, len(inst.instant))
		for key, value := range inst.instant {
			copied[key] = value
		}
		out[name] = copied
	}
	return out
}

// Range reads one series across all instances at the given tier, as
// aligned columns: one shared ascending time axis, and per instance a
// value column with nil where that instance reported nothing. A span
// with no samples at all becomes an explicit nil row, so a renderer
// draws a gap instead of a line across the outage.
func (s *Store) Range(key string, tier Tier) (times []int64, byInstance map[string][]*float64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	perInstance := map[string]map[int64]float64{}
	timeSet := map[int64]bool{}
	for name, inst := range s.instances {
		ins := inst.byKey[key]
		if ins == nil {
			continue
		}
		points := map[int64]float64{}
		switch tier {
		case TierRaw:
			inst.times.indices(func(i int) {
				if v := ins.raw.buf[i]; !math.IsNaN(v) {
					at := inst.times.buf[i]
					points[at] = v
					timeSet[at] = true
				}
			})
		case TierRollup:
			fold := func(b bucket) {
				if b.count == 0 {
					return
				}
				points[b.start] = b.sum / float64(b.count)
				timeSet[b.start] = true
			}
			ins.roll.each(fold)
			fold(ins.open)
		}
		if len(points) > 0 {
			perInstance[name] = points
		}
	}
	if len(timeSet) == 0 {
		return nil, nil
	}

	times = make([]int64, 0, len(timeSet))
	for at := range timeSet {
		times = append(times, at)
	}
	sort.Slice(times, func(a, b int) bool { return times[a] < times[b] })
	times = s.withGapBreaksLocked(times, tier)

	byInstance = make(map[string][]*float64, len(perInstance))
	for name, points := range perInstance {
		column := make([]*float64, len(times))
		for i, at := range times {
			if v, ok := points[at]; ok {
				value := v
				column[i] = &value
			}
		}
		byInstance[name] = column
	}
	return times, byInstance
}

// withGapBreaksLocked inserts a synthetic timestamp inside every span
// where no sweep happened, so every instance reads nil there and the
// outage renders as a hole.
func (s *Store) withGapBreaksLocked(times []int64, tier Tier) []int64 {
	step := int64(s.limits.Interval / time.Second)
	if tier == TierRollup {
		step = int64(s.limits.RollupEvery / time.Second)
	}
	out := make([]int64, 0, len(times))
	for i, at := range times {
		if i > 0 && at-times[i-1] > step*3/2 {
			out = append(out, times[i-1]+step)
		}
		out = append(out, at)
	}
	return out
}

// SeriesStats summarises one series per instance: the latest raw claim
// and the min/max/avg over the retained rollup window.
func (s *Store) SeriesStats(key string) map[string]Stats {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := map[string]Stats{}
	for name, inst := range s.instances {
		ins := inst.byKey[key]
		if ins == nil {
			continue
		}
		var st Stats
		inst.times.indices(func(i int) {
			if v := ins.raw.buf[i]; !math.IsNaN(v) {
				st.Latest = &v
			}
		})
		var minV, maxV, sum float64
		count := 0
		fold := func(b bucket) {
			if b.count == 0 {
				return
			}
			if count == 0 || b.min < minV {
				minV = b.min
			}
			if count == 0 || b.max > maxV {
				maxV = b.max
			}
			sum += b.sum
			count += b.count
		}
		ins.roll.each(fold)
		fold(ins.open)
		if count > 0 {
			mn, mx, avg := minV, maxV, sum/float64(count)
			st.Min, st.Max, st.Avg = &mn, &mx, &avg
		}
		out[name] = st
	}
	return out
}
