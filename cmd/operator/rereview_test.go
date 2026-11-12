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

package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fyannk/pgToolBox/internal/operator/managerrole"
)

// granting is the rules that grant reReviewRequired.
var granting = []authorizationv1.ResourceRule{{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"list"}}}

// reReviewRequired is what the re-review asks for in these tests.
var reReviewRequired = []managerrole.Permission{{Group: "apps", Resource: "deployments", Verbs: []string{"list"}}}

// scriptedReviewer answers each namespace's n-th rules review (from 1)
// with answer: the rules granted, or an error.
type scriptedReviewer struct {
	mu     sync.Mutex
	calls  map[string]int
	answer func(namespace string, n int) ([]authorizationv1.ResourceRule, error)
}

func (r *scriptedReviewer) Create(_ context.Context, obj client.Object, _ ...client.CreateOption) error {
	review, ok := obj.(*authorizationv1.SelfSubjectRulesReview)
	if !ok {
		return errors.New("unexpected object")
	}
	r.mu.Lock()
	if r.calls == nil {
		r.calls = map[string]int{}
	}
	r.calls[review.Spec.Namespace]++
	n := r.calls[review.Spec.Namespace]
	answer := r.answer
	r.mu.Unlock()
	rules, err := answer(review.Spec.Namespace, n)
	if err != nil {
		return err
	}
	review.Status.ResourceRules = rules
	return nil
}

func (r *scriptedReviewer) callsOf(namespace string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[namespace]
}

func (r *scriptedReviewer) set(answer func(namespace string, n int) ([]authorizationv1.ResourceRule, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answer = answer
}

// fakeUptime is a clock whose uptime the test sets.
type fakeUptime struct {
	started time.Time
	uptime  atomic.Int64
}

func (c *fakeUptime) now() time.Time { return c.started.Add(time.Duration(c.uptime.Load())) }

// testPolicy is the default policy on a fast ticker and clock, its jitter
// counted.
func testPolicy(clock *fakeUptime, jitters *atomic.Int32) reReviewPolicy {
	return reReviewPolicy{
		interval: time.Millisecond, passes: reReviewPasses, minUptime: restartMinUptime,
		started: clock.started, now: clock.now,
		jitter: func() time.Duration { jitters.Add(1); return time.Millisecond },
	}
}

// runReReview runs reReviewFailed in the background; the restarts land in
// the channel returned, and done closes when it returns.
func runReReview(t *testing.T, api managerrole.Creator, dropped []droppedNamespace, policy reReviewPolicy) (restarts chan error, done chan struct{}) {
	t.Helper()
	return runReReviewGauged(t, api, dropped, droppedGauge{}, policy)
}

// runReReviewGauged is runReReview moving gauge.
func runReReviewGauged(t *testing.T, api managerrole.Creator, dropped []droppedNamespace, gauge droppedGauge, policy reReviewPolicy) (restarts chan error, done chan struct{}) {
	t.Helper()
	restarts, done = make(chan error, 4), make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() {
		cancel()
		<-done
	})
	go func() {
		reReviewFailed(ctx, api, dropped, gauge, policy, func(cause error) { restarts <- cause })
		close(done)
	}()
	return restarts, done
}

