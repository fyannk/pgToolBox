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

// Property: under random remembers, lookups and forgets, the
// known-bearer set never grows past its bound and never answers a known
// token without the person it was accepted for.
func TestKnownTokensKeepTheirInvariants(t *testing.T) {
	t.Parallel()
	for seed := range uint64(4) {
		r := rand.New(rand.NewPCG(seed, 2))
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		k := newKnownTokens(40)
		for i := range 50_000 {
			now = now.Add(time.Duration(r.IntN(3000)) * time.Millisecond)
			tok, sub := strconv.Itoa(r.IntN(400)), "u"+strconv.Itoa(r.IntN(15))
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
			if k.len() > k.max {
				t.Fatalf("seed %d op %d: %d known, max %d", seed, i, k.len(), k.max)
			}
		}
	}
}
