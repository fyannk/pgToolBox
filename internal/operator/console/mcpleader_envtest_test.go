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
	"slices"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// TestMCPLeaderOnEnvtest: with MCP on, the operator (holding only its
// generated ClusterRole) creates the MCP Lease and the Service selecting
// the holder's label, and grants the console get and update on that Lease
// by name and nothing on Pods; it labels the Pod the Lease names, moves the
// label when the holder changes, and takes it off with MCP off.
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
	if service.Spec.Selector[pgtoolboxv1alpha1.LabelMCPLeader] != pgtoolboxv1alpha1.MCPLeaderLabelValue {
		t.Fatalf("the MCP Service selects %v: want the holder's label", service.Spec.Selector)
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
		{"get", "coordination.k8s.io", "leases", name, true},
		{"update", "coordination.k8s.io", "leases", name, true},
		{"update", "coordination.k8s.io", "leases", "another", false},
		{"create", "coordination.k8s.io", "leases", "", false},
		{"delete", "coordination.k8s.io", "leases", name, false},
		{"patch", "", "pods", "console-a", false},
		{"update", "", "pods", "console-a", false},
		{"create", "discovery.k8s.io", "endpointslices", "", false},
	} {
		if got := allowed(tc.verb, tc.group, tc.resource, tc.name); got != tc.want {
			t.Errorf("the console may %s %s.%s %q: %v, want %v", tc.verb, tc.resource, tc.group, tc.name, got, tc.want)
		}
	}

	// Two console Pods, as the Deployment would make them.
	for _, pod := range []string{"console-a", "console-b"} {
		must(t, s.admin.Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: pod, Namespace: suiteNamespace, Labels: selectorLabels(console)},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "console", Image: "registry.example/pgtoolbox-console:1.2.0"}}},
		}))
	}
	labelled := func() []string {
		t.Helper()
		var pods corev1.PodList
		must(t, s.admin.List(ctx, &pods, client.InNamespace(suiteNamespace),
			client.MatchingLabels{pgtoolboxv1alpha1.LabelMCPLeader: pgtoolboxv1alpha1.MCPLeaderLabelValue}))
		names := make([]string, 0, len(pods.Items))
		for _, pod := range pods.Items {
			names = append(names, pod.Name)
		}
		return names
	}
	// The console takes the Lease, as its elector does.
	consoleClient := s.asConsole(t, console)
	hold := func(pod string) {
		t.Helper()
		lease := &coordinationv1.Lease{}
		must(t, consoleClient.Get(ctx, client.ObjectKey{Namespace: suiteNamespace, Name: name}, lease))
		lease.Spec = coordinationv1.LeaseSpec{
			HolderIdentity: new(pod + "_0a1b2c3d4e5f"), LeaseDurationSeconds: new(int32(15)),
			AcquireTime: &metav1.MicroTime{Time: time.Now()}, RenewTime: &metav1.MicroTime{Time: time.Now()},
		}
		must(t, consoleClient.Update(ctx, lease))
	}
	hold("console-a")
	s.reconcile(ctx, t, "console")
	if got := labelled(); !slices.Equal(got, []string{"console-a"}) {
		t.Fatalf("labelled %v, want console-a", got)
	}
	hold("console-b")
	s.reconcile(ctx, t, "console")
	if got := labelled(); !slices.Equal(got, []string{"console-b"}) {
		t.Fatalf("labelled %v after the hand-over, want console-b", got)
	}

	// MCP off: no Service, no grant, no label; the Lease goes with the console.
	console = s.console(ctx, t, "console")
	console.Spec.MCP.Enabled = false
	must(t, s.admin.Update(ctx, console))
	s.reconcile(ctx, t, "console")
	s.absent(ctx, t, name, &corev1.Service{})
	s.absent(ctx, t, name, &rbacv1.Role{})
	s.absent(ctx, t, name, &rbacv1.RoleBinding{})
	if got := labelled(); len(got) != 0 {
		t.Errorf("labelled %v with MCP off", got)
	}
	if allowed("update", "coordination.k8s.io", "leases", name) {
		t.Error("the console may still update the MCP Lease with MCP off")
	}
}
