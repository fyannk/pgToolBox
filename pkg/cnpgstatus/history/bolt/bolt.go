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

// Package bolt persists the history store into a single bbolt file, so
// the revision timeline survives pod restarts and the first seeds of a
// new process life reconcile against the previous one.
//
// It is a write-behind mirror, not a second store: the in-memory store
// stays authoritative and bounded, mutations are enqueued under the
// store's lock and written by a background flush loop, and reads never
// touch the file. The trade is stated rather than hidden: a hard kill
// loses at most the last flush interval of history, never the file's
// integrity — bbolt's transactions guarantee the journal is either the
// old state or the new one.
package bolt

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	bbolt "go.etcd.io/bbolt"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/history"
	"github.com/fyannk/pgToolBox/pkg/redact"
)

var errDrillRecord = errors.New("empty key or oversized record")

// flushInterval paces the write-behind loop. A constant, not
// configuration: it trades a bounded loss window against write
// amplification from pod status churn, and that balance is this
// package's to own.
const flushInterval = 2 * time.Second

// openTimeout bounds the file-lock wait, so a second process pointed at
// the same journal fails fast instead of hanging the startup.
const openTimeout = time.Second

// The journal's buckets: revisions keyed by big-endian sequence,
// objects keyed by UID, and the meta counters.
var (
	revisionsBucket = []byte("revisions")
	objectsBucket   = []byte("objects")
	metaBucket      = []byte("meta")
	// drillsBucket holds the console's restore-drill records, keyed by
	// namespace/name: small, rare, written through (see PutDrill).
	drillsBucket   = []byte("drills")
	metaSeqKey     = []byte("seq")
	metaEvictedKey = []byte("evicted")
	// metaDrillsSinceKey is when the journal started keeping drill
	// records (RFC 3339, UTC).
	metaDrillsSinceKey = []byte("drillsSince")
)

// Clock supplies the interruptible waiting of the flush loop. It is the
// waiting half of observe.Clock, so observe.RealClock satisfies it.
type Clock interface {
	// Wait sleeps for d or until ctx is done, returning ctx.Err in that
	// case.
	Wait(ctx context.Context, d time.Duration) error
}

// Journal is the durable mirror of one history store. Open builds it,
// the store fills it through the history.Persister contract, and Run —
// registered as one of the application's background runners — owns the
// flush loop and the file's lifecycle.
type Journal struct {
	db     *bbolt.DB
	clock  Clock
	logger *slog.Logger

	mu sync.Mutex
	// revisions holds the pending revision writes; nil marks an
	// eviction. One key holds only its final state, so an append
	// overwritten by an eviction inside one interval writes nothing.
	revisions map[uint64]*history.Revision
	// objects holds the pending object-state writes; nil marks the end
	// of an incarnation.
	objects map[string]*history.ObjectRecord
	// seq is the highest sequence pending persistence; zero means none.
	// Persisted separately from the revision keys, so sequences stay
	// unique even after every revision carrying them was evicted.
	seq uint64
	// evicted marks that retention dropped history since the last flush.
	evicted bool
}

// The journal is the Persister the store mirrors into.
var _ history.Persister = (*Journal)(nil)

