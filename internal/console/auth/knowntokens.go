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

// knownTokens remembers the raw bearers this replica has accepted, by a
// digest, until an expiry (the token's own, or knownBearerTTL), so that
// re-reviewing one spends a bucket no unauthenticated sender reaches.
// Only a token the API server accepted is ever added, so the set cannot
// be grown by junk; it is bounded all the same: when full, expired
// entries go first, and when none has expired a new token is not
// remembered (it then re-reviews from the shared bucket, as before it
// was known). Nothing about a token is kept but the digest.
type knownTokens struct {
	max int

	mu    sync.Mutex
	until map[[sha256.Size]byte]time.Time
}

func newKnownTokens(maxTokens int) *knownTokens {
	return &knownTokens{max: maxTokens, until: map[[sha256.Size]byte]time.Time{}}
}

func (k *knownTokens) known(tok string, now time.Time) bool {
	key := sha256.Sum256([]byte(tok))
	k.mu.Lock()
	defer k.mu.Unlock()
	u, ok := k.until[key]
	return ok && now.Before(u)
}

func (k *knownTokens) remember(tok string, until, now time.Time) {
	if !now.Before(until) {
		return
	}
	key := sha256.Sum256([]byte(tok))
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.until[key]; !ok && len(k.until) >= k.max {
		for d, u := range k.until {
			if !now.Before(u) {
				delete(k.until, d)
			}
		}
		if len(k.until) >= k.max {
			return
		}
	}
	k.until[key] = until
}

func (k *knownTokens) forget(tok string) {
	key := sha256.Sum256([]byte(tok))
	k.mu.Lock()
	delete(k.until, key)
	k.mu.Unlock()
}

func (k *knownTokens) len() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.until)
}
