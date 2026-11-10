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
	"strconv"
	"testing"
	"time"
)

func TestSpentFlows(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	s := newSpentFlows(3)
	if s.spent("oidc", "a", now) || !s.spend("oidc", "a", now.Add(time.Minute), now) {
		t.Fatal("a fresh flow is not spendable")
	}
	if !s.spent("oidc", "a", now) || s.spend("oidc", "a", now.Add(time.Minute), now) {
		t.Fatal("a spent flow is spendable again")
	}
	if s.spent("openshift", "a", now) {
		t.Fatal("the same state of another provider counts as spent")
	}
	if s.spent("oidc", "a", now.Add(time.Minute)) {
		t.Fatal("a flow is remembered past its expiry")
	}
	// Full: expired flows go first, then the one that expires first.
	s.spend("oidc", "b", now.Add(2*time.Minute), now)
	s.spend("oidc", "c", now.Add(3*time.Minute), now)
	later := now.Add(90 * time.Second)
	s.spend("oidc", "d", now.Add(4*time.Minute), later)
	if s.len() != 3 || s.spent("oidc", "a", later) || !s.spent("oidc", "b", later) {
		t.Fatalf("after pruning the expired flow: %d remembered", s.len())
	}
	s.spend("oidc", "e", now.Add(5*time.Minute), later)
	if s.len() != 3 || s.spent("oidc", "b", later) || !s.spent("oidc", "e", later) {
		t.Fatalf("after evicting the earliest live flow: %d remembered", s.len())
	}
	for i := range 100 {
		s.spend("oidc", strconv.Itoa(i), now.Add(time.Hour), later)
	}
	if s.len() > 3 {
		t.Fatalf("%d flows remembered, bound is 3", s.len())
	}
}

// TestSpentFlowsAtTheBound is round 6's P2: at maxSpentFlows every spend
// scanned the whole set under the lock. The set now evicts by expiry
// order: it stays at its bound, forgets exactly the earliest flows, and
// its heap matches its map.
func TestSpentFlowsAtTheBound(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	s := newSpentFlows(maxSpentFlows)
	for i := range maxSpentFlows + 1000 {
		if !s.spend("oidc", strconv.Itoa(i), now.Add(loginFlowMaxAge+time.Duration(i)*time.Millisecond), now) {
			t.Fatalf("flow %d refused", i)
		}
	}
	if s.len() != maxSpentFlows {
		t.Fatalf("%d flows remembered, bound %d", s.len(), maxSpentFlows)
	}
	if s.spent("oidc", "999", now) || !s.spent("oidc", "1000", now) || !s.spent("oidc", strconv.Itoa(maxSpentFlows+999), now) {
		t.Fatal("the flows forgotten at the bound must be the ones that expire first")
	}
	for i, f := range s.order {
		if f.index != i || s.until[f.key] != f {
			t.Fatalf("heap entry %d out of step with the map", i)
		}
	}
	// Once they expire, the next spend forgets them without a scan.
	later := now.Add(loginFlowMaxAge + time.Duration(maxSpentFlows)*time.Millisecond)
	s.spend("oidc", "late", later.Add(loginFlowMaxAge), later)
	if s.len() > 1001 {
		t.Fatalf("%d flows remembered after the others expired", s.len())
	}
}

func BenchmarkSpendAtTheBound(b *testing.B) {
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	s := newSpentFlows(maxSpentFlows)
	for i := range maxSpentFlows {
		s.spend("oidc", strconv.Itoa(i), now.Add(loginFlowMaxAge), now)
	}
	b.ResetTimer()
	for i := range b.N {
		s.spend("oidc", "b"+strconv.Itoa(i), now.Add(loginFlowMaxAge), now)
	}
}
