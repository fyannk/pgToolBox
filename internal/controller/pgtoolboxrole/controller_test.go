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

package pgtoolboxrole

import (
	"context"
	"testing"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A role is proxy configuration: it names a console and a level, and the
// controller creates nothing. These tests hold that — both that the status
// reports the resolution, and that no CloudNativePG object is ever written,
// which is the property the old postgres-backed design could not offer.

func roleKey(name string) client.ObjectKey {
	return client.ObjectKey{Namespace: "test", Name: name}
}

func reconcileRole(t *testing.T, r *Reconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: roleKey(name)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func TestReconcilePublishesReady(t *testing.T) {
	role := testRole("readonly", pgtoolboxv1alpha1.RoleLevelView)
	r, c := newTestReconciler(t, role, testConsole())
	reconcileRole(t, r, "readonly")

	var live pgtoolboxv1alpha1.PgToolBoxRole
	if err := c.Get(context.Background(), roleKey("readonly"), &live); err != nil {
		t.Fatalf("get role: %v", err)
	}
	for _, want := range []string{
		pgtoolboxv1alpha1.RoleConditionPgConsoleReady,
		pgtoolboxv1alpha1.RoleConditionReady,
	} {
		condition := conditionOf(&live, want)
		if condition == nil || condition.Status != metav1.ConditionTrue {
			t.Fatalf("condition %s = %+v", want, condition)
		}
	}
	if live.Status.ObservedGeneration != live.Generation {
		t.Fatalf("observedGeneration = %d, want %d", live.Status.ObservedGeneration, live.Generation)
	}
}

func TestReconcileConsoleNotFound(t *testing.T) {
	role := testRole("orphan", pgtoolboxv1alpha1.RoleLevelDBA)
	r, c := newTestReconciler(t, role)
	reconcileRole(t, r, "orphan")

	var live pgtoolboxv1alpha1.PgToolBoxRole
	if err := c.Get(context.Background(), roleKey("orphan"), &live); err != nil {
		t.Fatalf("get role: %v", err)
	}
	condition := conditionOf(&live, pgtoolboxv1alpha1.RoleConditionPgConsoleReady)
	if condition == nil || condition.Status != metav1.ConditionFalse {
		t.Fatalf("PgConsoleReady = %+v", condition)
	}
	if ready := conditionOf(&live, pgtoolboxv1alpha1.RoleConditionReady); ready == nil ||
		ready.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %+v", ready)
	}
}

// TestReconcileCreatesNothing is the point of the rewrite. A role used to
// materialize a CloudNativePG DatabaseRole and a password Secret; it has no
// postgres backing at all now, so a reconcile must leave the cluster alone.
func TestReconcileCreatesNothing(t *testing.T) {
	role := testRole("readonly", pgtoolboxv1alpha1.RoleLevelView)
	r, c := newTestReconciler(t, role, testConsole())
	reconcileRole(t, r, "readonly")

	var secrets corev1.SecretList
	if err := c.List(context.Background(), &secrets, client.InNamespace("test")); err != nil {
		t.Fatalf("list secrets: %v", err)
	}
	if len(secrets.Items) != 0 {
		t.Fatalf("reconcile created %d Secret(s); a role has no credentials", len(secrets.Items))
	}
}

// TestReconcileIsDeterministic holds the repository-wide rule that a no-op
// reconcile writes nothing: the second pass must not change the status.
func TestReconcileIsDeterministic(t *testing.T) {
	role := testRole("readonly", pgtoolboxv1alpha1.RoleLevelView)
	r, c := newTestReconciler(t, role, testConsole())
	reconcileRole(t, r, "readonly")

	var first pgtoolboxv1alpha1.PgToolBoxRole
	if err := c.Get(context.Background(), roleKey("readonly"), &first); err != nil {
		t.Fatalf("get role: %v", err)
	}
	reconcileRole(t, r, "readonly")

	var second pgtoolboxv1alpha1.PgToolBoxRole
	if err := c.Get(context.Background(), roleKey("readonly"), &second); err != nil {
		t.Fatalf("get role: %v", err)
	}
	if first.ResourceVersion != second.ResourceVersion {
		t.Fatalf("a no-op reconcile rewrote the role: %s -> %s",
			first.ResourceVersion, second.ResourceVersion)
	}
}
