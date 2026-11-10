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
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// TestExposurePolicyGolden pins the console's ingress NetworkPolicy: the
// OpenShift routers by default for a Route, the spec's peers for an
// Ingress or a Gateway, on the console port only; the probe port stays
// open. Rewrite with -update.
func TestExposurePolicyGolden(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*pgtoolboxv1alpha1.PgToolBoxConsole){
		"route": func(c *pgtoolboxv1alpha1.PgToolBoxConsole) {
			c.Spec.Exposure.Type = pgtoolboxv1alpha1.ExposureTypeRoute
		},
		// from adds to the routers on a Route, never replaces them.
		"route-from": func(c *pgtoolboxv1alpha1.PgToolBoxConsole) {
			c.Spec.Exposure.Type = pgtoolboxv1alpha1.ExposureTypeRoute
			c.Spec.Exposure.NetworkPolicy = &pgtoolboxv1alpha1.PgToolBoxConsoleExposurePolicySpec{From: []pgtoolboxv1alpha1.PgToolBoxConsolePolicyPeer{
				{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "mcp-client"}}},
				{NamespaceSelector: routerPeer().NamespaceSelector},
			}}
		},
		"ingress-from": func(c *pgtoolboxv1alpha1.PgToolBoxConsole) {
			c.Spec.Exposure.Type = pgtoolboxv1alpha1.ExposureTypeIngress
			c.Spec.Exposure.NetworkPolicy = &pgtoolboxv1alpha1.PgToolBoxConsoleExposurePolicySpec{From: []pgtoolboxv1alpha1.PgToolBoxConsolePolicyPeer{
				{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "ingress-nginx"}},
					PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "ingress-nginx"}},
				},
			}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "console", Namespace: "team-a", UID: "u"}}
			c.Spec.Exposure.Hostname = "console.example"
			mutate(c)
			policy := buildExposureNetworkPolicy(c, true)
			if policy == nil {
				t.Fatal("no NetworkPolicy rendered")
			}
			got, err := yaml.Marshal(policy)
			if err != nil {
				t.Fatal(err)
			}
			again, _ := yaml.Marshal(buildExposureNetworkPolicy(c, true))
			if !bytes.Equal(got, again) {
				t.Fatal("two renderings differ: the apply would never settle")
			}
			path := filepath.Join("testdata", "exposure-policy-"+name+".golden.yaml")
			if *updateGolden {
				if err := os.WriteFile(path, got, 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs from the golden file (run with -update to accept)", path)
			}
		})
	}
}

// TestExposurePolicyNotRendered: no policy without a published exposure,
// when turned off, or for ingress and gateway exposure that name no peer
// (no namespace is a safe default there); ExposureConfined says so.
func TestExposurePolicyNotRendered(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		typ     pgtoolboxv1alpha1.ExposureType
		exposed bool
		policy  *pgtoolboxv1alpha1.PgToolBoxConsoleExposurePolicySpec
		fact    string // "" = no condition; else reason
		message string
	}{
		{name: "clusterIP", typ: pgtoolboxv1alpha1.ExposureTypeClusterIP},
		{name: "route not published", typ: pgtoolboxv1alpha1.ExposureTypeRoute},
		{name: "route turned off", typ: pgtoolboxv1alpha1.ExposureTypeRoute, exposed: true, policy: &pgtoolboxv1alpha1.PgToolBoxConsoleExposurePolicySpec{Enabled: new(false)}, fact: "ExposureOpen", message: "networkPolicy.enabled is false"},
		{name: "ingress without from", typ: pgtoolboxv1alpha1.ExposureTypeIngress, exposed: true, fact: "ExposureOpen", message: "networkPolicy.from is not set"},
		{name: "gateway without from", typ: pgtoolboxv1alpha1.ExposureTypeGateway, exposed: true, policy: &pgtoolboxv1alpha1.PgToolBoxConsoleExposurePolicySpec{}, fact: "ExposureOpen", message: "X-Forwarded-For is ignored"},
		{name: "route confined", typ: pgtoolboxv1alpha1.ExposureTypeRoute, exposed: true, fact: "AsExpected", message: "from peers in 0.0.0.0/0,::/0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "console", Namespace: "team-a"}}
			c.Spec.Exposure.Type, c.Spec.Exposure.Hostname = tc.typ, "console.example"
			c.Spec.Exposure.NetworkPolicy = tc.policy
			if p := buildExposureNetworkPolicy(c, tc.exposed); (p != nil) != (tc.fact == "AsExpected") {
				t.Errorf("NetworkPolicy rendered = %v", p != nil)
			}
			fact := exposureConfinedFact(c, tc.exposed, "")
			switch {
			case tc.fact == "" && fact != nil:
				t.Errorf("ExposureConfined = %+v, want none", fact)
			case tc.fact != "" && (fact == nil || string(fact.Reason) != tc.fact || !strings.Contains(fact.Message, tc.message)):
				t.Errorf("ExposureConfined = %+v, want %s with %q", fact, tc.fact, tc.message)
			}
		})
	}
}

