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

package mcpoauth

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/sharedstate"
)

// Several people (or ServiceAccounts one namespace admin mints), each
// with MaxGrantsPerUser live grants refreshing at the per-grant ceiling,
// used to keep the shared bucket empty: a victim's once-per-access-token
// refresh was refused (round-7 repro, 11 refusals of 16). Each person's
// grants now share a per-person bucket charged before the shared one.
// The clock is the console's for every bucket, so the run is
// deterministic.
func TestManySubjectsDoNotStarveRefresh(t *testing.T) {
	c, r := sharedProxyReplica(t)
	f := r.f
	type grant struct{ id, refresh string }
	const users = 8
	grants := make([]grant, 0, users*sharedstate.MaxGrantsPerUser)
	for u := range users {
		name := fmt.Sprintf("system:serviceaccount:team:sa%d", u)
		f.signIn(name, "idp-"+name)
		for k := range sharedstate.MaxGrantsPerUser {
			// Let the person's spent consent values expire now and then.
			if k%5 == 4 {
				c.Add(4 * time.Minute)
			}
			c.Add(5 * time.Second)
			id, tok := r.connect(t)
			grants = append(grants, grant{id, tok.Refresh})
		}
	}
	f.signIn("bob", "idp-bob")
	c.Add(time.Minute)
	vid, vtok := r.connect(t)
	c.Add(time.Minute)
	victimOK, victimRefused, attackerOK := 0, 0, 0
	for round := range 65 {
		c.Add(time.Second)
		for i := range grants {
			w, tok, _ := r.token(t, refreshForm(grants[i].id, grants[i].refresh))
			if w.Code == http.StatusOK {
				grants[i].refresh = tok.Refresh
				attackerOK++
			}
		}
		// The first seconds see every person's burst at once, which the
		// shared bucket's burst does not cover (a client told slow_down
		// retries); the sustained flood is what must not starve anyone.
		if round < 5 || round%5 != 0 {
			continue
		}
		w, tok, _ := r.token(t, refreshForm(vid, vtok.Refresh))
		if w.Code == http.StatusOK {
			vtok = tok
			victimOK++
		} else {
			victimRefused++
			t.Logf("round %d: %d %s", round, w.Code, w.Body)
		}
	}
	t.Logf("victim: %d ok, %d refused; %d attacker refreshes of %d grants of %d subjects in 65 s", victimOK, victimRefused, attackerOK, len(grants), users)
	if victimRefused > 0 {
		t.Errorf("the victim's refresh was refused %d of %d times", victimRefused, victimOK+victimRefused)
	}
	// The per-person bound: some burst and the refill, never the
	// per-grant ceiling times MaxGrantsPerUser.
	if most := users * (subjectTokenRequestsBurst + subjectTokenRequestsPerMinute + 1); attackerOK > most {
		t.Errorf("%d attacker refreshes in a minute, more than %d", attackerOK, most)
	}
}

// A refresh token behind its grant's generation is a replay: it is
// refused, and the grant revoked, among the checks that cost nothing,
// before the API server is asked anything (here it cannot answer, so the
// expensive path would have said 503).
func TestAStaleGenerationIsRefusedBeforeTheReview(t *testing.T) {
	_, r := sharedProxyReplica(t)
	id, first := r.connect(t)
	w, second, _ := r.token(t, refreshForm(id, first.Refresh))
	if w.Code != http.StatusOK {
		t.Fatalf("first refresh: %d %s", w.Code, w.Body)
	}
	r.f.mu.Lock()
	r.f.reviewErr = errors.New("the API server is down")
	r.f.mu.Unlock()
	w, _, e := r.token(t, refreshForm(id, first.Refresh))
	if w.Code != http.StatusBadRequest || e != "invalid_grant" {
		t.Fatalf("replayed refresh token: %d %s, want 400 invalid_grant", w.Code, w.Body)
	}
	r.f.mu.Lock()
	r.f.reviewErr = nil
	r.f.mu.Unlock()
	if w, _, e := r.token(t, refreshForm(id, second.Refresh)); w.Code != http.StatusBadRequest || e != "invalid_grant" {
		t.Errorf("the current refresh token after a replay: %d %s, want the grant revoked", w.Code, w.Body)
	}
}

// While the grant index cannot be trusted, a refresh token replayed in a
// loop gets a bounded number of audited 503s: the answer spends the
// bucket of unknown tokens, and the rest are throttled (not audited).
func TestAnOutageReplayedInALoopIsBounded(t *testing.T) {
	c := &clock{now: t0}
	f := newSessions(t, c, secretA)
	store := memoryStore(c)
	a := newReplica(t, f, store, c, nil)
	id, tok := a.connect(t)
	b := newReplica(t, f, unusable{store}, c, nil)
	unavailable, throttled := 0, 0
	for range 5 * unknownTokenRequestsBurst {
		switch b.tokenViaProxy(refreshForm(id, tok.Refresh)).Code {
		case http.StatusServiceUnavailable:
			unavailable++
		case http.StatusTooManyRequests:
			throttled++
		}
	}
	if unavailable > unknownTokenRequestsBurst || throttled == 0 {
		t.Errorf("%d audited 503s and %d throttled answers during an outage, want at most %d and some throttled",
			unavailable, throttled, unknownTokenRequestsBurst)
	}
}
