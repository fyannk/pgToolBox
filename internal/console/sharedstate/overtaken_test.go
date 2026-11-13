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
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// getHook runs after the API server answered a Get, before the caller
// sees the answer: the window in which the caller's watch delivers a newer
// ConfigMap.
type getHook struct {
	*apiServer
	after func()
}

func (g *getHook) Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.ConfigMap, error) {
	cm, err := g.apiServer.Get(ctx, name, opts)
	if f := g.after; f != nil {
		g.after = nil
		f()
	}
	return cm, err
}

// updateHook runs after the API server took an Update, before the caller
// sees the answer.
type updateHook struct {
	*apiServer
	after func()
}

func (u *updateHook) Update(ctx context.Context, cm *corev1.ConfigMap, opts metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	out, err := u.apiServer.Update(ctx, cm, opts)
	if f := u.after; f != nil && err == nil {
		u.after = nil
		f()
	}
	return out, err
}

// A conflict's re-read overtaken by the watch (which delivered another
// replica's write of the same spend or mark while the re-read's answer was
// on its way) is not merged, as older than the base; the next write is
// built on that newer base, which holds the other replica's spend. The
// write-through looks for the spend there too: one single-use value is
// not spent on two replicas, and one confirmation does not run one MCP
// write twice (round 16, go P2-1).
func TestASpendIsNotMadeTwiceWhenTheWatchOvertakesTheConflictRead(t *testing.T) {
	until := t0.Add(time.Minute)
	alice := uh("alice")
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, s *Store)
		step    func(ctx context.Context, s *Store) error
	}{
		{"SpendCode", func(*testing.T, *Store) {}, func(ctx context.Context, s *Store) error { return s.SpendCode(ctx, "nonce-1", alice, until) }},
		{"MarkCode", func(t *testing.T, s *Store) {
			if err := s.SpendCode(t.Context(), "nonce-1", alice, until); err != nil {
				t.Fatal(err)
			}
		}, func(ctx context.Context, s *Store) error { return s.MarkCode(ctx, "nonce-1", "run", alice) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &testClock{now: t0}
			api := newAPIServer()
			b := loaded(t, api, clock)
			tc.prepare(t, b)
			hooked := &getHook{apiServer: api}
			a := newStore(hooked, clock, nil)
			watchMerge(t, api, a)
			a.mu.Lock()
			a.watchOpenedLocked()
			a.mu.Unlock()
			watchMerge(t, api, b)
			// Another write lands first, so a's write-through conflicts.
			b.InstanceSince("pod-b", "ok", t0)
			flushed(t, b)
			hooked.after = func() {
				// While a's conflict read is on its way back: b makes the
				// same step, and a's watch delivers b's write.
				if err := tc.step(t.Context(), b); err != nil {
					t.Errorf("b: %v", err)
				}
				watchMerge(t, api, a)
			}
			if err := tc.step(t.Context(), a); !errors.Is(err, ErrSpent) {
				t.Errorf("a's step after b made it: %v, want ErrSpent", err)
			}
		})
	}
}

