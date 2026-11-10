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
		"ingress-from": func(c *pgtoolboxv1alpha1.PgToolBoxConsole) {
			c.Spec.Exposure.Type = pgtoolboxv1alpha1.ExposureTypeIngress
			c.Spec.Exposure.NetworkPolicy = &pgtoolboxv1alpha1.PgToolBoxConsoleExposurePolicySpec{From: []networkingv1.NetworkPolicyPeer{
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
