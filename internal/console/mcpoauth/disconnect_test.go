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
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

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
	// failWith is the error of a write that fails (ErrNotRecorded when
	// nil).
	failWith error
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
		if u.failWith != nil {
			return u.failWith
		}
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
	if err := f.signOut(ctx, auth.Client{}, "alice"); !errors.Is(err, ErrNotRecorded) {
		t.Errorf("sign-out with the write failing: %v, want ErrNotRecorded (the page says so)", err)
	}
	if _, err := r.authenticate(d.Access); !errors.Is(err, auth.ErrBearerRevoked) {
		t.Errorf("after the sign-out: %v", err)
	}
}

// While the shared state ConfigMap does not exist every replica refuses
// every grant, so a disconnection then is answered done (and logged), not
// 503 forever; one the ConfigMap's permissions refuse stays ErrNotRecorded
// (round 17, P3).
func TestADisconnectionWhileTheConfigMapIsMissingIsDone(t *testing.T) {
	c := &clock{now: t0}
	f := newSessions(t, c, secretA)
	store := &unrecorded{Store: memoryStore(c), pending: map[string]bool{}, failWith: sharedstate.ErrNoConfigMap}
	r := newReplica(t, f, store, c, nil)
	ctx := context.Background()

	_, tok := r.connect(t)
	p, err := r.authenticate(tok.Access)
	if err != nil {
		t.Fatal(err)
	}
	_, other := r.connect(t)
	store.setFail(true)
	if err := r.s.RevokeGrant(ctx, auth.Client{}, "alice", p.SessionID); err != nil {
		t.Errorf("a disconnection while the ConfigMap is missing: %v, want done", err)
	}
	if _, err := r.authenticate(tok.Access); !errors.Is(err, auth.ErrBearerRevoked) {
		t.Errorf("this replica accepts the grant it revoked: %v", err)
	}
	if n, err := r.s.DisconnectAll(ctx, auth.Client{}, "alice"); n != 1 || err != nil {
		t.Errorf("disconnect all while the ConfigMap is missing: %d, %v", n, err)
	}
	if _, err := r.authenticate(other.Access); !errors.Is(err, auth.ErrBearerRevoked) {
		t.Errorf("after disconnect all: %v", err)
	}

	store.mu.Lock()
	store.failWith = sharedstate.ErrNotRecorded // forbidden, failing
	store.mu.Unlock()
	_, third := r.connect(t)
	q, err := r.authenticate(third.Access)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.s.RevokeGrant(ctx, auth.Client{}, "alice", q.SessionID); !errors.Is(err, ErrNotRecorded) {
		t.Errorf("a disconnection the ConfigMap refuses: %v, want ErrNotRecorded", err)
	}
}

// unknownGrant is a well-formed grant reference no replica recorded.
const unknownGrant = "grant-0123456789abcdef0123456789abcdef"

