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

package console

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// grantedSet is the console's StatefulSet whose template carries the
// any-peer grant, its generation observed.
func grantedSet(c *pgtoolboxv1alpha1.PgToolBoxConsole, grant bool) *appsv1.StatefulSet {
	set := owned(c, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: c.WorkloadName(), Namespace: c.Namespace, Generation: 1}})
	if grant {
		set.Spec.Template.Annotations = map[string]string{annotationTrustsEveryPeer: "true"}
	}
	set.Status.ObservedGeneration = 1
	return set
}

// widened is a console whose from adds a peer to the ipBlock one, its
// desired policy, and the narrower policy applied before it.
func widened() (*pgtoolboxv1alpha1.PgToolBoxConsole, *networkingv1.NetworkPolicy, *networkingv1.NetworkPolicy) {
	c := ipBlockConsole("192.168.10.0/24")
	c.Spec.Exposure.NetworkPolicy.From = append(c.Spec.Exposure.NetworkPolicy.From, nginxSpecPeer)
	desired := buildExposureNetworkPolicy(c, true)
	narrow := owned(c, buildExposureNetworkPolicy(ipBlockConsole("192.168.10.0/24"), true))
	narrow.Name = desired.Name
	return c, desired, narrow
}

// TestHeldWideningStaleWideCache: the cache still shows an older, wider
// policy equal to the desired one, while the API server holds the
// narrower one this operator applied last, and the template trusts every
// peer: the widening is held (the round-8 probe TestR8ProbeStaleWideCache).
func TestHeldWideningStaleWideCache(t *testing.T) {
	t.Parallel()
	c, desired, narrow := widened()
	set := grantedSet(c, true)
	cache := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(owned(c, desired.DeepCopy()), set.DeepCopy()).Build()
	live := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(narrow.DeepCopy(), set.DeepCopy()).Build()
	r := &Reconciler{Client: cache, APIReader: live}
	held, _, err := r.heldWidening(context.Background(), c, desired)
	if err != nil || held == "" {
		t.Fatalf("heldWidening = %q, %v: the API server has the narrower policy and the template trusts every peer, want it held", held, err)
	}
}

// TestHeldWideningReadsTheWorkloadPastCache: the template rendered with
// the grant a moment ago is not in the cache yet (it shows the template
// without), and no Pod carries the grant yet: the widening is still held,
// as the API server's template carries it.
func TestHeldWideningReadsTheWorkloadPastCache(t *testing.T) {
	t.Parallel()
	c, desired, narrow := widened()
	cache := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(narrow.DeepCopy(), grantedSet(c, false)).Build()
	live := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(narrow.DeepCopy(), grantedSet(c, true)).Build()
	r := &Reconciler{Client: cache, APIReader: live}
	held, _, err := r.heldWidening(context.Background(), c, desired)
	if err != nil || held == "" {
		t.Fatalf("heldWidening = %q, %v with the grant in the API server's template, want it held", held, err)
	}
	// Neither carries it: the widening goes through.
	live = fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(narrow.DeepCopy(), grantedSet(c, false)).Build()
	r = &Reconciler{Client: cache, APIReader: live}
	if held, _, err = r.heldWidening(context.Background(), c, desired); err != nil || held != "" {
		t.Fatalf("heldWidening = %q, %v without any grant, want nothing held", held, err)
	}
}

// blockingNodeList is an APIReader whose Node lists wait for release (or
// their context) and count.
func blockingNodeList(t *testing.T, objects ...client.Object) (client.WithWatch, chan struct{}, chan struct{}, *atomic.Int32) {
	t.Helper()
	release, started := make(chan struct{}), make(chan struct{}, 16)
	var calls atomic.Int32
	cl := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.NodeList); ok {
				calls.Add(1)
				started <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return cl.List(ctx, list, opts...)
		}}).Build()
	return cl, release, started, &calls
}

