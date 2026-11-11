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

package pgadmin

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/metadata"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/offer"
	"github.com/fyannk/pgToolBox/internal/operator/specs"
)

// Cross-namespace registration: a PgAdmin names a
// Cluster of another namespace in spec.crossNamespaceClusterRefs, and the
// Cluster is registered only while a PgAdminAccess in that namespace grants
// this PgAdmin that Cluster. Nothing is selected implicitly: no selector
// crosses a namespace. The grant is the target namespace owner's decision
// (whoever may create pgadminaccesses there); the operator only executes
// it: for each grant it maintains, in the grant's namespace, a Role
// allowing get and watch on exactly the offered Secrets of the granted
// Clusters and a RoleBinding to this PgAdmin's ServiceAccount, both owned
// by the grant, so deleting the grant revokes the access through garbage
// collection.

// componentAccess is the component of the grant's Role and RoleBinding.
const componentAccess = "pgadmin-access"

// remoteResolution is what spec.crossNamespaceClusterRefs resolves to.
type remoteResolution struct {
	// Clusters are the granted, existing Clusters.
	Clusters []cnpgv1.Cluster
	// Roles and Poolers are those of the granted Clusters' namespaces.
	Roles   []cnpgv1.DatabaseRole
	Poolers []cnpgv1.Pooler
	// Grants are the PgAdminAccess objects naming this PgAdmin, by
	// namespace, sorted by name.
	Grants map[string][]pgtoolboxv1alpha1.PgAdminAccess
	// PrivilegedGranted names the granted Clusters ("namespace/name")
	// whose grant allows privileged credentials.
	PrivilegedGranted map[string]bool
	// Ungranted and Missing name the references without a grant and the
	// granted Clusters that do not exist; Invalid the references that
	// cannot be honoured at all.
	Ungranted, Missing, Invalid []string
}

// resolveRemote reads the grants and the granted Clusters from the cache.
func (r *Reconciler) resolveRemote(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin) (remoteResolution, error) {
	out := remoteResolution{Grants: map[string][]pgtoolboxv1alpha1.PgAdminAccess{}, PrivilegedGranted: map[string]bool{}}
	byNamespace := map[string][]string{}
	for _, ref := range p.Spec.CrossNamespaceClusterRefs {
		if ref.Namespace == p.Namespace {
			out.Invalid = append(out.Invalid, fmt.Sprintf("crossNamespaceClusterRefs %s/%s is in this PgAdmin's own namespace: name it in clusterRefs", ref.Namespace, ref.Name))
			continue
		}
		if watched := r.Config.WatchNamespaces; len(watched) > 0 && !slices.Contains(watched, ref.Namespace) {
			out.Invalid = append(out.Invalid, fmt.Sprintf("crossNamespaceClusterRefs %s/%s: the operator does not watch namespace %s (WATCH_NAMESPACES)",
				ref.Namespace, ref.Name, ref.Namespace))
			continue
		}
		byNamespace[ref.Namespace] = append(byNamespace[ref.Namespace], ref.Name)
	}
	namespaces := make([]string, 0, len(byNamespace))
	for namespace := range byNamespace {
		namespaces = append(namespaces, namespace)
	}
	slices.Sort(namespaces)
	for _, namespace := range namespaces {
		var grants pgtoolboxv1alpha1.PgAdminAccessList
		if err := r.Client.List(ctx, &grants, client.InNamespace(namespace)); err != nil {
			return out, fmt.Errorf("list PgAdminAccess in %s: %w", namespace, err)
		}
		var mine []pgtoolboxv1alpha1.PgAdminAccess
		for i := range grants.Items {
			if g := &grants.Items[i]; g.Spec.PgAdmin.Namespace == p.Namespace && g.Spec.PgAdmin.Name == p.Name && g.DeletionTimestamp.IsZero() {
				mine = append(mine, *g)
			}
		}
		slices.SortFunc(mine, func(a, b pgtoolboxv1alpha1.PgAdminAccess) int { return strings.Compare(a.Name, b.Name) })
		out.Grants[namespace] = mine

		var clusters cnpgv1.ClusterList
		if err := r.Client.List(ctx, &clusters, client.InNamespace(namespace)); err != nil {
			return out, fmt.Errorf("list Clusters in %s: %w", namespace, err)
		}
		granted := 0
		for _, name := range byNamespace[namespace] {
			if !slices.ContainsFunc(mine, func(g pgtoolboxv1alpha1.PgAdminAccess) bool { return g.Grants(p.Namespace, p.Name, name) }) {
				out.Ungranted = append(out.Ungranted, namespace+"/"+name)
				continue
			}
			index := slices.IndexFunc(clusters.Items, func(c cnpgv1.Cluster) bool { return c.Name == name })
			if index < 0 {
				out.Missing = append(out.Missing, namespace+"/"+name)
				continue
			}
			out.Clusters = append(out.Clusters, clusters.Items[index])
			if slices.ContainsFunc(mine, func(g pgtoolboxv1alpha1.PgAdminAccess) bool {
				return g.AllowsPrivileged() && g.Grants(p.Namespace, p.Name, name)
			}) {
				out.PrivilegedGranted[namespace+"/"+name] = true
			}
			granted++
		}
		if granted == 0 {
			continue
		}
		if r.Features.In(namespace).DatabaseRoles.Usable() {
			var roles cnpgv1.DatabaseRoleList
			if err := r.Client.List(ctx, &roles, client.InNamespace(namespace)); err != nil {
				return out, fmt.Errorf("list DatabaseRoles in %s: %w", namespace, err)
			}
			out.Roles = append(out.Roles, roles.Items...)
		}
		if r.Features.In(namespace).Poolers.Usable() {
			var poolers cnpgv1.PoolerList
			if err := r.Client.List(ctx, &poolers, client.InNamespace(namespace)); err != nil {
				return out, fmt.Errorf("list Poolers in %s: %w", namespace, err)
			}
			out.Poolers = append(out.Poolers, poolers.Items...)
		}
	}
	return out, nil
}

