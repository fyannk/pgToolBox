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
	"net/netip"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

func ipBlockConsole(cidr string) *pgtoolboxv1alpha1.PgToolBoxConsole {
	c := &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "console", Namespace: "team-a", UID: "u"}}
	c.Spec.Exposure.Type, c.Spec.Exposure.Hostname = pgtoolboxv1alpha1.ExposureTypeIngress, "console.example"
	c.Spec.Exposure.NetworkPolicy = &pgtoolboxv1alpha1.PgToolBoxConsoleExposurePolicySpec{From: []pgtoolboxv1alpha1.PgToolBoxConsolePolicyPeer{{
		IPBlock: &pgtoolboxv1alpha1.PgToolBoxConsoleIPBlock{CIDR: cidr},
	}}}
	return c
}

// owned makes object controlled by c, as the applier writes it.
func owned[T client.Object](c *pgtoolboxv1alpha1.PgToolBoxConsole, object T) T {
	object.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: pgtoolboxv1alpha1.GroupVersion.String(), Kind: "PgToolBoxConsole", Name: c.Name, UID: c.UID, Controller: new(true),
	}})
	return object
}

// TestHeldWideningConfirmsPastCache: the cache still shows the narrower
// policy this operator replaced; the API server has the desired one, so
// nothing widens and the grant is not withdrawn. A widening the API
// server confirms is held.
func TestHeldWideningConfirmsPastCache(t *testing.T) {
	t.Parallel()
	c := ipBlockConsole("192.168.10.0/24")
	c.Spec.Exposure.NetworkPolicy.From = append(c.Spec.Exposure.NetworkPolicy.From, nginxSpecPeer)
	desired := buildExposureNetworkPolicy(c, true)
	narrow := owned(c, buildExposureNetworkPolicy(ipBlockConsole("192.168.10.0/24"), true))
	narrow.Name = desired.Name
	set := owned(c, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: c.WorkloadName(), Namespace: c.Namespace, Generation: 1}})
	set.Spec.Template.Annotations = map[string]string{annotationTrustsEveryPeer: "true"}
	set.Status.ObservedGeneration = 1
	cache := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(narrow.DeepCopy(), set.DeepCopy()).Build()

	current := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(owned(c, desired.DeepCopy()), set.DeepCopy()).Build()
	r := &Reconciler{Client: cache, APIReader: current}
	held, stuck, err := r.heldWidening(context.Background(), c, desired)
	if err != nil || held != "" || stuck {
		t.Fatalf("heldWidening = %q, %v, %v on a stale cache, want nothing held", held, stuck, err)
	}

	confirmed := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(narrow.DeepCopy(), set.DeepCopy()).Build()
	r = &Reconciler{Client: cache, APIReader: confirmed}
	if held, _, err = r.heldWidening(context.Background(), c, desired); err != nil || held == "" {
		t.Fatalf("heldWidening = %q, %v on a confirmed widening, want it held", held, err)
	}
}

// TestStuckTerminatingPodIsNamed: a console Pod stuck terminating with the
// grant holds a widening and a withdrawal (fail closed), and both say
// which Pod and since when, and ask for a look again.
func TestStuckTerminatingPodIsNamed(t *testing.T) {
	t.Parallel()
	c := ipBlockConsole("192.168.10.0/24")
	desired := buildExposureNetworkPolicy(c, true)
	narrow := owned(c, buildExposureNetworkPolicy(ipBlockConsole("192.168.10.0/25"), true))
	narrow.Name = desired.Name
	since := metav1.NewTime(time.Date(2026, 11, 9, 8, 30, 0, 0, time.UTC))
	stuckPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "console-0", Namespace: c.Namespace, Labels: selectorLabels(c), Finalizers: []string{"example.com/stuck"},
		DeletionTimestamp: &since, Annotations: map[string]string{annotationTrustsEveryPeer: "true"},
	}}
	cl := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(narrow, stuckPod).Build()
	r := &Reconciler{Client: cl, APIReader: cl}
	ctx := context.Background()

	held, stuck, err := r.heldWidening(ctx, c, desired)
	if err != nil || !stuck || !strings.Contains(held, "Pod console-0, terminating since 2026-11-09T08:30:00Z") {
		t.Fatalf("heldWidening = %q, %v, %v; want it held, naming the stuck Pod", held, stuck, err)
	}

	facts := &workloadFacts{Kind: "StatefulSet", Generation: 1, ObservedGeneration: 1, Desired: 1, Replicas: 1, Updated: 1, Available: 1}
	kept, stuck, err := r.withdrawExposurePolicy(ctx, c, facts)
	if err != nil || !stuck || !strings.Contains(kept, "Pod console-0, terminating since 2026-11-09T08:30:00Z") {
		t.Fatalf("withdrawExposurePolicy = %q, %v, %v; want it kept, naming the stuck Pod", kept, stuck, err)
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(narrow), &networkingv1.NetworkPolicy{}); err != nil {
		t.Fatalf("the policy was withdrawn under a Pod that trusts every peer: %v", err)
	}
}

