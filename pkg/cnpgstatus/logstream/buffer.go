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

package logstream

import (
	"sort"
	"sync"
	"time"
)

// Buffer retains recent log lines per container so a screen can show
// what happened before the reader arrived, and so a container that has
// since died still has an account of itself.
//
// This is the part that holds log text, so it is bounded in bytes rather
// than lines: a line's length is set by whatever wrote it, and an error
// loop emitting long lines would otherwise decide the console's memory
// footprint. Both a per-container and a total bound apply, and the
// oldest lines are dropped first.
//
// Retention is also bounded in time, because the value here decays
// quickly — a log line from yesterday is a question for a log system,
// not for this console.
type Buffer struct {
	perContainer int
	total        int
	maxAge       time.Duration

	mu      sync.RWMutex
	streams map[streamKey]*stream
	bytes   int
}

type streamKey struct{ pod, container string }

// entry is one retained line or gap marker.
type entry struct {
	at time.Time
	// text is the line, or the reason when gap is set.
	text string
	gap  bool
}

func (e entry) size() int { return len(e.text) + 48 }

type stream struct {
	entries []entry
	bytes   int
}

// NewBuffer builds a buffer. A perContainer or total of zero disables
// retention entirely: the buffer accepts lines and keeps none, which is
// the default posture and means the console holds no log text.
func NewBuffer(perContainer, total int, maxAge time.Duration) *Buffer {
	return &Buffer{
		perContainer: perContainer,
		total:        total,
		maxAge:       maxAge,
		streams:      map[streamKey]*stream{},
	}
}

// Enabled reports whether the buffer retains anything at all.
func (b *Buffer) Enabled() bool {
	return b != nil && b.perContainer > 0 && b.total > 0
}

// Observe retains one line, evicting oldest-first to stay inside both
// bounds.
func (b *Buffer) Observe(line Line) {
	if !b.Enabled() {
		return
	}
	b.append(streamKey{line.Pod, line.Container}, entry{at: line.At, text: line.Text})
}

// Gap retains an explicit marker. It is retained rather than dropped
// because a reader looking at two adjacent lines must be able to see
// that they were not adjacent in the container: joining across a gap
// silently would be the console inventing continuity.
func (b *Buffer) Gap(pod, container string, at time.Time, reason string) {
	if !b.Enabled() {
		return
	}
	b.append(streamKey{pod, container}, entry{at: at, text: reason, gap: true})
}

// Attached is a no-op for the buffer, as Detached and Dropped are. The
// buffer holds the text of what was said, and a window in which nothing
// was read leaves no text; the hole between two lines is already shown
// by the marker Gap retains. Coverage is a question about what a check
// may conclude from silence, which the matcher answers.
func (b *Buffer) Attached(string, string, time.Time) {}

// Detached is a no-op. See Attached.
func (b *Buffer) Detached(string, string, time.Time, string) {}

// Dropped is a no-op. See Attached; a container that goes away is
// forgotten through Forget, which drops its retained text as well.
func (b *Buffer) Dropped(string, string) {}

func (b *Buffer) append(key streamKey, e entry) {
	b.mu.Lock()
	defer b.mu.Unlock()

	current, ok := b.streams[key]
	if !ok {
		current = &stream{}
		b.streams[key] = current
	}
	current.entries = append(current.entries, e)
	current.bytes += e.size()
	b.bytes += e.size()

	// Per-container bound first, then the global one, oldest first in
	// both cases. A single noisy container therefore loses its own
	// history before it costs another container theirs.
	for current.bytes > b.perContainer && len(current.entries) > 0 {
		b.dropOldest(key, current)
	}
	for b.bytes > b.total {
		oldest, oldestStream := b.oldestStream()
		if oldestStream == nil {
			break
		}
		b.dropOldest(oldest, oldestStream)
	}
}

// dropOldest removes one entry from the front of a stream.
func (b *Buffer) dropOldest(key streamKey, s *stream) {
	size := s.entries[0].size()
	s.entries = s.entries[1:]
	s.bytes -= size
	b.bytes -= size
	if len(s.entries) == 0 {
		delete(b.streams, key)
	}
}

// oldestStream finds the stream holding the oldest entry.
func (b *Buffer) oldestStream() (streamKey, *stream) {
	var key streamKey
	var found *stream
	for candidate, s := range b.streams {
		if len(s.entries) == 0 {
			continue
		}
		if found == nil || s.entries[0].at.Before(found.entries[0].at) {
			key, found = candidate, s
		}
	}
	return key, found
}

// Retained is one line or gap marker read back out.
type Retained struct {
	At   time.Time
	Text string
	// Gap marks a break in the stream rather than a logged line.
	Gap bool
}

// Tail returns what is retained for one container, oldest first, dropping
// anything past the age bound.
//
// The second return reports whether this buffer holds anything for that
// container at all. It matters: nothing retained and retention disabled
// are different claims, and only the caller can phrase either honestly.
func (b *Buffer) Tail(pod, container string, now time.Time) ([]Retained, bool) {
	if !b.Enabled() {
		return nil, false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	s, ok := b.streams[streamKey{pod, container}]
	if !ok {
		return nil, false
	}
	cutoff := now.Add(-b.maxAge)
	out := make([]Retained, 0, len(s.entries))
	for _, e := range s.entries {
		if b.maxAge > 0 && e.at.Before(cutoff) {
			continue
		}
		out = append(out, Retained{At: e.at, Text: e.text, Gap: e.gap})
	}
	return out, true
}

// Containers lists what the buffer holds, so a screen can offer only the
// containers it can actually serve.
func (b *Buffer) Containers(pod string) []string {
	if !b.Enabled() {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	var names []string
	for key := range b.streams {
		if key.pod == pod {
			names = append(names, key.container)
		}
	}
	sort.Strings(names)
	return names
}

// Forget drops everything retained for a pod that no longer exists.
func (b *Buffer) Forget(pod string) {
	if !b.Enabled() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for key, s := range b.streams {
		if key.pod == pod {
			b.bytes -= s.bytes
			delete(b.streams, key)
		}
	}
}

// sortObservations orders by rule, then pod, then container.
func sortObservations(out []Observation) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].RuleID != out[j].RuleID {
			return out[i].RuleID < out[j].RuleID
		}
		if out[i].Pod != out[j].Pod {
			return out[i].Pod < out[j].Pod
		}
		return out[i].Container < out[j].Container
	})
}

// MaxAge reports the retention window, so a screen rendering retained
// lines can state how far back the record could possibly reach instead
// of leaving the reader to guess why older lines are absent.
func (b *Buffer) MaxAge() time.Duration {
	if b == nil {
		return 0
	}
	return b.maxAge
}
