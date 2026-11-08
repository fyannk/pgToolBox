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

// Package statesnap snapshots the console's in-memory series to the
// history volume: the sweep's rings, last good readings,
// metrics windows and insight histories, and the log matcher's
// observations. It runs only when the history is persistent
// (HISTORY_PATH): each part is written beside the history journal once a
// minute and at graceful shutdown, and read back once at start, before
// the stores run.
//
// The trade is the volume trend's (pkg/cnpgstatus/volumetrend): a
// snapshot, not a journal. A hard kill loses at most a minute, which then
// reads as a gap. Every file carries a header (magic, layout version,
// time saved); a file that is unreadable, of another version or larger
// than its part's bound is logged once and the part starts empty, the
// honest state of a first boot. Nothing here fails the start or crashes
// the console: the history journal beside it already proved the volume
// writable.
package statesnap

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

// Every is the snapshot cadence.
const Every = time.Minute

// magic opens every snapshot file; it tells a snapshot from any other
// file someone left under the same name.
var magic = [8]byte{'P', 'G', 'C', 'S', 'N', 'A', 'P', '\n'}

// headerSize is the magic, the layout version (uint32) and the time the
// snapshot was taken (Unix nanoseconds, int64), big-endian.
const headerSize = len(magic) + 4 + 8

// Suffix is the snapshot files' extension.
const Suffix = ".snapshot"

// Part is one in-memory store written to the volume.
type Part interface {
	// Name is the file's stem and the metrics label: a short constant.
	Name() string
	// Version gates the payload layout: a file of another version is
	// discarded at load.
	Version() uint32
	// MaxBytes bounds the payload, read and written.
	MaxBytes() int64
	// Save writes the payload as of now.
	Save(w io.Writer, now time.Time) error
	// Load reads a payload of this version saved at savedAt. It is
	// called once, before the store runs, and keeps what is still inside
	// the store's retention at now; it reports how many entries it kept
	// of how many it read.
	Load(r io.Reader, savedAt, now time.Time) (kept, read int, err error)
}

// Gob is a Part whose payload is one gob-encoded value: export copies the
// store out, load puts what is still valid back.
func Gob[T any](name string, version uint32, maxBytes int64, export func(now time.Time) T,
	load func(state T, savedAt, now time.Time) (kept, read int),
) Part {
	return gobPart[T]{name: name, version: version, maxBytes: maxBytes, export: export, load: load}
}

type gobPart[T any] struct {
	name     string
	version  uint32
	maxBytes int64
	export   func(time.Time) T
	load     func(T, time.Time, time.Time) (int, int)
}

func (p gobPart[T]) Name() string    { return p.name }
func (p gobPart[T]) Version() uint32 { return p.version }
func (p gobPart[T]) MaxBytes() int64 { return p.maxBytes }

func (p gobPart[T]) Save(w io.Writer, now time.Time) error {
	return gob.NewEncoder(w).Encode(p.export(now))
}

func (p gobPart[T]) Load(r io.Reader, savedAt, now time.Time) (int, int, error) {
	var state T
	if err := gob.NewDecoder(r).Decode(&state); err != nil {
		return 0, 0, err
	}
	kept, read := p.load(state, savedAt, now)
	return kept, read, nil
}

// Options configure a Snapshotter.
type Options struct {
	// Dir is the history volume's directory (that of HISTORY_PATH).
	Dir    string
	Clock  observe.Clock
	Logger *slog.Logger
	// Every overrides the cadence (tests).
	Every time.Duration
}

// partState is what the console says about one part.
type partState struct {
	savedAt   time.Time
	bytes     int64
	errors    int
	lastError string
	loaded    string
}

// Snapshotter owns the snapshot files.
type Snapshotter struct {
	opts  Options
	parts []Part

	mu     sync.Mutex
	states map[string]*partState
	since  time.Time
	failed bool
}

// New wires a snapshotter over parts; nothing is read until Load.
func New(opts Options, parts ...Part) *Snapshotter {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Clock == nil {
		opts.Clock = observe.RealClock{}
	}
	if opts.Every <= 0 {
		opts.Every = Every
	}
	s := &Snapshotter{opts: opts, parts: parts, states: map[string]*partState{}}
	for _, p := range parts {
		s.states[p.Name()] = &partState{}
	}
	return s
}

