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

package evidenceagent

import (
	"bytes"
	"context"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/pkg/discovery"
)

// Field indexes for the reverse lookups: no list-and-filter on
// a watch event.
const (
	indexClusterRef  = "evidenceagent.spec.clusterRef.name"
	indexObjectStore = "evidenceagent.status.objectStoreName"
	// indexPgBackRest is "<Kind>/<name>" of status.pgbackrest's Stanza or
	// Archive, so a Stanza and an Archive of the same name stay apart.
	indexPgBackRest = "evidenceagent.status.pgbackrest.object"

	reconciliationTimeout = 30 * time.Second
)

// ManagedSelector selects the objects the operator creates; CacheByObject
// filters the informers of the kinds this controller owns with it.
func ManagedSelector() labels.Selector {
	return labels.SelectorFromSet(labels.Set{pgtoolboxv1alpha1.LabelManagedBy: pgtoolboxv1alpha1.ManagedByValue})
}

// CacheByObject returns the label filters of the informers this controller
// adds. Deployments, Services and ServiceAccounts are selected on
// app.kubernetes.io/managed-by=pgtoolbox, which the other controllers
// stamp too, so the informers the manager shares stay complete for all of
// them.
// Secrets and ConfigMaps are never cached for content (the manager's
// Client.Cache.DisableFor) and are watched metadata-only; the ConfigMap
// metadata informer is filtered too (the CA bundle ConfigMap is the only
// one this controller owns, and no other controller watches ConfigMaps),
// so the namespace's other ConfigMaps never enter the cache.
func CacheByObject() map[client.Object]cache.ByObject {
	managed := cache.ByObject{Label: ManagedSelector()}
	return map[client.Object]cache.ByObject{
		&corev1.ConfigMap{}:          managed,
		&appsv1.Deployment{}:         managed,
		&corev1.Service{}:            managed,
		&corev1.ServiceAccount{}:     managed,
		&rbacv1.ClusterRole{}:        managed,
		&rbacv1.ClusterRoleBinding{}: managed,
	}
}

// OptionalAPIs says which optional kinds the start-time probes found
// usable; each is watched (metadata only) when it is.
type OptionalAPIs struct {
	ObjectStore, Stanza, Archive bool
}

// CachedKinds are the kinds this controller's informers hold, for
// pgtoolbox_cached_objects: the EvidenceAgents, the label-filtered owned
// kinds (CacheByObject), the CloudNativePG Clusters it watches, and the
// metadata-only Secret, ConfigMap and (when served) ObjectStore, Stanza and
// Archive informers. Kept next to SetupWithManager so a new watch updates
// both.
func CachedKinds(apis OptionalAPIs) []client.Object {
	owned := CacheByObject()
	kinds := make([]client.Object, 0, 2+len(owned)+5)
	kinds = append(kinds, &pgtoolboxv1alpha1.EvidenceAgent{}, &cnpgv1.Cluster{})
	for object := range owned {
		kinds = append(kinds, object)
	}
	metadataKinds := []schema.GroupVersionKind{
		corev1.SchemeGroupVersion.WithKind("Secret"), corev1.SchemeGroupVersion.WithKind("ConfigMap"),
	}
	for _, optional := range []struct {
		usable bool
		gvk    schema.GroupVersionKind
	}{{apis.ObjectStore, discovery.ObjectStoreGVK}, {apis.Stanza, discovery.StanzaGVK}, {apis.Archive, discovery.ArchiveGVK}} {
		if optional.usable {
			metadataKinds = append(metadataKinds, optional.gvk)
		}
	}
	for _, gvk := range metadataKinds {
		object := &metav1.PartialObjectMetadata{}
		object.SetGroupVersionKind(gvk)
		kinds = append(kinds, object)
	}
	return kinds
}

