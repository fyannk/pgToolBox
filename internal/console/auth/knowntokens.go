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
// Only a token the API server accepted is ever added, so junk cannot grow
// it. It holds at most max entries: when full, the expired ones go, and
// if none has expired the set starts over (like RateLimiter's table). A
// forgotten token re-reviews from the shared bucket once, as before it
// was known, and is remembered again. Nothing about a token is kept but
// the digest and the name the API server gave its person, to count by.
type knownTokens struct {
	max int

	mu  sync.Mutex
	all map[[sha256.Size]byte]knownToken
}

type knownToken struct {
	subject string
	until   time.Time
}

func newKnownTokens(maxTokens int) *knownTokens {
	return &knownTokens{max: maxTokens, all: map[[sha256.Size]byte]knownToken{}}
}

func (k *knownTokens) known(tok string, now time.Time) bool {
	_, ok := k.subjectOf(tok, now)
	return ok
}

// subjectOf reports whether tok is known and unexpired, and then the name
// the API server gave for it when it was accepted.
func (k *knownTokens) subjectOf(tok string, now time.Time) (string, bool) {
	key := sha256.Sum256([]byte(tok))
	k.mu.Lock()
	defer k.mu.Unlock()
	t, ok := k.all[key]
	if !ok || !now.Before(t.until) {
		return "", false
	}
	return t.subject, true
}

func (k *knownTokens) remember(tok, subject string, until, now time.Time) {
	if !now.Before(until) {
		return
	}
	key := sha256.Sum256([]byte(tok))
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.all[key]; !ok && len(k.all) >= k.max {
		for key, t := range k.all {
			if !now.Before(t.until) {
				delete(k.all, key)
			}
		}
		if len(k.all) >= k.max {
			clear(k.all)
		}
	}
	k.all[key] = knownToken{subject: subject, until: until}
}

func (k *knownTokens) forget(tok string) {
	key := sha256.Sum256([]byte(tok))
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.all, key)
}

func (k *knownTokens) len() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.all)
}
