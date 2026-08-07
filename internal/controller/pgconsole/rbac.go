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

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// The generated Roles are a PgConsole's entire authority — the pod's
// ServiceAccount holds no other credential. Their shape is taken from the
// manifests the console application itself ships. Keeping read and operate
// as two separate Roles is deliberate: the operate Role is a distinct,
// auditable object naming exactly the day-2 mutations.

// readRole builds the always-present read Role, plus the one write the
// proxy's access-request flow needs: creating PgToolBoxAccessRequests, and
// nothing else on the pgtoolbox API. Rule order is fixed so a no-op
// reconcile never rewrites the object.
func (r *Reconciler) readRole(console *pgtoolboxv1alpha1.PgConsole) (*rbacv1.Role, error) {
	clusterName := console.Spec.CNPGClusterRef.Name
	rules := []rbacv1.PolicyRule{
		// Cluster status: get pinned to the one target cluster.
		{
			APIGroups:     []string{"postgresql.cnpg.io"},
			Resources:     []string{"clusters"},
			Verbs:         []string{"get"},
			ResourceNames: []string{clusterName},
		},
		// RBAC cannot pin watch by resourceNames; the application watches
		// with a metadata.name field selector and holds no list verb.
		{
			APIGroups: []string{"postgresql.cnpg.io"},
			Resources: []string{"clusters"},
			Verbs:     []string{"watch"},
		},
		{
			APIGroups: []string{"postgresql.cnpg.io"},
			Resources: []string{"backups", "scheduledbackups"},
			Verbs:     []string{"get", "list", "watch"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"pods"},
			Verbs:     []string{"get", "list", "watch"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"events"},
			Verbs:     []string{"list", "watch"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"pods/log"},
			Verbs:     []string{"get"},
		},
		// The proxy's 403-page request-access flow. The proxy code only ever
		// creates requests; reading and deciding them is the dba panel's job
		// (operate Role).
		{
			APIGroups: []string{"pgtoolbox.fyannk.dev"},
			Resources: []string{"pgtoolboxaccessrequests"},
			Verbs:     []string{"create"},
		},
	}
	// Granting a rule against an unserved API would not fail, but it would
	// misstate what the console can do; absent the API, its repository
	// panel reports unknown.
	if r.BarmanObjectStoreAvailable {
		rules = append(rules, rbacv1.PolicyRule{
			APIGroups: []string{"barmancloud.cnpg.io"},
			Resources: []string{"objectstores"},
			Verbs:     []string{"get"},
		})
	}

	role := &rbacv1.Role{
		TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      application.ResourceName(console.Name, ""),
			Namespace: console.Namespace,
			Labels:    application.CommonLabels(console.Name),
		},
		Rules: rules,
	}
	if err := controllerutil.SetControllerReference(console, role, r.Scheme); err != nil {
		return nil, err
	}
	return role, nil
}

// operateRole builds the Role behind the enumerated day-2 operations:
// backup creation plus the annotation and status patches that trigger
// reload, restart and promote, pinned to the one target cluster, plus the
// access-request review rules the dba panel needs. There is no delete verb —
// upstream deliberately excludes single-instance restart because it would
// need one — and nothing on secrets.
func (r *Reconciler) operateRole(console *pgtoolboxv1alpha1.PgConsole) (*rbacv1.Role, error) {
	clusterName := console.Spec.CNPGClusterRef.Name
	role := &rbacv1.Role{
		TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      application.ResourceName(console.Name, "-operate"),
			Namespace: console.Namespace,
			Labels:    application.CommonLabels(console.Name),
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"postgresql.cnpg.io"},
				Resources: []string{"backups"},
				Verbs:     []string{"create"},
			},
			{
				APIGroups:     []string{"postgresql.cnpg.io"},
				Resources:     []string{"clusters"},
				Verbs:         []string{"patch"},
				ResourceNames: []string{clusterName},
			},
			{
				APIGroups:     []string{"postgresql.cnpg.io"},
				Resources:     []string{"clusters/status"},
				Verbs:         []string{"patch"},
				ResourceNames: []string{clusterName},
			},
			// The dba review panel reads pending access requests and writes
			// only their status subresource; it never creates or deletes them.
			{
				APIGroups: []string{"pgtoolbox.fyannk.dev"},
				Resources: []string{"pgtoolboxaccessrequests"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				APIGroups: []string{"pgtoolbox.fyannk.dev"},
				Resources: []string{"pgtoolboxaccessrequests/status"},
				Verbs:     []string{"update", "patch"},
			},
			{
				APIGroups: []string{"pgtoolbox.fyannk.dev"},
				Resources: []string{"pgtoolboxroles"},
				Verbs:     []string{"get", "list", "watch"},
			},
		},
	}
	if err := controllerutil.SetControllerReference(console, role, r.Scheme); err != nil {
		return nil, err
	}
	return role, nil
}

// roleBinding binds a generated Role to the workload ServiceAccount.
func (r *Reconciler) roleBinding(
	console *pgtoolboxv1alpha1.PgConsole,
	suffix string,
) (*rbacv1.RoleBinding, error) {
	binding := &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      application.ResourceName(console.Name, suffix),
			Namespace: console.Namespace,
			Labels:    application.CommonLabels(console.Name),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     application.ResourceName(console.Name, suffix),
		},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      application.ResourceName(console.Name, ""),
			Namespace: console.Namespace,
		}},
	}
	if err := controllerutil.SetControllerReference(console, binding, r.Scheme); err != nil {
		return nil, err
	}
	return binding, nil
}

// reconcileRBAC converges the ServiceAccount's authority: the read and
// operate pairs, both always present in this deployment mode.
func (r *Reconciler) reconcileRBAC(ctx context.Context, console *pgtoolboxv1alpha1.PgConsole) error {
	readRole, err := r.readRole(console)
	if err != nil {
		return err
	}
	if err := r.applyRole(ctx, readRole); err != nil {
		return err
	}
	readBinding, err := r.roleBinding(console, "")
	if err != nil {
		return err
	}
	if err := r.applyRoleBinding(ctx, readBinding); err != nil {
		return err
	}

	operateRole, err := r.operateRole(console)
	if err != nil {
		return err
	}
	if err := r.applyRole(ctx, operateRole); err != nil {
		return err
	}
	operateBinding, err := r.roleBinding(console, "-operate")
	if err != nil {
		return err
	}
	return r.applyRoleBinding(ctx, operateBinding)
}

// applyRole writes the Role only when the live object differs on the fields
// this operator renders, so a no-op reconcile issues no API writes.
func (r *Reconciler) applyRole(ctx context.Context, desired *rbacv1.Role) error {
	var existing rbacv1.Role
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if err == nil && metadataMatches(&existing, desired) &&
		reflect.DeepEqual(existing.Rules, desired.Rules) {
		return nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return r.ApplyObject(ctx, desired)
}

// applyRoleBinding writes the RoleBinding only when the live object differs.
// roleRef is immutable on a RoleBinding, but the generated reference is a
// pure function of the instance name, so it cannot drift.
func (r *Reconciler) applyRoleBinding(ctx context.Context, desired *rbacv1.RoleBinding) error {
	var existing rbacv1.RoleBinding
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if err == nil && metadataMatches(&existing, desired) &&
		reflect.DeepEqual(existing.RoleRef, desired.RoleRef) &&
		reflect.DeepEqual(existing.Subjects, desired.Subjects) {
		return nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return r.ApplyObject(ctx, desired)
}