// TestVerifiedEarlier: with no console Pod address, an ipBlock is trusted
// only when the console's workload (read past the cache) carries the grant
// for the same blocks; at creation, or for other blocks, it is not.
func TestVerifiedEarlier(t *testing.T) {
	t.Parallel()
	c := ipBlockConsole("192.168.10.0/24")
	peers := buildExposureNetworkPolicy(c, true).Spec.Ingress[0].From
	pending := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "console-0", Namespace: c.Namespace, Labels: selectorLabels(c)}}
	granted := func(blocks string) *appsv1.StatefulSet {
		set := owned(c, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: c.WorkloadName(), Namespace: c.Namespace}})
		set.Spec.Template.Annotations = map[string]string{annotationTrustsEveryPeer: "true", annotationTrustedBlocks: blocks}
		return set
	}
	cases := []struct {
		name     string
		cached   []client.Object
		live     []client.Object
		verified bool
	}{
		{name: "creation", live: []client.Object{pending}},
		{name: "the grant for these blocks", live: []client.Object{pending, granted(trustedBlocksHash(peers))}, verified: true},
		{name: "the grant for other blocks", live: []client.Object{pending, granted(trustedBlocksHash(buildExposureNetworkPolicy(ipBlockConsole("10.0.0.0/24"), true).Spec.Ingress[0].From))}},
		// The cache has not delivered the template with the grant yet:
		// the API server's is the one read.
		{name: "only the cache is stale", cached: []client.Object{pending}, live: []client.Object{pending, granted(trustedBlocksHash(peers))}, verified: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cached := tc.cached
			if cached == nil {
				cached = tc.live
			}
			copies := func(objects []client.Object) []client.Object {
				out := make([]client.Object, 0, len(objects))
				for _, o := range objects {
					out = append(out, o.DeepCopyObject().(client.Object))
				}
				return out
			}
			r := &Reconciler{
				Client:    fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(copies(cached)...).Build(),
				APIReader: fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(copies(tc.live)...).Build(),
			}
			found, err := r.exposureConfinement(context.Background(), c, peers)
			if err != nil {
				t.Fatal(err)
			}
			if got := found.Unverified == ""; got != tc.verified {
				t.Fatalf("confinement = %+v, want verified %v", found, tc.verified)
			}
			if tc.verified != effectiveProxyTrust(c, true, found).EveryPeer {
				t.Fatalf("any-peer trust %v, want %v", !tc.verified, tc.verified)
			}
		})
	}
}