// The API server's revision went back (an etcd restore without
// --bump-revision): every read is older than the base. Two Gets in a row
// so are taken as the ConfigMap; the writes land again, a revocation made
// after the restore reaches a replica that only reads (its watch
// restarted: a Get, then the watch's first event), and that replica's own
// writes land too (round 16, go P3-1).
func TestARevisionThatWentBackIsTakenAsTheConfigMap(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	api.set(func(x *apiServer) { x.rv = 1000; x.bumpLocked(x.cm.DeepCopy()) })
	a := loaded(t, api, clock)
	b := loaded(t, api, clock)
	if err := a.AddGrant(testGrant(1, "alice", t0)); err != nil {
		t.Fatal(err)
	}
	flushed(t, a)
	watchMerge(t, api, b)
	api.set(func(x *apiServer) { x.rv = 49; x.bumpLocked(x.cm.DeepCopy()) })
	if !b.RevokeGrant(grantID(1), uh("alice"), clock.Now(), clock.Now().Add(time.Hour)) {
		t.Fatal("revoke")
	}
	if err := b.flush(t.Context()); err != nil {
		t.Fatalf("b's write after the restore: %v", err)
	}
	if e := docGrants(t, api)[grantID(1)]; e.Revoked == 0 {
		t.Fatal("the revocation is not in the ConfigMap")
	}
	// a, idle: its watch restarts on the restored API server.
	for range rewoundAfter {
		cm, err := api.Get(t.Context(), "console-state", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		a.mu.Lock()
		a.mergeReadLocked(cm)
		a.mu.Unlock()
	}
	if !a.GrantRevoked(grantID(1)) {
		t.Error("a replica that only reads never took in the revocation made after the restore")
	}
	a.InstanceSince("pod-a", "ok", t0)
	if err := a.flush(t.Context()); err != nil {
		t.Errorf("a's write after the restore: %v", err)
	}
	// One Get older than the base (the watch overtook it) is not enough.
	c := loaded(t, api, clock)
	stale, err := api.Get(t.Context(), "console-state", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c.InstanceSince("pod-c", "ok", t0)
	flushed(t, c)
	before := c.base.ResourceVersion
	c.mu.Lock()
	c.mergeReadLocked(stale)
	c.mu.Unlock()
	if c.base.ResourceVersion != before {
		t.Errorf("a single older Get set the base back from %s to %s", before, c.base.ResourceVersion)
	}
}

// The watch path of the same: the Get of a restarted watch and the
// watch's first event are the two reads.
func TestARestartedWatchTakesInARevisionThatWentBack(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	api.initialEvent = true
	api.set(func(x *apiServer) { x.rv = 1000; x.bumpLocked(x.cm.DeepCopy()) })
	a := newStore(api, clock, slog.New(slog.DiscardHandler))
	run(t, a)
	b := loaded(t, api, clock)
	if err := b.AddGrant(testGrant(1, "alice", t0)); err != nil {
		t.Fatal(err)
	}
	flushed(t, b)
	eventually(t, "a learns the grant", func() bool { return standing(a, grantID(1)) })
	api.set(func(x *apiServer) {
		x.rv = 49
		x.bumpLocked(x.cm.DeepCopy())
		// The API server restarted after the restore: the watches end.
		for _, w := range x.watchers {
			w.Stop()
		}
		x.watchers = nil
	})
	watchMerge(t, api, b)
	if !b.RevokeGrant(grantID(1), uh("alice"), clock.Now(), clock.Now().Add(time.Hour)) {
		t.Fatal("revoke")
	}
	for range rewoundAfter + 1 {
		if err := b.flush(t.Context()); err == nil {
			break
		}
	}
	if e := docGrants(t, api)[grantID(1)]; e.Revoked == 0 {
		t.Fatal("the revocation is not in the ConfigMap")
	}
	eventually(t, "a takes in the revocation", func() bool { return a.GrantRevoked(grantID(1)) })
}

// A write still conflicting after maxAttempts counts as a failure (the
// failing mode), and the exhaustion is counted.
func TestAnExhaustedWriteCountsAsFailing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(ctx context.Context, s *Store) error
	}{
		{"flush", func(ctx context.Context, s *Store) error {
			s.InstanceSince("pod", "ok", t0)
			return s.flush(ctx)
		}},
		{"SpendCode", func(ctx context.Context, s *Store) error {
			return s.SpendCode(ctx, "c", uh("alice"), t0.Add(time.Minute))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &testClock{now: t0}
			api := newAPIServer()
			a := loaded(t, api, clock)
			api.set(func(x *apiServer) { x.conflicts = 2 * maxAttempts })
			before := testutil.ToFloat64(sharedExhausted)
			if err := tc.write(t.Context(), a); err == nil {
				t.Fatal("the write landed through every conflict")
			}
			if got := testutil.ToFloat64(sharedExhausted) - before; got != 1 {
				t.Errorf("write_exhausted_total grew by %v, want 1", got)
			}
			if a.mode != modeFailing {
				t.Errorf("mode %s after an exhausted write, want failing", a.mode)
			}
		})
	}
}

// A write's answer older than the base (the watch delivered another
// replica's later write before the answer was read) does not set the base
// back: the next write would conflict, and what that later write added
// must stay (round 16, go P3-2).
func TestAWriteAnswerDoesNotSetTheBaseBack(t *testing.T) {
	for _, through := range []bool{false, true} {
		t.Run(strconv.FormatBool(through), func(t *testing.T) {
			clock := &testClock{now: t0}
			api := newAPIServer()
			hooked := &updateHook{apiServer: api}
			a := newStore(hooked, clock, nil)
			watchMerge(t, api, a)
			a.mu.Lock()
			a.watchOpenedLocked()
			a.mu.Unlock()
			b := loaded(t, api, clock)
			hooked.after = func() {
				watchMerge(t, api, b)
				if err := b.AddGrant(testGrant(2, "bob", t0)); err != nil {
					t.Error(err)
				}
				flushed(t, b)
				watchMerge(t, api, a)
			}
			if through {
				if err := a.SpendCode(t.Context(), "c", uh("alice"), t0.Add(time.Minute)); err != nil {
					t.Fatal(err)
				}
			} else {
				a.InstanceSince("pod-a", "ok", t0)
				flushed(t, a)
			}
			api.mu.Lock()
			newest := api.cm.ResourceVersion
			api.mu.Unlock()
			if a.base.ResourceVersion != newest {
				t.Errorf("base at %s after the write's answer, the watch delivered %s", a.base.ResourceVersion, newest)
			}
			if !standing(a, grantID(2)) {
				t.Error("the grant the later write added is not standing")
			}
		})
	}
}

type failingWrites struct{ *apiServer }

func (f failingWrites) Update(context.Context, *corev1.ConfigMap, metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	return nil, errors.New("connection refused")
}

