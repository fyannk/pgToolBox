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
	"maps"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// TestMCPLeaderOnEnvtest: with MCP on, the operator (holding only its
// generated ClusterRole, which grants nothing on Pods but reading them)
// creates the MCP Lease, the one-replica MCP Deployment, the only
// candidate for that Lease, and the Service selecting its Pod alone, and
// grants the console get and update on that Lease by name and nothing on
// Pods; the console's own replicas never campaign. With MCP off the
// Deployment, the Service and the grant go.
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
	mcp := &appsv1.Deployment{}
	s.get(ctx, t, name, mcp)
	if *mcp.Spec.Replicas != 1 || !maps.Equal(mcp.Spec.Selector.MatchLabels, service.Spec.Selector) ||
		service.Spec.Selector[componentLabel] != componentMCP {
		t.Fatalf("MCP Deployment of %d replicas selecting %v, its Service selects %v", *mcp.Spec.Replicas, mcp.Spec.Selector.MatchLabels, service.Spec.Selector)
	}
	candidate := func(envs []corev1.EnvVar) string {
		for _, e := range envs {
			if e.Name == "MCP_CANDIDATE" {
				return e.Value
			}
		}
		return "unset"
	}
	if got := candidate(mcp.Spec.Template.Spec.Containers[0].Env); got != "unset" {
		t.Errorf("the MCP Pod's MCP_CANDIDATE=%s, want it unset (a candidate)", got)
	}
	replicas := &appsv1.StatefulSet{}
	s.get(ctx, t, console.WorkloadName(), replicas)
	if got := candidate(replicas.Spec.Template.Spec.Containers[0].Env); got != "false" {
		t.Errorf("the console's replicas have MCP_CANDIDATE=%s, want false", got)
	}

	allowed := func(user, verb, group, resource, name string) bool {
		t.Helper()
		review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   user,
			Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + suiteNamespace, "system:authenticated"},
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: suiteNamespace, Verb: verb, Group: group, Resource: resource, Name: name,
			},
		}}
		must(t, s.admin.Create(ctx, review))
		return review.Status.Allowed
	}
	consoleUser := "system:serviceaccount:" + suiteNamespace + ":" + console.WorkloadName()
	for _, tc := range []struct {
		user, verb, group, resource, name string
		want                              bool
	}{
		{consoleUser, "get", "coordination.k8s.io", "leases", name, true},
		{consoleUser, "update", "coordination.k8s.io", "leases", name, true},
		{consoleUser, "update", "coordination.k8s.io", "leases", "another", false},
		{consoleUser, "create", "coordination.k8s.io", "leases", "", false},
		{consoleUser, "delete", "coordination.k8s.io", "leases", name, false},
		{consoleUser, "patch", "", "pods", "console-a", false},
		{consoleUser, "update", "", "pods", "console-a", false},
		{consoleUser, "create", "discovery.k8s.io", "endpointslices", "", false},
		// The operator labels no Pod and follows no Lease.
		{operatorUser, "patch", "", "pods", "console-a", false},
		{operatorUser, "update", "", "pods", "console-a", false},
		{operatorUser, "watch", "coordination.k8s.io", "leases", "", false},
		{operatorUser, "list", "coordination.k8s.io", "leases", "", false},
	} {
		if got := allowed(tc.user, tc.verb, tc.group, tc.resource, tc.name); got != tc.want {
			t.Errorf("%s may %s %s.%s %q: %v, want %v", tc.user, tc.verb, tc.resource, tc.group, tc.name, got, tc.want)
		}
	}

	// MCP off: no MCP Pod, no Service, no grant; the Lease goes with the console.
	console = s.console(ctx, t, "console")
	console.Spec.MCP.Enabled = false
	must(t, s.admin.Update(ctx, console))
	s.reconcile(ctx, t, "console")
	s.absent(ctx, t, name, &appsv1.Deployment{})
	s.absent(ctx, t, name, &corev1.Service{})
	s.absent(ctx, t, name, &rbacv1.Role{})
	s.absent(ctx, t, name, &rbacv1.RoleBinding{})
	if allowed(consoleUser, "update", "coordination.k8s.io", "leases", name) {
		t.Error("the console may still update the MCP Lease with MCP off")
	}
	replicas = &appsv1.StatefulSet{}
	s.get(ctx, t, console.WorkloadName(), replicas)
	if slices.ContainsFunc(replicas.Spec.Template.Spec.Containers[0].Env, func(e corev1.EnvVar) bool { return e.Name == "MCP_LEASE" }) {
		t.Error("the console's replicas still name the MCP Lease with MCP off")
	}
}