// Path is the file of one part.
func (s *Snapshotter) Path(p Part) string { return filepath.Join(s.opts.Dir, p.Name()+Suffix) }

// Load reads every part's file into its store. It never fails: a
// missing file is a first boot, anything unusable is logged once and
// the part starts empty.
func (s *Snapshotter) Load() {
	now := s.opts.Clock.Now()
	for _, p := range s.parts {
		outcome := s.loadPart(p, now)
		s.mu.Lock()
		s.states[p.Name()].loaded = outcome
		s.mu.Unlock()
	}
}

func (s *Snapshotter) loadPart(p Part, now time.Time) string {
	log := s.opts.Logger.With(slog.String("snapshot", p.Name()))
	f, err := os.Open(s.Path(p))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "none"
	case err != nil:
		log.Warn("state snapshot unreadable; this part starts empty", slog.String("reason", reasonOf(err)))
		return "unreadable"
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReader(f)
	version, savedAt, err := readHeader(r)
	if err != nil {
		log.Warn("state snapshot is not one this console wrote; this part starts empty")
		return "unreadable"
	}
	if version != p.Version() {
		log.Warn("state snapshot of another layout; this part starts empty",
			slog.Int("version", int(version)), slog.Int("want", int(p.Version())))
		return "other version"
	}
	limited := &io.LimitedReader{R: r, N: p.MaxBytes() + 1}
	kept, read, err := p.Load(limited, savedAt, now)
	switch {
	case limited.N <= 0:
		log.Warn("state snapshot exceeds its bound; this part starts empty", slog.Int64("maxBytes", p.MaxBytes()))
		return "too large"
	case err != nil:
		log.Warn("state snapshot does not decode; this part starts empty")
		return "unreadable"
	}
	log.Info("state snapshot loaded", slog.Time("savedAt", savedAt), slog.Duration("age", now.Sub(savedAt).Round(time.Second)),
		slog.Int("kept", kept), slog.Int("read", read))
	return "loaded"
}

func readHeader(r io.Reader) (uint32, time.Time, error) {
	var head [headerSize]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, time.Time{}, err
	}
	if [8]byte(head[:8]) != magic {
		return 0, time.Time{}, errors.New("not a state snapshot")
	}
	version := binary.BigEndian.Uint32(head[8:12])
	savedAt := time.Unix(0, int64(binary.BigEndian.Uint64(head[12:20]))).UTC() //nolint:gosec // a time read back, bounds irrelevant
	return version, savedAt, nil
}

// Run snapshots every part once per cadence until ctx ends, then once
// more: a graceful shutdown loses nothing.
func (s *Snapshotter) Run(ctx context.Context) error {
	for {
		if err := s.opts.Clock.Wait(ctx, s.opts.Every); err != nil {
			s.Flush()
			return nil //nolint:nilerr // cancellation is the clean end of Run
		}
		s.Flush()
	}
}

// Flush writes every part now.
func (s *Snapshotter) Flush() {
	now := s.opts.Clock.Now().UTC()
	for _, p := range s.parts {
		size, err := s.write(p, now)
		s.record(p, now, size, err)
	}
}

func (s *Snapshotter) record(p Part, now time.Time, size int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.states[p.Name()]
	if err != nil {
		st.errors++
		reason := reasonOf(err)
		if st.lastError != reason {
			s.opts.Logger.Warn("state snapshot failed; the previous one stays on the volume",
				slog.String("snapshot", p.Name()), slog.String("reason", reason))
		}
		st.lastError = reason
		snapshotErrors.WithLabelValues(p.Name()).Inc()
		if !s.failed {
			s.failed, s.since = true, now
		}
		return
	}
	if st.lastError != "" {
		s.opts.Logger.Info("state snapshot works again", slog.String("snapshot", p.Name()))
	}
	st.lastError, st.savedAt, st.bytes = "", now, size
	snapshotSaved.WithLabelValues(p.Name()).Set(float64(now.Unix()))
	snapshotBytes.WithLabelValues(p.Name()).Set(float64(size))
	failed := false
	for _, other := range s.states {
		failed = failed || other.lastError != ""
	}
	if failed != s.failed || s.since.IsZero() {
		s.failed, s.since = failed, now
	}
}

// errTooLarge is a snapshot larger than its part's bound.
var errTooLarge = errors.New("snapshot exceeds its bound")

// boundedWriter fails once more than n bytes went through.
type boundedWriter struct {
	w io.Writer
	n int64
}