// TestNodePodCIDRsSingleFlight: concurrent callers share one lookup, the
// memo mutex is not held across it, a waiter whose context ends returns at
// once, and a failed lookup (its caller's context ended) leaves the next
// to a waiter still live.
func TestNodePodCIDRsSingleFlight(t *testing.T) {
	t.Parallel()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Spec: corev1.NodeSpec{PodCIDRs: []string{"10.128.0.0/24"}}}

	t.Run("callers share one lookup", func(t *testing.T) {
		t.Parallel()
		cl, release, started, calls := blockingNodeList(t, node.DeepCopy())
		r := &Reconciler{Client: cl, APIReader: cl}
		var wg sync.WaitGroup
		results := make([]int, 5)
		for i := range results {
			wg.Go(func() {
				nodes, _, err := r.nodePodCIDRs(context.Background())
				if err != nil {
					t.Error(err)
				}
				results[i] = len(nodes)
			})
		}
		<-started
		// The memo is not locked while the lookup runs.
		locked := make(chan struct{})
		go func() { r.nodes.mu.Lock(); r.nodes.mu.Unlock(); close(locked) }() //nolint:staticcheck // probes the lock
		select {
		case <-locked:
		case <-time.After(5 * time.Second):
			t.Fatal("the memo mutex is held across the Node list")
		}
		close(release)
		wg.Wait()
		if got := calls.Load(); got != 1 {
			t.Fatalf("%d Node lists for 5 concurrent callers, want 1", got)
		}
		for i, n := range results {
			if n != 1 {
				t.Fatalf("caller %d got %d Nodes, want 1", i, n)
			}
		}
	})

	t.Run("a cancelled waiter returns at once", func(t *testing.T) {
		t.Parallel()
		cl, release, started, _ := blockingNodeList(t, node.DeepCopy())
		defer close(release)
		r := &Reconciler{Client: cl, APIReader: cl}
		go func() { _, _, _ = r.nodePodCIDRs(context.Background()) }()
		<-started
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		done := make(chan error, 1)
		go func() { _, _, err := r.nodePodCIDRs(ctx); done <- err }()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("a cancelled waiter returned %v, want its context's error", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a cancelled waiter waited for the lookup it does not run")
		}
	})

	t.Run("a failed lookup is not kept and a live waiter runs the next", func(t *testing.T) {
		t.Parallel()
		cl, release, started, calls := blockingNodeList(t, node.DeepCopy())
		r := &Reconciler{Client: cl, APIReader: cl}
		leader, cancel := context.WithCancel(context.Background())
		leaderDone := make(chan error, 1)
		go func() { _, _, err := r.nodePodCIDRs(leader); leaderDone <- err }()
		<-started
		waiterDone := make(chan int, 1)
		go func() {
			nodes, _, err := r.nodePodCIDRs(context.Background())
			if err != nil {
				t.Error(err)
			}
			waiterDone <- len(nodes)
		}()
		cancel()
		if err := <-leaderDone; !errors.Is(err, context.Canceled) {
			t.Fatalf("the leader returned %v, want its context's error", err)
		}
		<-started // the waiter's own lookup
		close(release)
		if n := <-waiterDone; n != 1 {
			t.Fatalf("the waiter got %d Nodes, want 1", n)
		}
		if got := calls.Load(); got != 2 {
			t.Fatalf("%d Node lists, want 2 (the failed one is not kept)", got)
		}
	})
}

// TestNodeCoverageNote: Nodes without spec.podCIDRs are a check that could
// not run, said in the ExposureConfined message, not one that passed.
func TestNodeCoverageNote(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name              string
		listed, withCIDRs int
		want              string
	}{
		{"every Node has a range", 3, 3, ""},
		{"no Node listed", 0, 0, ""},
		{"no Node has a range", 3, 0, "could not run"},
		{"some have none", 3, 1, "2 of the 3 Nodes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := nodeCoverageNote(tc.listed, tc.withCIDRs)
			if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
				t.Fatalf("nodeCoverageNote(%d, %d) = %q, want %q", tc.listed, tc.withCIDRs, got, tc.want)
			}
		})
	}

	c := ipBlockConsole("192.168.10.0/24")
	running := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "console-0", Namespace: c.Namespace, Labels: selectorLabels(c)},
		Status:     corev1.PodStatus{PodIPs: []corev1.PodIP{{IP: "10.128.0.5"}}},
	}
	calico := func(name string) *corev1.Node { return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}} }
	cl := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(running, calico("a"), calico("b")).Build()
	r := &Reconciler{Client: cl, APIReader: cl}
	found, err := r.exposureConfinement(context.Background(), c, buildExposureNetworkPolicy(c, true).Spec.Ingress[0].From)
	if err != nil {
		t.Fatal(err)
	}
	fact := exposureConfinedFact(c, true, found)
	if !strings.Contains(fact.Message, "the Node check could not run: none of the 2 Nodes has spec.podCIDRs") {
		t.Fatalf("ExposureConfined = %+v, want the message to say the Node check could not run", fact)
	}
}

// TestPolicyKeptOnStall: stalled with the exposure withdrawn, the console
// says its ingress policy is kept while it is (read past the cache), and
// has no ExposureConfined without one, or with a policy it does not own.
func TestPolicyKeptOnStall(t *testing.T) {
	t.Parallel()
	c := ipBlockConsole("192.168.10.0/24")
	policy := buildExposureNetworkPolicy(c, true)
	foreign := policy.DeepCopy()
	foreign.OwnerReferences = nil
	cases := []struct {
		name string
		live []client.Object
		kept bool
	}{
		{"the console's policy", []client.Object{owned(c, policy.DeepCopy())}, true},
		{"none", nil, false},
		{"someone else's", []client.Object{foreign}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			console := c.DeepCopy()
			// Published before: the condition is there to be replaced or removed.
			setExposureConfined(console, true, confinement{})
			r := &Reconciler{
				Client:    fake.NewClientBuilder().WithScheme(cnpgScheme(t)).Build(), // a cache that has not seen it
				APIReader: fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(tc.live...).Build(),
			}
			if err := r.policyKeptOnStall(context.Background(), console); err != nil {
				t.Fatal(err)
			}
			got := meta.FindStatusCondition(console.Status.Conditions, pgtoolboxv1alpha1.ConditionExposureConfined)
			switch {
			case tc.kept && (got == nil || got.Status != metav1.ConditionFalse || got.Reason != string(pgtoolboxv1alpha1.ReasonExposurePolicyKept)):
				t.Fatalf("ExposureConfined = %+v, want False ExposurePolicyKept", got)
			case !tc.kept && got != nil:
				t.Fatalf("ExposureConfined = %+v, want none", got)
			}
		})
	}
}