// SetupWithManager registers the controller, its indexes and watches.
func (r *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	indexer := mgr.GetFieldIndexer()
	if err := indexer.IndexField(ctx, &pgtoolboxv1alpha1.EvidenceAgent{}, indexClusterRef, func(object client.Object) []string {
		return []string{object.(*pgtoolboxv1alpha1.EvidenceAgent).Spec.ClusterRef.Name}
	}); err != nil {
		return err
	}
	if err := indexer.IndexField(ctx, &pgtoolboxv1alpha1.EvidenceAgent{}, indexObjectStore, func(object client.Object) []string {
		if name := object.(*pgtoolboxv1alpha1.EvidenceAgent).Status.ObjectStoreName; name != "" {
			return []string{name}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := indexer.IndexField(ctx, &pgtoolboxv1alpha1.EvidenceAgent{}, indexPgBackRest, pgBackRestObjectOf); err != nil {
		return err
	}

	singleton := predicate.NewPredicateFuncs(func(object client.Object) bool {
		return object.GetName() == ClusterRoleName || object.GetName() == ClusterRoleBindingName
	})
	specChanged := predicate.GenerationChangedPredicate{}
	b := ctrl.NewControllerManagedBy(mgr).
		Named("evidenceagent").
		For(&pgtoolboxv1alpha1.EvidenceAgent{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}, predicate.LabelChangedPredicate{},
			consumerStatusChanged()))).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.Secret{}, builder.OnlyMetadata).
		Owns(&corev1.ConfigMap{}, builder.OnlyMetadata).
		Watches(&rbacv1.ClusterRole{}, handler.EnqueueRequestsFromMapFunc(r.anyAgent), builder.WithPredicates(singleton)).
		Watches(&rbacv1.ClusterRoleBinding{}, handler.EnqueueRequestsFromMapFunc(r.anyAgent), builder.WithPredicates(singleton)).
		// CNPG kinds are not ours, so not label-filtered. Clusters are
		// watched typed: the PgAdmin controller sharing the manager holds
		// that informer anyway, and a metadata-only watch would be a second
		// informer of the same objects. Spec changes only; the Cluster is
		// still read through the APIReader.
		Watches(&cnpgv1.Cluster{}, handler.EnqueueRequestsFromMapFunc(r.agentsForCluster), builder.WithPredicates(specChanged)).
		// EnableWarmup starts the sources before leadership is won, so a
		// new leader reconciles from synced caches at once.
		WithOptions(controllerOptions())
	if r.ObjectStoreAPI.Usable() {
		objectStore := &metav1.PartialObjectMetadata{}
		objectStore.SetGroupVersionKind(discovery.ObjectStoreGVK)
		b = b.WatchesMetadata(objectStore, handler.EnqueueRequestsFromMapFunc(r.agentsForObjectStore), builder.WithPredicates(specChanged))
	}
	// The pgBackRest plugins' objects, metadata only like the ObjectStore
	// (the spec is read through the APIReader), each when its API is
	// served and listable.
	for _, optional := range []struct {
		probe discovery.Result
		gvk   schema.GroupVersionKind
	}{{r.StanzaAPI, discovery.StanzaGVK}, {r.ArchiveAPI, discovery.ArchiveGVK}} {
		if !optional.probe.Usable() {
			continue
		}
		object := &metav1.PartialObjectMetadata{}
		object.SetGroupVersionKind(optional.gvk)
		b = b.WatchesMetadata(object, handler.EnqueueRequestsFromMapFunc(r.agentsForPgBackRest(optional.gvk.Kind)), builder.WithPredicates(specChanged))
	}
	return b.Complete(r)
}

// pgBackRestObjectOf indexes an EvidenceAgent by the Stanza or Archive its
// status names.
func pgBackRestObjectOf(object client.Object) []string {
	if status := object.(*pgtoolboxv1alpha1.EvidenceAgent).Status.PgBackRest; status != nil && status.ObjectName != "" {
		return []string{pgBackRestIndexValue(status.Kind, status.ObjectName)}
	}
	return nil
}

// pgBackRestIndexValue is the indexPgBackRest key of a Stanza or Archive.
func pgBackRestIndexValue(kind, name string) string { return kind + "/" + name }

// agentsForPgBackRest maps a Stanza or Archive event to the agents whose
// status names it.
func (r *Reconciler) agentsForPgBackRest(kind string) handler.MapFunc {
	return func(ctx context.Context, object client.Object) []reconcile.Request {
		return r.agentsMatching(ctx, object.GetNamespace(), indexPgBackRest, pgBackRestIndexValue(kind, object.GetName()))
	}
}

// consumerStatusChanged admits an update that changed status.endpoint or
// status.caBundle, the two fields a console acts on before the agent has
// answered. The operator alone should write them; when anyone else does,
// the next reconcile puts the operator's values back under its status
// field manager at once instead of at the next resync (the console
// refuses an agent whose endpoint another manager wrote). The
// operator's own write of a new value costs one no-op reconcile.
func consumerStatusChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(update event.UpdateEvent) bool {
			before, okBefore := update.ObjectOld.(*pgtoolboxv1alpha1.EvidenceAgent)
			after, okAfter := update.ObjectNew.(*pgtoolboxv1alpha1.EvidenceAgent)
			if !okBefore || !okAfter {
				return false
			}
			return before.Status.Endpoint != after.Status.Endpoint || !bytes.Equal(before.Status.CABundle, after.Status.CABundle)
		},
	}
}

// anyAgent maps a change of the shared RBAC objects to one agent, whose
// reconcile re-applies them from the full set.
func (r *Reconciler) anyAgent(ctx context.Context, _ client.Object) []reconcile.Request {
	var agents pgtoolboxv1alpha1.EvidenceAgentList
	if err := r.Client.List(ctx, &agents, client.Limit(1)); err != nil || len(agents.Items) == 0 {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(&agents.Items[0])}}
}

func (r *Reconciler) agentsForCluster(ctx context.Context, object client.Object) []reconcile.Request {
	return r.agentsMatching(ctx, object.GetNamespace(), indexClusterRef, object.GetName())
}

func (r *Reconciler) agentsForObjectStore(ctx context.Context, object client.Object) []reconcile.Request {
	return r.agentsMatching(ctx, object.GetNamespace(), indexObjectStore, object.GetName())
}

func (r *Reconciler) agentsMatching(ctx context.Context, namespace, index, value string) []reconcile.Request {
	var agents pgtoolboxv1alpha1.EvidenceAgentList
	if err := r.Client.List(ctx, &agents, client.InNamespace(namespace), client.MatchingFields{index: value}); err != nil {
		logf.FromContext(ctx).Error(err, "list EvidenceAgents for a watched object", "index", index)
		return nil
	}
	requests := make([]reconcile.Request, 0, len(agents.Items))
	for i := range agents.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&agents.Items[i])})
	}
	return requests
}

// controllerOptions are the EvidenceAgent controller's options: bounded concurrency, a per-reconcile timeout, and warmed-up
// sources.
func controllerOptions() controller.Options {
	warmup := true
	return controller.Options{MaxConcurrentReconciles: 2, ReconciliationTimeout: reconciliationTimeout, EnableWarmup: &warmup}
}
