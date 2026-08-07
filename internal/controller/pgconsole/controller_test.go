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
	"testing"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	proxyconfig "github.com/fyannk/pgtoolbox/internal/proxy/config"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var consoleKey = client.ObjectKey{Namespace: "test", Name: "console"}

// reconcileToSteadyState runs the reconcile until it stops returning early
// for the finalizer add, then once more for the full pass.
func reconcileToSteadyState(t *testing.T, r *Reconciler) {
	t.Helper()
	request := ctrl.Request{NamespacedName: consoleKey}
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("finalizer reconcile: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("full reconcile: %v", err)
	}
}

func getObject(t *testing.T, c client.Client, key client.ObjectKey, object client.Object) {
	t.Helper()
	if err := c.Get(context.Background(), key, object); err != nil {
		t.Fatalf("get %T %s: %v", object, key, err)
	}
}

func conditionOf(console *pgtoolboxv1alpha1.PgConsole, conditionType string) *metav1.Condition {
	for i := range console.Status.Conditions {
		if console.Status.Conditions[i].Type == conditionType {
			return &console.Status.Conditions[i]
		}
	}
	return nil
}

func userConditionOf(user *pgtoolboxv1alpha1.PgToolBoxUser, conditionType string) *metav1.Condition {
	for i := range user.Status.Conditions {
		if user.Status.Conditions[i].Type == conditionType {
			return &user.Status.Conditions[i]
		}
	}
	return nil
}

func TestReconcileHappyPath(t *testing.T) {
	console := testConsole()
	r, c := newTestReconciler(t, console, testCluster())
	reconcileToSteadyState(t, r)
	base := client.ObjectKey{Namespace: "test", Name: "console-pgconsole"}

	getObject(t, c, base, &appsv1.Deployment{})
	getObject(t, c, base, &corev1.Service{})
	getObject(t, c, base, &corev1.ServiceAccount{})
	getObject(t, c, base, &rbacv1.Role{})
	getObject(t, c, base, &rbacv1.RoleBinding{})
	getObject(t, c, base, &networkingv1.NetworkPolicy{})
	getObject(t, c, client.ObjectKey{Namespace: "test", Name: "console-pgconsole-operate"}, &rbacv1.Role{})
	getObject(t, c, client.ObjectKey{Namespace: "test", Name: "console-pgconsole-operate"}, &rbacv1.RoleBinding{})
	getObject(t, c, client.ObjectKey{Namespace: "test", Name: "console-pgconsole-proxy"}, &corev1.Secret{})
	getObject(t, c, client.ObjectKey{Namespace: "test", Name: "console-pgconsole-pgadmin"}, &corev1.PersistentVolumeClaim{})

	// Every owned child names the console as controller owner.
	var deployment appsv1.Deployment
	getObject(t, c, base, &deployment)
	owner := metav1.GetControllerOf(&deployment)
	if owner == nil || owner.UID != console.UID {
		t.Fatalf("deployment owner = %+v", owner)
	}
	if got := len(deployment.Spec.Template.Spec.Containers); got != 3 {
		t.Fatalf("deployment containers = %d, want 3 (proxy, pgconsole, pgadmin)", got)
	}

	// The access-request flow is the one pgtoolbox-API write the pod holds.
	var readRole rbacv1.Role
	getObject(t, c, base, &readRole)
	accessRequestRule := false
	for _, rule := range readRole.Rules {
		if reflect.DeepEqual(rule.APIGroups, []string{"pgtoolbox.fyannk.dev"}) &&
			reflect.DeepEqual(rule.Resources, []string{"pgtoolboxaccessrequests"}) &&
			reflect.DeepEqual(rule.Verbs, []string{"create"}) {
			accessRequestRule = true
		}
	}
	if !accessRequestRule {
		t.Fatalf("read role lacks the create-pgtoolboxaccessrequests rule: %+v", readRole.Rules)
	}

	var live pgtoolboxv1alpha1.PgConsole
	getObject(t, c, consoleKey, &live)

	if condition := conditionOf(&live, pgtoolboxv1alpha1.PgConsoleConditionClusterReady); condition == nil ||
		condition.Status != metav1.ConditionTrue {
		t.Fatalf("ClusterReady = %+v", condition)
	}
	if condition := conditionOf(&live, pgtoolboxv1alpha1.PgConsoleConditionProxyConfigReady); condition == nil ||
		condition.Status != metav1.ConditionTrue {
		t.Fatalf("ProxyConfigReady = %+v", condition)
	}
	// The fake Deployment never reports a completed rollout.
	if condition := conditionOf(&live, pgtoolboxv1alpha1.PgConsoleConditionReady); condition == nil ||
		condition.Status != metav1.ConditionFalse ||
		condition.Reason != pgtoolboxv1alpha1.ReasonRolloutInProgress {
		t.Fatalf("Ready = %+v", condition)
	}
	if live.Status.ConfigRevision == "" || live.Status.ConfigRevision[:4] != "cfg-" {
		t.Fatalf("configRevision = %q", live.Status.ConfigRevision)
	}
	if live.Status.ObservedGeneration != live.Generation {
		t.Fatalf("observedGeneration = %d, want %d", live.Status.ObservedGeneration, live.Generation)
	}
}