// accessName is the name of a grant's Role and RoleBinding.
func accessName(grant *pgtoolboxv1alpha1.PgAdminAccess) string {
	return metadata.BoundedName(grant.Name, "-pgadmin-access")
}

// accessMeta is the metadata of a grant's Role or RoleBinding: this
// PgAdmin's identity labels plus its namespace, the grant as controller.
func accessMeta(p *pgtoolboxv1alpha1.PgAdmin, grant *pgtoolboxv1alpha1.PgAdminAccess) metav1.ObjectMeta {
	labels, annotations := metadata.Propagate(pgtoolboxv1alpha1.PropagationTargetAll, p.Spec.MetadataPropagation,
		identity(p).Labels(componentAccess), nil)
	labels[pgtoolboxv1alpha1.PgAdminNamespaceLabelKey] = p.Namespace
	return metav1.ObjectMeta{
		Name:            accessName(grant),
		Namespace:       grant.Namespace,
		Labels:          labels,
		Annotations:     annotations,
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(grant, pgtoolboxv1alpha1.GroupVersion.WithKind("PgAdminAccess"))},
	}
}

// grantSecretNames are the Secrets of the grant's namespace the offer names
// for Clusters this grant allows: what its Role may read.
func grantSecretNames(p *pgtoolboxv1alpha1.PgAdmin, grant *pgtoolboxv1alpha1.PgAdminAccess, doc offer.Document) []string {
	var names []string
	for _, server := range doc.Connections() {
		if server.Namespace != grant.Namespace || !grant.Grants(p.Namespace, p.Name, server.Cluster) {
			continue
		}
		names = append(names, server.SecretName, server.CASecretName)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// buildAccessRole is a grant's name-scoped Role. With no offered Secret it
// has no rule at all: a rule with empty resourceNames would allow every
// Secret of the namespace.
func buildAccessRole(p *pgtoolboxv1alpha1.PgAdmin, grant *pgtoolboxv1alpha1.PgAdminAccess, doc offer.Document) *rbacv1.Role {
	role := &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: accessMeta(p, grant),
	}
	if names := grantSecretNames(p, grant, doc); len(names) > 0 {
		role.Rules = []rbacv1.PolicyRule{{
			APIGroups:     []string{""},
			Resources:     []string{"secrets"},
			ResourceNames: names,
			Verbs:         []string{"get", "watch"},
		}}
	}
	specs.SetHash(role, specs.Hash(role.Rules))
	return role
}

// buildAccessRoleBinding binds a grant's Role to this PgAdmin's
// ServiceAccount, in the PgAdmin's namespace.
func buildAccessRoleBinding(p *pgtoolboxv1alpha1.PgAdmin, grant *pgtoolboxv1alpha1.PgAdminAccess) *rbacv1.RoleBinding {
	binding := &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: accessMeta(p, grant),
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: accessName(grant)},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: namesOf(p).base, Namespace: p.Namespace}},
	}
	specs.SetHash(binding, specs.Hash(binding.Subjects))
	return binding
}

// applyAccess writes the Role and RoleBinding of every grant naming this
// PgAdmin in a namespace it references, and deletes those of grants no
// longer referenced (the grant still exists, but this PgAdmin stopped
// naming its namespace). A deleted grant takes its own with it; a deleted
// PgAdmin's are deleted by pruneAccess from Reconcile.
func (r *Reconciler) applyAccess(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin, remote remoteResolution, doc offer.Document) error {
	applier := r.applier()
	keep := map[types.NamespacedName]bool{}
	grants := remote.Grants
	if hasGrants(grants) {
		// Defence in depth: the cache may still hold a PgAdmin the API
		// server deleted, and nothing this PgAdmin owns would take the
		// access back. A metadata-only read, only for a PgAdmin with
		// grants.
		live, err := r.exists(ctx, p)
		if err != nil {
			return err
		}
		if !live {
			grants = nil
		}
	}
	for _, namespace := range slices.Sorted(maps.Keys(grants)) {
		for i := range grants[namespace] {
			grant := &grants[namespace][i]
			keep[types.NamespacedName{Namespace: grant.Namespace, Name: accessName(grant)}] = true
			if err := applier.Apply(ctx, buildAccessRole(p, grant, doc)); err != nil {
				return r.notOwned(p, err)
			}
			if err := applier.Apply(ctx, buildAccessRoleBinding(p, grant)); err != nil {
				return r.notOwned(p, err)
			}
		}
	}
	return r.pruneAccess(ctx, types.NamespacedName{Namespace: p.Namespace, Name: p.Name}, keep)
}

