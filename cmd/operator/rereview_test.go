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
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fyannk/pgToolBox/internal/operator/managerrole"
)

// healingReviewer refuses every rules review with a permanent error until
// healed, then grants granted.
type healingReviewer struct {
	mu      sync.Mutex
	healed  bool
	granted []authorizationv1.ResourceRule
	calls   map[string]int
}

func (r *healingReviewer) Create(_ context.Context, obj client.Object, _ ...client.CreateOption) error {
	review, ok := obj.(*authorizationv1.SelfSubjectRulesReview)
	if !ok {
		return errors.New("unexpected object")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls == nil {
		r.calls = map[string]int{}
	}
	r.calls[review.Spec.Namespace]++
	if !r.healed {
		return managerrole.ErrReviewTooLarge
	}
	review.Status.ResourceRules = r.granted
	return nil
}

func (r *healingReviewer) heal(granted []authorizationv1.ResourceRule) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.healed, r.granted = true, granted
}

// TestReReviewFailed: round 16, K8S P2-2. A namespace left out because its
// review failed is reviewed again in the background, alone (never a
// namespace left out for another cause): while it fails, or lacks a
// permission, nothing happens; once it grants every permission, the
// operator is restarted once, naming it.
func TestReReviewFailed(t *testing.T) {
	required := []managerrole.Permission{{Group: "apps", Resource: "deployments", Verbs: []string{"list"}}}
	dropped := []droppedNamespace{
		{Namespace: "team-b", Cause: dropReviewFailed, Reason: "too large", required: required},
		{Namespace: "team-c", Cause: dropPermissions, Reason: "narrowed", required: required},
		{Namespace: "team-d", Cause: dropClustersUnlistable, Reason: "forbidden"},
	}
	c := &healingReviewer{}
	restarts := make(chan error, 2)
	restart := func(cause error) { restarts <- cause }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		reReviewFailed(ctx, c, dropped, 10*time.Millisecond, restart)
		close(done)
	}()
	// Still failing: reviewed again, no restart.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		calls := c.calls["team-b"]
		c.mu.Unlock()
		if calls >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("team-b was not reviewed again")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case cause := <-restarts:
		t.Fatalf("restarted while team-b still fails: %v", cause)
	default:
	}
	// Reviewable but lacking the permission: no restart.
	c.heal(nil)
	time.Sleep(50 * time.Millisecond)
	select {
	case cause := <-restarts:
		t.Fatalf("restarted while team-b lacks a permission: %v", cause)
	default:
	}
	// Permitted: one restart, naming team-b, and the watch ends.
	c.heal([]authorizationv1.ResourceRule{{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"list"}}})
	select {
	case cause := <-restarts:
		if !errors.Is(cause, errRestartToWatch) || !strings.Contains(cause.Error(), "team-b") {
			t.Errorf("restart cause = %v, want errRestartToWatch naming team-b", cause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no restart once team-b grants every permission")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the re-review went on after the restart")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls["team-c"] != 0 || c.calls["team-d"] != 0 {
		t.Errorf("namespaces dropped for another cause were reviewed: %v", c.calls)
	}
}

// TestReReviewFailedNothingToDo: no namespace failed its review: the
// re-review returns at once.
func TestReReviewFailedNothingToDo(t *testing.T) {
	done := make(chan struct{})
	go func() {
		reReviewFailed(t.Context(), &healingReviewer{}, []droppedNamespace{{Namespace: "team-c", Cause: dropPermissions}}, time.Millisecond,
			func(error) { t.Error("restarted with nothing to review") })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the re-review did not return")
	}
}