// A replica that cannot read the shared state (one cut off from the API
// server while it still takes requests) acknowledges no disconnection:
// "disconnect all" finds nothing to revoke there and a retried
// disconnection finds no such grant, and both would read as done on the
// session page (204, 404) while another replica serves the grants. Both
// are ErrNotConfirmed (503, retry) until a read of the ConfigMap answers
// (round 18, security P3; round 21, security P2-1; round 22, DBA: not
// ErrNotRecorded, which says the replica revoked something).
func TestAPendingReplicaAcknowledgesNoDisconnection(t *testing.T) {
	c := &clock{now: t0}
	f := newSessions(t, c, secretA)
	cs := fake.NewClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "console-state", Namespace: "team-a", ResourceVersion: "1"}})
	cs.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("dial tcp: i/o timeout"))
	})
	pending := sharedstate.New(sharedstate.Options{Client: cs.CoreV1().ConfigMaps("team-a"), Name: "console-state", Now: c.Now})
	r := newReplica(t, f, pending, c, nil)
	ctx := context.Background()
	if n, err := r.s.DisconnectAll(ctx, auth.Client{}, "alice"); n != 0 || !errors.Is(err, ErrNotConfirmed) {
		t.Errorf("disconnect all on a replica that read nothing: %d, %v, want ErrNotConfirmed", n, err)
	}
	if err := r.s.RevokeGrant(ctx, auth.Client{}, "alice", unknownGrant); !errors.Is(err, ErrNotConfirmed) {
		t.Errorf("a disconnection on a replica that read nothing: %v, want ErrNotConfirmed", err)
	}
	if err := r.s.RevokeGrant(ctx, auth.Client{}, "alice", "grant-0123"); !errors.Is(err, ErrNoSuchGrant) {
		t.Errorf("an ID that is no grant reference: %v, want ErrNoSuchGrant without a read", err)
	}

	// Once read, the same answers are answers.
	a, _ := sharedStores(t, c)
	r = newReplica(t, f, a, c, nil)
	if n, err := r.s.DisconnectAll(ctx, auth.Client{}, "alice"); n != 0 || err != nil {
		t.Errorf("disconnect all with nothing to revoke: %d, %v", n, err)
	}
	if err := r.s.RevokeGrant(ctx, auth.Client{}, "alice", unknownGrant); !errors.Is(err, ErrNoSuchGrant) {
		t.Errorf("an unknown grant on a replica that read the index: %v, want ErrNoSuchGrant", err)
	}
	// One replica, no ConfigMap: the index is its memory, always read.
	r = newReplica(t, f, memoryStore(c), c, nil)
	if err := r.s.RevokeGrant(ctx, auth.Client{}, "alice", unknownGrant); !errors.Is(err, ErrNoSuchGrant) {
		t.Errorf("an unknown grant on a single replica: %v, want ErrNoSuchGrant", err)
	}
}

// A replica that never saw a grant recorded on another (its watch lags,
// has ended, or never opened) reads the ConfigMap before it answers, so
// that "disconnect all" and a single disconnection revoke it there rather
// than answer done with nothing revoked while the other replica serves it
// until it expires (round 21, security P2-1).
func TestADisconnectionReadsTheIndexFirst(t *testing.T) {
	for name, disconnect := range map[string]func(ctx context.Context, r *replica, id string) error{
		"disconnect all": func(ctx context.Context, r *replica, _ string) error {
			n, err := r.s.DisconnectAll(ctx, auth.Client{}, "alice")
			if err == nil && n != 1 {
				return fmt.Errorf("%d revoked, want 1", n)
			}
			return err
		},
		"one": func(ctx context.Context, r *replica, id string) error {
			return r.s.RevokeGrant(ctx, auth.Client{}, "alice", id)
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := &clock{now: t0}
			f := newSessions(t, c, secretA)
			cs := fake.NewClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "console-state", Namespace: "team-a", ResourceVersion: "1"}})
			optimisticConcurrency(cs)
			b := sharedstate.New(sharedstate.Options{
				Client: cs.CoreV1().ConfigMaps("team-a"), Name: "console-state", Now: c.Now,
				Debounce: 5 * time.Millisecond, UrgentDebounce: time.Millisecond,
				Backoff: func(int) time.Duration { return 5 * time.Millisecond },
			})
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() { _ = b.Run(ctx); close(done) }()
			t.Cleanup(func() { cancel(); <-done })
			eventually(t, "b reading", b.GrantsUsable)
			rb := newReplica(t, f, b, c, nil)
			_, tok := rb.connect(t)
			p, err := rb.authenticate(tok.Access)
			if err != nil {
				t.Fatal(err)
			}
			eventually(t, "the grant written", func() bool {
				cm, err := cs.CoreV1().ConfigMaps("team-a").Get(t.Context(), "console-state", metav1.GetOptions{})
				return err == nil && strings.Contains(cm.Data[sharedstate.KeyGrants], p.SessionID)
			})
			// c has no watch: it knows none of b's grants.
			cStore := sharedstate.New(sharedstate.Options{Client: cs.CoreV1().ConfigMaps("team-a"), Name: "console-state", Now: c.Now})
			rc := newReplica(t, f, cStore, c, nil)
			if err := disconnect(t.Context(), rc, p.SessionID); err != nil {
				t.Fatalf("on the replica that never saw the grant: %v", err)
			}
			eventually(t, "b refuses the grant", func() bool {
				_, err := rb.authenticate(tok.Access)
				return errors.Is(err, auth.ErrBearerRevoked)
			})
		})
	}
}
