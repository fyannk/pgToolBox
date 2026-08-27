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
	"github.com/fyannk/pgtoolbox/internal/controller/shared"
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
//
// The rule set is the console application's own read manifest, rule for
// rule. That correspondence is the point: a console screen whose resource
// is missing here does not fail, it renders "not granted" forever, which
// reads as a broken console rather than a missing grant. Nothing here is
// cluster-scoped — the one cluster-scoped read a console can hold is opt-in
// and lives in its own ClusterRole, built by catalogClusterRole.
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
		// Poolers, selected by spec.cluster.name in the application.
		{
			APIGroups: []string{"postgresql.cnpg.io"},
			Resources: []string{"poolers"},
			Verbs:     []string{"get", "list", "watch"},
		},
		// Declared database objects. Only the declaration and the
		// operator's reconciliation report are read: the console never
		// connects to PostgreSQL and never reads the Secrets a role
		// declaration may name.
		{
			APIGroups: []string{"postgresql.cnpg.io"},
			Resources: []string{"databases", "databaseroles", "publications", "subscriptions"},
			Verbs:     []string{"get", "list", "watch"},
		},
		// Pooler pod ownership. CloudNativePG runs poolers as a Deployment,
		// so proving membership means walking Pod -> ReplicaSet ->
		// Deployment -> Pooler. Get only, and nothing is read from these
		// objects but their owner reference.
		{
			APIGroups: []string{"apps"},
			Resources: []string{"replicasets", "deployments"},
			Verbs:     []string{"get"},
		},
		// Failover quorum: one object named after the cluster, pinned the
		// same way the Cluster's get is.
		{
			APIGroups:     []string{"postgresql.cnpg.io"},
			Resources:     []string{"failoverquorums"},
			Verbs:         []string{"get"},
			ResourceNames: []string{clusterName},
		},
		{
			APIGroups: []string{"postgresql.cnpg.io"},
			Resources: []string{"failoverquorums"},
			Verbs:     []string{"watch"},
		},
		// Namespaced image catalogs a Cluster may draw its image from.
		{
			APIGroups: []string{"postgresql.cnpg.io"},
			Resources: []string{"imagecatalogs"},
			Verbs:     []string{"get", "list", "watch"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"pods"},
			Verbs:     []string{"get", "list", "watch"},
		},
		// The cluster's own Services, so the console states the addresses
		// clients actually dial instead of assuming the standard names.
		{
			APIGroups: []string{""},
			Resources: []string{"services"},
			Verbs:     []string{"get", "list", "watch"},
		},
		// The claims each instance keeps its data on: capacity, class and
		// binding, none of which is inferable from the Cluster alone.
		{
			APIGroups: []string{""},
			Resources: []string{"persistentvolumeclaims"},
			Verbs:     []string{"get", "list", "watch"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"events"},
			Verbs:     []string{"list", "watch"},
		},
		// Namespace quota, read by the console's quota-exhausted
		// diagnostic since pgConsole 0.6.0. Without it the check reports
		// "could not run" rather than failing, which is the honest
		// degradation the application was written for — but a console
		// that cannot see the quota cannot tell a stuck pod caused by one
		// from a stuck pod caused by anything else.
		{
			APIGroups: []string{""},
			Resources: []string{"resourcequotas"},
			Verbs:     []string{"list", "watch"},
		},
	}

	// Instance logs can carry query text. With the tail switched off the
	// application registers no route, and the grant goes with it: RBAC
	// denies the read independently of the flag.
	if consoleAllowLogs(console) {
		rules = append(rules, rbacv1.PolicyRule{
			APIGroups: []string{""},
			Resources: []string{"pods/log"},
			Verbs:     []string{"get"},
		})
	}

	// The further objects the Cluster owns, for the children inventory
	// drawing. Kept only when their controller owner reference names the
	// cluster. Deliberately absent: any grant on secrets — the console's
	// read-only guarantee includes "nothing on secrets", and RBAC cannot
	// express "metadata only", so the drawing states Secrets as not granted.
	rules = append(rules,
		rbacv1.PolicyRule{
			APIGroups: []string{""},
			Resources: []string{"configmaps", "serviceaccounts"},
			Verbs:     []string{"get", "list", "watch"},
		},
		rbacv1.PolicyRule{
			APIGroups: []string{"policy"},
			Resources: []string{"poddisruptionbudgets"},
			Verbs:     []string{"get", "list", "watch"},
		},
		rbacv1.PolicyRule{
			APIGroups: []string{"rbac.authorization.k8s.io"},
			Resources: []string{"roles", "rolebindings"},
			Verbs:     []string{"get", "list", "watch"},
		},
		rbacv1.PolicyRule{
			APIGroups: []string{"batch"},
			Resources: []string{"jobs"},
			Verbs:     []string{"get", "list", "watch"},
		},
		// The proxy's 403-page request-access flow. The proxy code only
		// ever creates requests; reading and deciding them is the dba
		// panel's job (operate Role).
		rbacv1.PolicyRule{
			APIGroups: []string{"pgtoolbox.fyannk.dev"},
			Resources: []string{"pgtoolboxaccessrequests"},
			Verbs:     []string{"create"},
		},
	)

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

