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
	if len(s.until) != 3 || s.spent("oidc", "a", later) || !s.spent("oidc", "b", later) {
		t.Fatalf("after pruning the expired flow: %d remembered", len(s.until))
	}
	s.spend("oidc", "e", now.Add(5*time.Minute), later)
	if len(s.until) != 3 || s.spent("oidc", "b", later) || !s.spent("oidc", "e", later) {
		t.Fatalf("after evicting the earliest live flow: %d remembered", len(s.until))
	}
	for i := range 100 {
		s.spend("oidc", strconv.Itoa(i), now.Add(time.Hour), later)
	}
	if len(s.until) > 3 {
		t.Fatalf("%d flows remembered, bound is 3", len(s.until))
	}
}
