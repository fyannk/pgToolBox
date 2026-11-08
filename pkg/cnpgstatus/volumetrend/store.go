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

// Package volumetrend keeps one coarse, long series per instance volume
// — bytes and inodes consumed, a sample every 15 minutes for 30 days —
// and extrapolates it into a time until full.
//
// The console's metrics window holds an hour: enough to see a spike, too
// short for a trend. A forecast needs weeks, and weeks of every metric
// would cost the console its memory budget, so this package keeps only
// what a forecast reads: per volume and instance, the bytes and the
// inodes the filesystem no longer gives PostgreSQL. A sample is two
// float32 (8 bytes) at a fixed cadence, so its time is its position and
// costs nothing: 2,880 samples, about 23 KB, per volume over 30 days.
// The filesystem's size and inode count move only on a resize, so they
// are kept as a short list of changes instead of per sample.
//
// Every value is the instance's claim about its own filesystem (the
// fork's /pg/status volumeUsages[] or its cnpg_disk_* series); nothing
// here measures anything. A sweep that read nothing leaves a gap, and
// the forecast says how many hours it actually fitted.
//
// Memory is bounded by construction: a series is a ring of fixed
// capacity (retention / cadence), the number of series is capped (a new
// series past the cap is refused, not traded for an old one), and a series
// not observed for a whole retention is forgotten.
package volumetrend

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Defaults of Limits.
const (
	DefaultEvery     = 15 * time.Minute
	DefaultRetention = 30 * 24 * time.Hour
	// DefaultMaxSeries bounds the series of one console. It covers the
	// console's fleet ceiling of 1,600 Clusters at three instances of
	// three volumes (data, WAL, one tablespace: 14,400 series). A full
	// series of 30 days at 15 minutes is 2,880 samples of 8 bytes, so
	// 16,384 full series are about 380 MB; rings grow as samples arrive,
	// so a younger or smaller fleet holds proportionally less.
	DefaultMaxSeries = 16384
	// maxSteps bounds the size changes kept per series.
	maxSteps = 64
)

// Sources of a reading, for provenance.
const (
	SourceStatus  = "/pg/status volumeUsages"
	SourceMetrics = "cnpg_disk_* metrics"
)

// Limits bound a Store.
type Limits struct {
	// Every is the sample cadence: the first reading of each Every-long
	// slot is kept, later readings of the slot only refresh Latest.
	Every time.Duration
	// Retention is how far back the series reach.
	Retention time.Duration
	// MaxSeries caps the number of (instance, volume) series.
	MaxSeries int
}

func (l Limits) withDefaults() Limits {
	if l.Every < time.Minute {
		l.Every = DefaultEvery
	}
	if l.Retention < l.Every {
		l.Retention = DefaultRetention
	}
	if l.MaxSeries < 1 {
		l.MaxSeries = DefaultMaxSeries
	}
	return l
}

// Capacity is the number of samples one series holds.
func (l Limits) Capacity() int {
	l = l.withDefaults()
	return int(l.Retention / l.Every)
}

// Key names one series: a volume of an instance of a Cluster. Volume is
// the fork's name: pgdata, wal or tbs-<tablespace>.
type Key struct {
	Namespace, Cluster, Instance, Volume string
}

// Reading is one statfs report of a volume.
type Reading struct {
	At             time.Time
	TotalBytes     uint64
	AvailableBytes uint64
	// UsedBytes is blocks - bfree: it counts the reserved blocks.
	UsedBytes  uint64
	Inodes     uint64
	InodesFree uint64
	HasInodes  bool
	// Source is SourceStatus or SourceMetrics.
	Source string
}

// consumed is what the filesystem no longer gives PostgreSQL: the
// volume is full for PostgreSQL when it reaches the total.
func (r Reading) consumed() float64 {
	if r.AvailableBytes > r.TotalBytes {
		return 0
	}
	return float64(r.TotalBytes - r.AvailableBytes)
}

// point is one sample; NaN is a slot no reading reached (or, for
// inodes, a filesystem that reports none).
type point struct {
	bytes, inodes float32
}

// step is the filesystem's size from a slot on.
type step struct {
	slot          int64
	total, inodes float64
}

// series is one volume's ring.
type series struct {
	buf []point
	// head is the index of the oldest sample once the ring is full.
	head int
	// newest is the slot of the newest sample.
	newest int64
	steps  []step
	latest Reading
	// version changes when a sample is added, so a cached fit knows it
	// is current.
	version uint64
}

