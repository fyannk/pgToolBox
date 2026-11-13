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

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
				{PodSelector: &pgtoolboxv1alpha1.PgToolBoxConsoleLabelSelector{MatchLabels: map[string]pgtoolboxv1alpha1.PgToolBoxConsoleLabelValue{"app": "mcp-client"}}},
				{NamespaceSelector: &pgtoolboxv1alpha1.PgToolBoxConsoleLabelSelector{MatchLabels: map[string]pgtoolboxv1alpha1.PgToolBoxConsoleLabelValue{openShiftIngressPolicyGroup: ""}}},
			}}
		},
		"ingress-from": func(c *pgtoolboxv1alpha1.PgToolBoxConsole) {
			c.Spec.Exposure.Type = pgtoolboxv1alpha1.ExposureTypeIngress
			c.Spec.Exposure.NetworkPolicy = &pgtoolboxv1alpha1.PgToolBoxConsoleExposurePolicySpec{From: []pgtoolboxv1alpha1.PgToolBoxConsolePolicyPeer{
				{
					NamespaceSelector: &pgtoolboxv1alpha1.PgToolBoxConsoleLabelSelector{MatchLabels: map[string]pgtoolboxv1alpha1.PgToolBoxConsoleLabelValue{"kubernetes.io/metadata.name": "ingress-nginx"}},
					PodSelector:       &pgtoolboxv1alpha1.PgToolBoxConsoleLabelSelector{MatchLabels: map[string]pgtoolboxv1alpha1.PgToolBoxConsoleLabelValue{"app.kubernetes.io/name": "ingress-nginx"}},
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
		{name: "route confined", typ: pgtoolboxv1alpha1.ExposureTypeRoute, exposed: true, fact: "AsExpected", message: "X-Forwarded-For is ignored: trustedProxy.cidrs is not set"},
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
			fact := exposureConfinedFact(c, tc.exposed)
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
		NamespaceSelector: &pgtoolboxv1alpha1.PgToolBoxConsoleLabelSelector{MatchLabels: map[string]pgtoolboxv1alpha1.PgToolBoxConsoleLabelValue{"kubernetes.io/metadata.name": "ingress-nginx"}},
		PodSelector:       &pgtoolboxv1alpha1.PgToolBoxConsoleLabelSelector{MatchLabels: map[string]pgtoolboxv1alpha1.PgToolBoxConsoleLabelValue{"app.kubernetes.io/name": "ingress-nginx"}},
	}
	nginxPeer = nginxSpecPeer.NetworkPolicyPeer()
)

// expr is a selector of one expression.
func expr(key string, op metav1.LabelSelectorOperator, values ...string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: key, Operator: op, Values: values}}}
}

