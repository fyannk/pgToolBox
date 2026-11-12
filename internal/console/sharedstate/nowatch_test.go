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
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// watchControl is the API server with watches the test opens, refuses
// (an overloaded API server shedding long-running requests: 429/503) and
// ends with an error event.
type watchControl struct {
	*apiServer
	mu      sync.Mutex
	refuse  bool
	watches int
	last    *watch.RaceFreeFakeWatcher
}

func (w *watchControl) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.watches++
	if w.refuse {
		return nil, apierrors.NewTooManyRequests("watch shed", 1)
	}
	fw, err := w.apiServer.Watch(ctx, opts)
	if err == nil {
		w.last = fw.(*watch.RaceFreeFakeWatcher)
	}
	return fw, err
}

func (w *watchControl) set(f func(*watchControl)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f(w)
}

func (w *watchControl) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.watches
}

func controlled(api ConfigMapClient, clock *testClock, backoff time.Duration) *Store {
	return New(Options{
		Client: api, Name: "console-state", Now: clock.Now, Logger: slog.New(slog.DiscardHandler),
		Debounce: 5 * time.Millisecond, UrgentDebounce: time.Millisecond,
		Backoff: func(int) time.Duration { return backoff },
	})
}

// With every watch refused, the failing mode is not left by the writer's
// write that lands: a revocation written by another replica is not seen,
// so grants are refused once staleGrace has run (round 18, Go P2; the
// reviewer's probe TestR18GoLiveWithoutAWatch, which saw the replica live
// again with no watch and the revoked grant usable).
func TestALiveModeNeedsAWatch(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	// b: another replica, which records a grant and later revokes it.
	b := loaded(t, api, clock)
	id := grantID(7)
	if err := b.AddGrant(Grant{ID: id, UserHash: uh("alice"), Client: "c", Created: t0, Expiry: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	flushed(t, b)

	wr := &watchControl{apiServer: api, refuse: true}
	// The Run loop's pause before the next read (Backoff caps at a minute
	// in production).
	a := controlled(wr, clock, 3*time.Second)
	run(t, a)
	eventually(t, "a knows the grant", func() bool { _, ok := a.Grant(id); return ok })
	eventually(t, "a failing", func() bool { return wr.count() >= 1 && modeOf(a) == modeFailing })
	a.InstanceSince("pod", "ok", t0) // a batch to write, which lands
	eventually(t, "a's batch written", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.gen == a.written
	})
	if m := modeOf(a); m != modeFailing {
		t.Fatalf("a: mode %s with no watch open, want failing", m)
	}
	b.RevokeGrant(id, uh("alice"), clock.Now(), t0.Add(time.Hour))
	watchMerge(t, api, b)
	if err := b.WriteRevocations(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	clock.Add(staleGrace + 5*time.Second)
	time.Sleep(100 * time.Millisecond)
	if a.GrantsUsable() && !a.GrantRevoked(id) {
		t.Errorf("a revoked grant stays usable on a replica with no watch, %s after it lost it", staleGrace+5*time.Second)
	}
}

// No write path leaves the failing mode while no watch is open: a batch
// (with something to write or nothing), a spend, a mark and a
// revocation, each landing. Each asks the Run loop to open the watch
// again, and the replica refuses grants once staleGrace has run.
func TestNoWatchMeansNoLive(t *testing.T) {
	paths := map[string]struct {
		setup func(t *testing.T, s *Store)
		op    func(ctx context.Context, s *Store) error
	}{
		"batch": {
			setup: func(_ *testing.T, s *Store) { s.InstanceSince("pod", "ok", t0) },
			op:    func(ctx context.Context, s *Store) error { return s.flush(ctx) },
		},
		"idle batch": {
			op: func(ctx context.Context, s *Store) error { return s.flush(ctx) },
		},
		"spend": {
			op: func(ctx context.Context, s *Store) error {
				return s.SpendCode(ctx, "c", uh("alice"), t0.Add(time.Minute))
			},
		},
		"mark": {
			setup: func(t *testing.T, s *Store) {
				if err := s.SpendCode(t.Context(), "c", uh("alice"), t0.Add(time.Minute)); err != nil {
					t.Fatal(err)
				}
			},
			op: func(ctx context.Context, s *Store) error { return s.MarkCode(ctx, "c", "run", uh("alice")) },
		},
		"revocations": {
			setup: func(t *testing.T, s *Store) {
				if err := s.AddGrant(testGrant(1, "alice", t0)); err != nil {
					t.Fatal(err)
				}
				flushed(t, s)
				s.RevokeGrant(grantID(1), uh("alice"), t0, t0.Add(time.Hour))
			},
			op: func(ctx context.Context, s *Store) error { return s.WriteRevocations(ctx, grantID(1)) },
		},
	}
	for name, p := range paths {
		t.Run(name, func(t *testing.T) {
			clock := &testClock{now: t0}
			api := newAPIServer()
			s := loaded(t, api, clock)
			if p.setup != nil {
				p.setup(t, s)
			}
			// The watch ended and the next was refused.
			s.mu.Lock()
			s.watching = false
			s.mu.Unlock()
			_ = s.failed(apierrors.NewTooManyRequests("watch shed", 1))
			select {
			case <-s.rewatch:
			default:
			}
			if err := p.op(t.Context(), s); err != nil {
				t.Fatalf("the write: %v", err)
			}
			if m := modeOf(s); m != modeFailing {
				t.Errorf("after a write with no watch: mode %s, want failing", m)
			}
			select {
			case <-s.rewatch:
			default:
				t.Error("no new watch asked for")
			}
			clock.Add(staleGrace)
			if s.GrantsUsable() {
				t.Error("grants usable a staleGrace after the watch was lost")
			}
		})
	}
}

// A watch that ends with an error event (watch.Error) leaves no replica
// live during the Run loop's backoff: the failing mode starts, grants are
// refused past staleGrace while the next watch is refused, and the
// replica is live again once one opens.
func TestAWatchErrorStartsTheGrace(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	wc := &watchControl{apiServer: api}
	a := controlled(wc, clock, 2*time.Second)
	run(t, a)
	eventually(t, "a live", func() bool { return modeOf(a) == modeLive })
	wc.set(func(w *watchControl) {
		w.refuse = true
		w.last.Error(&metav1.Status{Status: metav1.StatusFailure, Code: 500, Reason: metav1.StatusReasonInternalError, Message: "etcd"})
	})
	eventually(t, "a failing", func() bool { return modeOf(a) == modeFailing })
	clock.Add(staleGrace)
	if a.GrantsUsable() {
		t.Error("grants usable a staleGrace after the watch failed, with no watch since")
	}
	wc.set(func(w *watchControl) { w.refuse = false })
	eventually(t, "a live again with a new watch", func() bool { return modeOf(a) == modeLive })
	if !a.GrantsUsable() {
		t.Error("grants refused once a watch is open again")
	}
}

// A watch the API server keeps refusing is retried at once after the
// write that asks for it, then with the backoff: never in a loop,
// whatever is written meanwhile.
func TestARefusedWatchIsNotRetriedInALoop(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	wc := &watchControl{apiServer: api, refuse: true}
	a := controlled(wc, clock, time.Hour)
	run(t, a)
	eventually(t, "a failing", func() bool { return modeOf(a) == modeFailing })
	eventually(t, "the immediate retry", func() bool { return wc.count() == 2 })
	for i := range 20 {
		a.InstanceSince("pod", string(rune('a'+i)), t0)
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if n := wc.count(); n != 2 {
		t.Errorf("%d watches asked for, want 2 (the first and one retry at once)", n)
	}
	if m := modeOf(a); m != modeFailing {
		t.Errorf("mode %s with every watch refused, want failing", m)
	}
}

// hiding is the API server with a replica's reads answering NotFound
// while hidden: that replica has not polled the recreated ConfigMap yet.
type hiding struct {
	*apiServer
	mu     sync.Mutex
	hidden bool
}

func (h *hiding) Get(ctx context.Context, name string, o metav1.GetOptions) (*corev1.ConfigMap, error) {
	h.mu.Lock()
	hidden := h.hidden
	h.mu.Unlock()
	if hidden {
		return nil, apierrors.NewNotFound(configMaps, name)
	}
	return h.apiServer.Get(ctx, name, o)
}

// While the ConfigMap is missing it is polled every second or two, not
// backed off to a minute: once it is recreated, a revocation a held (made
// while it was missing) is written within seconds, even when b, which
// knows the grant, read the new ConfigMap first and serves it meanwhile
// (round 18, P3).
func TestAMissingConfigMapIsPolled(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	ha := &hiding{apiServer: api}
	// A backoff of an hour: only the poll of the missing ConfigMap reads
	// it again within the test.
	a := controlled(ha, clock, time.Hour)
	b := controlled(api, clock, time.Hour)
	run(t, a)
	run(t, b)
	eventually(t, "both live", func() bool { return modeOf(a) == modeLive && modeOf(b) == modeLive })
	if err := b.AddGrant(testGrant(1, "alice", t0)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a knows the grant", func() bool { return standing(a, grantID(1)) })

	api.mu.Lock()
	for _, w := range api.watchers {
		if !w.IsStopped() {
			w.Delete(api.cm.DeepCopy())
		}
	}
	api.cm = nil
	api.mu.Unlock()
	eventually(t, "both missing", func() bool { return modeOf(a) == modeMissing && modeOf(b) == modeMissing })
	a.RevokeGrant(grantID(1), uh("alice"), t0, t0.Add(time.Hour))
	if err := a.WriteRevocations(t.Context(), grantID(1)); !errors.Is(err, ErrNoConfigMap) {
		t.Fatalf("missing: %v, want ErrNoConfigMap", err)
	}

	ha.mu.Lock()
	ha.hidden = true
	ha.mu.Unlock()
	api.mu.Lock()
	api.bumpLocked(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "console-state", Namespace: "team-a", UID: "uid-2"}})
	api.mu.Unlock()
	// b first: it reads the new ConfigMap and writes the grant it
	// recorded, unrevoked.
	eventually(t, "b live on the new ConfigMap", func() bool { return modeOf(b) == modeLive })
	ha.mu.Lock()
	ha.hidden = false
	ha.mu.Unlock()
	start := time.Now()
	eventually(t, "a's revocation written to the new ConfigMap", func() bool {
		return docGrants(t, api)[grantID(1)].Revoked > 0
	})
	eventually(t, "b refuses the grant", func() bool { return !standing(b, grantID(1)) })
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("the held revocation took %s to reach the other replica, want at most about 2 s", d)
	}
}

// WriteRevocations of no IDs ("disconnect all" with nothing to revoke
// here) is nil only on a replica that has read the grants document or
// knows the ConfigMap missing or forbidden: one that read nothing yet, or cannot read
// the document, acknowledges no disconnection (round 18, security P3; the
// reviewer's probe TestR18PendingReplicaAcknowledgesDisconnectAll).
func TestNothingToRevokeIsAnAnswerOnlyOnceRead(t *testing.T) {
	for name, tc := range map[string]struct {
		prepare func(t *testing.T, s *Store, api *apiServer)
		want    error
	}{
		"pending": {want: ErrNotRecorded},
		"read":    {prepare: func(t *testing.T, s *Store, api *apiServer) { watchMerge(t, api, s) }},
		"missing": {prepare: func(_ *testing.T, s *Store, _ *apiServer) {
			_ = s.failed(apierrors.NewNotFound(configMaps, "console-state"))
		}},
		// Every grant is refused on every replica (they share the
		// ServiceAccount): nothing to disconnect is an answer, not a 503
		// forever on an install without the RBAC (round 19, Go P3-1).
		"forbidden from the start": {
			prepare: func(_ *testing.T, s *Store, _ *apiServer) {
				_ = s.failed(apierrors.NewForbidden(configMaps, "console-state", errors.New("no")))
			},
		},
		"grants unreadable": {
			prepare: func(t *testing.T, s *Store, api *apiServer) {
				api.set(func(x *apiServer) {
					cm := x.cm.DeepCopy()
					cm.Data = map[string]string{KeyGrants: "{not json"}
					x.bumpLocked(cm)
				})
				watchMerge(t, api, s)
			},
			want: ErrNotRecorded,
		},
	} {
		t.Run(name, func(t *testing.T) {
			clock := &testClock{now: t0}
			api := newAPIServer()
			s := newStore(api, clock, nil)
			if tc.prepare != nil {
				tc.prepare(t, s, api)
			}
			ids := s.RevokeGrantsOf(uh("alice"), t0)
			if err := s.WriteRevocations(t.Context(), ids...); !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Errorf("WriteRevocations of %d IDs: %v, want %v", len(ids), err, tc.want)
			}
		})
	}
}

