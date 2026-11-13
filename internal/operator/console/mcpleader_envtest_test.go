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
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// TestMCPLeaderOnEnvtest: with MCP on, the operator (holding only its
// generated ClusterRole) creates the leader's Lease, Service and slices and
// grants the console exactly get and update on them by name; a reconcile
// leaves what the leader wrote; MCP off withdraws the Service and the grant.
func TestMCPLeaderOnEnvtest(t *testing.T) {
	s := newEnvtestSuite(t)
	ctx := context.Background()
	s.installShippedRoles(ctx, t)
	console := &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "console", Namespace: suiteNamespace}}
	console.Spec.PublicURL = "https://console.example"
	console.Spec.MCP = &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{Enabled: true}
	must(t, s.admin.Create(ctx, console))
	s.reconcile(ctx, t, "console")
	console = s.console(ctx, t, "console")
	name := console.MCPLeaseName()

	s.get(ctx, t, name, &coordinationv1.Lease{})
	service := &corev1.Service{}
	s.get(ctx, t, name, service)
	if service.Spec.Selector != nil {
		t.Fatalf("the MCP Service selects %v: the leader's slices alone must back it", service.Spec.Selector)
	}
	for _, slice := range mcpSliceNames(console) {
		s.get(ctx, t, slice, &discoveryv1.EndpointSlice{})
	}

	allowed := func(verb, group, resource, name string) bool {
		t.Helper()
		review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   "system:serviceaccount:" + suiteNamespace + ":" + console.WorkloadName(),
			Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + suiteNamespace, "system:authenticated"},
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: suiteNamespace, Verb: verb, Group: group, Resource: resource, Name: name,
			},
		}}
		must(t, s.admin.Create(ctx, review))
		return review.Status.Allowed
	}
	for _, tc := range []struct {
		verb, group, resource, name string
		want                        bool
	}{
		{"update", "discovery.k8s.io", "endpointslices", name + "-ipv4", true},
		{"update", "discovery.k8s.io", "endpointslices", name + "-ipv6", true},
		{"get", "discovery.k8s.io", "endpointslices", name + "-ipv4", true},
		{"update", "coordination.k8s.io", "leases", name, true},
		{"update", "discovery.k8s.io", "endpointslices", "console-ipv4", false},
		{"create", "discovery.k8s.io", "endpointslices", "", false},
		{"patch", "discovery.k8s.io", "endpointslices", name + "-ipv4", false},
		{"delete", "discovery.k8s.io", "endpointslices", name + "-ipv4", false},
		{"list", "discovery.k8s.io", "endpointslices", "", false},
		{"update", "coordination.k8s.io", "leases", "another", false},
		{"patch", "", "pods", "console-0", false},
		{"update", "", "pods", "console-0", false},
	} {
		if got := allowed(tc.verb, tc.group, tc.resource, tc.name); got != tc.want {
			t.Errorf("the console may %s %s.%s %q: %v, want %v", tc.verb, tc.resource, tc.group, tc.name, got, tc.want)
		}
	}

	// The leader's write, as the console, survives a reconcile.
	consoleClient := s.asConsole(t, console)
	slice := &discoveryv1.EndpointSlice{}
	must(t, consoleClient.Get(ctx, client.ObjectKey{Namespace: suiteNamespace, Name: name + "-ipv4"}, slice))
	slice.Endpoints = []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.7"}}}
	must(t, consoleClient.Update(ctx, slice))
	s.reconcile(ctx, t, "console")
	s.get(ctx, t, name+"-ipv4", slice)
	if len(slice.Endpoints) != 1 || slice.Endpoints[0].Addresses[0] != "10.0.0.7" {
		t.Fatalf("a reconcile changed the leader's slice: %+v", slice.Endpoints)
	}

	// MCP off: no Service, no grant; the Lease and slices go with the console.
	console = s.console(ctx, t, "console")
	console.Spec.MCP.Enabled = false
	must(t, s.admin.Update(ctx, console))
	s.reconcile(ctx, t, "console")
	s.absent(ctx, t, name, &corev1.Service{})
	s.absent(ctx, t, name, &rbacv1.Role{})
	s.absent(ctx, t, name, &rbacv1.RoleBinding{})
	if allowed("update", "discovery.k8s.io", "endpointslices", name+"-ipv4") {
		t.Error("the console may still update its slice with MCP off")
	}
}
