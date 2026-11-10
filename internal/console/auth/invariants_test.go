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
	"math/rand/v2"
	"strconv"
	"testing"
	"time"
)

// Property: under random spends, checks, remembers, lookups and forgets,
// the spent-flow table and the known-bearer set keep their invariants
// after every operation: the flow map and its heap hold the same entries,
// every heap index points back to its own entry, and neither grows past
// its bound; the known set's index, its LRU and the per-person lists
// hold the same entries, no person holds more than their share, and the
// set stays within its bound.
func TestSpentFlowsAndKnownTokensKeepTheirInvariants(t *testing.T) {
	t.Parallel()
	for seed := range uint64(4) {
		r := rand.New(rand.NewPCG(seed, 2))
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		s := newSpentFlows(50)
		k := newKnownTokens(40, 5)
		for i := range 50_000 {
			now = now.Add(time.Duration(r.IntN(3000)) * time.Millisecond)
			st := strconv.Itoa(r.IntN(300))
			if r.IntN(3) < 2 {
				s.spend("p", st, now.Add(time.Duration(r.IntN(120))*time.Second), now)
			} else {
				s.spent("p", st, now)
			}
			tok, sub := strconv.Itoa(r.IntN(400)), strconv.Itoa(r.IntN(15))
			switch r.IntN(5) {
			case 0, 1:
				k.remember(tok, sub, now.Add(time.Duration(r.IntN(120)-5)*time.Second), now)
			case 2:
				k.known(tok, now)
			case 3:
				if got, ok := k.subjectOf(tok, now); ok && got == "" {
					t.Fatalf("seed %d op %d: a known token without its person", seed, i)
				}
			default:
				k.forget(tok)
			}
			if len(s.until) != len(s.order) || len(s.until) > s.max {
				t.Fatalf("seed %d op %d: flows map %d, heap %d, max %d", seed, i, len(s.until), len(s.order), s.max)
			}
			for j, f := range s.order {
				if f.index != j || s.until[f.key] != f {
					t.Fatalf("seed %d op %d: heap entry %d is not indexed as itself", seed, i, j)
				}
			}
			n := 0
			for sub, l := range k.bySubject {
				if l.Len() == 0 || l.Len() > k.perSubject {
					t.Fatalf("seed %d op %d: person %s holds %d", seed, i, sub, l.Len())
				}
				n += l.Len()
			}
			if n != len(k.all) || k.lru.Len() != len(k.all) || len(k.all) > k.max {
				t.Fatalf("seed %d op %d: per person %d, index %d, lru %d, max %d", seed, i, n, len(k.all), k.lru.Len(), k.max)
			}
		}
	}
}
