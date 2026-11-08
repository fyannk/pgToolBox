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

package volumetrend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
	"github.com/fyannk/pgToolBox/pkg/redact"
)

// Persistence is a periodic snapshot beside the history journal, written
// atomically (temp file, fsync, rename) once per sample cadence and at
// shutdown, and read back at startup. A hard kill loses at most one
// cadence of samples, which then reads as a gap. A snapshot that does
// not decode, or of another layout, is discarded with a warning: an
// empty trend is the honest state of a first boot.

// snapshotVersion gates the gob layout.
const snapshotVersion = 1

// PersistedState is the serialized store.
type PersistedState struct {
	Version int
	SavedAt int64
	// EverySeconds is the cadence the samples were taken at; a snapshot
	// of another cadence is discarded (its positions would lie).
	EverySeconds int64
	Series       []PersistedSeries
}

// PersistedSeries is one series, samples oldest first.
type PersistedSeries struct {
	Key    Key
	Newest int64
	Bytes  []float32
	Inodes []float32
	Steps  []PersistedStep
	Latest Reading
}

// PersistedStep is one size change.
type PersistedStep struct {
	Slot          int64
	Total, Inodes float64
}

// Export copies the store out. The lock is held to list the keys, then
// once per series while it is copied into preallocated slices: a large
// store never blocks the sweep for the whole copy.
func (s *Store) Export(at time.Time) PersistedState {
	s.mu.Lock()
	keys := make([]Key, 0, len(s.series))
	for key := range s.series {
		keys = append(keys, key)
	}
	every := s.limits.Every
	s.mu.Unlock()
	sort.Slice(keys, func(i, j int) bool { return keyLess(keys[i], keys[j]) })
	state := PersistedState{
		Version: snapshotVersion, SavedAt: at.Unix(), EverySeconds: int64(every / time.Second),
		Series: make([]PersistedSeries, 0, len(keys)),
	}
	for _, key := range keys {
		s.mu.Lock()
		ser := s.series[key]
		if ser == nil {
			s.mu.Unlock()
			continue
		}
		p := PersistedSeries{
			Key: key, Newest: ser.newest, Latest: ser.latest,
			Bytes: make([]float32, 0, len(ser.buf)), Inodes: make([]float32, 0, len(ser.buf)),
			Steps: make([]PersistedStep, 0, len(ser.steps)),
		}
		ser.each(func(_ int64, pt point) {
			p.Bytes = append(p.Bytes, pt.bytes)
			p.Inodes = append(p.Inodes, pt.inodes)
		})
		for _, st := range ser.steps {
			p.Steps = append(p.Steps, PersistedStep{Slot: st.slot, Total: st.total, Inodes: st.inodes})
		}
		s.mu.Unlock()
		state.Series = append(state.Series, p)
	}
	return state
}

func keyLess(a, b Key) bool {
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	if a.Cluster != b.Cluster {
		return a.Cluster < b.Cluster
	}
	if a.Instance != b.Instance {
		return a.Instance < b.Instance
	}
	return a.Volume < b.Volume
}

// maxKeyPart bounds each part of a key: a Kubernetes name.
const maxKeyPart = 253

// validSeries re-applies Observe's checks to a persisted series, plus
// the bounds a live store keeps by construction: a snapshot is a file
// on a volume, not a trusted source.
func (s *Store) validSeries(p PersistedSeries, now time.Time) bool {
	for _, part := range []string{p.Key.Namespace, p.Key.Cluster, p.Key.Instance, p.Key.Volume} {
		if part == "" || len(part) > maxKeyPart {
			return false
		}
	}
	r := p.Latest
	if r.At.IsZero() || r.AvailableBytes > r.TotalBytes || r.InodesFree > r.Inodes || r.At.After(now.Add(s.limits.Every)) {
		return false
	}
	if len(p.Bytes) != len(p.Inodes) || len(p.Bytes) == 0 || p.Newest > s.slot(now)+1 || p.Newest > s.slot(r.At) || p.Newest < s.slot(r.At)-1 {
		return false
	}
	for i := range p.Bytes {
		if !validValue(p.Bytes[i]) || !validValue(p.Inodes[i]) {
			return false
		}
	}
	if len(p.Steps) == 0 || len(p.Steps) > maxSteps {
		return false
	}
	for i, st := range p.Steps {
		if st.Slot > p.Newest || (i > 0 && st.Slot <= p.Steps[i-1].Slot) || math.IsInf(st.Total, 0) || math.IsNaN(st.Total) || st.Total < 0 {
			return false
		}
	}
	return true
}

// validValue: a sample is NaN (a gap) or a finite count, never negative.
func validValue(v float32) bool {
	f := float64(v)
	return math.IsNaN(f) || (!math.IsInf(f, 0) && f >= 0)
}