var (
	nginxSpecPeer = pgtoolboxv1alpha1.PgToolBoxConsolePolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "ingress-nginx"}},
		PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "ingress-nginx"}},
	}
	nginxPeer = nginxSpecPeer.NetworkPolicyPeer()
)

// TestBroadPeer: a peer that admits every namespace, every Pod of the
// namespace or every address is not a proxy; the narrow ones are.
func TestBroadPeer(t *testing.T) {
	t.Parallel()
	all := &metav1.LabelSelector{}
	some := &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}}
	cases := []struct {
		name  string
		peer  networkingv1.NetworkPolicyPeer
		broad bool
	}{
		{"named namespace and pods", nginxPeer, false},
		{"router namespaces", routerPeer(), false},
		{"labelled pods of the namespace", networkingv1.NetworkPolicyPeer{PodSelector: some}, false},
		{"a namespace by expression", networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "a", Operator: metav1.LabelSelectorOpExists}}}}, false},
		{"a /16", networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "10.1.0.0/16"}}, false},
		{"every namespace", networkingv1.NetworkPolicyPeer{NamespaceSelector: all}, true},
		{"labelled pods of every namespace", networkingv1.NetworkPolicyPeer{NamespaceSelector: all, PodSelector: some}, true},
		{"every pod of the namespace", networkingv1.NetworkPolicyPeer{PodSelector: all}, true},
		{"every IPv4 address", networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{"10.0.0.0/8"}}}, true},
		{"every IPv6 address", networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "::/0"}}, true},
		{"an unparsable block", networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "nope"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := broadPeer([]networkingv1.NetworkPolicyPeer{nginxPeer, tc.peer})
			if (got != "") != tc.broad {
				t.Errorf("broadPeer = %q, want broad %v", got, tc.broad)
			}
		})
	}
}

