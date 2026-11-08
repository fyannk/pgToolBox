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

// Package boltfile is the timeline file's storage engine: raw bytes in a
// bbolt file, nothing else. It is kept apart so the read-only scan
// (hack/check-readonly.sh) can exclude its local Update and Delete
// calls while proving it cannot reach a Kubernetes client.
package boltfile

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"time"

	bbolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"

	"github.com/fyannk/pgToolBox/pkg/redact"
)

const openTimeout = time.Second

var (
	eventsBucket  = []byte("events")
	digestsBucket = []byte("digests")
	metaBucket    = []byte("meta")
	metaSeq       = []byte("seq")
	metaEvicted   = []byte("evicted")
	metaAlive     = []byte("alive")
)

// DB is the open file.
type DB struct{ db *bbolt.DB }

// ErrCorrupt marks a file bbolt cannot read: not a bbolt file, a bad
// checksum or version, a bucket that is not one, or a panic while
// reading its pages.
var ErrCorrupt = errors.New("timeline file corrupt")

// SetAside describes a corrupt file moved out of the way by OpenOrSetAside.
type SetAside struct {
	// Path is where the corrupt file now is.
	Path string
	// LastWrite is its modification time: the latest the previous process
	// life can have written, so an upper bound of when it was last alive.
	LastWrite time.Time
}

// OpenOrSetAside opens the file and reads it all once. When the file is
// corrupt, it is renamed to <path>.corrupt-<UTC time> and a new file is
// created in its place; the returned SetAside says so. Anything else —
// a lock held by another process, a permission error — is returned as
// is, and the file is left where it is.
func OpenOrSetAside(path string, now time.Time) (*DB, time.Time, *SetAside, error) {
	db, alive, err := openProbed(path)
	if !errors.Is(err, ErrCorrupt) {
		return db, alive, nil, err
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		return nil, time.Time{}, nil, err
	}
	aside := &SetAside{Path: fmt.Sprintf("%s.corrupt-%s", path, now.UTC().Format("20060102T150405Z")), LastWrite: info.ModTime().UTC()}
	if renameErr := os.Rename(path, aside.Path); renameErr != nil {
		return nil, time.Time{}, nil, errors.Join(err, redact.NewError("timeline file set aside", redact.CategoryInternal, renameErr))
	}
	db, alive, err = openProbed(path)
	if err != nil {
		return nil, time.Time{}, nil, err
	}
	return db, alive, aside, nil
}

// openProbed opens the file and reads every page once, so a corrupt
// file is found at start rather than by a later panic.
func openProbed(path string) (db *DB, alive time.Time, err error) {
	defer func() {
		if r := recover(); r != nil {
			if db != nil {
				_ = db.db.Close()
			}
			db, alive, err = nil, time.Time{}, fmt.Errorf("%w: a page could not be read", ErrCorrupt)
		}
	}()
	db, alive, err = Open(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	probe := func(tx *bbolt.Tx) error {
		return tx.ForEach(func(_ []byte, b *bbolt.Bucket) error {
			return b.ForEach(func(_, _ []byte) error { return nil })
		})
	}
	if verr := db.db.View(probe); verr != nil {
		_ = db.db.Close()
		return nil, time.Time{}, fmt.Errorf("%w: %w", ErrCorrupt, verr)
	}
	return db, alive, nil
}

func corrupt(err error) bool {
	for _, c := range []error{bolterrors.ErrInvalid, bolterrors.ErrChecksum, bolterrors.ErrVersionMismatch, bolterrors.ErrIncompatibleValue, bolterrors.ErrInvalidMapping} {
		if errors.Is(err, c) {
			return true
		}
	}
	return false
}

// Open opens or creates the file and returns the heartbeat it held: when
// the previous process life was last known alive (zero for a new file).
func Open(path string) (*DB, time.Time, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: openTimeout})
	if err != nil {
		if corrupt(err) {
			return nil, time.Time{}, fmt.Errorf("%w: %w", ErrCorrupt, redact.NewError("timeline file open", redact.CategoryInternal, err))
		}
		return nil, time.Time{}, redact.NewError("timeline file open", redact.CategoryUnavailable, err)
	}
	var alive time.Time
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, b := range [][]byte{eventsBucket, digestsBucket, metaBucket} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		if raw := tx.Bucket(metaBucket).Get(metaAlive); raw != nil {
			_ = alive.UnmarshalBinary(raw)
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		if corrupt(err) {
			return nil, time.Time{}, fmt.Errorf("%w: %w", ErrCorrupt, redact.NewError("timeline file init", redact.CategoryInternal, err))
		}
		return nil, time.Time{}, redact.NewError("timeline file init", redact.CategoryInternal, err)
	}
	return &DB{db: db}, alive, nil
}