func TestReconcileIsDeterministic(t *testing.T) {
	console := testConsole()
	r, c := newTestReconciler(t, console, testCluster())
	reconcileToSteadyState(t, r)
	ctx := context.Background()
	base := client.ObjectKey{Namespace: "test", Name: "console-pgconsole"}

	snapshot := map[string]client.Object{
		"deployment":     &appsv1.Deployment{},
		"service":        &corev1.Service{},
		"serviceaccount": &corev1.ServiceAccount{},
		"role":           &rbacv1.Role{},
		"rolebinding":    &rbacv1.RoleBinding{},
		"networkpolicy":  &networkingv1.NetworkPolicy{},
		"secret":         &corev1.Secret{},
	}
	keys := map[string]client.ObjectKey{
		"secret": {Namespace: "test", Name: "console-pgconsole-proxy"},
	}
	before := map[string]client.Object{}
	for name, object := range snapshot {
		key := base
		if override, ok := keys[name]; ok {
			key = override
		}
		getObject(t, c, key, object)
		before[name] = object.DeepCopyObject().(client.Object)
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: consoleKey}); err != nil {
		t.Fatalf("second full reconcile: %v", err)
	}

	for name, object := range snapshot {
		key := base
		if override, ok := keys[name]; ok {
			key = override
		}
		getObject(t, c, key, object)
		if !reflect.DeepEqual(before[name], object) {
			t.Fatalf("%s changed across a no-op reconcile", name)
		}
	}

	var live pgtoolboxv1alpha1.PgConsole
	getObject(t, c, consoleKey, &live)
}

func TestReconcileClusterNotFound(t *testing.T) {
	console := testConsole()
	r, c := newTestReconciler(t, console)

	request := ctrl.Request{NamespacedName: consoleKey}
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("finalizer reconcile: %v", err)
	}
	result, err := r.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("a missing cluster must not fail the reconcile: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("a missing cluster must keep retrying, got %+v", result)
	}

	var live pgtoolboxv1alpha1.PgConsole
	getObject(t, c, consoleKey, &live)
	condition := conditionOf(&live, pgtoolboxv1alpha1.PgConsoleConditionClusterReady)
	if condition == nil || condition.Status != metav1.ConditionFalse ||
		condition.Reason != pgtoolboxv1alpha1.ReasonClusterNotFound {
		t.Fatalf("ClusterReady = %+v", condition)
	}

	// Nothing is deployed for a console whose cluster does not exist.
	var deployment appsv1.Deployment
	err = c.Get(context.Background(), client.ObjectKey{Namespace: "test", Name: "console-pgconsole"}, &deployment)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("no Deployment may exist while the cluster is missing, got %v", err)
	}
}

func TestReconcileOpenShiftAuthAccepted(t *testing.T) {
	console := testConsole()
	console.Spec.Proxy.Authentication.Mode = pgtoolboxv1alpha1.ProxyAuthenticationModeOpenShift
	console.Spec.Exposure = pgtoolboxv1alpha1.ExposureSpec{
		Type:     pgtoolboxv1alpha1.ExposureTypeRoute,
		Hostname: "pgconsole.apps.example.com",
	}
	r, c := newTestReconciler(t, console, testCluster())

	reconcileToSteadyState(t, r)

	var live pgtoolboxv1alpha1.PgConsole
	getObject(t, c, consoleKey, &live)
	condition := conditionOf(&live, pgtoolboxv1alpha1.PgConsoleConditionProxyConfigReady)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("ProxyConfigReady = %+v", condition)
	}

	var serviceAccount corev1.ServiceAccount
	getObject(t, c, client.ObjectKey{Namespace: "test", Name: "console-pgconsole"}, &serviceAccount)
	if got := serviceAccount.Annotations[oauthRedirectAnnotation]; got != "https://pgconsole.apps.example.com" {
		t.Fatalf("oauth redirect annotation = %q", got)
	}
}

