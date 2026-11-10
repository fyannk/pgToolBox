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
	"container/list"
	"crypto/sha256"
	"sync"
	"time"
)

// knownTokens remembers the raw bearers this replica has accepted, by a
// digest, until an expiry (the token's own, or knownBearerTTL), so that
// re-reviewing one spends a bucket no unauthenticated sender reaches.
// Only a token the API server accepted is ever added, so the set cannot
// be grown by junk; one person minting tokens could fill it all the same,
// so it is bounded twice: a person keeps their perSubject most recently
// used tokens (a new one evicts their own least recently used), and when
// the set is full expired entries go first, then the least recently used
// of anyone. An evicted token re-reviews from the shared bucket, as before
// it was known. Nothing about a token is kept but the digest, and of its
// person only the name the API server gave, to count by.
type knownTokens struct {
	max, perSubject int

	mu  sync.Mutex
	all map[[sha256.Size]byte]*list.Element
	// lru orders every entry, most recently used first; bySubject each
	// person's, the same way.
	lru       *list.List
	bySubject map[string]*list.List
}

type knownToken struct {
	key     [sha256.Size]byte
	subject string
	until   time.Time
	// mine is the entry's element in its person's list.
	mine *list.Element
}

func newKnownTokens(maxTokens, perSubject int) *knownTokens {
	return &knownTokens{
		max: maxTokens, perSubject: perSubject,
		all: map[[sha256.Size]byte]*list.Element{}, lru: list.New(), bySubject: map[string]*list.List{},
	}
}

func (k *knownTokens) known(tok string, now time.Time) bool {
	key := sha256.Sum256([]byte(tok))
	k.mu.Lock()
	defer k.mu.Unlock()
	e, ok := k.all[key]
	if !ok {
		return false
	}
	t := e.Value.(*knownToken)
	if !now.Before(t.until) {
		return false
	}
	k.touchLocked(e)
	return true
}

func (k *knownTokens) touchLocked(e *list.Element) {
	t := e.Value.(*knownToken)
	k.lru.MoveToFront(e)
	k.bySubject[t.subject].MoveToFront(t.mine)
}

func (k *knownTokens) remember(tok, subject string, until, now time.Time) {
	if !now.Before(until) {
		return
	}
	key := sha256.Sum256([]byte(tok))
	k.mu.Lock()
	defer k.mu.Unlock()
	if e, ok := k.all[key]; ok {
		t := e.Value.(*knownToken)
		if t.subject == subject {
			t.until = until
			k.touchLocked(e)
			return
		}
		k.removeLocked(e)
	}
	if mine := k.bySubject[subject]; mine != nil && mine.Len() >= k.perSubject {
		k.removeLocked(mine.Back().Value.(*list.Element))
	}
	if len(k.all) >= k.max {
		for e := k.lru.Back(); e != nil; {
			prev := e.Prev()
			if !now.Before(e.Value.(*knownToken).until) {
				k.removeLocked(e)
			}
			e = prev
		}
		for len(k.all) >= k.max {
			k.removeLocked(k.lru.Back())
		}
	}
	t := &knownToken{key: key, subject: subject, until: until}
	e := k.lru.PushFront(t)
	mine := k.bySubject[subject]
	if mine == nil {
		mine = list.New()
		k.bySubject[subject] = mine
	}
	t.mine = mine.PushFront(e)
	k.all[key] = e
}

// removeLocked drops e, an element of lru.
func (k *knownTokens) removeLocked(e *list.Element) {
	t := e.Value.(*knownToken)
	k.lru.Remove(e)
	mine := k.bySubject[t.subject]
	mine.Remove(t.mine)
	if mine.Len() == 0 {
		delete(k.bySubject, t.subject)
	}
	delete(k.all, t.key)
}

func (k *knownTokens) forget(tok string) {
	key := sha256.Sum256([]byte(tok))
	k.mu.Lock()
	defer k.mu.Unlock()
	if e, ok := k.all[key]; ok {
		k.removeLocked(e)
	}
}

func (k *knownTokens) len() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.all)
}
