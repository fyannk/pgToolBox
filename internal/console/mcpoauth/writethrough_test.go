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
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/sharedstate"
)

// scripted is an index whose grant writes answer as the test says.
type scripted struct {
	*sharedstate.Store
	mu        sync.Mutex
	writeErr  error
	grantsSet []string
}

func (s *scripted) WriteGrant(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return s.writeErr
	}
	s.grantsSet = append(s.grantsSet, id)
	return nil
}

// A grant's first token is issued only once the grant is written to the
// shared state (round 22, Go P2-1, security): a token request whose write
// fails is 503 temporarily_unavailable, no token, and the grant is revoked
// here, so that it can never be written unrevoked later; every grant an
// assistant holds is in the ConfigMap the next leader loads.
func TestNoTokenBeforeTheGrantIsWritten(t *testing.T) {
	c := &clock{now: t0}
	f := newSessions(t, c, secretA)
	store := &scripted{Store: memoryStore(c)}
	r := newReplica(t, f, store, c, nil)

	store.writeErr = sharedstate.ErrNotRecorded
	id := r.clientID(t)
	q := r.consent(t, authorizeQuery(id, loopback), "approve")
	w, tok, code := r.token(t, codeForm(id, q.Get("code")))
	if w.Code != http.StatusServiceUnavailable || code != "temporarily_unavailable" || tok.Access != "" || tok.Refresh != "" {
		t.Fatalf("grant not written: %d %q, tokens issued %v; want 503 temporarily_unavailable and none", w.Code, code, tok.Access != "")
	}
	for _, g := range store.Store.GrantsOf(r.s.userKey("alice")) {
		t.Errorf("grant %s stands after its write failed", g.ID)
	}

	store.writeErr = nil
	_, tok = r.connect(t)
	p, err := r.authenticate(tok.Access)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.grantsSet) != 1 || store.grantsSet[0] != p.SessionID {
		t.Errorf("grants written before their token: %v, want [%s]", store.grantsSet, p.SessionID)
	}
}

// A code whose console session signed out before it was redeemed buys no
// grant, as at consent (round 22, security): the approval went with the
// session.
func TestACodeOfASignedOutSessionBuysNothing(t *testing.T) {
	_, f, r := setup(t)
	id := r.clientID(t)
	q := r.consent(t, authorizeQuery(id, loopback), "approve")
	f.revoke("out:" + f.session.Principal.SessionID)
	w, tok, code := r.token(t, codeForm(id, q.Get("code")))
	if w.Code != http.StatusBadRequest || code != "invalid_grant" || tok.Access != "" {
		t.Errorf("code of a signed-out session: %d %q, token issued %v; want invalid_grant", w.Code, code, tok.Access != "")
	}
}

// A disconnection whose write fails is ErrNotRecorded, and the grant is
// refused here at once all the same.
func TestADisconnectionWhoseWriteFailsIsNotRecorded(t *testing.T) {
	ctx := context.Background()
	c := &clock{now: t0}
	f := newSessions(t, c, secretA)
	u := &unrecorded{Store: memoryStore(c), pending: map[string]bool{}}
	r := newReplica(t, f, u, c, nil)
	_, tok := r.connect(t)
	u.setFail(true)
	if n, err := r.s.DisconnectAll(ctx, auth.Client{}, "alice"); n != 1 || !errors.Is(err, ErrNotRecorded) {
		t.Errorf("write failed: %d, %v; want 1 and ErrNotRecorded", n, err)
	}
	if _, err := r.authenticate(tok.Access); !errors.Is(err, auth.ErrBearerRevoked) {
		t.Errorf("the grant is not refused at once: %v", err)
	}
	if n, err := r.s.DisconnectAll(ctx, auth.Client{}, "nobody"); n != 0 || err != nil {
		t.Errorf("nothing to disconnect: %d, %v; want 0 and nil", n, err)
	}
}

// An ID that is no grant reference, or a grant the leader's index does not
// hold, is ErrNoSuchGrant: the index is the authority.
func TestAnUnknownGrantIsNoSuchGrant(t *testing.T) {
	ctx := context.Background()
	_, _, r := setup(t)
	for _, bad := range []string{"grant-x", "x", "", "grant-0123456789ABCDEF0123456789ABCDEF", unknownGrant} {
		if err := r.s.RevokeGrant(ctx, auth.Client{}, "alice", bad); !errors.Is(err, ErrNoSuchGrant) {
			t.Errorf("RevokeGrant(%q) = %v, want ErrNoSuchGrant", bad, err)
		}
	}
}
