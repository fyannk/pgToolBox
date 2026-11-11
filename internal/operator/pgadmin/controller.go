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

// Package pgadmin is the PgAdmin controller of the operator:
// one pgAdmin per team namespace, registered against that namespace's
// CloudNativePG Clusters as shared servers by a Job, with the offered
// credentials delivered to libpq by the passfile-sync sidecar through the
// Pod's name-scoped Role. The reconciler never holds a password.
package pgadmin

import (
	"context"
	"crypto/rand"
	"slices"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	routev1 "github.com/openshift/api/route/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/conditions"
	"github.com/fyannk/pgToolBox/internal/operator/configuration"
	"github.com/fyannk/pgToolBox/internal/operator/reconcile"
	"github.com/fyannk/pgToolBox/pkg/discovery"
)

// RBAC of the PgAdmin controller. The Secret verbs are what lets the
// operator grant the pgAdmin Pod its name-scoped Role (RBAC escalation
// prevention: a Role can grant only what its writer holds); the operator
// itself reads Secret metadata only, and the content of no Secret but its
// own generated ones.
//
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgadmins,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgadmins/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgadminaccesses,verbs=get;list;watch
// Controller owner references set blockOwnerDeletion, which an API server
// enforcing OwnerReferencesPermissionEnforcement (OpenShift) allows only to
// whoever may update the owner's finalizers.
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgadmins/finalizers;pgadminaccesses/finalizers,verbs=update
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters;poolers,verbs=get;list;watch
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=databaseroles;databases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services;serviceaccounts;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies;ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes/custom-host,verbs=create;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,resourceNames=kubernetes,verbs=get
// +kubebuilder:rbac:groups=security.openshift.io,resources=securitycontextconstraints,verbs=list
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// EventRecorder is the part of the events.k8s.io recorder the controller
// uses (the operator's controllers share it).
type EventRecorder = conditions.EventRecorder

// Features are the optional APIs, probed once at start
// (discovery.ProbeKinds: a scoped List(Limit 1) in every watched
// namespace). The zero Result is "unavailable".
type Features struct {
	Routes, Gateways, DatabaseRoles, Poolers discovery.Result
	// Databases is the CloudNativePG Database API, which configDatabase
	// needs.
	Databases discovery.Result
	// OpenShift is true when SecurityContextConstraints are listable: the
	// platform assigns UIDs and fsGroups, so the Pod fixes neither.
	OpenShift bool
}

// Reconciler reconciles PgAdmin objects.
type Reconciler struct {
	Client    client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  EventRecorder
	Config    configuration.Config
	Features  Features
	// Random fills generated secrets; crypto/rand by default.
	Random func([]byte) (int, error)
	// Now is the clock; time.Now by default.
	Now func() time.Time
}

func (r *Reconciler) applier() reconcile.Applier {
	return reconcile.Applier{Client: r.Client, APIReader: r.APIReader, Scheme: r.Scheme}
}

func (r *Reconciler) random(b []byte) (int, error) {
	if r.Random != nil {
		return r.Random(b)
	}
	return rand.Read(b)
}

// Field indexes of the reverse lookups: no list-and-filter on a watch
// event.
const (
	indexClusterRef   = "pgadmin.spec.clusterRefs"
	indexSelector     = "pgadmin.spec.clusterSelector"
	indexClientSecret = "pgadmin.spec.auth.oauth2.clientSecretRef" // #nosec G101 -- index name.
	indexPooler       = "pgadmin.spec.endpoints.pooler"
	// indexCrossNamespace holds the namespaces a PgAdmin names in
	// crossNamespaceClusterRefs: an object of such a namespace (a Cluster,
	// DatabaseRole, Pooler) maps to the PgAdmins of other namespaces.
	indexCrossNamespace = "pgadmin.spec.crossNamespaceClusterRefs.namespace"
	// indexConfigCluster holds spec.configDatabase.clusterRef.
	indexConfigCluster = "pgadmin.spec.configDatabase.clusterRef"
	selectorIndexed    = "selector"
)

