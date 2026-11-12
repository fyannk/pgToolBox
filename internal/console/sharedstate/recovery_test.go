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
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ctxAware answers as client-go does once the request's context is done.
type ctxAware struct{ *apiServer }

func (c ctxAware) Update(ctx context.Context, cm *corev1.ConfigMap, o metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.apiServer.Update(ctx, cm, o)
}

// flaky refuses the next fails updates as an API server under load does
// (503), then answers as apiServer.
type flaky struct {
	*apiServer
	mu    sync.Mutex
	fails int
}

func (f *flaky) Update(ctx context.Context, cm *corev1.ConfigMap, o metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	f.mu.Lock()
	if f.fails > 0 {
		f.fails--
		f.mu.Unlock()
		return nil, apierrors.NewServiceUnavailable("throttled")
	}
	f.mu.Unlock()
	return f.apiServer.Update(ctx, cm, o)
}

func (f *flaky) failNext(n int) {
	f.mu.Lock()
	f.fails = n
	f.mu.Unlock()
}

func modeOf(s *Store) mode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

// A caller that gave up (the client closed the connection) before its
// spend was written is no failure of the API server: the replica stays
// live, and a staleGrace later still serves grants (round 17, Go P2).
func TestACancelledSpendLeavesTheReplicaLive(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	a := newStore(ctxAware{api}, clock, nil)
	watchMerge(t, api, a)
	a.mu.Lock()
	a.watchOpenedLocked()
	a.mu.Unlock()
	a.InstanceSince("pod", "ok", t0)
	flushed(t, a)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := a.SpendCode(ctx, "c", uh("alice"), t0.Add(time.Minute)); !errors.Is(err, ErrNotRecorded) {
		t.Fatalf("a cancelled spend: %v, want ErrNotRecorded", err)
	}
	if a.CodeUsed("c") {
		t.Error("a spend never written left its reservation")
	}
	flushed(t, a)
	clock.Add(staleGrace + time.Second)
	if !a.GrantsUsable() {
		t.Errorf("grants refused on a healthy replica because one caller cancelled: mode %s", modeOf(a))
	}
}

