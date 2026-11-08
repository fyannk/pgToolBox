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

package accessrequest

import (
	"context"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/labels"
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
)

const (
	// indexCluster maps a Cluster to the requests naming it.
	indexCluster          = "pgaccessrequest.spec.cluster"
	reconciliationTimeout = 30 * time.Second
)

// CacheByObject returns the label filter of the RoleBinding informer: the
// requests' RoleBindings carry app.kubernetes.io/managed-by=pgtoolbox (the
// selector every controller sharing the manager agrees on) beside the
// access-request label, so the namespace's other RoleBindings never enter
// the cache.
func CacheByObject() map[client.Object]cache.ByObject {
	return map[client.Object]cache.ByObject{
		&rbacv1.RoleBinding{}: {Label: labels.SelectorFromSet(labels.Set{pgtoolboxv1alpha1.LabelManagedBy: pgtoolboxv1alpha1.ManagedByValue})},
	}
}

// CachedKinds are the kinds this controller's informers hold, for
// pgtoolbox_cached_objects. Kept next to SetupWithManager so a new watch
// updates both.
func CachedKinds() []client.Object {
	return []client.Object{&pgtoolboxv1alpha1.PgAccessRequest{}, &rbacv1.RoleBinding{}, &cnpgv1.Cluster{}}
}

// SetupWithManager registers the controller, its index and watches.
func (r *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(ctx, &pgtoolboxv1alpha1.PgAccessRequest{}, indexCluster, func(object client.Object) []string {
		if name := object.(*pgtoolboxv1alpha1.PgAccessRequest).Spec.Cluster; name != "" {
			return []string{name}
		}
		return nil
	}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("pgaccessrequest").
		// The decision is written to status by a person, so a status
		// change of the phase is an input here, unlike the other kinds.
		For(&pgtoolboxv1alpha1.PgAccessRequest{}, builder.WithPredicates(predicate.Or[client.Object](
			predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}, predicate.LabelChangedPredicate{}, decisionChanged{}))).
		// RoleBindings are filtered by the cache (managed-by=pgtoolbox);
		// Owns maps them to the request that controls them.
		Owns(&rbacv1.RoleBinding{}).
		// Clusters are not ours: watched unfiltered (the other controllers
		// hold that typed informer already), spec changes ignored, only
		// their arrival and departure matter here.
		Watches(&cnpgv1.Cluster{}, handler.EnqueueRequestsFromMapFunc(r.requestsForCluster), builder.WithPredicates(predicate.Funcs{
			UpdateFunc: func(event.UpdateEvent) bool { return false },
		})).
		WithOptions(controllerOptions()).
		Complete(r)
}

// decisionChanged passes an update whose status phase, decider or
// RoleBinding changed: a decision made through the console.
type decisionChanged struct{ predicate.Funcs }

func (decisionChanged) Update(e event.UpdateEvent) bool {
	before, okBefore := e.ObjectOld.(*pgtoolboxv1alpha1.PgAccessRequest)
	after, okAfter := e.ObjectNew.(*pgtoolboxv1alpha1.PgAccessRequest)
	if !okBefore || !okAfter {
		return false
	}
	return before.Status.Phase != after.Status.Phase || before.Status.DecidedBy != after.Status.DecidedBy ||
		before.Status.RoleBinding != after.Status.RoleBinding
}

func (r *Reconciler) requestsForCluster(ctx context.Context, object client.Object) []reconcile.Request {
	var list pgtoolboxv1alpha1.PgAccessRequestList
	if err := r.Client.List(ctx, &list, client.InNamespace(object.GetNamespace()), client.MatchingFields{indexCluster: object.GetName()}); err != nil {
		logf.FromContext(ctx).Error(err, "list PgAccessRequests for a Cluster")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return requests
}

func controllerOptions() controller.Options {
	warmup := true
	return controller.Options{MaxConcurrentReconciles: 2, ReconciliationTimeout: reconciliationTimeout, EnableWarmup: &warmup}
}