// TestPolicyBypass: another NetworkPolicy that selects the console Pods
// and admits, to the console port, a peer the exposure does not list,
// defeats the confinement (NetworkPolicies add up); one that selects other
// Pods, other ports or only egress, or lists the exposure's own peers,
// does not.
func TestPolicyBypass(t *testing.T) {
	t.Parallel()
	podLabels := map[string]string{"app.kubernetes.io/name": "pgtoolbox-console", "app.kubernetes.io/instance": "console", "team": "a"}
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	port := func(p intstr.IntOrString, end *int32, proto *corev1.Protocol) networkingv1.NetworkPolicyPort {
		return networkingv1.NetworkPolicyPort{Port: &p, EndPort: end, Protocol: proto}
	}
	other := networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"role": "debug"}}}
	policy := func(name string, selector metav1.LabelSelector, types []networkingv1.PolicyType, rules ...networkingv1.NetworkPolicyIngressRule) networkingv1.NetworkPolicy {
		return networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       networkingv1.NetworkPolicySpec{PodSelector: selector, PolicyTypes: types, Ingress: rules},
		}
	}
	every := metav1.LabelSelector{}
	ingress := []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
	cases := []struct {
		name     string
		policies []networkingv1.NetworkPolicy
		bypass   bool
	}{
		{name: "none"},
		{name: "our own policy is skipped", policies: []networkingv1.NetworkPolicy{policy("console-ingress", every, ingress, networkingv1.NetworkPolicyIngressRule{})}},
		{name: "allow from the same namespace", policies: []networkingv1.NetworkPolicy{policy("allow-same-ns", every, ingress, networkingv1.NetworkPolicyIngressRule{From: []networkingv1.NetworkPolicyPeer{{PodSelector: &every}}})}, bypass: true},
		{name: "allow everything (no from), no policyTypes", policies: []networkingv1.NetworkPolicy{policy("allow-all", every, nil, networkingv1.NetworkPolicyIngressRule{})}, bypass: true},
		{name: "selects the console by a propagated label", policies: []networkingv1.NetworkPolicy{policy("team", metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}}, ingress, networkingv1.NetworkPolicyIngressRule{From: []networkingv1.NetworkPolicyPeer{other}})}, bypass: true},
		{name: "selects other pods", policies: []networkingv1.NetworkPolicy{policy("db", metav1.LabelSelector{MatchLabels: map[string]string{"cnpg.io/cluster": "pg"}}, ingress, networkingv1.NetworkPolicyIngressRule{})}},
		{name: "egress only", policies: []networkingv1.NetworkPolicy{policy("egress", every, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress})}},
		{name: "deny all ingress (no rule)", policies: []networkingv1.NetworkPolicy{policy("deny", every, ingress)}},
		{name: "the exposure's own peer, elsewhere", policies: []networkingv1.NetworkPolicy{policy("nginx", every, ingress, networkingv1.NetworkPolicyIngressRule{From: []networkingv1.NetworkPolicyPeer{nginxPeer}})}},
		{name: "the probe port only", policies: []networkingv1.NetworkPolicy{policy("probes", every, ingress, networkingv1.NetworkPolicyIngressRule{Ports: []networkingv1.NetworkPolicyPort{port(intstr.FromString("probes"), nil, nil)}})}},
		{name: "another number", policies: []networkingv1.NetworkPolicy{policy("n", every, ingress, networkingv1.NetworkPolicyIngressRule{Ports: []networkingv1.NetworkPolicyPort{port(intstr.FromInt32(9090), nil, &tcp)}})}},
		{name: "the http port over UDP", policies: []networkingv1.NetworkPolicy{policy("udp", every, ingress, networkingv1.NetworkPolicyIngressRule{Ports: []networkingv1.NetworkPolicyPort{port(intstr.FromString("http"), nil, &udp)}})}},
		{name: "the http port by name", policies: []networkingv1.NetworkPolicy{policy("name", every, ingress, networkingv1.NetworkPolicyIngressRule{From: []networkingv1.NetworkPolicyPeer{other}, Ports: []networkingv1.NetworkPolicyPort{port(intstr.FromString("http"), nil, nil)}})}, bypass: true},
		{name: "the http port by number", policies: []networkingv1.NetworkPolicy{policy("num", every, ingress, networkingv1.NetworkPolicyIngressRule{From: []networkingv1.NetworkPolicyPeer{other}, Ports: []networkingv1.NetworkPolicyPort{port(intstr.FromInt32(httpPort), nil, &tcp)}})}, bypass: true},
		{name: "the http port in a range", policies: []networkingv1.NetworkPolicy{policy("range", every, ingress, networkingv1.NetworkPolicyIngressRule{From: []networkingv1.NetworkPolicyPeer{other}, Ports: []networkingv1.NetworkPolicyPort{port(intstr.FromInt32(1024), new(int32(65535)), nil)}})}, bypass: true},
		{name: "a protocol-only port", policies: []networkingv1.NetworkPolicy{policy("tcp", every, ingress, networkingv1.NetworkPolicyIngressRule{From: []networkingv1.NetworkPolicyPeer{other}, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp}}})}, bypass: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := policyBypass(tc.policies, "console-ingress", podLabels, []networkingv1.NetworkPolicyPeer{nginxPeer})
			if (got != "") != tc.bypass {
				t.Errorf("policyBypass = %q, want bypass %v", got, tc.bypass)
			}
		})
	}
}

