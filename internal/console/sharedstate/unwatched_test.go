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
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// hangGet: Get blocks (an API server that queues the read in its flow
// control, a dial into a partition) once hang is set, until the caller
// gives up.
type hangGet struct {
	*watchControl
	hang atomic.Bool
}

func (h *hangGet) Get(ctx context.Context, name string, o metav1.GetOptions) (*corev1.ConfigMap, error) {
	if h.hang.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return h.watchControl.Get(ctx, name, o)
}

// grantOnTwoReplicas sets up b, a replica that records a grant on api
// (revoke has it revoke the grant later).
func grantOnTwoReplicas(t *testing.T, api *apiServer, clock *testClock) (b *Store, id string) {
	t.Helper()
	b = loaded(t, api, clock)
	id = grantID(9)
	if err := b.AddGrant(Grant{ID: id, UserHash: uh("alice"), Client: "c", Created: t0, Expiry: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	flushed(t, b)
	return b, id
}

func revoke(t *testing.T, b *Store, clock *testClock, id string) {
	t.Helper()
	b.RevokeGrant(id, uh("alice"), clock.Now(), t0.Add(time.Hour))
	if err := b.WriteRevocations(t.Context(), id); err != nil {
		t.Fatal(err)
	}
}

// The watch ends (closed by the API server, or a lost connection found
// by its ping) and the read that reopens it does not answer: the replica
// serves grants for staleGrace from the end of the watch, then refuses
// them, so a revocation another replica wrote is not served past it
// (round 19, Go P2-1 = security P2-1; the reviewers' probes
// TestR19LiveAfterTheWatchEnds and TestR19ClosedWatchThenHungGetStaysLive,
// which saw the revoked grant usable 2 and 10 minutes on).
func TestAnEndedWatchStartsTheGrace(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	b, id := grantOnTwoReplicas(t, api, clock)
	wc := &watchControl{apiServer: api}
	h := &hangGet{watchControl: wc}
	a := controlled(h, clock, 10*time.Millisecond)
	run(t, a)
	eventually(t, "a live", func() bool { return modeOf(a) == modeLive && wc.count() == 1 })
	eventually(t, "a knows the grant", func() bool { _, ok := a.Grant(id); return ok })

	h.hang.Store(true)
	wc.mu.Lock()
	wc.last.Stop()
	wc.mu.Unlock()
	eventually(t, "a not watching", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return !a.watching })
	revoke(t, b, clock, id)
	// A watch being reopened: still usable within staleGrace.
	clock.Add(staleGrace - time.Second)
	if !a.GrantsUsable() {
		t.Fatalf("a: mode %s, grants refused %s after the watch ended, want usable within staleGrace", modeOf(a), staleGrace-time.Second)
	}
	clock.Add(time.Second)
	for i := range 4 {
		if a.GrantsUsable() && !a.GrantRevoked(id) {
			t.Fatalf("a: mode %s, the revoked grant usable %s after the watch ended", modeOf(a), staleGrace+time.Duration(i)*time.Minute)
		}
		clock.Add(time.Minute)
		time.Sleep(20 * time.Millisecond)
	}
}

// A replica live with no watch that then fails (the hung read gives up,
// or answers 503) runs the failing mode's staleGrace from the end of the
// watch, not from the failure: the time spent reopening it counts.
func TestAFailureAfterTheWatchEndedKeepsItsStart(t *testing.T) {
	clock := &testClock{now: t0}
	s := newStore(newAPIServer(), clock, nil)
	s.mu.Lock()
	s.watchOpenedLocked()
	s.watching, s.unwatchedAt = false, t0
	s.mu.Unlock()
	clock.Add(20 * time.Second)
	_ = s.failed(apierrors.NewServiceUnavailable("overloaded"))
	if m := modeOf(s); m != modeFailing {
		t.Fatalf("mode %s, want failing", m)
	}
	if s.since != t0 {
		t.Errorf("failing since %s, want %s (the end of the watch)", s.since, t0)
	}
	clock.Add(staleGrace - 20*time.Second)
	if s.GrantsUsable() {
		t.Errorf("grants usable %s after the watch ended", staleGrace)
	}
}

// The read that reopens a watch has writeTimeout to answer: one that
// hangs ends as a failure (failing mode), not a live mode with no watch
// that lasts as long as the API server is silent.
func TestAHungReadEndsAsAFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("waits writeTimeout")
	}
	clock := &testClock{now: t0}
	api := newAPIServer()
	wc := &watchControl{apiServer: api}
	h := &hangGet{watchControl: wc}
	a := controlled(h, clock, 10*time.Millisecond)
	run(t, a)
	eventually(t, "a live", func() bool { return modeOf(a) == modeLive })
	h.hang.Store(true)
	wc.mu.Lock()
	wc.last.Stop()
	wc.mu.Unlock()
	deadline := time.Now().Add(writeTimeout + 5*time.Second)
	for modeOf(a) != modeFailing {
		if time.Now().After(deadline) {
			t.Fatalf("mode %s %s after the read hung, want failing", modeOf(a), writeTimeout+5*time.Second)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// deafWatch opens watches the API server never feeds once deaf is set.
type deafWatch struct {
	*apiServer
	deaf  atomic.Bool
	opens atomic.Int32
}

func (d *deafWatch) Watch(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
	d.opens.Add(1)
	if !o.AllowWatchBookmarks {
		return nil, errors.New("the watch asks for no bookmarks")
	}
	if d.deaf.Load() {
		return watch.NewRaceFreeFake(), nil
	}
	return d.apiServer.Watch(ctx, o)
}

// An open watch that delivers nothing, not even a bookmark, for
// WatchIdle is opened again, with a read: a revocation written elsewhere
// is seen, rather than missed for as long as the API server keeps the
// deaf watch open (round 19, security P3-1; the reviewer's probe
// TestR19DeafWatchStaysLive).
func TestADeafWatchIsReopened(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	b, id := grantOnTwoReplicas(t, api, clock)
	d := &deafWatch{apiServer: api}
	d.deaf.Store(true)
	a := New(Options{
		Client: d, Name: "console-state", Now: clock.Now, Logger: slog.New(slog.DiscardHandler),
		Debounce: 5 * time.Millisecond, UrgentDebounce: time.Millisecond,
		Backoff:   func(int) time.Duration { return 10 * time.Millisecond },
		WatchIdle: 100 * time.Millisecond,
	})
	run(t, a)
	eventually(t, "a live", func() bool { return modeOf(a) == modeLive })
	eventually(t, "a knows the grant", func() bool { _, ok := a.Grant(id); return ok })
	revoke(t, b, clock, id)
	eventually(t, "a sees the revocation through a deaf watch", func() bool { return a.GrantRevoked(id) })
	if d.opens.Load() < 2 {
		t.Errorf("%d watches opened, want the deaf one reopened", d.opens.Load())
	}
}

// A watch the API server closes and that reopens at once is a routine
// rotation: no "unreachable" line in the log, and grants served
// throughout.
func TestAWatchRotationIsQuiet(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	wc := &watchControl{apiServer: api}
	var mu sync.Mutex
	var logs bytes.Buffer
	a := New(Options{
		Client: wc, Name: "console-state", Now: clock.Now,
		Logger:   slog.New(slog.NewTextHandler(syncWriter{&mu, &logs}, nil)),
		Debounce: 5 * time.Millisecond, UrgentDebounce: time.Millisecond,
		Backoff: func(int) time.Duration { return 5 * time.Millisecond },
	})
	run(t, a)
	eventually(t, "a live", func() bool { return modeOf(a) == modeLive && wc.count() == 1 })
	for n := 2; n <= 4; n++ {
		wc.mu.Lock()
		wc.last.Stop()
		wc.mu.Unlock()
		eventually(t, "the watch reopened", func() bool {
			a.mu.Lock()
			defer a.mu.Unlock()
			return a.watching && wc.count() == n
		})
		if !a.GrantsUsable() {
			t.Fatalf("grants refused after a routine rotation")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("a routine watch rotation logged a warning:\n%s", logs.String())
	}
}

// scriptedGet answers each Get with the error err returns (nil: the
// API server's answer).
type scriptedGet struct {
	*apiServer
	mu   sync.Mutex
	err  error
	gets int
}

func (g *scriptedGet) Get(ctx context.Context, name string, o metav1.GetOptions) (*corev1.ConfigMap, error) {
	g.mu.Lock()
	err := g.err
	g.gets++
	g.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return g.apiServer.Get(ctx, name, o)
}

func (g *scriptedGet) answer(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.err = err
}

// The readiness check (MarksKnown) does not go back to false once the API
// server has answered: a ConfigMap missing or forbidden, then a transient
// failure (a 503, a 429, a timeout), keeps the replica ready. Before, all
// replicas failed readiness at once and the console lost its endpoints
// (round 19, k8s P2-1).
func TestReadinessIsSticky(t *testing.T) {
	for name, first := range map[string]error{
		"missing":   apierrors.NewNotFound(configMaps, "console-state"),
		"forbidden": apierrors.NewForbidden(configMaps, "console-state", errors.New("no")),
		"read":      nil,
	} {
		t.Run(name, func(t *testing.T) {
			clock := &testClock{now: t0}
			g := &scriptedGet{apiServer: newAPIServer(), err: first}
			a := controlled(g, clock, 5*time.Millisecond)
			if a.MarksKnown() {
				t.Fatal("known before any read")
			}
			run(t, a)
			eventually(t, "known", a.MarksKnown)
			g.answer(apierrors.NewServiceUnavailable("overloaded"))
			if name == "read" {
				// A read leaves the replica live with its watch open: end it
				// so that the failing read comes.
				g.apiServer.mu.Lock()
				for _, w := range g.apiServer.watchers {
					w.Stop()
				}
				g.apiServer.mu.Unlock()
			}
			eventually(t, "failing", func() bool { return modeOf(a) == modeFailing })
			if !a.MarksKnown() {
				t.Errorf("after %s then a 503: not known, the replica would fail readiness", name)
			}
		})
	}
}

// A run of 404s does not grow the backoff of the failure that follows:
// once a missing ConfigMap returns behind a transient failure, it is read
// again within the first backoff, not after a minute (round 19, k8s
// P2-1).
func TestAMissingConfigMapDoesNotGrowTheBackoff(t *testing.T) {
	clock := &testClock{now: t0}
	g := &scriptedGet{apiServer: newAPIServer(), err: apierrors.NewNotFound(configMaps, "console-state")}
	a := New(Options{
		Client: g, Name: "console-state", Now: clock.Now, Logger: slog.New(slog.DiscardHandler),
		Debounce: 5 * time.Millisecond, UrgentDebounce: time.Millisecond,
		Backoff: func(n int) time.Duration {
			if n > 1 {
				return time.Hour
			}
			return 5 * time.Millisecond
		},
	})
	run(t, a)
	eventually(t, "two reads of a missing ConfigMap", func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.gets >= 2
	})
	g.answer(apierrors.NewServiceUnavailable("overloaded"))
	eventually(t, "failing", func() bool { return modeOf(a) == modeFailing })
	g.answer(nil)
	eventually(t, "live again within the first backoff", func() bool { return modeOf(a) == modeLive })
}

// The missing ConfigMap is polled every second or two only while that
// matters (an urgent change held, the first minutes of the episode),
// else every ten seconds or so (round 19, k8s P3-1 = Go P3-3).
func TestTheMissingPollBacksOff(t *testing.T) {
	for name, tc := range map[string]struct {
		after  time.Duration
		held   bool
		fast   bool
		inMode mode
	}{
		"just missing":           {after: time.Minute, fast: true, inMode: modeMissing},
		"missing for long":       {after: 3 * time.Minute, inMode: modeMissing},
		"a revocation held":      {after: time.Hour, held: true, fast: true, inMode: modeMissing},
		"not missing, no poll":   {after: time.Hour, inMode: modeFailing},
		"routine change, long":   {after: 3 * time.Minute, inMode: modeMissing},
		"just missing, held too": {after: time.Second, held: true, fast: true, inMode: modeMissing},
	} {
		t.Run(name, func(t *testing.T) {
			clock := &testClock{now: t0}
			s := newStore(newAPIServer(), clock, nil)
			s.mu.Lock()
			s.setModeLocked(tc.inMode, "")
			switch {
			case tc.held:
				s.changedLocked(true)
			case name == "routine change, long":
				s.changedLocked(false)
			}
			s.mu.Unlock()
			clock.Add(tc.after)
			d, ok := s.missingWait()
			if ok != (tc.inMode == modeMissing) {
				t.Fatalf("missingWait polls: %v in mode %s", ok, tc.inMode)
			}
			if !ok {
				return
			}
			if fast := d < 2*missingPoll+time.Millisecond; fast != tc.fast {
				t.Errorf("wait %s, fast %v, want fast %v", d, fast, tc.fast)
			}
			if !tc.fast && d < missingSlowPoll {
				t.Errorf("wait %s, want at least %s", d, missingSlowPoll)
			}
		})
	}
}