// A disconnection is acknowledged only once written (WriteRevocations):
// a replica whose writes fail says so (ErrNotRecorded, the console answers
// 503) and keeps the revocation; one whose write landed loses nothing to a
// restart (round 16, security P3-2).
func TestARevocationIsAcknowledgedOnlyOnceWritten(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	a := loaded(t, api, clock)
	for n := 1; n <= 3; n++ {
		if err := a.AddGrant(testGrant(n, "alice", t0)); err != nil {
			t.Fatal(err)
		}
	}
	flushed(t, a)
	failing := newStore(failingWrites{api}, clock, slog.New(slog.DiscardHandler))
	watchMerge(t, api, failing)
	failing.mu.Lock()
	failing.watchOpenedLocked()
	failing.mu.Unlock()
	if !failing.RevokeGrant(grantID(1), uh("alice"), clock.Now(), clock.Now().Add(12*time.Hour)) {
		t.Fatal("revoke")
	}
	if err := failing.WriteRevocations(t.Context(), grantID(1)); !errors.Is(err, ErrNotRecorded) {
		t.Fatalf("a revocation whose write failed: %v, want ErrNotRecorded", err)
	}
	if !failing.GrantRevoked(grantID(1)) {
		t.Error("the local revocation was undone")
	}
	if got := failing.UnwrittenRevocationsOf(uh("alice")); len(got) != 1 || got[0] != grantID(1) {
		t.Errorf("unwritten revocations %v", got)
	}

	b := loaded(t, api, clock)
	if !b.RevokeGrant(grantID(2), uh("alice"), clock.Now(), clock.Now().Add(12*time.Hour)) {
		t.Fatal("revoke")
	}
	ids := b.RevokeGrantsOf(uh("alice"), clock.Now())
	ids = append(ids, grantID(2))
	if err := b.WriteRevocations(t.Context(), ids...); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := b.UnwrittenRevocationsOf(uh("alice")); len(got) != 0 {
		t.Errorf("unwritten after the write: %v", got)
	}
	// b restarts (rollout, eviction, OOM) right after the answer.
	b = loaded(t, api, clock)
	watchMerge(t, api, a)
	for _, s := range []*Store{a, b} {
		for _, id := range ids {
			if standing(s, id) {
				t.Errorf("%s stands after an acknowledged revocation and a restart", id)
			}
		}
	}
	// Written already (by another replica): nothing to write, nil.
	if err := a.WriteRevocations(t.Context(), grantID(2)); err != nil {
		t.Errorf("a revocation the ConfigMap holds: %v", err)
	}
	// One replica, no ConfigMap: nothing to write.
	if err := New(Options{}).WriteRevocations(t.Context(), grantID(1)); err != nil {
		t.Errorf("without a ConfigMap: %v", err)
	}
}

// A batch written while a write-through is in flight (the writer's
// debounce ends: a change waiting longer than UrgentDebounce is written at
// once) would carry the write-through's own spend or mark; the
// write-through, conflicting with it, would find the spend in the
// ConfigMap and call it another replica's: a confirmation's answer then
// failed with "could not be recorded" (round 16, the chart e2e). Batches
// wait for a write-through to end.
func TestABatchDoesNotRaceItsOwnWriteThrough(t *testing.T) {
	until := t0.Add(time.Minute)
	alice := uh("alice")
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, s *Store)
		step    func(ctx context.Context, s *Store) error
	}{
		{"SpendCode", func(*testing.T, *Store) {}, func(ctx context.Context, s *Store) error { return s.SpendCode(ctx, "nonce-1", alice, until) }},
		{"MarkCode", func(t *testing.T, s *Store) {
			if err := s.SpendCode(t.Context(), "nonce-1", alice, until); err != nil {
				t.Fatal(err)
			}
		}, func(ctx context.Context, s *Store) error { return s.MarkCode(ctx, "nonce-1", "declined", alice) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &testClock{now: t0}
			api := newAPIServer()
			hooked := &getHook{apiServer: api}
			a := newStore(hooked, clock, nil)
			watchMerge(t, api, a)
			a.mu.Lock()
			a.watchOpenedLocked()
			a.mu.Unlock()
			tc.prepare(t, a)
			a.InstanceSince("pod-a", "ok", t0) // a batch is waiting
			// Another replica's write lands first: the write-through
			// conflicts and re-reads.
			api.set(func(x *apiServer) { x.conflicts = 1 })
			batch := make(chan error, 1)
			hooked.after = func() {
				// The writer's batch starts while the re-read is on its way.
				go func() { batch <- a.flush(t.Context()) }()
				select {
				case err := <-batch:
					batch <- err
				case <-time.After(100 * time.Millisecond):
				}
			}
			if err := tc.step(t.Context(), a); err != nil {
				t.Errorf("the write-through raced by its own batch: %v", err)
			}
			if err := <-batch; err != nil {
				t.Errorf("the batch: %v", err)
			}
		})
	}
}
