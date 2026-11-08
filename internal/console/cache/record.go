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

package cache

import (
	"sync"
	"sync/atomic"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// recorder routes one epoch's informer deliveries to the history hooks
// (Options.Tap and Options.Seed).
//
// A watch add and the add of an initial list are different facts: the
// first is a creation, the second only says the object exists. The
// history needs the initial list whole, as one seed, to classify it
// against what it knew before (an edit made while the console was down,
// an object deleted meanwhile). So each source's initial-list adds are
// collected and handed over as one Seed when the handler has received the
// whole list; deliveries that arrive before then are held and replayed
// after the seed, in order.
//
// A rebuilt cache lists everything again: its seed accounts for what
// changed between the two epochs. The previous epoch is muted when the
// next one starts, so the two never interleave — the next epoch's list
// and watch cover everything from its list onward.
type recorder struct {
	tap   func(kind Kind, obj client.Object, deleted bool)
	seed  func(kind Kind, namespaces []string, objs []client.Object)
	muted atomic.Bool
}

func newRecorder(tap func(Kind, client.Object, bool), seed func(Kind, []string, []client.Object)) *recorder {
	if tap == nil && seed == nil {
		return nil
	}
	return &recorder{tap: tap, seed: seed}
}

// mute stops every further delivery of the epoch.
func (r *recorder) mute() {
	if r != nil {
		r.muted.Store(true)
	}
}

// source returns the recorder of one watched source, listed over the
// namespaces.
func (r *recorder) source(kind Kind, namespaces []string) *sourceRecorder {
	if r == nil {
		return nil
	}
	return &sourceRecorder{r: r, kind: kind, namespaces: namespaces, seeded: r.seed == nil}
}

// heldDelivery is a delivery that arrived before the seed.
type heldDelivery struct {
	obj     client.Object
	deleted bool
}

// sourceRecorder is one source's share of a recorder.
type sourceRecorder struct {
	r          *recorder
	kind       Kind
	namespaces []string

	mu      sync.Mutex
	seeded  bool
	initial []client.Object
	held    []heldDelivery
}

// deliver records one informer delivery; initial marks an add of the
// initial list.
func (s *sourceRecorder) deliver(obj client.Object, deleted, initial bool) {
	if s == nil || s.r.muted.Load() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.seeded {
		if initial && !deleted {
			s.initial = append(s.initial, obj)
		} else {
			s.held = append(s.held, heldDelivery{obj: obj, deleted: deleted})
		}
		return
	}
	s.tap(obj, deleted)
}

// flush hands the initial list over as one seed, then replays what was
// held. Called once, when the handler has received the whole list.
func (s *sourceRecorder) flush() {
	if s == nil || s.r.muted.Load() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seeded {
		return
	}
	s.seeded = true
	s.r.seed(s.kind, s.namespaces, s.initial)
	for _, d := range s.held {
		s.tap(d.obj, d.deleted)
	}
	s.initial, s.held = nil, nil
}

func (s *sourceRecorder) tap(obj client.Object, deleted bool) {
	if s.r.tap != nil {
		s.r.tap(s.kind, obj, deleted)
	}
}