// TestNodePodCIDRs: the Nodes are listed bounded and in pages; a block
// that overlaps another node's podCIDR is broad though no console Pod
// address is in it; without the grant to list Nodes the check is skipped
// and said so, without withholding the trust; another error is returned.
// The lookup is kept for nodeCIDRTTL.
func TestNodePodCIDRs(t *testing.T) {
	t.Parallel()
	c := ipBlockConsole("10.128.4.0/23")
	peers := buildExposureNetworkPolicy(c, true).Spec.Ingress[0].From
	running := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "console-0", Namespace: c.Namespace, Labels: selectorLabels(c)},
		Status:     corev1.PodStatus{PodIPs: []corev1.PodIP{{IP: "10.128.0.5"}}},
	}
	node := func(name string, cidrs ...string) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.NodeSpec{PodCIDRs: cidrs}}
	}
	t.Run("another node's range", func(t *testing.T) {
		t.Parallel()
		var limits []int64
		cl := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(running.DeepCopy(), node("a", "10.128.0.0/24"), node("b", "10.128.4.0/24")).
			WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.NodeList); ok {
					var o client.ListOptions
					o.ApplyOptions(opts)
					limits = append(limits, o.Limit)
				}
				return cl.List(ctx, list, opts...)
			}}).Build()
		now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
		r := &Reconciler{Client: cl, APIReader: cl, Now: func() time.Time { return now }}
		found, err := r.exposureConfinement(context.Background(), c, peers)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(found.Broad, "Node b") || effectiveProxyTrust(c, true, found).EveryPeer {
			t.Fatalf("confinement = %+v, want the block broad for node b's Pod range", found)
		}
		if fact := exposureConfinedFact(c, true, found); fact.Reason != pgtoolboxv1alpha1.ReasonExposurePeersBroad {
			t.Fatalf("ExposureConfined = %+v", fact)
		}
		if _, err := r.exposureConfinement(context.Background(), c, peers); err != nil {
			t.Fatal(err)
		}
		if len(limits) != 1 || limits[0] != nodeListPage {
			t.Fatalf("Node lists %v, want one of limit %d within the TTL", limits, nodeListPage)
		}
		now = now.Add(nodeCIDRTTL)
		if _, err := r.exposureConfinement(context.Background(), c, peers); err != nil {
			t.Fatal(err)
		}
		if len(limits) != 2 {
			t.Fatalf("%d Node lists after the TTL, want 2", len(limits))
		}
	})
	listError := func(err error) interceptor.Funcs {
		return interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.NodeList); ok {
				return err
			}
			return cl.List(ctx, list, opts...)
		}}
	}
	t.Run("may not list Nodes", func(t *testing.T) {
		t.Parallel()
		narrow := ipBlockConsole("192.168.10.0/24")
		cl := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(running.DeepCopy()).
			WithInterceptorFuncs(listError(apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", errors.New("no")))).Build()
		r := &Reconciler{Client: cl, APIReader: cl}
		found, err := r.exposureConfinement(context.Background(), narrow, buildExposureNetworkPolicy(narrow, true).Spec.Ingress[0].From)
		if err != nil {
			t.Fatal(err)
		}
		if !found.clear() || found.Note == "" || !effectiveProxyTrust(narrow, true, found).EveryPeer {
			t.Fatalf("confinement = %+v, want today's check alone, and a note", found)
		}
		if fact := exposureConfinedFact(narrow, true, found); fact.Status != metav1.ConditionTrue || !strings.Contains(fact.Message, "podCIDRs were not checked") {
			t.Fatalf("ExposureConfined = %+v, want True saying the Nodes were not checked", fact)
		}
	})
	t.Run("the Node list fails", func(t *testing.T) {
		t.Parallel()
		cl := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(running.DeepCopy()).
			WithInterceptorFuncs(listError(apierrors.NewServiceUnavailable("etcd"))).Build()
		r := &Reconciler{Client: cl, APIReader: cl}
		if _, err := r.exposureConfinement(context.Background(), c, peers); err == nil {
			t.Fatal("a failed Node list was not returned")
		}
	})
	t.Run("more Nodes than it lists", func(t *testing.T) {
		t.Parallel()
		cl := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(running.DeepCopy(), node("a", "10.128.0.0/24")).
			WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if err := cl.List(ctx, list, opts...); err != nil {
					return err
				}
				if nodes, ok := list.(*corev1.NodeList); ok {
					nodes.Continue = "more"
				}
				return nil
			}}).Build()
		r := &Reconciler{Client: cl, APIReader: cl}
		_, note, err := r.nodePodCIDRs(context.Background())
		if err != nil || !strings.Contains(note, "more than") {
			t.Fatalf("nodePodCIDRs note %q, err %v; want the cap said", note, err)
		}
	})
}