func TestReconcileSkipsByAnnotation(t *testing.T) {
	console := testConsole()
	console.Annotations = map[string]string{pgtoolboxv1alpha1.ReconcileAnnotation: "skip"}
	r, c := newTestReconciler(t, console, testCluster())
	reconcileToSteadyState(t, r)

	var live pgtoolboxv1alpha1.PgConsole
	getObject(t, c, consoleKey, &live)
	condition := conditionOf(&live, pgtoolboxv1alpha1.PgConsoleConditionProgressing)
	if condition == nil || condition.Reason != pgtoolboxv1alpha1.ReasonReconciliationSkipped {
		t.Fatalf("Progressing = %+v", condition)
	}

	var deployment appsv1.Deployment
	err := c.Get(context.Background(), client.ObjectKey{Namespace: "test", Name: "console-pgconsole"}, &deployment)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("a skipped console must not deploy, got %v", err)
	}
}

// TestReconcileNeverWritesTheCluster pins the hard rule: the CNPG Cluster
// object is read-only for this operator. The fake client cannot enforce
// RBAC, so the assertion is structural — a reconcile leaves the Cluster
// byte-identical, resource version included.

func TestReconcileNeverWritesTheCluster(t *testing.T) {
	console := testConsole()
	cluster := testCluster()
	r, c := newTestReconciler(t, console, cluster)
	reconcileToSteadyState(t, r)

	var live appsv1.Deployment
	getObject(t, c, client.ObjectKey{Namespace: "test", Name: "console-pgconsole"}, &live)

	var afterCluster = testCluster()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(cluster), afterCluster); err != nil {
		t.Fatalf("read cluster: %v", err)
	}
	if afterCluster.ResourceVersion != cluster.ResourceVersion {
		t.Fatalf("the reconcile wrote to the CNPG Cluster (resourceVersion %q → %q)",
			cluster.ResourceVersion, afterCluster.ResourceVersion)
	}
}

func TestReconcileRendersProxyUsersAndSyncsPgAdmin(t *testing.T) {
	console := testConsole()
	cluster := testCluster()
	role := testPgToolBoxRole("viewer", pgtoolboxv1alpha1.RoleLevelView)

	passwordSecret := testPasswordSecret("viewer-pgrole-credentials", "v1")
	databaseRole := testDatabaseRole("viewer-pgrole", "viewer-pgrole-credentials", "v1")

	hash := "$2a$04$1MkEYTirgqR9o.t6dMEyzOoRST1ueBQEAgrb3x8I9RRUD3XwpTBDG"
	localSecret := testLocalPasswordSecret("alice-local-password", hash)
	user := testPgToolBoxUser("alice", "viewer", "alice-local-password")

	r, c := newTestReconciler(t, console, cluster, role, databaseRole, passwordSecret, localSecret, user)
	reconcileToSteadyState(t, r)

	// The proxy config Secret must contain the rendered user with the bcrypt hash.
	var proxySecret corev1.Secret
	getObject(t, c, client.ObjectKey{Namespace: "test", Name: "console-pgconsole-proxy"}, &proxySecret)
	raw, ok := proxySecret.Data[proxyConfigFileName]
	if !ok {
		t.Fatalf("proxy config missing")
	}
	cfg, _, err := proxyconfig.Parse(raw)
	if err != nil {
		t.Fatalf("proxy config parse: %v", err)
	}
	if len(cfg.Users) != 1 {
		t.Fatalf("proxy users = %v", cfg.Users)
	}
	if cfg.Users[0].Subject != "alice@example.com" || cfg.Users[0].Level != proxyconfig.LevelView {
		t.Fatalf("proxy user = %+v", cfg.Users[0])
	}
	if cfg.Users[0].LocalPasswordBcrypt != hash {
		t.Fatalf("proxy user bcrypt not rendered")
	}

	// The PgToolBoxUser status must reflect both proxy and pgAdmin success.
	var liveUser pgtoolboxv1alpha1.PgToolBoxUser
	getObject(t, c, client.ObjectKeyFromObject(user), &liveUser)
	if !liveUser.Status.ProxySynced || !liveUser.Status.PgAdminSynced {
		t.Fatalf("user status = %+v", liveUser.Status)
	}
	if userConditionOf(&liveUser, pgtoolboxv1alpha1.UserConditionProxySynced).Status != metav1.ConditionTrue {
		t.Fatalf("ProxySynced condition = %+v", userConditionOf(&liveUser, pgtoolboxv1alpha1.UserConditionProxySynced))
	}
	if userConditionOf(&liveUser, pgtoolboxv1alpha1.UserConditionPgAdminSynced).Status != metav1.ConditionTrue {
		t.Fatalf("PgAdminSynced condition = %+v", userConditionOf(&liveUser, pgtoolboxv1alpha1.UserConditionPgAdminSynced))
	}
}