// Store holds the series. It is safe for concurrent use: the sweep
// writes, the read model and diagnostics read.
type Store struct {
	mu        sync.Mutex
	limits    Limits
	series    map[Key]*series
	fits      map[Key]cachedFit
	lastPrune time.Time
	// byCluster indexes the series per Cluster, so the read side of one
	// Cluster never walks the whole store.
	byCluster map[clusterKey]map[Key]struct{}
	// refused counts the new series turned away at the cap; refusedAt is
	// when each Cluster last had one turned away, for its blind spot.
	refused   uint64
	refusedAt map[clusterKey]time.Time
}

type clusterKey struct{ namespace, cluster string }

func (s *Store) addLocked(key Key, ser *series) {
	s.series[key] = ser
	ck := clusterKey{key.Namespace, key.Cluster}
	if s.byCluster[ck] == nil {
		s.byCluster[ck] = map[Key]struct{}{}
	}
	s.byCluster[ck][key] = struct{}{}
}

func (s *Store) removeLocked(key Key) {
	delete(s.series, key)
	delete(s.fits, key)
	ck := clusterKey{key.Namespace, key.Cluster}
	delete(s.byCluster[ck], key)
	if len(s.byCluster[ck]) == 0 {
		delete(s.byCluster, ck)
	}
}

// NewStore returns an empty store.
func NewStore(limits Limits) *Store {
	return &Store{
		limits: limits.withDefaults(), series: map[Key]*series{}, fits: map[Key]cachedFit{},
		byCluster: map[clusterKey]map[Key]struct{}{}, refusedAt: map[clusterKey]time.Time{},
	}
}

// Len is the number of series held.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.series)
}

// Refused reports how many new series the cap has turned away since the
// store started.
func (s *Store) Refused() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused
}

// RefusedFor reports whether a volume of the Cluster was turned away at
// the cap since since: its trend is missing, not flat.
func (s *Store) RefusedFor(namespace, cluster string, since time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.refusedAt[clusterKey{namespace, cluster}]
	return ok && !at.Before(since)
}

// Limits are the store's bounds, for the read side to state.
func (s *Store) Limits() Limits { return s.limits }

func (s *Store) slot(at time.Time) int64 {
	return at.Unix() / int64(s.limits.Every/time.Second)
}

func (s *Store) slotTime(slot int64) time.Time {
	return time.Unix(slot*int64(s.limits.Every/time.Second), 0).UTC()
}

// Observe records one reading. The first reading of a slot becomes the
// sample; every reading refreshes Latest. A reading older than the
// newest sample (a clock step back) only refreshes nothing.
func (s *Store) Observe(key Key, r Reading) {
	if r.At.IsZero() || r.AvailableBytes > r.TotalBytes || r.InodesFree > r.Inodes {
		return
	}
	r.At = r.At.UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(r.At)
	slot := s.slot(r.At)
	ser := s.series[key]
	if ser == nil {
		if len(s.series) >= s.limits.MaxSeries {
			// Refuse rather than evict: evicting the least recently seen
			// series would, past the cap, drop a series every sweep and
			// keep none long enough to forecast.
			s.refused++
			ck := clusterKey{key.Namespace, key.Cluster}
			if len(s.refusedAt) < s.limits.MaxSeries || !s.refusedAt[ck].IsZero() {
				s.refusedAt[ck] = r.At
			}
			return
		}
		ser = &series{newest: slot - 1}
		s.addLocked(key, ser)
	}
	if r.At.Before(ser.latest.At) {
		return
	}
	ser.latest = r
	if slot <= ser.newest {
		return
	}
	capacity := s.limits.Capacity()
	gap := slot - ser.newest - 1
	if gap >= int64(capacity) || len(ser.buf) == 0 {
		ser.buf, ser.head = ser.buf[:0], 0
	} else {
		for range gap {
			ser.push(point{bytes: nan32, inodes: nan32}, capacity)
		}
	}
	p := point{bytes: float32(r.consumed()), inodes: nan32}
	if r.HasInodes {
		p.inodes = float32(r.Inodes - r.InodesFree)
	}
	ser.push(p, capacity)
	ser.newest = slot
	ser.version++
	total, inodes := float64(r.TotalBytes), math.NaN()
	if r.HasInodes {
		inodes = float64(r.Inodes)
	}
	if n := len(ser.steps); n == 0 || ser.steps[n-1].total != total || !sameFloat(ser.steps[n-1].inodes, inodes) {
		ser.steps = append(ser.steps, step{slot: slot, total: total, inodes: inodes})
	}
	ser.trimSteps(slot - int64(len(ser.buf)) + 1)
}

var nan32 = float32(math.NaN())

func sameFloat(a, b float64) bool { return a == b || math.IsNaN(a) && math.IsNaN(b) }

func (ser *series) push(p point, capacity int) {
	if len(ser.buf) < capacity {
		ser.buf = append(ser.buf, p)
		return
	}
	ser.buf[ser.head] = p
	ser.head = (ser.head + 1) % len(ser.buf)
}

