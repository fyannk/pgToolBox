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

package server

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/fyannk/pgToolBox/internal/console/sharedstate"
)

// While the console's OAuth server serves MCP grants from the shared
// state, /readyz waits until the ConfigMap was read once, or is known
// missing: a pod just started takes no MCP traffic before it knows the
// grants and revocations the other replicas recorded (round 18, P3).
func TestReadinessWaitsForTheSharedState(t *testing.T) {
	store := func(t *testing.T, objects ...*corev1.ConfigMap) *sharedstate.Store {
		t.Helper()
		cs := fake.NewClientset()
		for _, cm := range objects {
			if _, err := cs.CoreV1().ConfigMaps("team-a").Create(t.Context(), cm, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		return sharedstate.New(sharedstate.Options{
			Client: cs.CoreV1().ConfigMaps("team-a"), Name: "console-state",
			Backoff: func(int) time.Duration { return 5 * time.Millisecond },
		})
	}
	run := func(t *testing.T, s *sharedstate.Store) {
		t.Helper()
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { _ = s.Run(ctx); close(done) }()
		t.Cleanup(func() { cancel(); <-done })
	}
	settled := func(t *testing.T, check func() error) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); check() != nil; {
			if time.Now().After(deadline) {
				t.Fatalf("still not ready: %v", check())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	t.Run("not read yet", func(t *testing.T) {
		check := sharedStateReady(store(t), true)
		if check == nil || check() == nil {
			t.Fatal("ready before the shared state was read")
		}
	})
	t.Run("read", func(t *testing.T) {
		s := store(t, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "console-state", Namespace: "team-a"}})
		check := sharedStateReady(s, true)
		run(t, s)
		settled(t, check)
	})
	t.Run("missing", func(t *testing.T) {
		s := store(t)
		check := sharedStateReady(s, true)
		run(t, s)
		settled(t, check)
	})
	t.Run("no OAuth server", func(t *testing.T) {
		if sharedStateReady(store(t), false) != nil {
			t.Error("a readiness check without the OAuth server")
		}
	})
	t.Run("memory index", func(t *testing.T) {
		if sharedStateReady(sharedstate.New(sharedstate.Options{}), true) != nil {
			t.Error("a readiness check of an index that is this replica's memory")
		}
	})
}
