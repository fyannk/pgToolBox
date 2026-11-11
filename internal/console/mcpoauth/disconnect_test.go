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
	"slices"
	"sync"
	"testing"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/sharedstate"
)

// unrecorded is an index whose revocations cannot be written to the
// shared state while fail is set (an APF throttle, a replica cut off from
// the API server for writes): they stay pending, as the real index keeps
// them unwritten.
type unrecorded struct {
	*sharedstate.Store
	mu      sync.Mutex
	fail    bool
	pending map[string]bool
}

func (u *unrecorded) WriteRevocations(_ context.Context, ids ...string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, id := range ids {
		if u.fail {
			u.pending[id] = true
		} else {
			delete(u.pending, id)
		}
	}
	if u.fail {
		return sharedstate.ErrNotRecorded
	}
	return nil
}

func (u *unrecorded) UnwrittenRevocationsOf(userHash string) []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []string
	for id := range u.pending {
		if g, ok := u.Grant(id); ok && g.UserHash == userHash {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

func (u *unrecorded) setFail(fail bool) {
	u.mu.Lock()
	u.fail = fail
	u.mu.Unlock()
}

// A disconnection is acknowledged only once the shared state holds it: one
// that could not be written is ErrNotRecorded (the session page answers
// 503), refused on this replica meanwhile, and a retry writes it (round
// 16, security P3-2).
func TestADisconnectionIsAcknowledgedOnlyOnceRecorded(t *testing.T) {
	c := &clock{now: t0}
	f := newSessions(t, c, secretA)
	store := &unrecorded{Store: memoryStore(c), pending: map[string]bool{}}
	r := newReplica(t, f, store, c, nil)
	ctx := context.Background()

	_, tok := r.connect(t)
	p, err := r.authenticate(tok.Access)
	if err != nil {
		t.Fatal(err)
	}
	store.setFail(true)
	if err := r.s.RevokeGrant(ctx, auth.Client{}, "alice", p.SessionID); !errors.Is(err, ErrNotRecorded) {
		t.Fatalf("a revocation not written: %v, want ErrNotRecorded", err)
	}
	if _, err := r.authenticate(tok.Access); !errors.Is(err, auth.ErrBearerRevoked) {
		t.Errorf("this replica accepts the grant it revoked: %v", err)
	}
	store.setFail(false)
	if err := r.s.RevokeGrant(ctx, auth.Client{}, "alice", p.SessionID); err != nil {
		t.Errorf("the retry: %v", err)
	}
	if err := r.s.RevokeGrant(ctx, auth.Client{}, "alice", p.SessionID); !errors.Is(err, ErrNoSuchGrant) {
		t.Errorf("a revocation written already: %v, want ErrNoSuchGrant", err)
	}

	_, a := r.connect(t)
	_, b := r.connect(t)
	store.setFail(true)
	if n, err := r.s.DisconnectAll(ctx, auth.Client{}, "alice"); n != 2 || !errors.Is(err, ErrNotRecorded) {
		t.Fatalf("disconnect all, not written: %d, %v", n, err)
	}
	for _, tok := range []tokens{a, b} {
		if _, err := r.authenticate(tok.Access); !errors.Is(err, auth.ErrBearerRevoked) {
			t.Errorf("after an unrecorded disconnect all: %v", err)
		}
	}
	store.setFail(false)
	if _, err := r.s.DisconnectAll(ctx, auth.Client{}, "alice"); err != nil {
		t.Errorf("the retry: %v", err)
	}
	if got := store.UnwrittenRevocationsOf(r.s.userKey("alice")); len(got) != 0 {
		t.Errorf("still unwritten after the retry: %v", got)
	}
	// A sign-out that disconnects goes on whatever the write's outcome.
	_, d := r.connect(t)
	store.setFail(true)
	if n := f.signOut(ctx, auth.Client{}, "alice"); n != 1 {
		t.Errorf("sign-out disconnected %d", n)
	}
	if _, err := r.authenticate(d.Access); !errors.Is(err, auth.ErrBearerRevoked) {
		t.Errorf("after the sign-out: %v", err)
	}
}