// LoadEvents returns every stored event, raw, by sequence, the highest
// sequence handed out and whether retention ever evicted.
func (d *DB) LoadEvents() (events map[uint64][]byte, seq uint64, evicted bool, err error) {
	events = map[uint64][]byte{}
	err = d.db.View(func(tx *bbolt.Tx) error {
		if err := tx.Bucket(eventsBucket).ForEach(func(k, v []byte) error {
			if len(k) != 8 {
				return nil
			}
			s := binary.BigEndian.Uint64(k)
			events[s] = append([]byte(nil), v...)
			seq = max(seq, s)
			return nil
		}); err != nil {
			return err
		}
		meta := tx.Bucket(metaBucket)
		if raw := meta.Get(metaSeq); len(raw) == 8 {
			seq = max(seq, binary.BigEndian.Uint64(raw))
		}
		evicted = meta.Get(metaEvicted) != nil
		return nil
	})
	if err != nil {
		return nil, 0, false, redact.NewError("timeline file load", redact.CategoryInternal, err)
	}
	return events, seq, evicted, nil
}

// LoadDigests returns every stored digest, raw.
func (d *DB) LoadDigests() ([][]byte, error) {
	var out [][]byte
	err := d.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(digestsBucket).ForEach(func(_, v []byte) error {
			out = append(out, append([]byte(nil), v...))
			return nil
		})
	})
	if err != nil {
		return nil, redact.NewError("digest load", redact.CategoryInternal, err)
	}
	return out, nil
}

// Batch is one flush. A nil value removes its key.
type Batch struct {
	Events  map[uint64][]byte
	Digests map[string][]byte
	// Seq, when above zero, is the highest sequence handed out.
	Seq     uint64
	Evicted bool
	// Alive, when set, is the new heartbeat.
	Alive time.Time
}

// Empty reports a batch with nothing to write.
func (b Batch) Empty() bool {
	return len(b.Events) == 0 && len(b.Digests) == 0 && b.Seq == 0 && !b.Evicted && b.Alive.IsZero()
}

// Write applies b in one transaction. A page bbolt panics on is
// returned as ErrCorrupt rather than taking the console down; the next
// start sets the file aside.
func (d *DB) Write(b Batch) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: a page could not be written", ErrCorrupt)
		}
	}()
	err = d.db.Update(func(tx *bbolt.Tx) error {
		eb := tx.Bucket(eventsBucket)
		for s, raw := range b.Events {
			if err := set(eb, seqKey(s), raw); err != nil {
				return err
			}
		}
		db := tx.Bucket(digestsBucket)
		for id, raw := range b.Digests {
			if err := set(db, []byte(id), raw); err != nil {
				return err
			}
		}
		meta := tx.Bucket(metaBucket)
		if b.Seq > 0 {
			if err := meta.Put(metaSeq, seqKey(b.Seq)); err != nil {
				return err
			}
		}
		if b.Evicted {
			if err := meta.Put(metaEvicted, []byte{1}); err != nil {
				return err
			}
		}
		if !b.Alive.IsZero() {
			raw, err := b.Alive.UTC().MarshalBinary()
			if err != nil {
				return err
			}
			if err := meta.Put(metaAlive, raw); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return redact.NewError("timeline file flush", redact.CategoryInternal, err)
	}
	return nil
}

func set(b *bbolt.Bucket, key, raw []byte) error {
	if raw == nil {
		return b.Delete(key)
	}
	return b.Put(key, raw)
}

// Close closes the file.
func (d *DB) Close() error { return d.db.Close() }

func seqKey(seq uint64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, seq)
	return key
}
