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
	"maps"
	"slices"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	routev1 "github.com/openshift/api/route/v1"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

const reconciliationTimeout = 30 * time.Second

// CacheByObject returns the label filters of the informers this controller
// needs: every kind it owns is selected on managed-by=pgtoolbox, the one
// selector the controllers sharing the manager agree on (a kind another
// controller already filters keeps that filter). Secrets and ConfigMaps are
// watched metadata-only and their content is never cached.
func CacheByObject(features Features) map[client.Object]cache.ByObject {
	managed := cache.ByObject{Label: labels.SelectorFromSet(labels.Set{pgtoolboxv1alpha1.LabelManagedBy: pgtoolboxv1alpha1.ManagedByValue})}
	byObject := map[client.Object]cache.ByObject{
		&appsv1.Deployment{}:            managed,
		&appsv1.StatefulSet{}:           managed,
		&corev1.Service{}:               managed,
		&corev1.ServiceAccount{}:        managed,
		&corev1.Pod{}:                   managed,
		&corev1.PersistentVolumeClaim{}: managed,
		&corev1.ConfigMap{}:             managed,
		&coordinationv1.Lease{}:         managed,
		&rbacv1.Role{}:                  managed,
		&rbacv1.RoleBinding{}:           managed,
		&rbacv1.ClusterRole{}:           managed,
		&rbacv1.ClusterRoleBinding{}:    managed,
		&policyv1.PodDisruptionBudget{}: managed,
		&networkingv1.Ingress{}:         managed,
		&networkingv1.NetworkPolicy{}:   managed,
	}
	if features.Routes.Usable() {
		byObject[&routev1.Route{}] = managed
	}
	if features.Gateways.Usable() {
		byObject[&gatewayv1.HTTPRoute{}] = managed
	}
	return byObject
}

// CachedKinds are the kinds this controller's informers hold, for
// pgtoolbox_cached_objects: the PgToolBoxConsoles, the owned kinds, the
// CloudNativePG Clusters (not filtered: not the operator's) and the
// metadata-only Secret and ConfigMap informers. Kept next to
// SetupWithManager so a new watch updates both.
func CachedKinds(features Features) []client.Object {
	kinds := []client.Object{&pgtoolboxv1alpha1.PgToolBoxConsole{}, &cnpgv1.Cluster{}}
	for object := range CacheByObject(features) {
		if _, configMap := object.(*corev1.ConfigMap); !configMap {
			kinds = append(kinds, object)
		}
	}
	for _, kind := range []string{"Secret", "ConfigMap"} {
		object := &metav1.PartialObjectMetadata{}
		object.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind(kind))
		kinds = append(kinds, object)
	}
	return kinds
}

// SetupWithManager registers the controller. Watches: the PgToolBoxConsole itself,
// every kind it owns (of its Leases, a new holder only: the MCP leader's
// label follows it) and its Pods (their facts are WorkloadReady's
// reasons), and, for the instance CA mirror, the namespace's Clusters and
// the metadata of their server CA Secrets and of the user Secrets each
// console mounts (indexReferencedSecret). No polling, except for a console
// stalled on a missing shipped ClusterRole.
//
// Why a Secret watch and no periodic requeue for the mirror: the manager
// already runs one metadata-only Secret informer that no controller
// label-filters (the PgAdmin controller watches user Secrets by name), so
// watching it costs no memory and sees every server CA Secret, including a
// user-managed serverCASecret without CloudNativePG's labels. CloudNativePG
// renews a CA by updating its Secret, which changes the resourceVersion the
// metadata informer delivers; indexServerCASecret turns that event into
// the consoles concerned. Should that informer ever be label-filtered,
// user-managed CA Secrets would stop being seen: keep it unfiltered.
func (r *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(ctx, &cnpgv1.Cluster{}, indexServerCASecret, func(object client.Object) []string {
		return []string{serverCASecretName(object.(*cnpgv1.Cluster))}
	}); err != nil {
		return err
	}
	if err := mgr.GetFieldIndexer().IndexField(ctx, &pgtoolboxv1alpha1.PgToolBoxConsole{}, indexReferencedSecret, referencedSecretNames); err != nil {
		return err
	}
	b := ctrl.NewControllerManagedBy(mgr).
		Named("pgtoolbox-console").
		For(&pgtoolboxv1alpha1.PgToolBoxConsole{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}, predicate.LabelChangedPredicate{}))).
		Owns(&appsv1.Deployment{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&coordinationv1.Lease{}, builder.WithPredicates(leaseHolderChanged())).
		Owns(&corev1.Secret{}, builder.OnlyMetadata).
		Owns(&corev1.ConfigMap{}, builder.OnlyMetadata, builder.WithPredicates(stateWritesIgnored())).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(byConsoleLabel)).
		Watches(&cnpgv1.Cluster{}, handler.EnqueueRequestsFromMapFunc(r.forCluster), builder.WithPredicates(clusterCAChanged())).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.forSecret), builder.OnlyMetadata).
		WithOptions(controller.Options{MaxConcurrentReconciles: 2, ReconciliationTimeout: reconciliationTimeout, EnableWarmup: new(true)})
	if r.Features.Routes.Usable() {
		b = b.Owns(&routev1.Route{})
	}
	if r.Features.Gateways.Usable() {
		b = b.Owns(&gatewayv1.HTTPRoute{})
	}
	return b.Complete(r)
}