// operateRules is the write surface of a console, assembled from the two
// capabilities that can carry one. Each capability contributes its own
// rules or none at all, so switching a capability off removes the authority
// as well as the routes — the flag alone is never the boundary.
//
// There is no delete verb — upstream deliberately excludes single-instance
// restart because it would need one — and nothing on secrets. An empty
// result means the console has no write surface, and reconcileRBAC deletes
// the Role rather than leaving an empty one behind.
func operateRules(console *pgtoolboxv1alpha1.PgConsole) []rbacv1.PolicyRule {
	clusterName := console.Spec.CNPGClusterRef.Name
	var rules []rbacv1.PolicyRule

	// The four enumerated day-2 operations: backup creation plus the
	// annotation and status patches that trigger reload, restart and
	// promote, pinned to the one target cluster.
	if consoleAllowOperations(console) {
		rules = append(rules,
			rbacv1.PolicyRule{
				APIGroups: []string{"postgresql.cnpg.io"},
				Resources: []string{"backups"},
				Verbs:     []string{"create"},
			},
			rbacv1.PolicyRule{
				APIGroups:     []string{"postgresql.cnpg.io"},
				Resources:     []string{"clusters"},
				Verbs:         []string{"patch"},
				ResourceNames: []string{clusterName},
			},
			rbacv1.PolicyRule{
				APIGroups:     []string{"postgresql.cnpg.io"},
				Resources:     []string{"clusters/status"},
				Verbs:         []string{"patch"},
				ResourceNames: []string{clusterName},
			},
		)
	}

	// The dba review panel reads pending access requests and writes only
	// their status subresource; it never creates or deletes them, and never
	// touches the users or roles an approval materializes — that is the
	// PgToolBoxAccessRequest controller's job. The verb must include patch:
	// the console writes the decision as a merge patch, and RBAC names the
	// HTTP verb rather than the intent.
	if consoleAllowAccessReview(console) {
		rules = append(rules,
			rbacv1.PolicyRule{
				APIGroups: []string{"pgtoolbox.fyannk.dev"},
				Resources: []string{"pgtoolboxaccessrequests"},
				Verbs:     []string{"get", "list", "watch"},
			},
			rbacv1.PolicyRule{
				APIGroups: []string{"pgtoolbox.fyannk.dev"},
				Resources: []string{"pgtoolboxaccessrequests/status"},
				Verbs:     []string{"update", "patch"},
			},
		)
	}
	return rules
}

// operateRole builds the Role behind the console's write surface, or nil
// when the console has none.
func (r *Reconciler) operateRole(console *pgtoolboxv1alpha1.PgConsole) (*rbacv1.Role, error) {
	rules := operateRules(console)
	if len(rules) == 0 {
		return nil, nil
	}
	role := &rbacv1.Role{
		TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      application.ResourceName(console.Name, "-operate"),
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

// catalogClusterRoleName is the cluster-scoped name of a console's catalog
// ClusterRole. Cluster-scoped names share one namespace across the whole
// cluster, so the console's own namespace is part of the name; two consoles
// called "main" in different namespaces must not collide.
func catalogClusterRoleName(console *pgtoolboxv1alpha1.PgConsole) string {
	return shared.BoundedName(console.Namespace+"-"+console.Name, "-pgconsole-catalogs")
}

// catalogClusterRole builds the one cluster-scoped grant a console may
// hold: get on clusterimagecatalogs, because a Cluster may draw its image
// from a cluster-scoped catalog and the console can otherwise see the
// reference but not its content.
//
// It is a get and never a list or a watch, so nothing cluster-wide is ever
// enumerated. A cluster-scoped object cannot carry an owner reference to a
// namespaced one, so this pair is not garbage-collected by Kubernetes: the
// labels identify it and deleteCatalogClusterRBAC removes it, both when the
// capability is switched off and when the console is deleted.
func catalogClusterRole(console *pgtoolboxv1alpha1.PgConsole) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   catalogClusterRoleName(console),
			Labels: application.CommonLabels(console.Name),
		},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{"postgresql.cnpg.io"},
			Resources: []string{"clusterimagecatalogs"},
			Verbs:     []string{"get"},
		}},
	}
}