// TestNodeNetworkBlock: a block that overlaps a Node's podCIDR, either
// way, is broad unless an except entry holds the whole range.
func TestNodeNetworkBlock(t *testing.T) {
	t.Parallel()
	nodes := []nodePodCIDRs{
		{Name: "a", CIDRs: []netip.Prefix{netip.MustParsePrefix("10.128.0.0/24"), netip.MustParsePrefix("fd00:10:244:1::/64")}},
		{Name: "b", CIDRs: []netip.Prefix{netip.MustParsePrefix("10.128.4.0/24")}},
	}
	block := func(cidr string, except ...string) []networkingv1.NetworkPolicyPeer {
		return []networkingv1.NetworkPolicyPeer{nginxPeer, {IPBlock: &networkingv1.IPBlock{CIDR: cidr, Except: except}}}
	}
	cases := []struct {
		name  string
		peers []networkingv1.NetworkPolicyPeer
		nodes []nodePodCIDRs
		broad bool
	}{
		{"no block", []networkingv1.NetworkPolicyPeer{nginxPeer}, nodes, false},
		{"the node network", block("192.168.10.0/24"), nodes, false},
		{"holds node b's range", block("10.128.4.0/23"), nodes, true},
		{"inside node a's range", block("10.128.0.16/28"), nodes, true},
		{"IPv6 node range", block("fd00:10:244:1::/80"), nodes, true},
		{"node b's range excepted", block("10.128.4.0/23", "10.128.4.0/24"), nodes, false},
		{"part of node b's range excepted", block("10.128.4.0/23", "10.128.4.0/25"), nodes, true},
		{"no podCIDRs (another IPAM)", block("10.128.4.0/23"), nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := nodeNetworkBlock(tc.peers, tc.nodes); (got != "") != tc.broad {
				t.Errorf("nodeNetworkBlock = %q, want broad %v", got, tc.broad)
			}
		})
	}
}

// TestTrustedBlocksHash: the hash names the ipBlocks (CIDR and except
// entries), not their order nor the other peers.
func TestTrustedBlocksHash(t *testing.T) {
	t.Parallel()
	a := networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "192.168.10.0/24", Except: []string{"192.168.10.1/32", "192.168.10.2/32"}}}
	b := networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "192.168.20.0/24"}}
	reordered := networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "192.168.10.0/24", Except: []string{"192.168.10.2/32", "192.168.10.1/32"}}}
	if trustedBlocksHash([]networkingv1.NetworkPolicyPeer{nginxPeer}) != "" {
		t.Error("a hash without ipBlocks")
	}
	base := trustedBlocksHash([]networkingv1.NetworkPolicyPeer{a, b})
	if got := trustedBlocksHash([]networkingv1.NetworkPolicyPeer{b, nginxPeer, reordered}); got != base {
		t.Errorf("order or other peers changed the hash: %s != %s", got, base)
	}
	if trustedBlocksHash([]networkingv1.NetworkPolicyPeer{a}) == base {
		t.Error("a block fewer, the same hash")
	}
	noExcept := networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "192.168.10.0/24"}}
	if trustedBlocksHash([]networkingv1.NetworkPolicyPeer{noExcept, b}) == base {
		t.Error("except entries do not count")
	}
}

// forgetNodes drops the Node podCIDR memo, for a suite that adds Nodes.
func (r *Reconciler) forgetNodes() {
	r.nodes.mu.Lock()
	defer r.nodes.mu.Unlock()
	r.nodes.at, r.nodes.nodes, r.nodes.note = time.Time{}, nil, ""
}

// TestTrustedBlocksAnnotation: the grant for ipBlock peers names their
// hash in the template; without ipBlocks, or without the grant, nothing.
func TestTrustedBlocksAnnotation(t *testing.T) {
	t.Parallel()
	c := ipBlockConsole("192.168.10.0/24")
	template := podTemplate(c, deploymentInputs{Exposed: true})
	if got, want := template.Annotations[annotationTrustedBlocks], trustedBlocksHash(buildExposureNetworkPolicy(c, true).Spec.Ingress[0].From); got != want || want == "" {
		t.Fatalf("trusted blocks %q, want %q", got, want)
	}
	if _, ok := podTemplate(c, deploymentInputs{Exposed: true, Confinement: confinement{Unverified: "x"}}).Annotations[annotationTrustedBlocks]; ok {
		t.Fatal("trusted blocks without the grant")
	}
	c.Spec.Exposure.NetworkPolicy.From = []pgtoolboxv1alpha1.PgToolBoxConsolePolicyPeer{nginxSpecPeer}
	if _, ok := podTemplate(c, deploymentInputs{Exposed: true}).Annotations[annotationTrustedBlocks]; ok {
		t.Fatal("trusted blocks without an ipBlock peer")
	}
}