// IndexFields registers the PgAdmin field indexes.
func IndexFields(ctx context.Context, indexer client.FieldIndexer) error {
	for field, extract := range fieldIndexes() {
		if err := indexer.IndexField(ctx, &pgtoolboxv1alpha1.PgAdmin{}, field, extract); err != nil {
			return err
		}
	}
	return nil
}

// fieldIndexes are the PgAdmin indexes by field name.
func fieldIndexes() map[string]client.IndexerFunc {
	return map[string]client.IndexerFunc{
		indexClusterRef: func(obj client.Object) []string {
			refs := obj.(*pgtoolboxv1alpha1.PgAdmin).Spec.ClusterRefs
			out := make([]string, 0, len(refs))
			for _, ref := range refs {
				out = append(out, ref.Name)
			}
			return out
		},
		indexSelector: func(obj client.Object) []string {
			if obj.(*pgtoolboxv1alpha1.PgAdmin).Spec.ClusterSelector != nil {
				return []string{selectorIndexed}
			}
			return nil
		},
		indexClientSecret: func(obj client.Object) []string {
			if o := obj.(*pgtoolboxv1alpha1.PgAdmin).Spec.Auth.OAuth2; o != nil {
				return []string{o.ClientSecretRef.Name}
			}
			return nil
		},
		indexCrossNamespace: func(obj client.Object) []string {
			p := obj.(*pgtoolboxv1alpha1.PgAdmin)
			var out []string
			for _, ref := range p.Spec.CrossNamespaceClusterRefs {
				if ref.Namespace != p.Namespace && !slices.Contains(out, ref.Namespace) {
					out = append(out, ref.Namespace)
				}
			}
			return out
		},
		indexConfigCluster: func(obj client.Object) []string {
			if c := obj.(*pgtoolboxv1alpha1.PgAdmin).Spec.ConfigDatabase; c != nil {
				return []string{c.ClusterRef.Name}
			}
			return nil
		},
		indexPooler: func(obj client.Object) []string {
			var out []string
			for _, endpoint := range obj.(*pgtoolboxv1alpha1.PgAdmin).Spec.Endpoints {
				if name, ok := endpoint.PoolerName(); ok {
					out = append(out, name)
				}
			}
			return out
		},
	}
}

// OwnedSelector selects every object the operator creates:
// managed-by=pgtoolbox, the one selector the controllers sharing the
// manager agree on.
func OwnedSelector() labels.Selector {
	return labels.SelectorFromSet(labels.Set{pgtoolboxv1alpha1.LabelManagedBy: pgtoolboxv1alpha1.ManagedByValue})
}

// CacheByObject returns the label-filtered informers of the kinds this
// controller owns (cache.Options.ByObject): the operator caches what it
// created and nothing else of those kinds. Secrets are watched metadata-only
// and are not label-filtered here: the EvidenceAgent controller, which
// shares the manager, watches unlabelled object-store Secrets, and the
// OAuth2 client Secret (reload label) is a user Secret too. CNPG kinds are
// not filtered: they are not the operator's.
func CacheByObject(features Features) map[client.Object]cache.ByObject {
	owned := cache.ByObject{Label: OwnedSelector()}
	byObject := map[client.Object]cache.ByObject{
		&appsv1.Deployment{}:            owned,
		&batchv1.Job{}:                  owned,
		&corev1.Pod{}:                   owned,
		&corev1.Service{}:               owned,
		&corev1.ServiceAccount{}:        owned,
		&corev1.PersistentVolumeClaim{}: owned,
		&rbacv1.Role{}:                  owned,
		&rbacv1.RoleBinding{}:           owned,
		&networkingv1.NetworkPolicy{}:   owned,
		&networkingv1.Ingress{}:         owned,
	}
	if features.Routes.Usable() {
		byObject[&routev1.Route{}] = owned
	}
	if features.Gateways.Usable() {
		byObject[&gatewayv1.HTTPRoute{}] = owned
	}
	return byObject
}