// catalogClusterRoleBinding binds the catalog ClusterRole to the console's
// ServiceAccount.
func catalogClusterRoleBinding(console *pgtoolboxv1alpha1.PgConsole) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   catalogClusterRoleName(console),
			Labels: application.CommonLabels(console.Name),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     catalogClusterRoleName(console),
		},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      application.ResourceName(console.Name, ""),
			Namespace: console.Namespace,
		}},
	}
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

// reconcileRBAC converges the ServiceAccount's authority: the always-present
// read pair, the operate pair when the console has a write surface, and the
// opt-in cluster-scoped catalog pair. A capability switched off is removed
// here, not merely left unused — the authority and the routes move together.
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
	if operateRole == nil {
		// A console with neither operations nor access review holds no
		// write surface at all, so the Role is deleted rather than left
		// empty: an empty Role reads as an oversight, an absent one states
		// the intent.
		if err := r.deleteOperateRBAC(ctx, console); err != nil {
			return err
		}
	} else {
		if err := r.applyRole(ctx, operateRole); err != nil {
			return err
		}
		operateBinding, err := r.roleBinding(console, "-operate")
		if err != nil {
			return err
		}
		if err := r.applyRoleBinding(ctx, operateBinding); err != nil {
			return err
		}
	}

	if !consoleAllowClusterCatalogs(console) {
		return r.deleteCatalogClusterRBAC(ctx, console)
	}
	if err := r.applyClusterRole(ctx, catalogClusterRole(console)); err != nil {
		return err
	}
	return r.applyClusterRoleBinding(ctx, catalogClusterRoleBinding(console))
}

// deleteOperateRBAC removes the operate pair. Both objects carry an owner
// reference, so this only matters when the capability is switched off on a
// living console; deleting the console collects them anyway.
func (r *Reconciler) deleteOperateRBAC(ctx context.Context, console *pgtoolboxv1alpha1.PgConsole) error {
	name := application.ResourceName(console.Name, "-operate")
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: console.Namespace}}
	if err := r.Delete(ctx, role); client.IgnoreNotFound(err) != nil {
		return err
	}
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: console.Namespace}}
	return client.IgnoreNotFound(r.Delete(ctx, binding))
}

// deleteCatalogClusterRBAC removes the cluster-scoped catalog pair. Nothing
// else does: a cluster-scoped object cannot be owned by a namespaced one, so
// this is called both when the capability is switched off and from the
// console's deletion path.
func (r *Reconciler) deleteCatalogClusterRBAC(ctx context.Context, console *pgtoolboxv1alpha1.PgConsole) error {
	name := catalogClusterRoleName(console)
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := r.Delete(ctx, role); client.IgnoreNotFound(err) != nil {
		return err
	}
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}}
	return client.IgnoreNotFound(r.Delete(ctx, binding))
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

// applyClusterRole writes the catalog ClusterRole only when the live object
// differs on the fields this operator renders.
func (r *Reconciler) applyClusterRole(ctx context.Context, desired *rbacv1.ClusterRole) error {
	var existing rbacv1.ClusterRole
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if err == nil && shared.LabelsContain(existing.Labels, desired.Labels) &&
		reflect.DeepEqual(existing.Rules, desired.Rules) {
		return nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return r.ApplyObject(ctx, desired)
}

// applyClusterRoleBinding writes the catalog ClusterRoleBinding only when
// the live object differs.
func (r *Reconciler) applyClusterRoleBinding(ctx context.Context, desired *rbacv1.ClusterRoleBinding) error {
	var existing rbacv1.ClusterRoleBinding
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if err == nil && shared.LabelsContain(existing.Labels, desired.Labels) &&
		reflect.DeepEqual(existing.RoleRef, desired.RoleRef) &&
		reflect.DeepEqual(existing.Subjects, desired.Subjects) {
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
