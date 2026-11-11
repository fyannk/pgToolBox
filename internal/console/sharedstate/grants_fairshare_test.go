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
	"math/rand/v2"
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
		{"at the threshold, holding less than the mean but the late bound", 12, 1300, 100, true},
		{"at the threshold, holding less than the late bound", 3, 1300, 100, false},
		{"at the threshold, at the late bound", 4, 1300, 400, true},
		{"below the threshold, past the late bound", 15, 1279, 100, false},
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

// hoardOrder drives a hundred hoarders: step(s, p) has hoarder p take
// what it may for its turn and reports whether it took anything; turns
// are given in order until nobody takes anything.
type hoardOrder struct {
	name string
	// turns returns the hoarders' order for one round.
	turns func(round int) []int
	// burst is how many places a hoarder tries to take in one turn (0:
	// all it may).
	burst int
	// placeholders: a hoarder refused a grant also replays its own codes
	// for revocation placeholders.
	placeholders bool
}

func hostileOrders() []hoardOrder {
	seq := func(int) []int {
		out := make([]int, 100)
		for i := range out {
			out[i] = i
		}
		return out
	}
	rev := func(int) []int {
		out := make([]int, 100)
		for i := range out {
			out[i] = 99 - i
		}
		return out
	}
	r := rand.New(rand.NewPCG(11, 3))
	shuffled := func(int) []int { return r.Perm(100) }
	return []hoardOrder{
		{name: "sequential (the review's repro)", turns: seq},
		{name: "round robin", turns: seq, burst: 1},
		{name: "bursts of three", turns: seq, burst: 3},
		{name: "bursts of eight, reversed", turns: rev, burst: 8},
		{name: "bursts of five, shuffled every round", turns: shuffled, burst: 5},
		{name: "sequential with placeholders", turns: seq, placeholders: true},
		{name: "round robin with placeholders", turns: shuffled, burst: 1, placeholders: true},
	}
}

// hoard runs order on s and returns the grants and codes the hoarders
// hold.
func hoard(s *Store, order hoardOrder, clock *testClock, next *int) (grants, codes int) {
	for round, progress := 0, true; progress; round++ {
		progress = false
		for _, p := range order.turns(round) {
			user := fmt.Sprint("hoarder", p)
			for took := 0; order.burst == 0 || took < order.burst; took++ {
				g := s.AddGrant(testGrant(*next, user, clock.Now())) == nil
				if !g && order.placeholders {
					// A placeholder recorded is a revoked entry the index holds
					// (RevokeGrant reports whether a grant was standing).
					s.RevokeGrant(grantID(*next), uh(user), clock.Now(), clock.Now().Add(RevokedKeep))
					_, g = s.Grant(grantID(*next))
				}
				if g {
					grants++
				}
				*next++
				c := s.UseCode(fmt.Sprint(user, "/", *next), uh(user), t0.Add(time.Hour)) == nil
				if c {
					codes++
				}
				if !g && !c {
					break
				}
				progress = true
			}
		}
	}
	return grants, codes
}

// The review's finding (round 11): the mean share stopped a hundred
// hoarders arriving together, not one after another (each of the last
// ones holds less than the mean until their own bound): sequential
// hoarders filled 1,600 of 1,600 grants and codes, and a newcomer was
// refused for twelve hours. Whatever the order, a hundred identities now
// hold at most 1,280 + 4 x 20 places (plus one revocation placeholder
// each), and every place left goes to newcomers.
func TestNoArrivalOrderLetsAHundredHoardersFillTheIndex(t *testing.T) {
	const most = MaxGrants*fairFromPercent/100 + 4*20
	for _, order := range hostileOrders() {
		t.Run(order.name, func(t *testing.T) {
			clock := &testClock{now: t0}
			s := New(Options{Now: clock.Now})
			next := 0
			grants, codes := hoard(s, order, clock, &next)
			if got := entries(s); got != grants {
				t.Fatalf("%d entries, the hoarders took %d", got, grants)
			}
			limit := most
			if order.placeholders {
				limit += 100
			}
			t.Logf("a hundred hoarders hold %d grants of %d, %d codes of %d", grants, MaxGrants, codes, maxCodes)
			if grants > limit || codes > most {
				t.Fatalf("a hundred hoarders hold %d grants (at most %d) and %d codes (at most %d)", grants, limit, codes, most)
			}
			room := 0
			for p := range MaxGrants {
				user := fmt.Sprint("newcomer", p)
				err := s.AddGrant(testGrant(10_000_000+p, user, clock.Now()))
				if errors.Is(err, ErrTooManyGrants) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := s.UseCode(user+"/1", uh(user), t0.Add(time.Hour)); err != nil {
					t.Fatalf("newcomer %d refused a sign-in with %d hoarded codes: %v", p, codes, err)
				}
				room++
			}
			if want := MaxGrants - grants; room != want {
				t.Errorf("%d newcomers connected after the hoarders took %d grants, want the %d places left", room, grants, want)
			}
		})
	}
}