// Import rebuilds the store from a snapshot, clamped to its current
// bounds: each ring keeps its newest samples up to the capacity, and
// past the series cap the most recently observed win. A snapshot of
// another cadence or layout imports nothing; a series that fails the
// checks Observe applies (or the store's own bounds) is dropped, the
// others kept. It reports how many series it imported and whether the
// snapshot was of this layout and cadence.
func (s *Store) Import(state PersistedState, now time.Time) (int, bool) {
	if state.Version != snapshotVersion || state.EverySeconds != int64(s.limits.Every/time.Second) {
		return 0, false
	}
	all := make([]PersistedSeries, 0, min(len(state.Series), s.limits.MaxSeries))
	seen := make(map[Key]bool, len(state.Series))
	for _, p := range state.Series {
		if seen[p.Key] || !s.validSeries(p, now) {
			continue
		}
		seen[p.Key] = true
		all = append(all, p)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Latest.At.After(all[j].Latest.At) })
	if len(all) > s.limits.MaxSeries {
		all = all[:s.limits.MaxSeries]
	}
	capacity := s.limits.Capacity()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range all {
		ser := &series{newest: p.Newest, latest: p.Latest}
		samples := len(p.Bytes)
		from := max(samples-capacity, 0)
		ser.buf = make([]point, 0, min(samples, capacity))
		for i := from; i < samples; i++ {
			ser.push(point{bytes: p.Bytes[i], inodes: p.Inodes[i]}, capacity)
		}
		for _, st := range p.Steps {
			ser.steps = append(ser.steps, step{slot: st.Slot, total: st.Total, inodes: st.Inodes})
		}
		ser.trimSteps(ser.newest - int64(len(ser.buf)) + 1)
		ser.version = 1
		s.addLocked(p.Key, ser)
	}
	return len(all), true
}

// maxSnapshotBytes bounds what is read back: every series full, its
// steps, and a generous allowance for keys and gob framing.
func (s *Store) maxSnapshotBytes() int64 {
	perSeries := int64(s.limits.Capacity())*8 + maxSteps*24 + 4*maxKeyPart + 1024
	return int64(s.limits.MaxSeries)*perSeries + 1<<20
}

// decodeSnapshot reads one snapshot of at most limit bytes.
func decodeSnapshot(r io.Reader, limit int64) (PersistedState, error) {
	var state PersistedState
	lr := &io.LimitedReader{R: r, N: limit + 1}
	if err := gob.NewDecoder(lr).Decode(&state); err != nil {
		return PersistedState{}, err
	}
	if lr.N <= 0 {
		return PersistedState{}, errors.New("volume trend snapshot exceeds its bound")
	}
	return state, nil
}

// Persister owns the snapshot file.
type Persister struct {
	store  *Store
	path   string
	clock  observe.Clock
	logger *slog.Logger
}

// OpenPersister loads any existing snapshot into the store and proves
// the path writable by writing one now: a mounted volume that cannot be
// written fails the start, like the history journal beside it.
func OpenPersister(path string, store *Store, clock observe.Clock, logger *slog.Logger) (*Persister, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	p := &Persister{store: store, path: path, clock: clock, logger: logger}
	f, err := os.Open(path) //nolint:gosec // the path derives from the validated HISTORY_PATH configuration.
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, redact.NewError("volume trend snapshot read", redact.CategoryInternal, err)
	default:
		state, decodeErr := decodeSnapshot(bufio.NewReader(f), store.maxSnapshotBytes())
		_ = f.Close()
		imported, ok := 0, false
		if decodeErr == nil {
			imported, ok = store.Import(state, clock.Now())
		}
		switch {
		case !ok:
			logger.Warn("volume trend snapshot unreadable, too large or of another cadence; starting empty",
				slog.Int("version", state.Version), slog.Int64("everySeconds", state.EverySeconds))
		case imported < len(state.Series):
			logger.Warn("volume trend snapshot: series failing the checks or past the cap were dropped",
				slog.Int("imported", imported), slog.Int("inSnapshot", len(state.Series)))
		}
	}
	if err := p.flush(); err != nil {
		return nil, err
	}
	return p, nil
}

// Run snapshots once per cadence until ctx ends, then once more.
func (p *Persister) Run(ctx context.Context) error {
	for {
		if err := p.clock.Wait(ctx, p.store.limits.Every); err != nil {
			if flushErr := p.flush(); flushErr != nil {
				p.logger.Warn("volume trend final snapshot failed", slog.String("category", redact.Safe(flushErr)))
			}
			return err
		}
		if err := p.flush(); err != nil {
			p.logger.Warn("volume trend snapshot failed", slog.String("category", redact.Safe(err)))
		}
	}
}

func (p *Persister) flush() error {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(p.store.Export(p.clock.Now())); err != nil {
		return redact.NewError("volume trend snapshot encode", redact.CategoryInternal, err)
	}
	tmp := p.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // the path derives from the validated HISTORY_PATH configuration.
	if err != nil {
		return redact.NewError("volume trend snapshot write", redact.CategoryInternal, err)
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		return redact.NewError("volume trend snapshot write", redact.CategoryInternal, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return redact.NewError("volume trend snapshot sync", redact.CategoryInternal, err)
	}
	if err := f.Close(); err != nil {
		return redact.NewError("volume trend snapshot close", redact.CategoryInternal, err)
	}
	if err := os.Rename(tmp, p.path); err != nil {
		return redact.NewError("volume trend snapshot rename", redact.CategoryInternal, err)
	}
	// The rename is durable only once the directory entry is.
	dir, err := os.Open(filepath.Dir(p.path))
	if err != nil {
		return redact.NewError("volume trend snapshot directory sync", redact.CategoryInternal, err)
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Sync(); err != nil {
		return redact.NewError("volume trend snapshot directory sync", redact.CategoryInternal, err)
	}
	return nil
}