// byConsoleLabel maps a console Pod to its PgToolBoxConsole.
func byConsoleLabel(_ context.Context, object client.Object) []ctrlreconcile.Request {
	name := object.GetLabels()[pgtoolboxv1alpha1.LabelPgToolBoxConsole]
	if name == "" {
		return nil
	}
	return []ctrlreconcile.Request{{NamespacedName: types.NamespacedName{Namespace: object.GetNamespace(), Name: name}}}
}

// stateWritesIgnored drops the console's own writes of its state ConfigMap
// (every few seconds while sign-outs and "since" times change): the
// operator only creates that ConfigMap and has nothing to do when its data
// changes. A change of its labels, annotations, owners or deletion still
// reconciles, as does every event of the other owned ConfigMaps.
func stateWritesIgnored() predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			before, after := e.ObjectOld, e.ObjectNew
			owner := metav1.GetControllerOf(after)
			if owner == nil || owner.Kind != "PgToolBoxConsole" {
				return true
			}
			console := &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: owner.Name}}
			if after.GetName() != console.StateConfigMapName() {
				return true
			}
			return !labels.Equals(before.GetLabels(), after.GetLabels()) ||
				!maps.Equal(before.GetAnnotations(), after.GetAnnotations()) ||
				!slices.EqualFunc(before.GetOwnerReferences(), after.GetOwnerReferences(), func(a, b metav1.OwnerReference) bool { return a.UID == b.UID && a.Name == b.Name }) ||
				!slices.Equal(before.GetFinalizers(), after.GetFinalizers()) ||
				!before.GetDeletionTimestamp().Equal(after.GetDeletionTimestamp())
		},
	}
}

// clusterCAChanged passes the Cluster events that can change a mirror: a
// Cluster created or deleted, its labels (the clusterSelector) or its
// server CA Secret changed. A Cluster's status changes often; those
// updates are not the mirror's business.
func clusterCAChanged() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true },
		DeleteFunc: func(event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			before, ok := e.ObjectOld.(*cnpgv1.Cluster)
			after, ok2 := e.ObjectNew.(*cnpgv1.Cluster)
			if !ok || !ok2 {
				return false
			}
			return !labels.Equals(before.Labels, after.Labels) || serverCASecretName(before) != serverCASecretName(after)
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// forCluster maps a Cluster to the PgToolBoxConsoles of its namespace that mirror
// the instance CAs; each re-applies its selector itself.
func (r *Reconciler) forCluster(ctx context.Context, object client.Object) []ctrlreconcile.Request {
	return r.mirroringConsoles(ctx, object.GetNamespace())
}

// forCASecret maps a Secret to the mirroring PgToolBoxConsoles of its namespace
// when a Cluster there uses it as its server CA Secret.
func (r *Reconciler) forCASecret(ctx context.Context, object client.Object) []ctrlreconcile.Request {
	var clusters cnpgv1.ClusterList
	if err := r.Client.List(ctx, &clusters, client.InNamespace(object.GetNamespace()),
		client.MatchingFields{indexServerCASecret: object.GetName()}); err != nil || len(clusters.Items) == 0 {
		return nil
	}
	return r.mirroringConsoles(ctx, object.GetNamespace())
}

func (r *Reconciler) mirroringConsoles(ctx context.Context, namespace string) []ctrlreconcile.Request {
	var consoles pgtoolboxv1alpha1.PgToolBoxConsoleList
	if err := r.Client.List(ctx, &consoles, client.InNamespace(namespace)); err != nil {
		return nil
	}
	var requests []ctrlreconcile.Request
	for i := range consoles.Items {
		if consoles.Items[i].InstanceCAMirrored() {
			requests = append(requests, ctrlreconcile.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: consoles.Items[i].Name}})
		}
	}
	return requests
}
