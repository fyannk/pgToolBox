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

package timeline

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/timeline/boltfile"
	"github.com/fyannk/pgToolBox/pkg/redact"
)

// The file is a write-behind mirror like the history journal's: the
// in-memory journal and digest store stay authoritative, mutations are
// enqueued under their locks and written by Run's flush loop, and a hard
// kill loses at most one interval, never the file's integrity.
const (
	flushInterval = 2 * time.Second
	// aliveInterval paces the heartbeat that dates the end of a process
	// life, so the next start can say how long the console was gone.
	aliveInterval = 30 * time.Second
)

// Waiter is the interruptible sleep of the flush loop
// (observe.RealClock satisfies it).
type Waiter interface {
	Wait(ctx context.Context, d time.Duration) error
}

// File persists the timeline journal and the digests in one bbolt file
// next to the history journal (the engine is boltfile).
type File struct {
	db     *boltfile.DB
	clock  Clock
	wait   Waiter
	logger *slog.Logger

	mu        sync.Mutex
	events    map[uint64]*Event // nil value: delete
	digests   map[string][]byte // nil value: delete
	seq       uint64
	evicted   bool
	lastAlive time.Time
	// previousAlive is the heartbeat the file held when opened: when the
	// previous process life was last known alive.
	previousAlive time.Time
	setAside      string
}

// Open opens or creates the file; failure is returned, never degraded
// around (the history's rule).
func Open(path string, clock Clock, wait Waiter, logger *slog.Logger) (*File, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	db, alive, aside, err := boltfile.OpenOrSetAside(path, clock.Now())
	if err != nil {
		return nil, err
	}
	f := &File{db: db, clock: clock, wait: wait, logger: logger, events: map[uint64]*Event{}, digests: map[string][]byte{}, previousAlive: alive}
	if aside != nil {
		// The previous life wrote the file last at its modification time:
		// the start says the console was gone since then at the latest.
		f.previousAlive, f.setAside = aside.LastWrite, filepath.Base(aside.Path)
		logger.Warn("timeline file unreadable: set aside, the timeline and digests start empty",
			slog.String("setAside", f.setAside))
	}
	return f, nil
}

// SetAside names the corrupt file Open moved aside, or "" when the file
// was readable. The console start records it.
func (f *File) SetAside() string { return f.setAside }

// PreviousAlive is when the previous console life was last known alive;
// zero for a new file.
func (f *File) PreviousAlive() time.Time { return f.previousAlive }

// LoadEvents implements Persister.
func (f *File) LoadEvents() (events []Event, seq uint64, evicted bool, err error) {
	raws, seq, evicted, err := f.db.LoadEvents()
	if err != nil {
		return nil, 0, false, err
	}
	skipped := 0
	for _, raw := range raws {
		var e Event
		if err := json.Unmarshal(raw, &e); err != nil || e.Seq == 0 {
			// One unreadable record costs that record, not the timeline.
			skipped++
			continue
		}
		events = append(events, e)
	}
	if skipped > 0 {
		f.logger.Warn("timeline file: unreadable events skipped", slog.Int("skipped", skipped))
		// The journal must treat what it lost like an eviction: no
		// "appeared" or "changed" against a state it no longer holds.
		evicted = true
	}
	return events, seq, evicted, nil
}

// LoadDigests returns every persisted digest, raw.
func (f *File) LoadDigests() ([][]byte, error) { return f.db.LoadDigests() }

// PutEvent implements Persister.
func (f *File) PutEvent(e Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events[e.Seq] = &e
	f.seq = max(f.seq, e.Seq)
}

// DeleteEvent implements Persister.
func (f *File) DeleteEvent(seq uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events[seq] = nil
}

// MarkEvicted implements Persister.
func (f *File) MarkEvicted() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evicted = true
}

// PutDigest enqueues one digest.
func (f *File) PutDigest(id string, raw []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.digests[id] = append([]byte(nil), raw...)
}

// DeleteDigest enqueues a digest's removal.
func (f *File) DeleteDigest(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.digests[id] = nil
}

// Run flushes until ctx ends, then flushes once more and closes the file.
func (f *File) Run(ctx context.Context) error {
	for {
		if err := f.wait.Wait(ctx, flushInterval); err != nil {
			f.log(f.flush())
			if err := f.db.Close(); err != nil {
				f.logger.Warn("timeline file close failed", slog.String("category", redact.Safe(redact.NewError("timeline file close", redact.CategoryInternal, err))))
			}
			return nil
		}
		f.log(f.flush())
	}
}

// Flush writes what is pending now (tests, and the final flush).
func (f *File) Flush() error { return f.flush() }

func (f *File) flush() error {
	now := f.clock.Now().UTC()
	f.mu.Lock()
	events, digests, seq, evicted := f.events, f.digests, f.seq, f.evicted
	alive := now.Sub(f.lastAlive) >= aliveInterval
	f.events, f.digests, f.seq, f.evicted = map[uint64]*Event{}, map[string][]byte{}, 0, false
	f.mu.Unlock()
	batch := boltfile.Batch{Events: make(map[uint64][]byte, len(events)), Digests: digests, Seq: seq, Evicted: evicted}
	if alive {
		batch.Alive = now
	}
	for s, e := range events {
		if e == nil {
			batch.Events[s] = nil
			continue
		}
		raw, err := json.Marshal(e)
		if err != nil {
			continue // an Event always marshals; skip rather than wedge the queue
		}
		batch.Events[s] = raw
	}
	if batch.Empty() {
		return nil
	}
	err := f.db.Write(batch)
	if err == nil {
		if alive {
			f.mu.Lock()
			f.lastAlive = now
			f.mu.Unlock()
		}
		return nil
	}
	// Merge back what failed; newer pending states win.
	f.mu.Lock()
	for s, e := range events {
		if _, pending := f.events[s]; !pending {
			f.events[s] = e
		}
	}
	for id, raw := range digests {
		if _, pending := f.digests[id]; !pending {
			f.digests[id] = raw
		}
	}
	f.seq = max(f.seq, seq)
	f.evicted = f.evicted || evicted
	f.mu.Unlock()
	return err
}

func (f *File) log(err error) {
	if err != nil {
		f.logger.Warn("timeline file flush failed", slog.String("category", redact.Safe(err)))
	}
}