// CachedKinds are the kinds this controller's informers hold, for
// pgtoolbox_cached_objects: the PgAdmins, every label-filtered owned kind
// (CacheByObject), the CloudNativePG kinds it watches (not filtered: not
// the operator's) and the metadata-only Secret informer. Kept next to
// SetupWithManager so a new watch updates both.
func CachedKinds(features Features) []client.Object {
	kinds := []client.Object{&pgtoolboxv1alpha1.PgAdmin{}, &pgtoolboxv1alpha1.PgAdminAccess{}, &cnpgv1.Cluster{}}
	for object := range CacheByObject(features) {
		kinds = append(kinds, object)
	}
	if features.DatabaseRoles.Usable() {
		kinds = append(kinds, &cnpgv1.DatabaseRole{})
	}
	if features.Databases.Usable() {
		kinds = append(kinds, &cnpgv1.Database{})
	}
	if features.Poolers.Usable() {
		kinds = append(kinds, &cnpgv1.Pooler{})
	}
	secret := &metav1.PartialObjectMetadata{}
	secret.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
	return append(kinds, secret)
}

// SetupWithManager registers the controller. Watches: the PgAdmin itself
// (generation, annotation or label change), every owned kind, the Pods of
// pgAdmin (their facts are WorkloadReady's reasons), CNPG Clusters,
// DatabaseRoles and Poolers through the field indexes, and the metadata of
// reload-labelled Secrets. No polling: every input is a watch.
func (r *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	if err := IndexFields(ctx, mgr.GetFieldIndexer()); err != nil {
		return err
	}
	b := ctrl.NewControllerManagedBy(mgr).
		Named("pgadmin").
		For(&pgtoolboxv1alpha1.PgAdmin{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}, predicate.LabelChangedPredicate{},
		))).
		Owns(&appsv1.Deployment{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&corev1.Secret{}, builder.OnlyMetadata).
		Watches(&corev1.PersistentVolumeClaim{}, handler.EnqueueRequestsFromMapFunc(byInstanceLabel)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(byInstanceLabel)).
		Watches(&cnpgv1.Cluster{}, handler.EnqueueRequestsFromMapFunc(r.forCluster)).
		// Cross-namespace grants and what they produce in the grant's
		// namespace (owned by the grant, labelled with this PgAdmin).
		Watches(&pgtoolboxv1alpha1.PgAdminAccess{}, handler.EnqueueRequestsFromMapFunc(forGrant)).
		Watches(&rbacv1.Role{}, handler.EnqueueRequestsFromMapFunc(byAccessLabels)).
		Watches(&rbacv1.RoleBinding{}, handler.EnqueueRequestsFromMapFunc(byAccessLabels)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.forClientSecret), builder.OnlyMetadata).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: r.Config.MaxConcurrentReconciles,
			ReconciliationTimeout:   r.Config.ReconciliationTimeout,
			EnableWarmup:            new(true),
		})
	if r.Features.DatabaseRoles.Usable() {
		// Owned for configDatabase, mapped for the offered roles.
		b = b.Watches(&cnpgv1.DatabaseRole{}, handler.EnqueueRequestsFromMapFunc(r.forDatabaseRole))
	}
	if r.Features.Databases.Usable() {
		b = b.Owns(&cnpgv1.Database{})
	}
	if r.Features.Poolers.Usable() {
		b = b.Watches(&cnpgv1.Pooler{}, handler.EnqueueRequestsFromMapFunc(r.forPooler))
	}
	if r.Features.Routes.Usable() {
		b = b.Owns(&routev1.Route{})
	}
	if r.Features.Gateways.Usable() {
		b = b.Owns(&gatewayv1.HTTPRoute{})
	}
	return b.Complete(r)
}

// byInstanceLabel maps a generated object to its PgAdmin by label (Pods,
// and the settings volume, which has no owner under Retain).
func byInstanceLabel(_ context.Context, obj client.Object) []ctrlreconcile.Request {
	name := obj.GetLabels()[pgtoolboxv1alpha1.PgAdminLabelKey]
	if name == "" {
		return nil
	}
	return []ctrlreconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
}