// each visits the samples oldest first with their slot.
func (ser *series) each(fn func(slot int64, p point)) {
	n := len(ser.buf)
	for i := range n {
		fn(ser.newest-int64(n-1-i), ser.buf[(ser.head+i)%n])
	}
}

// trimSteps drops the size changes before oldest, keeping the one in
// force at oldest, and keeps at most maxSteps.
func (ser *series) trimSteps(oldest int64) {
	keepFrom := 0
	for i := 1; i < len(ser.steps); i++ {
		if ser.steps[i].slot <= oldest {
			keepFrom = i
		}
	}
	if len(ser.steps)-keepFrom > maxSteps {
		keepFrom = len(ser.steps) - maxSteps
	}
	if keepFrom > 0 {
		ser.steps = append(ser.steps[:0], ser.steps[keepFrom:]...)
	}
}

// pruneLocked forgets the series not observed for a whole retention, at
// most once per cadence.
func (s *Store) pruneLocked(now time.Time) {
	if now.Sub(s.lastPrune) < s.limits.Every {
		return
	}
	s.lastPrune = now
	for key, ser := range s.series {
		if now.Sub(ser.latest.At) > s.limits.Retention {
			s.removeLocked(key)
		}
	}
	for ck, at := range s.refusedAt {
		if now.Sub(at) > 4*s.limits.Every {
			delete(s.refusedAt, ck)
		}
	}
}

// Point is one sample as read back. Bytes and Inodes are consumed
// (bytes not available to PostgreSQL, inodes in use); nil is a gap.
type Point struct {
	At     time.Time `json:"at"`
	Bytes  *float64  `json:"bytes,omitempty"`
	Inodes *float64  `json:"inodes,omitempty"`
}

// Volume is one series as read back.
type Volume struct {
	Key    Key
	Latest Reading
	// Samples counts the samples that are not gaps.
	Samples int
	// Since is the time of the oldest retained slot.
	Since time.Time
}

// Volumes lists the series of one Cluster, sorted by instance then
// volume.
func (s *Store) Volumes(namespace, cluster string) []Volume {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := s.byCluster[clusterKey{namespace, cluster}]
	out := make([]Volume, 0, len(keys))
	for key := range keys {
		ser := s.series[key]
		v := Volume{Key: key, Latest: ser.latest}
		ser.each(func(slot int64, p point) {
			if !math.IsNaN(float64(p.bytes)) {
				v.Samples++
			}
		})
		if len(ser.buf) > 0 {
			v.Since = s.slotTime(ser.newest - int64(len(ser.buf)) + 1)
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key.Instance != out[j].Key.Instance {
			return out[i].Key.Instance < out[j].Key.Instance
		}
		return out[i].Key.Volume < out[j].Key.Volume
	})
	return out
}

// Downsample returns the series of one volume as at most buckets
// points, each the median of its bucket's samples (nil when the bucket
// holds none), oldest first.
func (s *Store) Downsample(key Key, buckets int) []Point {
	s.mu.Lock()
	defer s.mu.Unlock()
	ser := s.series[key]
	if ser == nil || len(ser.buf) == 0 || buckets < 1 {
		return nil
	}
	per := (len(ser.buf) + buckets - 1) / buckets
	var out []Point
	var bytesAcc, inodesAcc []float64
	var start int64
	flush := func() {
		p := Point{At: s.slotTime(start)}
		if len(bytesAcc) > 0 {
			v := median(bytesAcc)
			p.Bytes = &v
		}
		if len(inodesAcc) > 0 {
			v := median(inodesAcc)
			p.Inodes = &v
		}
		out = append(out, p)
		bytesAcc, inodesAcc = bytesAcc[:0], inodesAcc[:0]
	}
	i := 0
	ser.each(func(slot int64, p point) {
		if i%per == 0 {
			if i > 0 {
				flush()
			}
			start = slot
		}
		if !math.IsNaN(float64(p.bytes)) {
			bytesAcc = append(bytesAcc, float64(p.bytes))
		}
		if !math.IsNaN(float64(p.inodes)) {
			inodesAcc = append(inodesAcc, float64(p.inodes))
		}
		i++
	})
	flush()
	return out
}

// median of a non-empty slice; it reorders the slice.
func median(values []float64) float64 {
	sort.Float64s(values)
	n := len(values)
	if n%2 == 1 {
		return values[n/2]
	}
	return (values[n/2-1] + values[n/2]) / 2
}

// Reported says whether any instance of the Cluster reported a volume
// at or after since: the data, not the build, decides whether forecasts
// exist.
func (s *Store) Reported(namespace, cluster string, since time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.byCluster[clusterKey{namespace, cluster}] {
		if !s.series[key].latest.At.Before(since) {
			return true
		}
	}
	return false
}