// The same with the replica running (its watch and writer).
func TestACancelledSpendStrandsNoReplica(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	a := newStore(ctxAware{api}, clock, nil)
	run(t, a)
	eventually(t, "a settled", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.mode == modeLive && a.gen == a.written && a.base != nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_ = a.SpendCode(ctx, "nonce-z", uh("mallory"), t0.Add(time.Minute))
	time.Sleep(50 * time.Millisecond)
	clock.Add(staleGrace + time.Second)
	if !a.GrantsUsable() {
		t.Errorf("one cancelled request makes the replica refuse every MCP grant: mode %s", modeOf(a))
	}
}

// A caller still waiting for another write when it gives up starts no
// write, records nothing, and changes no mode.
func TestACallerThatGivesUpWaitingStartsNoWrite(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	a := loaded(t, api, clock)
	if err := a.lockWrites(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := api.updates
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	for name, op := range map[string]func() error{
		"spend":       func() error { return a.SpendCode(ctx, "c", uh("alice"), t0.Add(time.Minute)) },
		"revocations": func() error { return a.WriteRevocations(ctx, grantID(1)) },
		"batch":       func() error { return a.flush(ctx) },
	} {
		if err := op(); err == nil {
			t.Errorf("%s: wrote while another write held the ConfigMap", name)
		}
	}
	a.unlockWrites()
	if api.updates != before || a.CodeUsed("c") || modeOf(a) != modeLive {
		t.Errorf("updates %d -> %d, code reserved %v, mode %s", before, api.updates, a.CodeUsed("c"), modeOf(a))
	}
}

// blocking holds every update until released, and records whether the
// update's context was cancelled meanwhile.
type blocking struct {
	*apiServer
	started, release chan struct{}
	cancelled        bool
}

func (b *blocking) Update(ctx context.Context, cm *corev1.ConfigMap, o metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	close(b.started)
	<-b.release
	if ctx.Err() != nil {
		b.cancelled = true
		return nil, ctx.Err()
	}
	return b.apiServer.Update(ctx, cm, o)
}

// A caller that gives up while its write is on the wire does not cancel
// the write: it lands (bounded by writeTimeout), the spend stays recorded,
// and the replica is live.
func TestAStartedWriteThroughOutlivesItsCaller(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	b := &blocking{apiServer: api, started: make(chan struct{}), release: make(chan struct{})}
	a := newStore(b, clock, nil)
	watchMerge(t, api, a)
	a.mu.Lock()
	a.watchOpenedLocked()
	a.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.SpendCode(ctx, "c", uh("alice"), t0.Add(time.Minute)) }()
	<-b.started
	cancel()
	close(b.release)
	if err := <-done; err != nil {
		t.Fatalf("a spend whose caller left: %v", err)
	}
	if b.cancelled || !api.holds("c") || !a.CodeUsed("c") || modeOf(a) != modeLive {
		t.Errorf("cancelled %v, held %v, reserved %v, mode %s", b.cancelled, api.holds("c"), a.CodeUsed("c"), modeOf(a))
	}
}

// Every write path that fails, conflicting maxAttempts times or refused
// by an API server under load, leaves the failing mode with the next
// batch, although its change was rolled back and nothing is pending
// (round 17, Go and security P2: the replica stayed failing, refusing
// every grant past staleGrace, until an unrelated write).
func TestEveryFailedWriteRecoversWithTheNextBatch(t *testing.T) {
	paths := map[string]struct {
		setup func(t *testing.T, s *Store)
		op    func(ctx context.Context, s *Store) error
	}{
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
		"batch": {
			setup: func(_ *testing.T, s *Store) { s.InstanceSince("pod", "ok", t0) },
			op:    func(ctx context.Context, s *Store) error { return s.flush(ctx) },
		},
	}
	causes := map[string]func(api *apiServer, c *flaky){
		"exhausted": func(api *apiServer, _ *flaky) { api.set(func(x *apiServer) { x.conflicts = maxAttempts }) },
		"transient": func(_ *apiServer, c *flaky) { c.failNext(1) },
	}
	for pname, p := range paths {
		for cname, cause := range causes {
			t.Run(pname+"/"+cname, func(t *testing.T) {
				clock := &testClock{now: t0}
				api := newAPIServer()
				c := &flaky{apiServer: api}
				s := newStore(c, clock, nil)
				watchMerge(t, api, s)
				s.mu.Lock()
				s.watchOpenedLocked()
				s.mu.Unlock()
				if p.setup != nil {
					p.setup(t, s)
				}
				cause(api, c)
				if err := p.op(t.Context(), s); err == nil {
					t.Fatal("the write landed")
				}
				if m := modeOf(s); m != modeFailing {
					t.Fatalf("after the failed write: mode %s, want failing", m)
				}
				// The writer was woken for the retry.
				select {
				case <-s.wake:
				default:
					t.Error("the failing mode wakes no writer")
				}
				flushed(t, s)
				if m := modeOf(s); m != modeLive {
					t.Errorf("after the next batch: mode %s, want live", m)
				}
				clock.Add(staleGrace + time.Second)
				if !s.GrantsUsable() {
					t.Error("grants refused once the API server takes writes again")
				}
			})
		}
	}
}

// An idle replica (its watch live, other replicas writing) whose one
// write-through was exhausted is live again by itself (round 17,
// security P2).
func TestAnExhaustedWriteStrandsNoIdleReplica(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	a := newStore(api, clock, nil)
	b := newStore(api, clock, nil)
	run(t, a)
	run(t, b)
	eventually(t, "a live", func() bool { return modeOf(a) == modeLive })
	eventually(t, "b live", func() bool { return modeOf(b) == modeLive })
	// Each replica's start-up write (its heartbeat) lands first: one still
	// in flight would take an injected conflict meant for a's spend, which
	// would then land (round 21, k8s P3-1: 26 runs in 200 under -race).
	eventually(t, "the start-up writes landed", func() bool { return startedUp(a) && startedUp(b) })
	api.set(func(x *apiServer) { x.conflicts = maxAttempts })
	if err := a.SpendCode(t.Context(), "code-x", uh("mallory"), t0.Add(time.Minute)); err == nil {
		t.Fatal("landed")
	}
	for i := 2; i < 6; i++ {
		if err := b.AddGrant(testGrant(i, "bob", t0)); err != nil {
			t.Fatal(err)
		}
		want := grantID(i)
		eventually(t, "a merges b's write", func() bool { _, ok := a.Grant(want); return ok })
	}
	eventually(t, "a live again", func() bool { return modeOf(a) == modeLive })
	clock.Add(staleGrace + time.Second)
	if !a.GrantsUsable() {
		t.Errorf("a healthy replica refuses every MCP grant after one exhausted write (mode %s)", modeOf(a))
	}
}

// startedUp reports whether a running replica has nothing left to write:
// its heartbeat is in the ConfigMap and every local change written.
func startedUp(s *Store) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ownBeat != "" && s.gen == s.written && s.base != nil && s.inSyncLocked()
}