func (b *boundedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > b.n {
		return 0, errTooLarge
	}
	b.n -= int64(len(p))
	return b.w.Write(p)
}

// write streams one part to a temporary file in the same directory,
// syncs it, renames it over the previous snapshot and syncs the
// directory, so the file on the volume is always one whole snapshot.
func (s *Snapshotter) write(p Part, now time.Time) (int64, error) {
	// Every file operation goes through a root on the snapshot directory:
	// the part names are fixed words, and the root keeps them there.
	root, err := os.OpenRoot(s.opts.Dir)
	if err != nil {
		return 0, fmt.Errorf("open directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	name := p.Name() + Suffix
	tmp := name + ".tmp"
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("create: %w", err)
	}
	fail := func(err error) (int64, error) {
		_ = f.Close()
		_ = root.Remove(tmp)
		return 0, err
	}
	buf := bufio.NewWriterSize(f, 256<<10)
	var head [headerSize]byte
	copy(head[:8], magic[:])
	binary.BigEndian.PutUint32(head[8:12], p.Version())
	binary.BigEndian.PutUint64(head[12:20], uint64(now.UnixNano())) //nolint:gosec // a wall-clock time, positive
	if _, err := buf.Write(head[:]); err != nil {
		return fail(fmt.Errorf("write: %w", err))
	}
	bounded := &boundedWriter{w: buf, n: p.MaxBytes()}
	if err := p.Save(bounded, now); err != nil {
		return fail(fmt.Errorf("encode: %w", err))
	}
	if err := buf.Flush(); err != nil {
		return fail(fmt.Errorf("write: %w", err))
	}
	if err := f.Sync(); err != nil {
		return fail(fmt.Errorf("sync: %w", err))
	}
	if err := f.Close(); err != nil {
		_ = root.Remove(tmp)
		return 0, fmt.Errorf("close: %w", err)
	}
	if err := root.Rename(tmp, name); err != nil {
		_ = root.Remove(tmp)
		return 0, fmt.Errorf("rename: %w", err)
	}
	// The rename is durable only once the directory entry is.
	dir, err := root.Open(".")
	if err != nil {
		return 0, fmt.Errorf("directory sync: %w", err)
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Sync(); err != nil {
		return 0, fmt.Errorf("directory sync: %w", err)
	}
	return int64(headerSize) + p.MaxBytes() - bounded.n, nil
}

// reasonOf names a failure without its text: the reason is shown on the
// admin page and in logs, and a fixed word is all either needs.
func reasonOf(err error) string {
	switch {
	case errors.Is(err, errTooLarge):
		return "larger than its bound"
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return "the history volume is full"
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EROFS):
		return "the history volume is not writable"
	default:
		return "input/output error"
	}
}

// State is the snapshots as a source of the admin page: live while every
// part's last write succeeded, unavailable with the first failing part's
// reason otherwise.
func (s *Snapshotter) State() viewmodel.SourceState {
	if s == nil {
		return viewmodel.SourceState{Source: SourceName, State: viewmodel.SourceDisabled, Reason: "the history is not persistent (HISTORY_PATH is not set)"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var since *time.Time
	if !s.since.IsZero() {
		at := s.since
		since = &at
	}
	if s.since.IsZero() {
		return viewmodel.SourceState{Source: SourceName, State: viewmodel.SourcePending}
	}
	for _, p := range s.parts {
		if st := s.states[p.Name()]; st.lastError != "" {
			return viewmodel.SourceState{
				Source: SourceName, State: viewmodel.SourceUnavailable, Since: since,
				Reason: p.Name() + " snapshot: " + st.lastError + "; the previous snapshot stays on the volume",
			}
		}
	}
	return viewmodel.SourceState{Source: SourceName, State: viewmodel.SourceLive, Since: since}
}

// SourceName is the admin page's key of the snapshots.
const SourceName = "stateSnapshots"

// Saved reports a part's last successful write and its size (tests and
// the admin page).
func (s *Snapshotter) Saved(name string) (time.Time, int64, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[name]
	if !ok {
		return time.Time{}, 0, 0
	}
	return st.savedAt, st.bytes, st.errors
}

// Loaded reports what Load did with a part's file: none, loaded,
// unreadable, other version or too large.
func (s *Snapshotter) Loaded(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.states[name]; ok {
		return st.loaded
	}
	return ""
}
