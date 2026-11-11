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
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

func consolePod(name, node string, ips ...string) corev1.Pod {
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.PodSpec{NodeName: node}}
	for _, ip := range ips {
		pod.Status.PodIPs = append(pod.Status.PodIPs, corev1.PodIP{IP: ip})
	}
	return pod
}

// TestNodeCoherenceNote: a console Pod whose address is outside its own
// Node's podCIDRs (of that family) makes the Node check not meaningful;
// nothing else does.
func TestNodeCoherenceNote(t *testing.T) {
	t.Parallel()
	nodes := []nodePodCIDRs{
		{Name: "a", CIDRs: []netip.Prefix{netip.MustParsePrefix("10.244.0.0/24")}},
		{Name: "b", CIDRs: []netip.Prefix{netip.MustParsePrefix("10.244.1.0/24"), netip.MustParsePrefix("fd00:10:244:1::/64")}},
	}
	hostNetwork := consolePod("console-h", "a", "192.168.1.10")
	hostNetwork.Spec.HostNetwork = true
	cases := []struct {
		name string
		pods []corev1.Pod
		want string // a substring, "" for no note
	}{
		{"inside its node's range", []corev1.Pod{consolePod("console-0", "a", "10.244.0.7")}, ""},
		{"another IPAM", []corev1.Pod{consolePod("console-0", "a", "192.168.50.5")}, "console Pod console-0 has address 192.168.50.5, outside the podCIDRs of its Node a"},
		// The Node ranges are still compared (nodeNetworkBlock runs): the
		// note must not claim that only the Pods' addresses were checked.
		{"says what the Node ranges prove", []corev1.Pod{consolePod("console-0", "a", "192.168.50.5")}, "the Node ranges prove nothing here"},
		{"inside another node's range, not its own", []corev1.Pod{consolePod("console-0", "a", "10.244.1.7")}, "Node a"},
		{"node without podCIDRs", []corev1.Pod{consolePod("console-0", "c", "192.168.50.5")}, ""},
		{"not scheduled", []corev1.Pod{consolePod("console-0", "", "192.168.50.5")}, ""},
		{"no address", []corev1.Pod{consolePod("console-0", "a")}, ""},
		{"IPv6 address, no IPv6 range on the node", []corev1.Pod{consolePod("console-0", "a", "10.244.0.7", "fd00:99::7")}, ""},
		{"IPv6 address outside the node's IPv6 range", []corev1.Pod{consolePod("console-0", "b", "10.244.1.7", "fd00:99::7")}, "fd00:99::7"},
		{"host network", []corev1.Pod{hostNetwork}, ""},
		{"the first by name", []corev1.Pod{consolePod("console-1", "b", "192.168.50.6"), consolePod("console-0", "a", "192.168.50.5")}, "console Pod console-0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := nodeCoherenceNote(tc.pods, nodes)
			if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
				t.Fatalf("nodeCoherenceNote = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "only the console Pods' own addresses") {
				t.Fatalf("nodeCoherenceNote = %q claims the Node ranges were not checked", got)
			}
		})
	}
}

// TestNodeCheckNotMeaningful: with Calico-like IPAM (the console Pod's
// address is outside its Node's podCIDRs, which kube-controller-manager
// still fills), ExposureConfined says the Node check is not meaningful,
// in the message only: the reason, the trust and the rendered Pod
// template stay as they were.
func TestNodeCheckNotMeaningful(t *testing.T) {
	t.Parallel()
	c := ipBlockConsole("192.168.10.0/24")
	pod := consolePod("console-0", "a", "172.20.3.9")
	pod.Namespace, pod.Labels = c.Namespace, selectorLabels(c)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Spec: corev1.NodeSpec{PodCIDRs: []string{"10.244.0.0/24"}}}
	cl := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(&pod, node).Build()
	r := &Reconciler{Client: cl, APIReader: cl}
	found, err := r.exposureConfinement(context.Background(), c, buildExposureNetworkPolicy(c, true).Spec.Ingress[0].From)
	if err != nil {
		t.Fatal(err)
	}
	if !found.clear() || !strings.Contains(found.Note, "Node check is not meaningful") {
		t.Fatalf("confinement = %+v, want clear with the note", found)
	}
	fact := exposureConfinedFact(c, true, found)
	if fact.Status != metav1.ConditionTrue || fact.Reason != pgtoolboxv1alpha1.ReasonAsExpected || !strings.Contains(fact.Message, "Node check is not meaningful") {
		t.Fatalf("ExposureConfined = %+v, want True/AsExpected with the note", fact)
	}
	without := found
	without.Note = ""
	if fact := exposureConfinedFact(c, true, without); fact.Reason != pgtoolboxv1alpha1.ReasonAsExpected || fact.Status != metav1.ConditionTrue {
		t.Fatalf("the note changed the condition's status or reason: %+v", fact)
	}
	if !equality.Semantic.DeepEqual(podTemplate(c, deploymentInputs{Exposed: true, Confinement: found}), podTemplate(c, deploymentInputs{Exposed: true, Confinement: without})) {
		t.Fatal("the note changed the rendered Pod template")
	}
}