// Two Gets older than the base are a rewound revision only in a row: a
// newer ConfigMap merged between them (the watch) ends the run, so two
// unrelated races hours apart do not make an older ConfigMap
// authoritative (round 17, P3).
func TestANewerMergeEndsARunOfOlderGets(t *testing.T) {
	clock := &testClock{now: t0}
	s := newStore(nil, clock, nil)
	at := func(rv string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "console-state", UID: "uid-1", ResourceVersion: rv}}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mergeLocked(at("10"))
	s.mergeReadLocked(at("8"))  // a Get overtaken by the watch
	s.mergeLocked(at("20"))     // the watch, hours later
	s.mergeReadLocked(at("18")) // another Get overtaken
	if s.base.ResourceVersion != "20" {
		t.Errorf("base at %s, want 20: two unrelated older Gets were taken as a rewound revision", s.base.ResourceVersion)
	}
	s.mergeReadLocked(at("19"))
	if s.base.ResourceVersion != "19" {
		t.Errorf("base at %s, want 19: two older Gets in a row are a rewound revision", s.base.ResourceVersion)
	}
}

// A revocation made while the ConfigMap is missing is ErrNoConfigMap
// (every replica refuses every grant meanwhile), one while it is not
// writable ErrNotRecorded; the first is written within about a second of
// the ConfigMap's return, with no routine batch to wait for.
func TestRevocationsWhileTheConfigMapIsGone(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	a := New(Options{
		Client: api, Name: "console-state", Now: clock.Now,
		Debounce: time.Hour, UrgentDebounce: time.Millisecond,
		Backoff: func(int) time.Duration { return 5 * time.Millisecond },
	})
	run(t, a)
	eventually(t, "live", func() bool { return modeOf(a) == modeLive })
	if err := a.AddGrant(testGrant(1, "alice", t0)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the grant written", func() bool { return api.data(KeyGrants) != "" })

	api.set(func(x *apiServer) { x.forbidden = true })
	a.mu.Lock()
	a.setModeLocked(modeForbidden, "")
	a.mu.Unlock()
	a.RevokeGrant(grantID(1), uh("alice"), t0, t0.Add(time.Hour))
	if err := a.WriteRevocations(t.Context(), grantID(1)); !errors.Is(err, ErrNotRecorded) {
		t.Errorf("forbidden: %v, want ErrNotRecorded", err)
	}

	// Deleted: the watch delivers it.
	api.mu.Lock()
	api.forbidden = false
	for _, w := range api.watchers {
		if !w.IsStopped() {
			w.Delete(api.cm.DeepCopy())
		}
	}
	api.cm = nil
	api.mu.Unlock()
	eventually(t, "missing", func() bool { return modeOf(a) == modeMissing })
	if a.GrantsUsable() {
		t.Error("grants usable while the ConfigMap is missing")
	}
	if err := a.WriteRevocations(t.Context(), grantID(1)); !errors.Is(err, ErrNoConfigMap) {
		t.Errorf("missing: %v, want ErrNoConfigMap", err)
	}
	// The merge of the new ConfigMap itself makes the write urgent, not
	// a flag left from the revocation.
	a.mu.Lock()
	a.urgent = false
	a.mu.Unlock()
	api.mu.Lock()
	api.bumpLocked(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "console-state", Namespace: "team-a", UID: "uid-2"}})
	api.mu.Unlock()
	eventually(t, "the revocation written to the new ConfigMap", func() bool {
		return docGrants(t, api)[grantID(1)].Revoked > 0
	})
}
