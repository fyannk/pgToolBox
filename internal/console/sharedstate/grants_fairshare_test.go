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

package sharedstate

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestOverFairShare(t *testing.T) {
	b := tableBound{perUser: 16, total: 1600}
	limit, perUser := b.total, b.perUser
	for _, tc := range []struct {
		name                string
		mine, total, people int
		want                bool
	}{
		{"an empty table", 0, 0, 0, false},
		{"at the per-person bound", perUser, 10, 1, true},
		{"a full table, a newcomer", 0, limit, 100, true},
		{"below the threshold, holding the most", 15, 1279, 90, false},
		{"at the threshold, holding the mean", 13, 1300, 100, true},
		{"at the threshold, holding more than the mean", 14, 1300, 100, true},
		{"at the threshold, holding less than the mean", 12, 1300, 100, false},
		{"at the threshold, a newcomer", 0, 1300, 100, false},
		{"one entry each, a second", 1, 1300, 1300, true},
		{"one entry each, a newcomer", 0, 1599, 1599, false},
	} {
		if got := b.overFairShare(tc.mine, tc.total, tc.people); got != tc.want {
			t.Errorf("%s: overFairShare(%d, %d, %d) = %v, want %v", tc.name, tc.mine, tc.total, tc.people, got, tc.want)
		}
	}
}

// The review's repro (round 10): a hundred identities, each taking all
// the grants and codes it may, filled every place of the index and of the
// codes table for twelve hours, refusing everyone else's MCP connection.
// Past four fifths, a person holding the mean share is refused, so the
// last places go to newcomers.
func TestAHundredHoardersCannotFillTheIndex(t *testing.T) {
	clock := &testClock{now: t0}
	s := New(Options{Now: clock.Now})
	n, codes := 0, 0
	// Round robin, the hoarders' best order: each takes one more while it
	// may.
	for progress := true; progress; {
		progress = false
		for p := range 100 {
			user := fmt.Sprint("hoarder", p)
			if s.AddGrant(testGrant(n, user, clock.Now())) == nil {
				n++
				progress = true
			}
			if s.UseCode(fmt.Sprint(user, "/", codes), uh(user), t0.Add(time.Hour)) == nil {
				codes++
				progress = true
			}
		}
	}
	if n >= MaxGrants || codes >= maxCodes {
		t.Fatalf("a hundred hoarders hold %d grants and %d codes, every place of %d and %d", n, codes, MaxGrants, maxCodes)
	}
	room := 0
	for p := range MaxGrants {
		user := fmt.Sprint("newcomer", p)
		err := s.AddGrant(testGrant(1_000_000+p, user, clock.Now()))
		if errors.Is(err, ErrTooManyGrants) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := s.UseCode(fmt.Sprint(user, "/1"), uh(user), t0.Add(time.Hour)); err != nil {
			t.Fatalf("newcomer %d refused a sign-in with %d hoarded codes: %v", p, codes, err)
		}
		room++
	}
	if want := MaxGrants - n; room != want {
		t.Errorf("%d newcomers connected after the hoarders took %d grants, want the %d places left", room, n, want)
	}
	if room < MaxGrants/10 {
		t.Errorf("only %d places left for newcomers", room)
	}
	if err := s.AddGrant(testGrant(2_000_000, "hoarder0", clock.Now())); !errors.Is(err, ErrTooManyGrants) {
		t.Errorf("a hoarder added a grant to a full index: %v", err)
	}
	if s.RevokeGrant(grantID(2_000_001), uh("hoarder1"), clock.Now(), clock.Now().Add(time.Minute)) {
		t.Error("a hoarder's placeholder took a place")
	}
}

// Each replica refuses from what it holds and the merge stays a union:
// replicas admitting hoarders on both sides end identical, and each still
// admits a newcomer.
func TestFairShareOfTheIndexConverges(t *testing.T) {
	clock := &testClock{now: t0}
	a := synced(t, Options{Name: "console-state", Now: clock.Now})
	b := synced(t, Options{Name: "console-state", Now: clock.Now})
	n := 0
	for round := range 40 {
		for p := range 100 {
			s := a
			if (p+round)%2 == 1 {
				s = b
			}
			user := fmt.Sprint("hoarder", p)
			if s.AddGrant(testGrant(n, user, clock.Now())) == nil {
				n++
			}
			_ = s.UseCode(fmt.Sprint(user, "/", round), uh(user), t0.Add(time.Hour))
		}
		if round%3 == 0 {
			exchange(a, b)
			exchange(b, a)
		}
	}
	exchange(a, b)
	exchange(b, a)
	a.mu.Lock()
	b.mu.Lock()
	sameGrants, sameCodes, held := len(a.grants) == len(b.grants), len(a.codes) == len(b.codes), len(a.grants)
	for id := range a.grants {
		if _, ok := b.grants[id]; !ok {
			sameGrants = false
		}
	}
	b.mu.Unlock()
	a.mu.Unlock()
	if !sameGrants || !sameCodes {
		t.Fatalf("replicas differ after exchanging: grants %v, codes %v", sameGrants, sameCodes)
	}
	if held >= MaxGrants {
		t.Fatalf("hoarders on two replicas hold %d grants, every place", held)
	}
	for i, s := range []*Store{a, b} {
		user := fmt.Sprint("newcomer", i)
		if err := s.AddGrant(testGrant(1_000_000+i, user, clock.Now())); err != nil {
			t.Errorf("replica %d refused a newcomer: %v", i, err)
		}
		if err := s.UseCode(user+"/1", uh(user), t0.Add(time.Hour)); err != nil {
			t.Errorf("replica %d refused a newcomer's sign-in: %v", i, err)
		}
	}
}