// TestExposureConfinedReasons: a broad peer and another NetworkPolicy each
// turn ExposureConfined False with their own reason, and the any-peer
// trust off.
func TestExposureConfinedReasons(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		from   []pgtoolboxv1alpha1.PgToolBoxConsolePolicyPeer
		bypass string
		status metav1.ConditionStatus
		reason pgtoolboxv1alpha1.Reason
	}{
		{"confined", []pgtoolboxv1alpha1.PgToolBoxConsolePolicyPeer{nginxSpecPeer}, "", metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected},
		{"broad", []pgtoolboxv1alpha1.PgToolBoxConsolePolicyPeer{{NamespaceSelector: &metav1.LabelSelector{}}}, "", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonExposurePeersBroad},
		{"bypassed", []pgtoolboxv1alpha1.PgToolBoxConsolePolicyPeer{nginxSpecPeer}, "NetworkPolicy allow-all admits every source to the console port", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonExposurePolicyBypassed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "console", Namespace: "team-a"}}
			c.Spec.Exposure.Type, c.Spec.Exposure.Hostname = pgtoolboxv1alpha1.ExposureTypeIngress, "console.example"
			c.Spec.Exposure.NetworkPolicy = &pgtoolboxv1alpha1.PgToolBoxConsoleExposurePolicySpec{From: tc.from}
			fact := exposureConfinedFact(c, true, tc.bypass)
			if fact == nil || fact.Status != tc.status || fact.Reason != tc.reason {
				t.Fatalf("ExposureConfined = %+v, want %s/%s", fact, tc.status, tc.reason)
			}
			trusted := strings.Contains(fact.Message, "from peers in 0.0.0.0/0,::/0")
			if trusted != (tc.status == metav1.ConditionTrue) {
				t.Errorf("message %q: every peer trusted = %v", fact.Message, trusted)
			}
			if tc.bypass != "" && !strings.Contains(fact.Message, tc.bypass) {
				t.Errorf("message %q does not name the other policy", fact.Message)
			}
		})
	}
}

// TestRolloutComplete: the policy is withdrawn only after the rollout.
func TestRolloutComplete(t *testing.T) {
	t.Parallel()
	done := workloadFacts{Kind: "StatefulSet", Generation: 3, ObservedGeneration: 3, Desired: 2, Replicas: 2, Updated: 2, Available: 2}
	cases := []struct {
		name   string
		mutate func(*workloadFacts)
		want   bool
	}{
		{"complete", func(*workloadFacts) {}, true},
		{"generation not seen", func(w *workloadFacts) { w.ObservedGeneration = 2 }, false},
		{"an old pod left", func(w *workloadFacts) { w.Replicas = 3 }, false},
		{"one not updated", func(w *workloadFacts) { w.Updated = 1 }, false},
		{"one not available", func(w *workloadFacts) { w.Available = 1 }, false},
		{"revisions differ", func(w *workloadFacts) { w.RevisionsDiffer = true }, false},
		{"nothing observed", func(w *workloadFacts) { *w = workloadFacts{} }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := done
			tc.mutate(&w)
			if got := w.rolloutComplete(); got != tc.want {
				t.Errorf("rolloutComplete = %v, want %v", got, tc.want)
			}
		})
	}
}