// TestNodeCheckNotMeaningfulStillBroad: the Node ranges are compared even
// when a console Pod's address is outside its Node's podCIDRs, so a peer
// that covers a Node's range is still reported broad, beside the note.
func TestNodeCheckNotMeaningfulStillBroad(t *testing.T) {
	t.Parallel()
	c := ipBlockConsole("10.244.0.0/16")
	pod := consolePod("console-0", "a", "172.20.3.9")
	pod.Namespace, pod.Labels = c.Namespace, selectorLabels(c)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Spec: corev1.NodeSpec{PodCIDRs: []string{"10.244.0.0/24"}}}
	cl := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(&pod, node).Build()
	r := &Reconciler{Client: cl, APIReader: cl}
	found, err := r.exposureConfinement(context.Background(), c, buildExposureNetworkPolicy(c, true).Spec.Ingress[0].From)
	if err != nil {
		t.Fatal(err)
	}
	if found.Broad == "" || !strings.Contains(found.Note, "the Node ranges prove nothing here") {
		t.Fatalf("confinement = %+v, want broad with the note", found)
	}
}

// pagedNodes is an APIReader whose Node lists serve pages of one Node
// each, by continue token ("1", "2", ...), and fail as fail says; every
// Node list's continue token is recorded.
type pagedNodes struct {
	mu     sync.Mutex
	total  int
	fail   func(token string) error
	tokens []string
}

func (p *pagedNodes) client(t *testing.T) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(cnpgScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			nodes, ok := list.(*corev1.NodeList)
			if !ok {
				return cl.List(ctx, list, opts...)
			}
			var o client.ListOptions
			o.ApplyOptions(opts)
			p.mu.Lock()
			p.tokens = append(p.tokens, o.Continue)
			fail := p.fail
			p.mu.Unlock()
			if fail != nil {
				if err := fail(o.Continue); err != nil {
					return err
				}
			}
			index := 0
			if o.Continue != "" {
				index, _ = strconv.Atoi(o.Continue)
			}
			nodes.Items = []corev1.Node{{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("node-%d", index)},
				Spec:       corev1.NodeSpec{PodCIDRs: []string{fmt.Sprintf("10.128.%d.0/24", index)}},
			}}
			if index+1 < p.total {
				nodes.Continue = strconv.Itoa(index + 1)
			}
			return nil
		}}).Build()
}

func (p *pagedNodes) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.tokens...)
}