// TestBroadPeer: a peer that admits (nearly) every namespace, every Pod of
// the namespace, the Pod network or every address is not a proxy; the
// narrow ones are. A selector is narrow only when it pins a label value.
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
		{"a namespace by In", networkingv1.NetworkPolicyPeer{NamespaceSelector: expr("kubernetes.io/metadata.name", metav1.LabelSelectorOpIn, "ingress-nginx")}, false},
		{"pods by In alone", networkingv1.NetworkPolicyPeer{PodSelector: expr("app", metav1.LabelSelectorOpIn, "proxy")}, false},
		{"a /16", networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "10.1.0.0/16"}}, false},
		{"a public /8", networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "11.0.0.0/8"}}, false},
		{"a /12 inside 10/8", networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "10.128.0.0/12"}}, false},
		{"an IPv6 /32", networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "2001:db8::/32"}}, false},
		{"every namespace with the metadata name", networkingv1.NetworkPolicyPeer{NamespaceSelector: expr("kubernetes.io/metadata.name", metav1.LabelSelectorOpExists)}, true},
		{"every namespace but one", networkingv1.NetworkPolicyPeer{NamespaceSelector: expr("kubernetes.io/metadata.name", metav1.LabelSelectorOpNotIn, "kube-system")}, true},
		{"namespaces without a label", networkingv1.NetworkPolicyPeer{NamespaceSelector: expr("restricted", metav1.LabelSelectorOpDoesNotExist), PodSelector: some}, true},
		{"every labelled pod of the namespace", networkingv1.NetworkPolicyPeer{PodSelector: expr("app", metav1.LabelSelectorOpExists)}, true},
		{"pods but one", networkingv1.NetworkPolicyPeer{PodSelector: expr("app", metav1.LabelSelectorOpNotIn, "db")}, true},
		{"half of IPv4", networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/1"}}, true},
		{"the other half", networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "128.0.0.0/1"}}, true},
		{"a /7", networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "10.0.0.0/7"}}, true},
		{"all of 10/8", networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "10.0.0.0/8", Except: []string{"10.244.0.0/16"}}}, true},
		{"all of 172.16/12", networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "172.16.0.0/12"}}, true},
		{"all of 192.168/16", networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "192.168.0.0/16"}}, true},
		{"the IPv6 unique local range", networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "fc00::/7"}}, true},
		{"an IPv6 /31", networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "2001:db8::/31"}}, true},
		{"every namespace", networkingv1.NetworkPolicyPeer{NamespaceSelector: all}, true},
		{"labelled pods of every namespace", networkingv1.NetworkPolicyPeer{NamespaceSelector: all, PodSelector: some}, true},
		{"every pod of the namespace", networkingv1.NetworkPolicyPeer{PodSelector: all}, true},
		{"every IPv4 address", networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{"10.0.0.0/8"}}}, true},
		{"every IPv6 address", networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "::/0"}}, true},
		// IPv6 Pod networks: unique-local blocks wider than a /64 (the
		// fc00::/7 entry of the IPv4 rule could never match: no IPv6
		// block wider than a /32 gets that far).
		{"a kind IPv6 Pod network /48", networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "fd00:10:244::/48"}}, true},
		{"a unique-local /56", networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "fd12:3456:789a:100::/56"}}, true},
		{"a unique-local /32", networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "fd00::/32"}}, true},
		{"a unique-local /64", networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "fd00:10:244:1::/64"}}, false},
		{"a unique-local /120", networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "fd00:10:244:1::/120"}}, false},
		{"a global IPv6 /48", networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "2001:db8:1::/48"}}, false},
		// Namespace labels many namespaces share pin nothing of the
		// proxies'.
		{"namespaces of a Pod Security level", networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"pod-security.kubernetes.io/enforce": "restricted"}}}, true},
		{"namespaces of a Pod Security level by In", networkingv1.NetworkPolicyPeer{NamespaceSelector: expr("pod-security.kubernetes.io/audit", metav1.LabelSelectorOpIn, "restricted", "baseline")}, true},
		{"namespaces of a level, labelled pods", networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"pod-security.kubernetes.io/enforce": "restricted"}}, PodSelector: some}, true},
		{"namespaces with the SCC label sync", networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"security.openshift.io/scc.podSecurityLabelSync": "true"}}}, true},
		{"namespaces of a run level", networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"openshift.io/run-level": "0"}}}, true},
		{"a named namespace of a level", networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"pod-security.kubernetes.io/enforce": "restricted", "kubernetes.io/metadata.name": "ingress-nginx"}}}, false},
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

// TestExposureConfinedReasons: a broad peer turns ExposureConfined False,
// and the message never claims a trusted peer without trustedProxy.cidrs.
func TestExposureConfinedReasons(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		from    []pgtoolboxv1alpha1.PgToolBoxConsolePolicyPeer
		cidrs   []string
		status  metav1.ConditionStatus
		reason  pgtoolboxv1alpha1.Reason
		message string
	}{
		{"confined, header ignored", []pgtoolboxv1alpha1.PgToolBoxConsolePolicyPeer{nginxSpecPeer}, nil, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected, "X-Forwarded-For is ignored: trustedProxy.cidrs is not set"},
		{"confined, cidrs set", []pgtoolboxv1alpha1.PgToolBoxConsolePolicyPeer{nginxSpecPeer}, []string{"10.1.0.0/16"}, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected, "from peers in 10.1.0.0/16"},
		{"broad", []pgtoolboxv1alpha1.PgToolBoxConsolePolicyPeer{{NamespaceSelector: &pgtoolboxv1alpha1.PgToolBoxConsoleLabelSelector{}}}, nil, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonExposurePeersBroad, "X-Forwarded-For is ignored"},
		{"open", nil, nil, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonExposureOpen, "X-Forwarded-For is ignored"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "console", Namespace: "team-a"}}
			c.Spec.Exposure.Type, c.Spec.Exposure.Hostname = pgtoolboxv1alpha1.ExposureTypeIngress, "console.example"
			if tc.from != nil {
				c.Spec.Exposure.NetworkPolicy = &pgtoolboxv1alpha1.PgToolBoxConsoleExposurePolicySpec{From: tc.from}
			}
			if tc.cidrs != nil {
				c.Spec.Exposure.TrustedProxy = &pgtoolboxv1alpha1.PgToolBoxConsoleTrustedProxySpec{CIDRs: tc.cidrs}
			}
			fact := exposureConfinedFact(c, true)
			if fact == nil || fact.Status != tc.status || fact.Reason != tc.reason {
				t.Fatalf("ExposureConfined = %+v, want %s/%s", fact, tc.status, tc.reason)
			}
			if !strings.Contains(fact.Message, tc.message) {
				t.Errorf("message %q does not say %q", fact.Message, tc.message)
			}
			if strings.Contains(fact.Message, "0.0.0.0/0") {
				t.Errorf("message %q trusts every peer", fact.Message)
			}
		})
	}
	if exposureConfinedFact(&pgtoolboxv1alpha1.PgToolBoxConsole{}, false) != nil {
		t.Error("ExposureConfined is set with nothing published")
	}
}
