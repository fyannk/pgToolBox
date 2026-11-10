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
	"crypto/sha256"
	"sync"
	"time"
)

// maxSpentFlows bounds the sign-in flows a replica remembers as used.
// A flow lives loginFlowMaxAge (ten minutes); 10,000 is far more than a
// team signs in in that time, and is reached only by callbacks that
// have each passed a sign-in budget.
const maxSpentFlows = 10000

// spentFlows remembers the sign-in flows whose callback this replica
// has accepted, by a digest of provider and state, until the flow's own
// expiry. The flow cookie is cleared by the callback, but only a
// browser honours that: without this a client could replay one valid
// cookie and state with junk codes, each costing a token exchange and a
// sign-in attempt. A replay is refused before any budget is charged.
//
// The memory is this replica's: behind several replicas a flow can be
// replayed once on each of the others, each such replay still charged
// to its sender's budget. Nothing about the flow is kept but its digest.
type spentFlows struct {
	max int

	mu    sync.Mutex
	until map[[sha256.Size]byte]time.Time
}

func newSpentFlows(maxFlows int) *spentFlows {
	return &spentFlows{max: maxFlows, until: map[[sha256.Size]byte]time.Time{}}
}

func flowDigest(provider, state string) [sha256.Size]byte {
	return sha256.Sum256([]byte(provider + "\x00" + state))
}

// spent reports whether the flow was already used.
func (s *spentFlows) spent(provider, state string, now time.Time) bool {
	key := flowDigest(provider, state)
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.until[key]
	return ok && now.Before(until)
}

// spend marks the flow used until its expiry; false when it already was
// (two callbacks of one flow racing).
func (s *spentFlows) spend(provider, state string, until, now time.Time) bool {
	key := flowDigest(provider, state)
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.until[key]; ok && now.Before(u) {
		return false
	}
	if len(s.until) >= s.max {
		for k, u := range s.until {
			if !now.Before(u) {
				delete(s.until, k)
			}
		}
	}
	if len(s.until) >= s.max {
		// Full of live flows: forget the one that expires first. Its
		// replay is then charged like any callback, as before this set.
		var oldest [sha256.Size]byte
		first := true
		var at time.Time
		for k, u := range s.until {
			if first || u.Before(at) {
				oldest, at, first = k, u, false
			}
		}
		delete(s.until, oldest)
	}
	s.until[key] = until
	return true
}
