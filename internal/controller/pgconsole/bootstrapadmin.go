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
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
)

// A console starts with no users. Granting access needs a dba to approve a
// PgToolBoxAccessRequest, so a console with none can never let anybody in —
// including the person who would have approved the first request.
//
// spec.proxy.authentication.bootstrapAdmin closes that loop, and the user it
// names is derived from the spec rather than maintained by hand: the operator
// creates it, keeps it matching the field, and puts it back when it is
// deleted. Kubernetes has no undeletable object, but an object the controller
// re-creates on the next reconcile is the same thing in practice — and one
// whose disappearance is a transient state rather than a locked-out console.
//
// Handing the role to somebody else means editing the console, which leaves a
// record in the object's history. Deleting the PgToolBoxUser does not.

// bootstrapAdminUserName is the name of the PgToolBoxUser the operator owns
// for the console's first administrator. It is derived from the console name
// so that two consoles in one namespace do not collide.
func bootstrapAdminUserName(console *pgtoolboxv1alpha1.PgConsole) string {
	return console.Name + "-bootstrap-admin"
}

// reconcileBootstrapAdmin creates or updates the PgToolBoxUser for the
// console's declared first administrator.
func (r *Reconciler) reconcileBootstrapAdmin(
	ctx context.Context,
	console *pgtoolboxv1alpha1.PgConsole,
) error {
	admin := console.Spec.Proxy.Authentication.BootstrapAdmin

	desired := &pgtoolboxv1alpha1.PgToolBoxUser{
		ObjectMeta: metav1.ObjectMeta{
			Name:      bootstrapAdminUserName(console),
			Namespace: console.Namespace,
			Labels:    application.CommonLabels(console.Name),
		},
		Spec: pgtoolboxv1alpha1.PgToolBoxUserSpec{
			PgConsoleRef: pgtoolboxv1alpha1.LocalObjectReference{Name: console.Name},
			Subject:      admin.Subject,
			// dba, always. The point of the object is to have somebody who
			// can approve the first access request, which no lower level
			// can do.
			Level:                  pgtoolboxv1alpha1.RoleLevelDBA,
			LocalPasswordSecretRef: admin.PasswordSecretRef,
		},
	}
	if err := controllerutil.SetControllerReference(console, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner on bootstrap admin: %w", err)
	}

	var existing pgtoolboxv1alpha1.PgToolBoxUser
	key := client.ObjectKeyFromObject(desired)
	err := r.Get(ctx, key, &existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating bootstrap admin: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading bootstrap admin: %w", err)
	}

	// A no-op reconcile must issue no write, or every pass would bump the
	// resourceVersion and re-trigger the watch that led here.
	if equalBootstrapAdminSpec(existing.Spec, desired.Spec) {
		return nil
	}
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	existing.OwnerReferences = desired.OwnerReferences
	if err := r.Update(ctx, &existing); err != nil {
		return fmt.Errorf("updating bootstrap admin: %w", err)
	}
	return nil
}

func equalBootstrapAdminSpec(a, b pgtoolboxv1alpha1.PgToolBoxUserSpec) bool {
	if a.PgConsoleRef != b.PgConsoleRef || a.Subject != b.Subject || a.Level != b.Level {
		return false
	}
	switch {
	case a.LocalPasswordSecretRef == nil && b.LocalPasswordSecretRef == nil:
		return true
	case a.LocalPasswordSecretRef == nil || b.LocalPasswordSecretRef == nil:
		return false
	default:
		return *a.LocalPasswordSecretRef == *b.LocalPasswordSecretRef
	}
}
