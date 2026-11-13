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
	"slices"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
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

func startReplica(t *testing.T, cs kubernetes.Interface, name, ip string) *replica {
	t.Helper()
	store := sharedstate.New(sharedstate.Options{Client: cs.CoreV1().ConfigMaps(ns), Name: "console-state"})
	l := mcpleader.New(mcpleader.Options{
		Leases: cs.CoordinationV1().Leases(ns), Slices: cs.DiscoveryV1().EndpointSlices(ns), Grants: store,
		Namespace: ns, Lease: "console-mcp", SliceNames: []string{"console-mcp-ipv4"}, PodIP: ip, Port: 3000,
		Identity: name,
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

// Two replicas on a real API server: A leads, publishes its address and
// records a grant; A stops (a graceful hand-over) and B leads within
// about the election's retry period, the slice names B alone, and B holds
// A's grant.
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
	_, err = cs.DiscoveryV1().EndpointSlices(ns).Create(ctx, &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "console-mcp-ipv4", Labels: map[string]string{
			discoveryv1.LabelServiceName: "console-mcp", discoveryv1.LabelManagedBy: mcpleader.ManagedBy,
		}},
		AddressType: discoveryv1.AddressTypeIPv4,
	}, metav1.CreateOptions{})
	must(err)
	addresses := func() []string {
		s, err := cs.DiscoveryV1().EndpointSlices(ns).Get(ctx, "console-mcp-ipv4", metav1.GetOptions{})
		if err != nil {
			return nil
		}
		var out []string
		for _, e := range s.Endpoints {
			out = append(out, e.Addresses...)
		}
		return out
	}

	a := startReplica(t, cs, "console-a", "10.0.0.1")
	waitFor(t, "A to lead", 20*time.Second, a.leader.Leading)
	waitFor(t, "the slice to name A", 5*time.Second, func() bool { return slices.Equal(addresses(), []string{"10.0.0.1"}) })
	b := startReplica(t, cs, "console-b", "10.0.0.2")

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
	waitFor(t, "the slice to name B", 5*time.Second, func() bool { return slices.Equal(addresses(), []string{"10.0.0.2"}) })
	if a.leader.Leading() {
		t.Fatal("A still serves")
	}
	if g, ok := b.store.Grant(id); !ok || g.Client != "Claude Code" {
		t.Fatalf("B's index lacks A's grant: %+v %v", g, ok)
	}
}
