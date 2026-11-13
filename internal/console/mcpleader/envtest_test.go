//go:build envtest

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

package mcpleader_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/fyannk/pgToolBox/internal/console/mcpleader"
	"github.com/fyannk/pgToolBox/internal/console/sharedstate"
)

const ns = "team-a"

type replica struct {
	store  *sharedstate.Store
	leader *mcpleader.Leader
	cancel context.CancelFunc
	done   chan struct{}
}

func startReplica(t *testing.T, cs kubernetes.Interface, name string) *replica {
	t.Helper()
	store := sharedstate.New(sharedstate.Options{Client: cs.CoreV1().ConfigMaps(ns), Name: "console-state"})
	l := mcpleader.New(mcpleader.Options{
		Leases: cs.CoordinationV1().Leases(ns), Grants: store, Namespace: ns, Lease: "console-mcp", Identity: name,
	})
	store.SetLeader(l.Leading)
	ctx, cancel := context.WithCancel(context.Background())
	r := &replica{store: store, leader: l, cancel: cancel, done: make(chan struct{})}
	go func() { _ = store.Run(ctx) }()
	go func() { _ = l.Run(ctx); close(r.done) }()
	t.Cleanup(func() { cancel(); <-r.done })
	return r
}

func waitFor(t *testing.T, what string, within time.Duration, cond func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for !cond() {
		if time.Since(start) > within {
			t.Fatalf("timed out after %s waiting for %s", within, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return time.Since(start)
}

// Two replicas on a real API server: A leads and records a grant; A
// stops (a graceful hand-over) and B leads within about the election's
// retry period, the Lease names B, and B holds A's grant.
func TestHandOverOnEnvtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset; run through make test-envtest-console-mcpleader")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	cs := kubernetes.NewForConfigOrDie(cfg)
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// What the install pre-creates (PR 2 renders it).
	_, err = cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
	must(err)
	_, err = cs.CoordinationV1().Leases(ns).Create(ctx, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "console-mcp"}}, metav1.CreateOptions{})
	must(err)
	_, err = cs.CoreV1().ConfigMaps(ns).Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "console-state"}}, metav1.CreateOptions{})
	must(err)
	holder := func() string {
		l, err := cs.CoordinationV1().Leases(ns).Get(ctx, "console-mcp", metav1.GetOptions{})
		if err != nil || l.Spec.HolderIdentity == nil {
			return ""
		}
		return *l.Spec.HolderIdentity
	}

	a := startReplica(t, cs, "console-a")
	waitFor(t, "A to lead", 20*time.Second, a.leader.Leading)
	if holder() != "console-a" {
		t.Fatalf("the Lease names %q, want console-a", holder())
	}
	b := startReplica(t, cs, "console-b")

	id := "grant-" + strings.Repeat("ab", 16)
	now := time.Now().UTC()
	must(a.store.AddGrant(sharedstate.Grant{
		ID: id, UserHash: strings.Repeat("cd", 16), Client: "Claude Code", Scope: "mcp:read",
		Created: now, LastUsed: now, Expiry: now.Add(time.Hour),
	}))
	must(a.store.WriteGrant(ctx, id))
	if b.leader.Leading() || b.store.GrantsUsable() {
		t.Fatal("B serves while A leads")
	}

	a.cancel()
	<-a.done
	took := waitFor(t, "B to lead", 10*time.Second, b.leader.Leading)
	t.Logf("hand-over took %s", took)
	if holder() != "console-b" {
		t.Fatalf("the Lease names %q, want console-b", holder())
	}
	if a.leader.Leading() {
		t.Fatal("A still serves")
	}
	if g, ok := b.store.Grant(id); !ok || g.Client != "Claude Code" {
		t.Fatalf("B's index lacks A's grant: %+v %v", g, ok)
	}
}