// A panic under the store's lock on a write path (recovered by net/http in
// the handler that called it) releases the lock and the write semaphore:
// the store goes on serving instead of deadlocking every later request
// (round 18, P3).
func TestAPanicOnAWritePathLeavesTheStoreUnlocked(t *testing.T) {
	for name, op := range map[string]func(ctx context.Context, s *Store) error{
		"batch": func(ctx context.Context, s *Store) error { return s.flush(ctx) },
		"spend": func(ctx context.Context, s *Store) error {
			return s.SpendCode(ctx, "c2", uh("alice"), t0.Add(time.Minute))
		},
		"mark": func(ctx context.Context, s *Store) error { return s.MarkCode(ctx, "c", "run", uh("alice")) },
		"revocations": func(ctx context.Context, s *Store) error {
			s.RevokeGrant(grantID(1), uh("alice"), t0, t0.Add(time.Hour))
			return s.WriteRevocations(ctx, grantID(1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			clock := &testClock{now: t0}
			api := newAPIServer()
			var panicking atomic.Bool
			s := New(Options{
				Client: api, Name: "console-state", Logger: slog.New(slog.DiscardHandler),
				Now: func() time.Time {
					if panicking.Load() {
						panic("injected")
					}
					return clock.Now()
				},
			})
			watchMerge(t, api, s)
			s.mu.Lock()
			s.watchOpenedLocked()
			s.mu.Unlock()
			if err := s.AddGrant(testGrant(1, "alice", t0)); err != nil {
				t.Fatal(err)
			}
			if err := s.SpendCode(t.Context(), "c", uh("alice"), t0.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			s.InstanceSince("pod", "ok", t0)
			panicking.Store(true)
			panicked := func() (p bool) {
				defer func() { p = recover() != nil }()
				_ = op(t.Context(), s)
				return false
			}()
			panicking.Store(false)
			if !panicked {
				t.Fatal("no panic injected on this path")
			}
			if !s.mu.TryLock() {
				t.Fatal("the store's lock is still held after the panic")
			}
			s.mu.Unlock()
			if err := s.lockWrites(t.Context()); err != nil {
				t.Fatal(err)
			}
			s.unlockWrites()
		})
	}
}
