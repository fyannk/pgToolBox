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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// hookClient is the API server with a hook run in place of the next
// Update: what happens on the API server while a write is in flight.
type hookClient struct {
	*apiServer
	beforeUpdate func() error
}

func (h *hookClient) Update(ctx context.Context, cm *corev1.ConfigMap, opts metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	if hook := h.beforeUpdate; hook != nil {
		h.beforeUpdate = nil
		if err := hook(); err != nil {
			return nil, err
		}
	}
	return h.apiServer.Update(ctx, cm, opts)
}

// watchMerge is s's watch delivering the ConfigMap as it now is.
func watchMerge(t *testing.T, api *apiServer, s *Store) {
	t.Helper()
	cm, err := api.Get(t.Context(), "console-state", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.mergeLocked(cm)
	s.mu.Unlock()
}

// A write-through that fails (the caller gone, the API server not
// answering) after this replica's watch merged another replica's same
// spend or mark rolls back only what the ConfigMap does not hold: the
// other replica's spend or mark survives this replica's next write, and
// a third replica refuses the value. Otherwise a confirmation would run
// twice.
func TestAFailedWriteThroughKeepsWhatAnotherReplicaWrote(t *testing.T) {
	until := t0.Add(time.Minute)
	alice := uh("alice")
	for _, tc := range []struct {
		name string
		// prepare spends what the step needs on a, before b loads.
		prepare func(t *testing.T, a *Store)
		// step is the write-through raced, on a then on b.
		step func(ctx context.Context, s *Store) error
		held func(api *apiServer) bool
	}{
		{
			name:    "SpendCode",
			prepare: func(*testing.T, *Store) {},
			step:    func(ctx context.Context, s *Store) error { return s.SpendCode(ctx, "confirm-1", alice, until) },
			held:    func(api *apiServer) bool { return api.holds("confirm-1") },
		},
		{
			name: "MarkCode",
			prepare: func(t *testing.T, a *Store) {
				if err := a.SpendCode(t.Context(), "confirm-1", alice, until); err != nil {
					t.Fatal(err)
				}
			},
			step: func(ctx context.Context, s *Store) error { return s.MarkCode(ctx, "confirm-1", "run", alice) },
			held: func(api *apiServer) bool { return api.marked("confirm-1", "run") },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &testClock{now: t0}
			api := newAPIServer()
			a := loaded(t, api, clock)
			tc.prepare(t, a)
			hc := &hookClient{apiServer: api}
			b := newStore(hc, clock, nil)
			watchMerge(t, api, b)
			b.mu.Lock()
			b.watchOpenedLocked()
			b.mu.Unlock()
			hc.beforeUpdate = func() error {
				// a's same step lands; b's watch delivers it; b's own
				// write then fails without a conflict.
				if err := tc.step(t.Context(), a); err != nil {
					t.Errorf("a: %v", err)
				}
				watchMerge(t, api, b)
				return context.Canceled
			}
			if err := tc.step(t.Context(), b); !errors.Is(err, ErrNotRecorded) {
				t.Fatalf("b's failed write: %v", err)
			}
			if !tc.held(api) {
				t.Fatal("the test needs a's write in the ConfigMap")
			}
			b.mu.Lock()
			b.watchOpenedLocked()
			b.mu.Unlock()
			// b's next write (anything else changing) must keep a's.
			b.InstanceSince("pod", "ok", t0)
			if err := b.flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !tc.held(api) {
				t.Fatal("b's next write erased what a recorded")
			}
			c := loaded(t, api, clock)
			if err := tc.step(t.Context(), c); !errors.Is(err, ErrSpent) {
				t.Errorf("a third replica: %v, want ErrSpent (the step would run twice)", err)
			}
		})
	}
}