// Open opens or creates the journal file. Failure is returned rather
// than degraded around, per the store's contract: a mounted journal
// that cannot be used is a deployment fault to surface before listen.
func Open(path string, clock Clock, logger *slog.Logger) (*Journal, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: openTimeout})
	if err != nil {
		return nil, redact.NewError("history journal open", redact.CategoryUnavailable, err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, bucket := range [][]byte{revisionsBucket, objectsBucket, metaBucket, drillsBucket} {
			if _, err := tx.CreateBucketIfNotExists(bucket); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, redact.NewError("history journal init", redact.CategoryInternal, err)
	}
	return &Journal{
		db:        db,
		clock:     clock,
		logger:    logger,
		revisions: map[uint64]*history.Revision{},
		objects:   map[string]*history.ObjectRecord{},
	}, nil
}

// Load reads everything persisted so far. It is called once by the
// store, before any mutation.
func (j *Journal) Load() (history.Contents, error) {
	contents := history.Contents{Objects: map[string]history.ObjectRecord{}}
	err := j.db.View(func(tx *bbolt.Tx) error {
		cursor := tx.Bucket(revisionsBucket).Cursor()
		for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
			var rev history.Revision
			if err := json.Unmarshal(value, &rev); err != nil {
				return err
			}
			contents.Revisions = append(contents.Revisions, rev)
			if seq := binary.BigEndian.Uint64(key); seq > contents.Seq {
				contents.Seq = seq
			}
		}
		if err := tx.Bucket(objectsBucket).ForEach(func(key, value []byte) error {
			var rec history.ObjectRecord
			if err := json.Unmarshal(value, &rec); err != nil {
				return err
			}
			contents.Objects[string(key)] = rec
			return nil
		}); err != nil {
			return err
		}
		meta := tx.Bucket(metaBucket)
		if raw := meta.Get(metaSeqKey); len(raw) == 8 {
			if seq := binary.BigEndian.Uint64(raw); seq > contents.Seq {
				contents.Seq = seq
			}
		}
		contents.Evicted = meta.Get(metaEvictedKey) != nil
		return nil
	})
	if err != nil {
		return history.Contents{}, redact.NewError("history journal load", redact.CategoryInternal, err)
	}
	return contents, nil
}

// Append enqueues one new revision.
func (j *Journal) Append(rev history.Revision) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.revisions[rev.Seq] = &rev
	if rev.Seq > j.seq {
		j.seq = rev.Seq
	}
}

// Update enqueues the rewrite of an existing revision.
func (j *Journal) Update(rev history.Revision) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.revisions[rev.Seq] = &rev
}

// Evict enqueues the removal of one revision.
func (j *Journal) Evict(seq uint64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.revisions[seq] = nil
}

// PutObject enqueues one live object's classification state.
func (j *Journal) PutObject(uid string, rec history.ObjectRecord) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.objects[uid] = &rec
}

// DeleteObject enqueues the end of one live incarnation.
func (j *Journal) DeleteObject(uid string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.objects[uid] = nil
}

// MarkEvicted enqueues the permanent mark that retention has dropped
// history.
func (j *Journal) MarkEvicted() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.evicted = true
}

// Run owns the flush loop and the file: it flushes on the interval,
// flushes one final time on cancellation, and closes the journal. It
// always returns nil — cancellation is the one clean exit, matching the
// collectors it runs beside.
func (j *Journal) Run(ctx context.Context) error {
	for {
		if err := j.clock.Wait(ctx, flushInterval); err != nil {
			j.logFlush(j.flush())
			if err := j.db.Close(); err != nil {
				j.logger.Warn("history journal close failed",
					slog.String("category", redact.Safe(redact.NewError("history journal close", redact.CategoryInternal, err))))
			}
			return nil
		}
		j.logFlush(j.flush())
	}
}