// Churn does not lift the bound: hoarders that let entries go (revoked
// and dropped) and take them again, in another order, stay within it.
func TestChurnDoesNotLetHoardersFillTheIndex(t *testing.T) {
	clock := &testClock{now: t0}
	s := New(Options{Now: clock.Now})
	next := 0
	orders := hostileOrders()
	hoard(s, orders[0], clock, &next)
	for cycle := range 4 {
		for p := cycle % 2; p < 100; p += 2 {
			s.RevokeGrantsOf(uh(fmt.Sprint("hoarder", p)), clock.Now())
		}
		clock.Add(RevokedKeep + time.Second)
		hoard(s, orders[1+cycle%3], clock, &next)
		if got, limit := entries(s), MaxGrants*fairFromPercent/100+4*20; got > limit {
			t.Fatalf("cycle %d: the hoarders hold %d grants, past %d", cycle, got, limit)
		}
	}
	if err := s.AddGrant(testGrant(20_000_000, "newcomer", clock.Now())); err != nil {
		t.Errorf("a newcomer refused after churn: %v", err)
	}
}

// Work that is not new is not refused by the fair share (round-11
// review): re-recording a grant another replica admitted and rotated, and
// a person's first revocation placeholder. The hard bounds still apply.
func TestTheFairShareRefusesOnlyNewWork(t *testing.T) {
	clock := &testClock{now: t0}
	fill := func(each int) *Store {
		s := New(Options{Now: clock.Now})
		for p := range 100 {
			for i := range each {
				s.grants[grantID(p*100+i)] = grantEntry{
					User: uh(fmt.Sprint("user", p)), Client: "c", Created: t0.Unix(), Used: t0.Unix(), Expiry: t0.Add(time.Hour).Unix(),
				}
			}
		}
		return s
	}
	s := fill(13)
	// user0 holds 13, the mean, past four fifths: a new grant is refused.
	if err := s.AddGrant(testGrant(99_000, "user0", clock.Now())); !errors.Is(err, ErrTooManyGrants) {
		t.Fatalf("a new grant past the fair share: %v", err)
	}
	rotated := testGrant(99_001, "user0", clock.Now())
	rotated.LastUsed, rotated.Generation, rotated.Rotated = clock.Now(), 3, true
	if err := s.AddGrant(rotated); err != nil {
		t.Errorf("a rotated grant another replica admitted was refused: %v", err)
	}
	// A replayed code's placeholder is recorded once past the fair share.
	if s.RevokeGrant(grantID(99_002), uh("user0"), clock.Now(), clock.Now().Add(RevokedKeep)); !s.GrantRevoked(grantID(99_002)) {
		t.Error("a replayed code's revocation placeholder was refused by the fair share")
	}
	// A second one is new work past the fair share: one placeholder each.
	if s.RevokeGrant(grantID(99_003), uh("user0"), clock.Now(), clock.Now().Add(RevokedKeep)); s.GrantRevoked(grantID(99_003)) {
		t.Error("a second placeholder past the fair share was recorded")
	}
	// The hard bounds hold for both.
	for i := range 16 {
		_ = s.AddGrant(Grant{ID: grantID(98_000 + i), UserHash: uh("user1"), Client: "c", Scope: "mcp:read",
			Created: clock.Now(), LastUsed: clock.Now(), Expiry: clock.Now().Add(time.Hour), Rotated: true})
	}
	s.mu.Lock()
	mine, _, _ := s.countLocked(uh("user1"))
	s.mu.Unlock()
	if mine != MaxGrantsPerUser {
		t.Errorf("rotated grants took user1 to %d, past the per-person bound %d", mine, MaxGrantsPerUser)
	}
	full := fill(MaxGrantsPerUser)
	if err := full.AddGrant(Grant{ID: grantID(97_000), UserHash: uh("newcomer"), Client: "c", Created: clock.Now(),
		LastUsed: clock.Now(), Expiry: clock.Now().Add(time.Hour), Rotated: true}); !errors.Is(err, ErrTooManyGrants) {
		t.Errorf("a rotated grant added to a full index: %v", err)
	}
	if full.RevokeGrant(grantID(97_001), uh("newcomer"), clock.Now(), clock.Now().Add(RevokedKeep)); full.GrantRevoked(grantID(97_001)) {
		t.Error("a placeholder added to a full index")
	}
}
