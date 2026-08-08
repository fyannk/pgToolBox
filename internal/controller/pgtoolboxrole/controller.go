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

// Package pgtoolboxrole reconciles PgToolBoxRole resources.
//
// A role is proxy configuration and nothing else: it names a PgConsole and
// the authorization level the proxy asserts for the users bound to it. It
// has no postgres backing and no relationship with the CloudNativePG
// cluster — the credentials pgAdmin connects with come from the cluster
// itself, never from who signed into the console.
//
// So this controller creates nothing. It resolves the referenced console
// and publishes the result, and the PgConsole controller — which watches
// roles — is what renders them into the proxy configuration.
package pgtoolboxrole

import (
	"context"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	"github.com/fyannk/pgtoolbox/internal/conditions"
	"github.com/fyannk/pgtoolbox/internal/controller/shared"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Reconciler reconciles a PgToolBoxRole resource.
type Reconciler struct {
	shared.Runtime
}

// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgtoolboxroles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgtoolboxroles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgconsoles,verbs=get;list;watch

// Reconcile resolves the referenced console and publishes the role's status.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var role pgtoolboxv1alpha1.PgToolBoxRole
	if err := r.Get(ctx, req.NamespacedName, &role); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !role.DeletionTimestamp.IsZero() {
		// Nothing is created for a role, so nothing has to be collected.
		return ctrl.Result{}, nil
	}

	before := role.DeepCopy()
	role.Status.ObservedGeneration = role.GetGeneration()

	var console pgtoolboxv1alpha1.PgConsole
	consoleKey := client.ObjectKey{Namespace: role.Namespace, Name: role.Spec.PgConsoleRef.Name}
	switch err := r.Get(ctx, consoleKey, &console); {
	case apierrors.IsNotFound(err):
		conditions.MarkFalse(
			&role,
			pgtoolboxv1alpha1.RoleConditionPgConsoleReady,
			pgtoolboxv1alpha1.ReasonPgConsoleNotFound,
			"PgConsole %s was not found in namespace %s",
			consoleKey.Name, consoleKey.Namespace,
		)
		conditions.MarkFalse(
			&role,
			pgtoolboxv1alpha1.RoleConditionReady,
			pgtoolboxv1alpha1.ReasonPgConsoleNotFound,
			"the referenced PgConsole does not exist",
		)
		return ctrl.Result{}, r.publish(ctx, before, &role)
	case err != nil:
		return ctrl.Result{}, err
	}

	conditions.MarkTrue(
		&role,
		pgtoolboxv1alpha1.RoleConditionPgConsoleReady,
		pgtoolboxv1alpha1.ReasonAsExpected,
		"PgConsole %s exists",
		console.Name,
	)
	conditions.MarkTrue(
		&role,
		pgtoolboxv1alpha1.RoleConditionReady,
		pgtoolboxv1alpha1.ReasonAsExpected,
		"role grants level %s on console %s",
		role.Spec.Level, console.Name,
	)
	return ctrl.Result{}, r.publish(ctx, before, &role)
}

// publish patches the status only when it semantically changed, so a
// steady-state reconcile issues no writes.
func (r *Reconciler) publish(ctx context.Context, before, after *pgtoolboxv1alpha1.PgToolBoxRole) error {
	if apiequality.Semantic.DeepEqual(before.Status, after.Status) {
		return nil
	}
	return r.Status().Patch(ctx, after, client.MergeFrom(before))
}

// SetupWithManager wires the controller.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&pgtoolboxv1alpha1.PgToolBoxRole{}).
		Named("pgtoolboxrole").
		Complete(r)
}