// flush writes everything pending in one transaction. On failure the
// pending mutations are merged back — newer states enqueued meanwhile
// win — so a transient write error delays persistence instead of
// silently dropping it.
func (j *Journal) flush() error {
	j.mu.Lock()
	revisions := j.revisions
	objects := j.objects
	seq := j.seq
	evicted := j.evicted
	j.revisions = map[uint64]*history.Revision{}
	j.objects = map[string]*history.ObjectRecord{}
	j.seq = 0
	j.evicted = false
	j.mu.Unlock()

	if len(revisions) == 0 && len(objects) == 0 && seq == 0 && !evicted {
		return nil
	}

	err := j.db.Update(func(tx *bbolt.Tx) error {
		revBucket := tx.Bucket(revisionsBucket)
		for s, rev := range revisions {
			key := seqKey(s)
			if rev == nil {
				if err := revBucket.Delete(key); err != nil {
					return err
				}
				continue
			}
			encoded, err := json.Marshal(rev)
			if err != nil {
				return err
			}
			if err := revBucket.Put(key, encoded); err != nil {
				return err
			}
		}
		objBucket := tx.Bucket(objectsBucket)
		for uid, rec := range objects {
			if rec == nil {
				if err := objBucket.Delete([]byte(uid)); err != nil {
					return err
				}
				continue
			}
			encoded, err := json.Marshal(rec)
			if err != nil {
				return err
			}
			if err := objBucket.Put([]byte(uid), encoded); err != nil {
				return err
			}
		}
		meta := tx.Bucket(metaBucket)
		if seq > 0 {
			if err := meta.Put(metaSeqKey, seqKey(seq)); err != nil {
				return err
			}
		}
		if evicted {
			if err := meta.Put(metaEvictedKey, []byte{1}); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		return nil
	}

	j.mu.Lock()
	for s, rev := range revisions {
		if _, pending := j.revisions[s]; !pending {
			j.revisions[s] = rev
		}
	}
	for uid, rec := range objects {
		if _, pending := j.objects[uid]; !pending {
			j.objects[uid] = rec
		}
	}
	if seq > j.seq {
		j.seq = seq
	}
	j.evicted = j.evicted || evicted
	j.mu.Unlock()
	return redact.NewError("history journal flush", redact.CategoryInternal, err)
}

// logFlush records a flush failure's category, never its text. A
// failing journal degrades durability, not capture: the in-memory
// timeline keeps serving.
func (j *Journal) logFlush(err error) {
	if err == nil {
		return
	}
	j.logger.Warn("history journal flush failed",
		slog.String("category", redact.Safe(err)))
}

// seqKey encodes a sequence as its big-endian bucket key, so the
// cursor's order is the timeline's order.
func seqKey(seq uint64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, seq)
	return key
}

// maxDrillRecord bounds one drill record, on write and on load.
const maxDrillRecord = 64 << 10

// LoadDrills returns the persisted restore-drill records.
func (j *Journal) LoadDrills() ([][]byte, error) {
	var out [][]byte
	err := j.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(drillsBucket).ForEach(func(_, value []byte) error {
			if len(value) <= maxDrillRecord {
				out = append(out, append([]byte(nil), value...))
			}
			return nil
		})
	})
	if err != nil {
		return nil, redact.NewError("history journal drills load", redact.CategoryInternal, err)
	}
	return out, nil
}

// PutDrill writes one restore-drill record through, in its own
// transaction: a drill is started by a person and judged a handful of
// times, so the write-behind loop's batching buys nothing, and the
// result of a drill is exactly the kind of record a hard kill must not
// lose.
func (j *Journal) PutDrill(key string, value []byte) error {
	if key == "" || len(value) > maxDrillRecord {
		return redact.NewError("history journal drill put", redact.CategoryInternal, errDrillRecord)
	}
	if err := j.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(drillsBucket).Put([]byte(key), value) }); err != nil {
		return redact.NewError("history journal drill put", redact.CategoryUnavailable, err)
	}
	return nil
}

// DrillsSince is when this journal started keeping restore-drill
// records: recorded as now on the first call (a journal from before the
// key existed starts then too, which is the safe direction), read back on
// every later one.
func (j *Journal) DrillsSince(now time.Time) (time.Time, error) {
	var since time.Time
	err := j.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(metaBucket)
		if raw := meta.Get(metaDrillsSinceKey); raw != nil {
			t, err := time.Parse(time.RFC3339, string(raw))
			if err == nil {
				since = t
				return nil
			}
			// An unreadable start is replaced: nothing before now is
			// then claimed.
		}
		since = now.UTC().Truncate(time.Second)
		return meta.Put(metaDrillsSinceKey, []byte(since.Format(time.RFC3339)))
	})
	if err != nil {
		return time.Time{}, redact.NewError("history journal drills since", redact.CategoryUnavailable, err)
	}
	return since, nil
}

// DeleteDrill removes one restore-drill record.
func (j *Journal) DeleteDrill(key string) error {
	if err := j.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(drillsBucket).Delete([]byte(key)) }); err != nil {
		return redact.NewError("history journal drill delete", redact.CategoryUnavailable, err)
	}
	return nil
}
