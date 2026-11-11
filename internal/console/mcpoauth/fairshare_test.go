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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/sharedstate"
)

// crowd fills store past four fifths of its grants, a hundred people
// holding thirteen each (the mean), and gives key four grants: key is
// then past the fair share of new work. The grants are recorded Rotated,
// which the fair share does not refuse.
func crowd(t *testing.T, store *sharedstate.Store, key string, at time.Time) {
	t.Helper()
	n := 0
	add := func(user string) {
		n++
		err := store.AddGrant(sharedstate.Grant{
			ID: fmt.Sprintf("grant-%032x", 1<<40+n), UserHash: user, Client: "other", Scope: "mcp:read",
			Created: at, LastUsed: at, Expiry: at.Add(time.Hour), Rotated: true,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for p := range 100 {
		sum := sha256.Sum256([]byte(fmt.Sprint("crowd", p)))
		for range 13 {
			add(hex.EncodeToString(sum[:16]))
		}
	}
	for range 4 {
		add(key)
	}
}

// Work that is not new is not refused by the fair share (round-11
// review): a refresh landing on a replica that has not seen its grant
// rotate re-records it there, and a legitimate holder past the fair
// share is not disconnected; a new grant of the same person is still
// refused.
func TestARefreshReRecordingAGrantIsNotRefusedByTheFairShare(t *testing.T) {
	c := &clock{now: t0}
	f := newSessions(t, c, secretA)
	a := newReplica(t, f, memoryStore(c), c, nil)
	sb := memoryStore(c)
	b := newReplica(t, f, sharedView{sb}, c, nil)
	id, tok := a.connect(t)
	crowd(t, sb, b.s.userKey("alice"), c.Now())

	c.Add(replicationGrace / 2)
	if w, _, _ := b.token(t, refreshForm(id, tok.Refresh)); w.Code != http.StatusOK {
		t.Fatalf("a refresh re-recording a grant past the fair share: %d %s", w.Code, w.Body)
	}
	// New work is still refused: a code exchange on B.
	q := b.consent(t, authorizeQuery(b.clientID(t), loopback), "approve")
	if w, _, e := b.token(t, codeForm(q.Get("client_id"), q.Get("code"))); w.Code != http.StatusBadRequest || e != "invalid_grant" {
		t.Errorf("a new grant past the fair share: %d %s", w.Code, w.Body)
	}
}

// A replayed code's revocation is recorded even when its person is past
// the fair share (round-11 review): the placeholder a later record of the
// grant merges into is not new work.
func TestAReplayedCodesRevocationIsRecordedPastTheFairShare(t *testing.T) {
	c := &clock{now: t0}
	f := newSessions(t, c, secretA)
	store := memoryStore(c)
	r := newReplica(t, f, store, c, nil)
	cid := r.clientID(t)
	crowd(t, store, r.s.userKey("alice"), c.Now())
	code := r.consent(t, authorizeQuery(cid, loopback), "approve").Get("code")
	// The first redemption spends the code; its grant is refused here (new
	// work past the fair share), as it could have been recorded elsewhere.
	if w, _, e := r.token(t, codeForm(cid, code)); w.Code != http.StatusBadRequest || e != "invalid_grant" {
		t.Fatalf("first redemption: %d %s", w.Code, w.Body)
	}
	if w, _, e := r.token(t, codeForm(cid, code)); w.Code != http.StatusBadRequest || e != "invalid_grant" {
		t.Fatalf("replay: %d %s", w.Code, w.Body)
	}
	if !store.GrantRevoked(grantOfCode(t, r, code)) {
		t.Error("the replayed code's revocation was not recorded past the fair share")
	}
}
