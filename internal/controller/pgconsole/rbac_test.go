/*
Copyright © contributors to the pgtoolbox project.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package pgconsole

import (
	"context"
	"reflect"
	"strings"
	"testing"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ruleKey identifies a policy rule the way a reader of the manifest would:
// group, resources, verbs.
func ruleKey(rule rbacv1.PolicyRule) string {
	return strings.Join(rule.APIGroups, "|") + " " +
		strings.Join(rule.Resources, ",") + " " +
		strings.Join(rule.Verbs, ",")
}

func ruleKeys(rules []rbacv1.PolicyRule) map[string]bool {
	keys := map[string]bool{}
	for _, rule := range rules {
		keys[ruleKey(rule)] = true
	}
	return keys
}

func buildReadRole(t *testing.T, console *pgtoolboxv1alpha1.PgConsole) *rbacv1.Role {
	t.Helper()
	r, _ := newTestReconciler(t)
	role, err := r.readRole(console)
	if err != nil {
		t.Fatalf("build read role: %v", err)
	}
	return role
}

// TestReadRoleCoversTheConsoleManifest is the alignment test for authority:
// the generated Role must carry every read rule the console application's
// own deploy manifest grants. A resource missing here does not fail the
// console — it renders "not granted" forever, which is worse, because it
// looks like a broken console rather than a missing grant.
func TestReadRoleCoversTheConsoleManifest(t *testing.T) {
	role := buildReadRole(t, testConsole())
	got := ruleKeys(role.Rules)

	// Taken rule for rule from pgconsole's deploy/kubernetes-example.yaml.
	want := []string{
		"postgresql.cnpg.io clusters get",
		"postgresql.cnpg.io clusters watch",
		"postgresql.cnpg.io backups,scheduledbackups get,list,watch",
		"postgresql.cnpg.io poolers get,list,watch",
		"postgresql.cnpg.io databases,databaseroles,publications,subscriptions get,list,watch",
		"apps replicasets,deployments get",
		"postgresql.cnpg.io failoverquorums get",
		"postgresql.cnpg.io failoverquorums watch",
		"postgresql.cnpg.io imagecatalogs get,list,watch",
		" pods get,list,watch",
		" services get,list,watch",
		" persistentvolumeclaims get,list,watch",
		" pods/log get",
		" events list,watch",
		" configmaps,serviceaccounts get,list,watch",
		"policy poddisruptionbudgets get,list,watch",
		"rbac.authorization.k8s.io roles,rolebindings get,list,watch",
		"batch jobs get,list,watch",
	}
	for _, key := range want {
		if !got[key] {
			t.Fatalf("read role is missing the rule %q\nhave: %+v", key, role.Rules)
		}
	}
}

// TestReadRolePinsTheClusterReads holds the scope: the two objects named
// after the cluster are fetched by name, and the watches that RBAC cannot
// pin hold no list verb, so nothing namespace-wide is ever enumerated
// through them.
func TestReadRolePinsTheClusterReads(t *testing.T) {
	role := buildReadRole(t, testConsole())

	pinned := map[string]bool{}
	for _, rule := range role.Rules {
		if len(rule.ResourceNames) == 0 {
			continue
		}
		if !reflect.DeepEqual(rule.ResourceNames, []string{"cluster-1"}) {
			t.Fatalf("rule pinned to %v, want the target cluster", rule.ResourceNames)
		}
		for _, resource := range rule.Resources {
			pinned[resource] = true
		}
	}
	for _, resource := range []string{"clusters", "failoverquorums"} {
		if !pinned[resource] {
			t.Fatalf("%s get is not pinned to the target cluster", resource)
		}
	}

	for _, rule := range role.Rules {
		for _, resource := range rule.Resources {
			if resource != "clusters" && resource != "failoverquorums" {
				continue
			}
			for _, verb := range rule.Verbs {
				if verb == "list" {
					t.Fatalf("%s must never carry a list verb: %+v", resource, rule)
				}
			}
		}
	}
}

// TestReadRoleGrantsNothingOnSecrets holds the console's read-only
// guarantee. RBAC cannot express "metadata only", so the grant is simply
// absent and the children drawing states Secrets as not granted.
func TestReadRoleGrantsNothingOnSecrets(t *testing.T) {
	role := buildReadRole(t, testConsole())
	for _, rule := range role.Rules {
		for _, resource := range rule.Resources {
			if resource == "secrets" {
				t.Fatalf("read role grants %v on secrets", rule.Verbs)
			}
		}
	}
}

// TestReadRoleDropsLogsWithTheCapability proves the flag is not the
// boundary: with the tail switched off, RBAC denies the read too.
func TestReadRoleDropsLogsWithTheCapability(t *testing.T) {
	console := testConsole()
	console.Spec.Console.AllowLogs = ptrTo(false)

	if ruleKeys(buildReadRole(t, console).Rules)[" pods/log get"] {
		t.Fatalf("pods/log granted with the log tail switched off")
	}
	if !ruleKeys(buildReadRole(t, testConsole()).Rules)[" pods/log get"] {
		t.Fatalf("pods/log not granted with the log tail switched on")
	}
}

// TestReadRoleIsClusterScopeFree holds the namespace boundary: the only
// cluster-scoped read a console may hold is the opt-in catalog get, and it
// lives in its own ClusterRole, never in this Role.
func TestReadRoleIsClusterScopeFree(t *testing.T) {
	console := testConsole()
	console.Spec.Console.AllowClusterCatalogs = ptrTo(true)
	for _, rule := range buildReadRole(t, console).Rules {
		for _, resource := range rule.Resources {
			if resource == "clusterimagecatalogs" {
				t.Fatalf("cluster-scoped read leaked into the namespaced Role: %+v", rule)
			}
		}
	}
}

// TestOperateRoleFollowsItsCapabilities: each capability contributes its own
// rules or none, and a console with no write surface gets no Role at all.
func TestOperateRoleFollowsItsCapabilities(t *testing.T) {
	operations := []string{
		"postgresql.cnpg.io backups create",
		"postgresql.cnpg.io clusters patch",
		"postgresql.cnpg.io clusters/status patch",
	}
	review := []string{
		"pgtoolbox.fyannk.dev pgtoolboxaccessrequests get,list,watch",
		"pgtoolbox.fyannk.dev pgtoolboxaccessrequests/status update,patch",
		"pgtoolbox.fyannk.dev pgtoolboxroles get,list,watch",
	}

	tests := []struct {
		name       string
		operations bool
		review     bool
		want       []string
		absent     []string
	}{
		{name: "both", operations: true, review: true, want: append(append([]string{}, operations...), review...)},
		{name: "operations only", operations: true, want: operations, absent: review},
		{name: "review only", review: true, want: review, absent: operations},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			console := testConsole()
			console.Spec.Console.AllowOperations = ptrTo(test.operations)
			console.Spec.Console.AllowAccessReview = ptrTo(test.review)

			got := ruleKeys(operateRules(console))
			for _, key := range test.want {
				if !got[key] {
					t.Fatalf("operate rules missing %q: %+v", key, operateRules(console))
				}
			}
			for _, key := range test.absent {
				if got[key] {
					t.Fatalf("operate rules carry %q for a disabled capability", key)
				}
			}
		})
	}

	t.Run("neither", func(t *testing.T) {
		console := testConsole()
		console.Spec.Console.AllowOperations = ptrTo(false)
		console.Spec.Console.AllowAccessReview = ptrTo(false)

		if rules := operateRules(console); len(rules) != 0 {
			t.Fatalf("a console with no write surface has rules: %+v", rules)
		}
		r, _ := newTestReconciler(t)
		role, err := r.operateRole(console)
		if err != nil {
			t.Fatalf("build operate role: %v", err)
		}
		if role != nil {
			t.Fatalf("expected no operate Role, got %+v", role)
		}
	})
}

// TestOperateRoleGrantsPatchOnTheDecision is the second alignment point with
// the console: it writes the reviewer's decision as a merge patch, and RBAC
// names the HTTP verb rather than the intent, so a Role granting update
// alone refuses the write.
func TestOperateRoleGrantsPatchOnTheDecision(t *testing.T) {
	for _, rule := range operateRules(testConsole()) {
		if !reflect.DeepEqual(rule.Resources, []string{"pgtoolboxaccessrequests/status"}) {
			continue
		}
		for _, verb := range rule.Verbs {
			if verb == "patch" {
				return
			}
		}
		t.Fatalf("decision rule verbs = %v, want patch among them", rule.Verbs)
	}
	t.Fatalf("no rule on pgtoolboxaccessrequests/status")
}

// TestReconcileRBACRemovesDisabledAuthority: switching a capability off on a
// living console withdraws the objects, it does not leave them behind.
func TestReconcileRBACRemovesDisabledAuthority(t *testing.T) {
	ctx := context.Background()
	console := testConsole()
	r, c := newTestReconciler(t, console, testCluster())
	reconcileToSteadyState(t, r)

	operateKey := client.ObjectKey{Namespace: "test", Name: "console-pgconsole-operate"}
	getObject(t, c, operateKey, &rbacv1.Role{})

	var live pgtoolboxv1alpha1.PgConsole
	getObject(t, c, consoleKey, &live)
	live.Spec.Console.AllowOperations = ptrTo(false)
	live.Spec.Console.AllowAccessReview = ptrTo(false)
	if err := c.Update(ctx, &live); err != nil {
		t.Fatalf("update console: %v", err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: consoleKey}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if err := c.Get(ctx, operateKey, &rbacv1.Role{}); !apierrors.IsNotFound(err) {
		t.Fatalf("operate Role survived both capabilities being switched off: %v", err)
	}
	if err := c.Get(ctx, operateKey, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("operate RoleBinding survived: %v", err)
	}
}

// TestReconcileCatalogClusterRBACLifecycle covers the one cluster-scoped
// grant end to end. It cannot be owned by the namespaced console, so nothing
// but this controller removes it — switching the capability off must, and so
// must deleting the console.
func TestReconcileCatalogClusterRBACLifecycle(t *testing.T) {
	ctx := context.Background()
	console := testConsole()
	console.Spec.Console.AllowClusterCatalogs = ptrTo(true)
	r, c := newTestReconciler(t, console, testCluster())
	reconcileToSteadyState(t, r)

	catalogKey := client.ObjectKey{Name: "test-console-pgconsole-catalogs"}
	var clusterRole rbacv1.ClusterRole
	getObject(t, c, catalogKey, &clusterRole)
	if len(clusterRole.Rules) != 1 ||
		!reflect.DeepEqual(clusterRole.Rules[0].Resources, []string{"clusterimagecatalogs"}) ||
		!reflect.DeepEqual(clusterRole.Rules[0].Verbs, []string{"get"}) {
		t.Fatalf("catalog ClusterRole grants %+v, want a single clusterimagecatalogs get", clusterRole.Rules)
	}

	var binding rbacv1.ClusterRoleBinding
	getObject(t, c, catalogKey, &binding)
	if len(binding.Subjects) != 1 || binding.Subjects[0].Name != "console-pgconsole" ||
		binding.Subjects[0].Namespace != "test" {
		t.Fatalf("catalog binding subjects = %+v", binding.Subjects)
	}

	// Switching the capability off withdraws the cluster-scoped grant.
	var live pgtoolboxv1alpha1.PgConsole
	getObject(t, c, consoleKey, &live)
	live.Spec.Console.AllowClusterCatalogs = ptrTo(false)
	if err := c.Update(ctx, &live); err != nil {
		t.Fatalf("update console: %v", err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: consoleKey}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := c.Get(ctx, catalogKey, &rbacv1.ClusterRole{}); !apierrors.IsNotFound(err) {
		t.Fatalf("catalog ClusterRole survived the capability being switched off: %v", err)
	}
	if err := c.Get(ctx, catalogKey, &rbacv1.ClusterRoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("catalog ClusterRoleBinding survived: %v", err)
	}
}

// TestDeletionCollectsTheCatalogClusterRBAC: the finalizer is what stops the
// cluster-scoped pair outliving the console that asked for it.
func TestDeletionCollectsTheCatalogClusterRBAC(t *testing.T) {
	ctx := context.Background()
	console := testConsole()
	console.Spec.Console.AllowClusterCatalogs = ptrTo(true)
	r, c := newTestReconciler(t, console, testCluster())
	reconcileToSteadyState(t, r)

	catalogKey := client.ObjectKey{Name: "test-console-pgconsole-catalogs"}
	getObject(t, c, catalogKey, &rbacv1.ClusterRole{})

	var live pgtoolboxv1alpha1.PgConsole
	getObject(t, c, consoleKey, &live)
	if err := c.Delete(ctx, &live); err != nil {
		t.Fatalf("delete console: %v", err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: consoleKey}); err != nil {
		t.Fatalf("deletion reconcile: %v", err)
	}

	if err := c.Get(ctx, catalogKey, &rbacv1.ClusterRole{}); !apierrors.IsNotFound(err) {
		t.Fatalf("catalog ClusterRole outlived the console: %v", err)
	}
	if err := c.Get(ctx, catalogKey, &rbacv1.ClusterRoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("catalog ClusterRoleBinding outlived the console: %v", err)
	}
}

// TestCatalogClusterRoleNameCarriesTheNamespace: cluster-scoped names share
// one namespace across the cluster, so two consoles called "main" in
// different namespaces must not collide.
func TestCatalogClusterRoleNameCarriesTheNamespace(t *testing.T) {
	first := testConsole()
	first.Name, first.Namespace = "main", "payments"
	second := testConsole()
	second.Name, second.Namespace = "main", "orders"

	if catalogClusterRoleName(first) == catalogClusterRoleName(second) {
		t.Fatalf("two consoles named main collide on %q", catalogClusterRoleName(first))
	}
}
