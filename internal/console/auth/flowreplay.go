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

package auth

import (
	"container/heap"
	"crypto/sha256"
	"sync"
	"time"
)

// maxSpentFlows bounds the sign-in flows a replica remembers as used.
// A flow lives loginFlowMaxAge (ten minutes), and only callbacks that
// passed the per-replica exchange bucket are remembered, so the set
// holds at most what that bucket admits in a flow's lifetime (60,200
// digests and expiries, a few MB): it is never full of live flows, and
// a full set first forgets the expired ones.
// Starting a flow adds nothing here: the flow lives in its sealed cookie.
const maxSpentFlows = codeExchangesPerSecond*int(loginFlowMaxAge/time.Second) + codeExchangesBurst

// spentFlows remembers the sign-in flows whose callback this replica
// has accepted, by a digest of provider and state, until the flow's own
// expiry. The flow cookie is cleared by the callback, but only a
// browser honours that: without this a client could replay one valid
// cookie and state with junk codes, each costing a token exchange and a
// sign-in attempt. A replay is refused before any budget is charged.
//
// The memory is this replica's: behind several replicas a flow can be
// replayed once on each of the others, each such replay still charged
// to that replica's budgets. Nothing about the flow is kept but its digest.
//
// The flows are also kept in a min-heap by expiry, so that spending one
// when the set is full forgets the expired and then the earliest flows
// in O(log n) each, never by scanning the whole set under the lock.
type spentFlows struct {
	max int

	mu    sync.Mutex
	until map[[sha256.Size]byte]*spentFlow
	order flowHeap
}

type spentFlow struct {
	key   [sha256.Size]byte
	until time.Time
	index int
}

// flowHeap is a container/heap of flows, the one that expires first on top.
type flowHeap []*spentFlow

func (h flowHeap) Len() int { return len(h) }
func (h flowHeap) Less(i, j int) bool {
	return h[i].until.Before(h[j].until)
}

func (h flowHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}
func (h *flowHeap) Push(x any) { f := x.(*spentFlow); f.index = len(*h); *h = append(*h, f) }
func (h *flowHeap) Pop() any {
	old := *h
	f := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	return f
}

func newSpentFlows(maxFlows int) *spentFlows {
	return &spentFlows{max: maxFlows, until: map[[sha256.Size]byte]*spentFlow{}}
}

func flowDigest(provider, state string) [sha256.Size]byte {
	return sha256.Sum256([]byte(provider + "\x00" + state))
}

// spent reports whether the flow was already used.
func (s *spentFlows) spent(provider, state string, now time.Time) bool {
	key := flowDigest(provider, state)
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.until[key]
	return ok && now.Before(f.until)
}

// spend marks the flow used until its expiry; false when it already was
// (two callbacks of one flow racing).
func (s *spentFlows) spend(provider, state string, until, now time.Time) bool {
	key := flowDigest(provider, state)
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.until[key]; ok {
		if now.Before(f.until) {
			return false
		}
		// Expired and spent again: it is a new flow of the same state.
		f.until = until
		heap.Fix(&s.order, f.index)
		return true
	}
	// Forget the expired flows on top, then, still full of live flows,
	// the one that expires first: its replay is then charged like any
	// callback, as before this set.
	for len(s.order) > 0 && (!now.Before(s.order[0].until) || len(s.order) >= s.max) {
		f := heap.Pop(&s.order).(*spentFlow)
		delete(s.until, f.key)
	}
	f := &spentFlow{key: key, until: until}
	heap.Push(&s.order, f)
	s.until[key] = f
	return true
}

// len is the number of flows remembered.
func (s *spentFlows) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.until)
}