// waitCalls waits until namespace was reviewed at least n times.
func waitCalls(t *testing.T, r *scriptedReviewer, namespace string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for r.callsOf(namespace) < n {
		if time.Now().After(deadline) {
			t.Fatalf("%s reviewed %d times, want at least %d", namespace, r.callsOf(namespace), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func noRestart(t *testing.T, restarts chan error, why string) {
	t.Helper()
	select {
	case cause := <-restarts:
		t.Fatalf("restarted %s: %v", why, cause)
	default:
	}
}

func oneRestart(t *testing.T, restarts chan error, done chan struct{}, naming string) {
	t.Helper()
	select {
	case cause := <-restarts:
		if !errors.Is(cause, errRestartToWatch) || !strings.Contains(cause.Error(), naming) {
			t.Errorf("restart cause = %v, want errRestartToWatch naming %s", cause, naming)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no restart once %s passed twice in a row", naming)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the re-review went on after the restart")
	}
}

var refused = apierrors.NewForbidden(schema.GroupResource{Group: "authorization.k8s.io", Resource: "selfsubjectrulesreviews"}, "", errors.New("refused"))

// TestReReviewFailed: round 16, K8S P2-2, and round 17. A namespace left
// out because its review failed is reviewed again in the background, alone
// (never a namespace left out for another cause, too-large included):
// while it fails, or lacks a permission, nothing happens; once it grants
// every permission twice in a row, the operator is restarted once, after
// the jitter, naming it.
func TestReReviewFailed(t *testing.T) {
	dropped := []droppedNamespace{
		{Namespace: "team-b", Cause: dropReviewFailed, Reason: "no answer", required: reReviewRequired},
		{Namespace: "team-c", Cause: dropPermissions, Reason: "narrowed", required: reReviewRequired},
		{Namespace: "team-d", Cause: dropClustersUnlistable, Reason: "forbidden"},
		{Namespace: "team-e", Cause: dropReviewTooLarge, Reason: "too large"},
	}
	r := &scriptedReviewer{answer: func(string, int) ([]authorizationv1.ResourceRule, error) { return nil, refused }}
	clock := &fakeUptime{started: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)}
	clock.uptime.Store(int64(time.Hour))
	var jitters atomic.Int32
	restarts, done := runReReview(t, r, dropped, testPolicy(clock, &jitters))
	waitCalls(t, r, "team-b", 3)
	noRestart(t, restarts, "while team-b still fails")
	// Reviewable but lacking the permission: no restart.
	r.set(func(string, int) ([]authorizationv1.ResourceRule, error) { return nil, nil })
	waitCalls(t, r, "team-b", r.callsOf("team-b")+3)
	noRestart(t, restarts, "while team-b lacks a permission")
	// Permitted: one restart, naming team-b, and the watch ends.
	r.set(func(string, int) ([]authorizationv1.ResourceRule, error) { return granting, nil })
	oneRestart(t, restarts, done, "team-b")
	if jitters.Load() != 1 {
		t.Errorf("jitter drawn %d times, want 1", jitters.Load())
	}
	for _, other := range []string{"team-c", "team-d", "team-e"} {
		if n := r.callsOf(other); n != 0 {
			t.Errorf("%s, dropped for another cause, was reviewed %d times", other, n)
		}
	}
}

// TestReReviewTakingTurns: round 17, OPERATOR and SECURITY P2. Two
// namespaces that pass and fail in turn (a tenant toggling the bindings)
// never pass twice in a row each: no restart, however long it goes on.
// One that then passes twice in a row restarts the operator once.
func TestReReviewTakingTurns(t *testing.T) {
	dropped := []droppedNamespace{
		{Namespace: "team-a", Cause: dropReviewFailed, Reason: "no answer", required: reReviewRequired},
		{Namespace: "team-b", Cause: dropReviewFailed, Reason: "no answer", required: reReviewRequired},
	}
	turns := func(namespace string, n int) ([]authorizationv1.ResourceRule, error) {
		// team-a passes on odd rounds, team-b on even ones.
		if (n%2 == 1) == (namespace == "team-a") {
			return granting, nil
		}
		return nil, nil
	}
	r := &scriptedReviewer{answer: turns}
	clock := &fakeUptime{started: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)}
	clock.uptime.Store(int64(time.Hour))
	var jitters atomic.Int32
	restarts, done := runReReview(t, r, dropped, testPolicy(clock, &jitters))
	waitCalls(t, r, "team-a", 40)
	waitCalls(t, r, "team-b", 40)
	noRestart(t, restarts, "while the namespaces take turns")
	r.set(func(namespace string, n int) ([]authorizationv1.ResourceRule, error) {
		if namespace == "team-a" {
			return granting, nil
		}
		return turns(namespace, n)
	})
	oneRestart(t, restarts, done, "team-a")
}

// TestReReviewWaitsForUptime: round 17, OPERATOR P2. A namespace that
// passes every review restarts nothing before the process has run
// restartMinUptime (kubelet then resets its restart back-off); it does
// once the uptime is reached.
func TestReReviewWaitsForUptime(t *testing.T) {
	dropped := []droppedNamespace{{Namespace: "team-b", Cause: dropReviewFailed, Reason: "no answer", required: reReviewRequired}}
	r := &scriptedReviewer{answer: func(string, int) ([]authorizationv1.ResourceRule, error) { return granting, nil }}
	clock := &fakeUptime{started: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)}
	clock.uptime.Store(int64(restartMinUptime - time.Second))
	var jitters atomic.Int32
	restarts, done := runReReview(t, r, dropped, testPolicy(clock, &jitters))
	waitCalls(t, r, "team-b", 20)
	noRestart(t, restarts, "before the process ran restartMinUptime")
	clock.uptime.Store(int64(restartMinUptime))
	oneRestart(t, restarts, done, "team-b")
}

// TestReReviewTooLargeNeverRestarts: round 17, OPERATOR and SECURITY P2,
// and round 18. A namespace whose review becomes too large or too slow
// leaves the background review (and moves in the gauge), and passing
// afterwards restarts nothing; one dropped as too large or too slow at
// start is never reviewed, so the re-review returns at once.
func TestReReviewTooLargeNeverRestarts(t *testing.T) {
	clock := &fakeUptime{started: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)}
	clock.uptime.Store(int64(time.Hour))
	var jitters atomic.Int32
	t.Run("too large or too slow at start: never reviewed", func(t *testing.T) {
		r := &scriptedReviewer{answer: func(string, int) ([]authorizationv1.ResourceRule, error) { return granting, nil }}
		restarts, done := runReReview(t, r, []droppedNamespace{
			{Namespace: "team-e", Cause: dropReviewTooLarge, Reason: "too large"},
			{Namespace: "team-f", Cause: dropReviewTooSlow, Reason: "too slow"},
		}, testPolicy(clock, &jitters))
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the re-review of a too-large namespace did not return")
		}
		noRestart(t, restarts, "for a namespace too large or too slow at start")
		for _, namespace := range []string{"team-e", "team-f"} {
			if n := r.callsOf(namespace); n != 0 {
				t.Errorf("%s reviewed %d times, want 0", namespace, n)
			}
		}
	})
	for _, tc := range []struct {
		name  string
		err   error
		cause dropCause
	}{
		{"too large later: leaves the loop", managerrole.ErrReviewTooLarge, dropReviewTooLarge},
		// Round 18, OPERATOR and SECURITY P3: a tenant who makes the API
		// server build the response slowly, then removes the bindings.
		{"too slow later: leaves the loop", managerrole.ErrReviewTooSlow, dropReviewTooSlow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &scriptedReviewer{answer: func(_ string, n int) ([]authorizationv1.ResourceRule, error) {
				if n == 1 {
					return nil, tc.err
				}
				return granting, nil
			}}
			// Round 18, K8S P3: the gauge follows the namespace to its new
			// cause, so the alert no longer promises a restart for it.
			dropped := []droppedNamespace{
				{Namespace: "team-b", Cause: dropReviewFailed, Reason: "no answer", required: reReviewRequired},
				{Namespace: "team-c", Cause: dropPermissions, Reason: "narrowed", required: reReviewRequired},
			}
			registry := prometheus.NewRegistry()
			gauge, err := registerWatchMetrics(registry, countDropped(dropped), nil)
			if err != nil {
				t.Fatal(err)
			}
			restarts, done := runReReviewGauged(t, r, dropped, gauge, testPolicy(clock, &jitters))
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the re-review went on with no namespace left")
			}
			noRestart(t, restarts, "for a namespace whose review became "+string(tc.cause))
			if n := r.callsOf("team-b"); n != 1 {
				t.Errorf("team-b reviewed %d times, want 1", n)
			}
			gauges := gatherGauges(t, registry)
			for _, cause := range dropCauses {
				want := 0.0
				if cause == tc.cause || cause == dropPermissions {
					want = 1
				}
				if got := gauges[`pgtoolbox_watch_namespaces_dropped{reason="`+string(cause)+`"}`]; got != want {
					t.Errorf("pgtoolbox_watch_namespaces_dropped{reason=%s} = %v, want %v", cause, got, want)
				}
			}
		})
	}
}

// TestReReviewFailedNothingToDo: no namespace failed its review: the
// re-review returns at once.
func TestReReviewFailedNothingToDo(t *testing.T) {
	done := make(chan struct{})
	go func() {
		reReviewFailed(t.Context(), &scriptedReviewer{}, []droppedNamespace{{Namespace: "team-c", Cause: dropPermissions}}, droppedGauge{}, defaultReReviewPolicy(time.Now()),
			func(error) { t.Error("restarted with nothing to review") })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the re-review did not return")
	}
}

// TestRestartJitter: the pause before a restart stays within
// restartMaxJitter, and the default policy is the one documented.
func TestRestartJitter(t *testing.T) {
	for range 1000 {
		if j := restartJitter(); j < 0 || j >= restartMaxJitter {
			t.Fatalf("jitter %s outside [0, %s)", j, restartMaxJitter)
		}
	}
	p := defaultReReviewPolicy(time.Now())
	if p.passes != 2 || p.minUptime < 10*time.Minute || p.interval != time.Minute {
		t.Errorf("default policy = %+v, want 2 passes, at least 10 minutes of uptime, a one-minute interval", p)
	}
}
