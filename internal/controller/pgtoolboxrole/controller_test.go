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

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func reconcileToSteadyState(t *testing.T, r *Reconciler) {
	t.Helper()
	req := ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "test", Name: "monitor-role"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("finalizer reconcile: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("full reconcile: %v", err)
	}
}

func TestReconcileManagedRole(t *testing.T) {
	role := testRoleProfile()
	console := testConsole()
	r, c := newTestReconciler(t, console, role)
	reconcileToSteadyState(t, r)

	var secret corev1.Secret
	secretKey := client.ObjectKey{Namespace: "test", Name: "monitor-role-pgrole-credentials"}
	if err := c.Get(context.Background(), secretKey, &secret); err != nil {
		t.Fatalf("credential secret was not created: %v", err)
	}
	if len(secret.Data[corev1.BasicAuthPasswordKey]) == 0 {
		t.Fatalf("credential secret has no password")
	}
	owner := metav1.GetControllerOf(&secret)
	if owner == nil || owner.UID != role.UID {
		t.Fatalf("credential secret owner = %+v", owner)
	}

	var databaseRole cnpgv1.DatabaseRole
	dbRoleKey := client.ObjectKey{Namespace: "test", Name: "monitor-role-pgrole"}
	if err := c.Get(context.Background(), dbRoleKey, &databaseRole); err != nil {
		t.Fatalf("DatabaseRole was not created: %v", err)
	}
	if databaseRole.Spec.Name != "monitor-role-pgrole" {
		t.Fatalf("postgres role name = %q", databaseRole.Spec.Name)
	}
	if len(databaseRole.Spec.InRoles) != 1 || databaseRole.Spec.InRoles[0] != "pg_monitor" {
		t.Fatalf("monitor inRoles = %v", databaseRole.Spec.InRoles)
	}
	owner = metav1.GetControllerOf(&databaseRole)
	if owner == nil || owner.UID != role.UID {
		t.Fatalf("DatabaseRole owner = %+v", owner)
	}

	var live pgtoolboxv1alpha1.PgToolBoxRole
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(role), &live); err != nil {
		t.Fatalf("read role: %v", err)
	}
	if live.Status.DatabaseRoleName != "monitor-role-pgrole" {
		t.Fatalf("status.databaseRoleName = %q", live.Status.DatabaseRoleName)
	}
	if conditionOf(&live, pgtoolboxv1alpha1.RoleConditionPgConsoleReady).Status != metav1.ConditionTrue {
		t.Fatalf("PgConsoleReady not true")
	}
	if conditionOf(&live, pgtoolboxv1alpha1.RoleConditionDatabaseRoleReady).Status != metav1.ConditionFalse {
		t.Fatalf("DatabaseRoleReady should be pending before CNPG applies")
	}

	// Simulate CloudNativePG applying the DatabaseRole.
	databaseRole.Status.Applied = boolPtr(true)
	databaseRole.Status.ObservedGeneration = databaseRole.Generation
	databaseRole.Status.SecretResourceVersion = secret.ResourceVersion
	if err := c.Status().Update(context.Background(), &databaseRole); err != nil {
		t.Fatalf("update databaserole status: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(role)}); err != nil {
		t.Fatalf("reconcile after applied: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(role), &live); err != nil {
		t.Fatalf("read role: %v", err)
	}
	if conditionOf(&live, pgtoolboxv1alpha1.RoleConditionDatabaseRoleReady).Status != metav1.ConditionTrue {
		t.Fatalf("DatabaseRoleReady should be true after CNPG applies: %+v", conditionOf(&live, pgtoolboxv1alpha1.RoleConditionDatabaseRoleReady))
	}
	if conditionOf(&live, pgtoolboxv1alpha1.RoleConditionCredentialReady).Status != metav1.ConditionTrue {
		t.Fatalf("CredentialReady should be true after CNPG applies")
	}
	if conditionOf(&live, pgtoolboxv1alpha1.RoleConditionReady).Status != metav1.ConditionTrue {
		t.Fatalf("Ready should be true")
	}
}

func TestReconcileDatabaseRoleRef(t *testing.T) {
	role := testRoleRef()
	console := testConsole()
	existing := &cnpgv1.DatabaseRole{
		ObjectMeta: metav1.ObjectMeta{Name: "existing-role", Namespace: "test"},
		Spec: cnpgv1.DatabaseRoleSpec{
			RoleConfiguration: cnpgv1.RoleConfiguration{
				Name:           "existing-role",
				PasswordSecret: &cnpgv1.LocalObjectReference{Name: "existing-password"},
			},
		},
	}
	r, c := newTestReconciler(t, console, role, existing)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(role)}); err != nil {
		t.Fatalf("finalizer reconcile: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(role)}); err != nil {
		t.Fatalf("full reconcile: %v", err)
	}

	var live pgtoolboxv1alpha1.PgToolBoxRole
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(role), &live); err != nil {
		t.Fatalf("read role: %v", err)
	}
	if live.Status.DatabaseRoleName != "existing-role" {
		t.Fatalf("status.databaseRoleName = %q", live.Status.DatabaseRoleName)
	}
	if conditionOf(&live, pgtoolboxv1alpha1.RoleConditionDatabaseRoleReady).Status != metav1.ConditionTrue {
		t.Fatalf("DatabaseRoleReady = %+v", conditionOf(&live, pgtoolboxv1alpha1.RoleConditionDatabaseRoleReady))
	}
	if conditionOf(&live, pgtoolboxv1alpha1.RoleConditionCredentialReady).Status != metav1.ConditionTrue {
		t.Fatalf("CredentialReady = %+v", conditionOf(&live, pgtoolboxv1alpha1.RoleConditionCredentialReady))
	}

	secretKey := client.ObjectKey{Namespace: "test", Name: "byoref-role-pgrole-credentials"}
	if err := c.Get(context.Background(), secretKey, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("managed credential secret must not be created for databaseRoleRef path: %v", err)
	}
}

func TestReconcileConsoleNotFound(t *testing.T) {
	role := testRoleProfile()
	r, c := newTestReconciler(t, role)

	reconcileToSteadyState(t, r)

	var live pgtoolboxv1alpha1.PgToolBoxRole
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(role), &live); err != nil {
		t.Fatalf("read role: %v", err)
	}
	cond := conditionOf(&live, pgtoolboxv1alpha1.RoleConditionPgConsoleReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != pgtoolboxv1alpha1.ReasonPgConsoleNotFound {
		t.Fatalf("PgConsoleReady = %+v", cond)
	}
}

func TestReconcileDeletionCleanup(t *testing.T) {
	role := testRoleProfile()
	console := testConsole()
	r, c := newTestReconciler(t, console, role)
	reconcileToSteadyState(t, r)

	// Simulate the user deleting the role while the finalizer is present.
	if err := c.Delete(context.Background(), role); err != nil {
		t.Fatalf("delete role: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(role)}); err != nil {
		t.Fatalf("deletion reconcile: %v", err)
	}

	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "test", Name: "monitor-role-pgrole"}, &cnpgv1.DatabaseRole{}); !apierrors.IsNotFound(err) {
		t.Fatalf("managed DatabaseRole should be deleted: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "test", Name: "monitor-role-pgrole-credentials"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("managed Secret should be deleted: %v", err)
	}
}

func boolPtr(b bool) *bool { return &b }