// hasGrants reports whether any namespace holds a grant.
func hasGrants(grants map[string][]pgtoolboxv1alpha1.PgAdminAccess) bool {
	for _, list := range grants {
		if len(list) > 0 {
			return true
		}
	}
	return false
}

// exists reads the PgAdmin's metadata from the API server: whether this
// very object (its UID) is still there and not being deleted.
func (r *Reconciler) exists(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin) (bool, error) {
	live := &metav1.PartialObjectMetadata{}
	live.SetGroupVersionKind(pgtoolboxv1alpha1.GroupVersion.WithKind("PgAdmin"))
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(p), live); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("read PgAdmin %s/%s: %w", p.Namespace, p.Name, err)
	}
	return live.UID == p.UID && live.DeletionTimestamp.IsZero(), nil
}

// pruneAccess deletes the access Roles and RoleBindings the operator wrote
// for the PgAdmin pgAdmin in every namespace, except those kept: all of
// them once the PgAdmin is gone or being deleted, since they are
// controlled by the grants and garbage collection would keep them.
func (r *Reconciler) pruneAccess(ctx context.Context, pgAdmin types.NamespacedName, keep map[types.NamespacedName]bool) error {
	selector := client.MatchingLabels{
		pgtoolboxv1alpha1.PgAdminLabelKey:          pgAdmin.Name,
		pgtoolboxv1alpha1.PgAdminNamespaceLabelKey: pgAdmin.Namespace,
		"app.kubernetes.io/component":              componentAccess,
	}
	var bindings rbacv1.RoleBindingList
	if err := r.Client.List(ctx, &bindings, selector); err != nil {
		return fmt.Errorf("list access RoleBindings: %w", err)
	}
	for i := range bindings.Items {
		if err := r.deleteStaleAccess(ctx, &bindings.Items[i], keep); err != nil {
			return err
		}
	}
	var roles rbacv1.RoleList
	if err := r.Client.List(ctx, &roles, selector); err != nil {
		return fmt.Errorf("list access Roles: %w", err)
	}
	for i := range roles.Items {
		if err := r.deleteStaleAccess(ctx, &roles.Items[i], keep); err != nil {
			return err
		}
	}
	return nil
}

// deleteStaleAccess deletes an access Role or RoleBinding the operator
// wrote for a grant (controlled by a PgAdminAccess) that is not kept.
func (r *Reconciler) deleteStaleAccess(ctx context.Context, obj client.Object, keep map[types.NamespacedName]bool) error {
	if keep[types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}] || obj.GetDeletionTimestamp() != nil {
		return nil
	}
	owner := metav1.GetControllerOf(obj)
	if owner == nil || owner.Kind != "PgAdminAccess" || obj.GetLabels()[pgtoolboxv1alpha1.LabelManagedBy] != pgtoolboxv1alpha1.ManagedByValue {
		return nil
	}
	if err := r.Client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete stale access %s/%s: %w", obj.GetNamespace(), obj.GetName(), err)
	}
	return nil
}

// forGrant maps a PgAdminAccess to the PgAdmin it names.
func forGrant(_ context.Context, obj client.Object) []ctrlreconcile.Request {
	grant, ok := obj.(*pgtoolboxv1alpha1.PgAdminAccess)
	if !ok {
		return nil
	}
	return []ctrlreconcile.Request{{NamespacedName: types.NamespacedName{Namespace: grant.Spec.PgAdmin.Namespace, Name: grant.Spec.PgAdmin.Name}}}
}

// byAccessLabels maps a grant's Role or RoleBinding to the PgAdmin it
// serves (its labels carry the PgAdmin's namespace and name).
func byAccessLabels(_ context.Context, obj client.Object) []ctrlreconcile.Request {
	labels := obj.GetLabels()
	namespace, name := labels[pgtoolboxv1alpha1.PgAdminNamespaceLabelKey], labels[pgtoolboxv1alpha1.PgAdminLabelKey]
	if namespace == "" || name == "" {
		return nil
	}
	return []ctrlreconcile.Request{{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}}
}

// forRemoteNamespace maps an object of a namespace to every PgAdmin of
// another namespace that references a Cluster there.
func (r *Reconciler) forRemoteNamespace(ctx context.Context, namespace string) []ctrlreconcile.Request {
	var list pgtoolboxv1alpha1.PgAdminList
	if err := r.Client.List(ctx, &list, client.MatchingFields{indexCrossNamespace: namespace}); err != nil {
		return nil
	}
	requests := make([]ctrlreconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		requests = append(requests, ctrlreconcile.Request{NamespacedName: types.NamespacedName{Namespace: list.Items[i].Namespace, Name: list.Items[i].Name}})
	}
	return requests
}