// TestNodeLookupResumes: a lookup that stops part way keeps the pages it
// read and the next resumes from its continue token instead of starting
// over; an expired token starts over once; each lookup runs within its
// own budget, under the reconcile timeout.
func TestNodeLookupResumes(t *testing.T) {
	t.Parallel()

	t.Run("a failed page is resumed, not restarted", func(t *testing.T) {
		t.Parallel()
		failed := false
		p := &pagedNodes{total: 4}
		p.fail = func(token string) error {
			if token == "2" && !failed {
				failed = true
				return apierrors.NewServiceUnavailable("etcd")
			}
			return nil
		}
		r := &Reconciler{APIReader: p.client(t)}
		if _, _, err := r.nodePodCIDRs(context.Background()); err == nil {
			t.Fatal("the failed page was not returned")
		}
		nodes, note, err := r.nodePodCIDRs(context.Background())
		if err != nil || note != "" {
			t.Fatalf("the resumed lookup: note %q, err %v", note, err)
		}
		if len(nodes) != 4 {
			t.Fatalf("%d Nodes, want 4 (the pages before the failure kept, none twice): %+v", len(nodes), nodes)
		}
		if got, want := strings.Join(p.seen(), ","), ",1,2,2,3"; got != want {
			t.Fatalf("continue tokens %q, want %q (the second lookup resumes at the failed page)", got, want)
		}
	})

	t.Run("an expired token starts over", func(t *testing.T) {
		t.Parallel()
		expired := false
		p := &pagedNodes{total: 3}
		p.fail = func(token string) error {
			switch {
			case token == "1" && !expired:
				expired = true
				return apierrors.NewServiceUnavailable("etcd")
			case token == "1" && expired && len(p.tokens) == 3:
				return apierrors.NewResourceExpired("the provided continue parameter is too old")
			}
			return nil
		}
		r := &Reconciler{APIReader: p.client(t)}
		if _, _, err := r.nodePodCIDRs(context.Background()); err == nil {
			t.Fatal("the failed page was not returned")
		}
		nodes, _, err := r.nodePodCIDRs(context.Background())
		if err != nil || len(nodes) != 3 {
			t.Fatalf("after an expired token: %d Nodes, err %v; want 3, read again from the start", len(nodes), err)
		}
		if got, want := strings.Join(p.seen(), ","), ",1,1,,1,2"; got != want {
			t.Fatalf("continue tokens %q, want %q", got, want)
		}
	})

	t.Run("a stopped scan too old is dropped", func(t *testing.T) {
		t.Parallel()
		now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
		failed := false
		p := &pagedNodes{total: 3}
		p.fail = func(token string) error {
			if token == "1" && !failed {
				failed = true
				return apierrors.NewServiceUnavailable("etcd")
			}
			return nil
		}
		r := &Reconciler{APIReader: p.client(t), Now: func() time.Time { return now }}
		if _, _, err := r.nodePodCIDRs(context.Background()); err == nil {
			t.Fatal("the failed page was not returned")
		}
		now = now.Add(nodeScanMaxAge + time.Second)
		if nodes, _, err := r.nodePodCIDRs(context.Background()); err != nil || len(nodes) != 3 {
			t.Fatalf("%d Nodes, err %v", len(nodes), err)
		}
		if got, want := strings.Join(p.seen(), ","), ",1,,1,2"; got != want {
			t.Fatalf("continue tokens %q, want %q (an old scan starts over)", got, want)
		}
	})

	t.Run("a lookup has its own deadline", func(t *testing.T) {
		t.Parallel()
		var deadlines []time.Duration
		cl := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).
			WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.NodeList); ok {
					deadline, ok := ctx.Deadline()
					if !ok {
						t.Error("a Node list without a deadline")
					}
					deadlines = append(deadlines, time.Until(deadline))
				}
				return cl.List(ctx, list, opts...)
			}}).Build()
		r := &Reconciler{APIReader: cl}
		if _, _, err := r.nodePodCIDRs(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(deadlines) != 1 || deadlines[0] > nodeLookupBudget || nodeLookupBudget >= reconciliationTimeout {
			t.Fatalf("Node list deadlines %v, want one within %s (under the %s reconcile timeout)", deadlines, nodeLookupBudget, reconciliationTimeout)
		}
	})

	t.Run("a lookup out of time says so and keeps its pages", func(t *testing.T) {
		t.Parallel()
		p := &pagedNodes{total: 3}
		p.fail = func(token string) error {
			if token == "1" {
				return fmt.Errorf("the page took too long: %w", context.DeadlineExceeded)
			}
			return nil
		}
		r := &Reconciler{APIReader: p.client(t)}
		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()
		<-ctx.Done()
		// The caller's own context ended: not the budget, so not said to be.
		if _, _, err := r.nodePodCIDRs(ctx); err == nil || strings.Contains(err.Error(), "did not finish within") {
			t.Fatalf("err %v, want the caller's context error alone", err)
		}
		r.nodes.mu.Lock()
		kept := r.nodes.scan
		r.nodes.mu.Unlock()
		if kept.pages != 1 || kept.token != "1" || len(kept.nodes) != 1 {
			t.Fatalf("kept scan %+v, want the first page and the token of the second", kept)
		}
	})
}