// forCluster maps a Cluster to the PgAdmins that name it or select it.
func (r *Reconciler) forCluster(ctx context.Context, obj client.Object) []ctrlreconcile.Request {
	clusterLabels := obj.GetLabels()
	if clusterLabels == nil {
		clusterLabels = map[string]string{}
	}
	return r.forClusterName(ctx, obj.GetNamespace(), obj.GetName(), clusterLabels)
}

func (r *Reconciler) forClusterName(ctx context.Context, namespace, name string, clusterLabels map[string]string) []ctrlreconcile.Request {
	seen := map[types.NamespacedName]bool{}
	var requests []ctrlreconcile.Request
	add := func(list *pgtoolboxv1alpha1.PgAdminList, match func(*pgtoolboxv1alpha1.PgAdmin) bool) {
		for i := range list.Items {
			p := &list.Items[i]
			key := types.NamespacedName{Namespace: p.Namespace, Name: p.Name}
			if !seen[key] && match(p) {
				seen[key] = true
				requests = append(requests, ctrlreconcile.Request{NamespacedName: key})
			}
		}
	}
	var byConfig pgtoolboxv1alpha1.PgAdminList
	if err := r.Client.List(ctx, &byConfig, client.InNamespace(namespace), client.MatchingFields{indexConfigCluster: name}); err == nil {
		add(&byConfig, func(*pgtoolboxv1alpha1.PgAdmin) bool { return true })
	}
	for _, request := range r.forRemoteNamespace(ctx, namespace) {
		if key := request.NamespacedName; !seen[key] {
			seen[key] = true
			requests = append(requests, request)
		}
	}
	var byRef pgtoolboxv1alpha1.PgAdminList
	if err := r.Client.List(ctx, &byRef, client.InNamespace(namespace), client.MatchingFields{indexClusterRef: name}); err == nil {
		add(&byRef, func(*pgtoolboxv1alpha1.PgAdmin) bool { return true })
	}
	var bySelector pgtoolboxv1alpha1.PgAdminList
	if err := r.Client.List(ctx, &bySelector, client.InNamespace(namespace), client.MatchingFields{indexSelector: selectorIndexed}); err == nil {
		add(&bySelector, func(p *pgtoolboxv1alpha1.PgAdmin) bool {
			if clusterLabels == nil {
				// The Cluster is gone or unknown: let every selecting
				// PgAdmin re-resolve.
				return true
			}
			selector, err := metav1.LabelSelectorAsSelector(p.Spec.ClusterSelector)
			return err == nil && selector.Matches(labels.Set(clusterLabels))
		})
	}
	return requests
}

func (r *Reconciler) forDatabaseRole(ctx context.Context, obj client.Object) []ctrlreconcile.Request {
	role, ok := obj.(*cnpgv1.DatabaseRole)
	if !ok {
		return nil
	}
	return r.forClusterName(ctx, role.Namespace, role.Spec.ClusterRef.Name, nil)
}

func (r *Reconciler) forPooler(ctx context.Context, obj client.Object) []ctrlreconcile.Request {
	requests := r.forRemoteNamespace(ctx, obj.GetNamespace())
	var list pgtoolboxv1alpha1.PgAdminList
	if err := r.Client.List(ctx, &list, client.InNamespace(obj.GetNamespace()), client.MatchingFields{indexPooler: obj.GetName()}); err != nil {
		return requests
	}
	for i := range list.Items {
		requests = append(requests, ctrlreconcile.Request{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: list.Items[i].Name}})
	}
	return requests
}

func (r *Reconciler) forClientSecret(ctx context.Context, obj client.Object) []ctrlreconcile.Request {
	var list pgtoolboxv1alpha1.PgAdminList
	if err := r.Client.List(ctx, &list, client.InNamespace(obj.GetNamespace()), client.MatchingFields{indexClientSecret: obj.GetName()}); err != nil {
		return nil
	}
	requests := make([]ctrlreconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		requests = append(requests, ctrlreconcile.Request{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: list.Items[i].Name}})
	}
	return requests
}
