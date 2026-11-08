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

package cache

import (
	"reflect"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	toolscache "k8s.io/client-go/tools/cache"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/console/evidence"
)

// Source keys: the stable names of the console's sources, used in
// viewmodel.SourceState.Source and in Manager.Synced.
const (
	SourceClusters             = "clusters"
	SourcePods                 = "pods"
	SourceEvents               = "events"
	SourcePVCs                 = "pvcs"
	SourceServices             = "services"
	SourcePDBs                 = "pdbs"
	SourceLeases               = "leases"
	SourceBackups              = "backups"
	SourceScheduledBackups     = "scheduledBackups"
	SourcePoolers              = "poolers"
	SourceDatabases            = "databases"
	SourceDatabaseRoles        = "databaseRoles"
	SourcePublications         = "publications"
	SourceSubscriptions        = "subscriptions"
	SourceImageCatalogs        = "imageCatalogs"
	SourceFailoverQuorums      = "failoverQuorums"
	SourceObjectStores         = "objectStores"
	SourceClusterImageCatalogs = "clusterImageCatalogs"
	// SourcePgAdmins and SourceEvidenceAgents are pgToolBox's own kinds
	// (api/v1alpha1): optional, probed like the CNPG kinds, never
	// label-filtered (the operator does not label them for the console).
	SourcePgAdmins       = "pgAdmins"
	SourceEvidenceAgents = "evidenceAgents"
	// SourceAccessRequests are the PgAccessRequests the access-request
	// screen lists; the console's Role grants get/list/watch on them.
	SourceAccessRequests = "accessRequests"
	// SourceClusterStorage is a second, unstructured view of the same
	// Clusters, stripped to the storage fields the console's typed API
	// (cloudnative-pg/api) does not know: the fork's autoGrow stanza
	// (origin/dev/64 api/v1/cluster_types.go AutoGrowConfiguration). The
	// typed decode drops unknown fields, so they can only be read raw.
	SourceClusterStorage = "clusterStorage"
)

// ClusterLabel is the label CNPG sets on every instance pod, pooler pod,
// PVC, Service and PDB of a Cluster; it is the label filter of those informers.
const ClusterLabel = "cnpg.io/cluster"

// ObjectStoreGVK is the barman-cloud plugin's ObjectStore, read as
// unstructured: its Go types are not a dependency of this module and
// only status.serverRecoveryWindow is read.
var ObjectStoreGVK = schema.GroupVersionKind{Group: "barmancloud.cnpg.io", Version: "v1", Kind: "ObjectStore"}

// Kind is one informer the console may run.
type Kind struct {
	// Source is the stable source key.
	Source string
	// GVK is the kind's group, version and kind.
	GVK schema.GroupVersionKind
	// Required kinds are the console's reason to exist in a namespace:
	// clusters, pods and events. A required kind that is forbidden or
	// absent is reported, never fatal — the rest of the namespace stays
	// observable.
	Required bool
	// Label filters the informer; nil watches every object of the kind
	// in the namespace (CNPG kinds are never label-filtered: they are not
	// owned).
	Label labels.Selector
	// ClusterScoped kinds are probed and watched cluster-wide.
	ClusterScoped bool
	// Transform replaces the default transform (managedFields stripped)
	// for this kind; nil keeps the default. EvidenceAgents keep their
	// status-subresource managedFields: the evidence runner calls an agent
	// only when the operator's field manager wrote its endpoint and CA.
	Transform toolscache.TransformFunc
	// prototype is a fresh object of the kind.
	prototype func() client.Object
}

// Prototype returns a new, empty object of the kind.
func (k Kind) Prototype() client.Object { return k.prototype() }

// typeDescription is how client-go's reflector names the kind in a watch
// error, so the error handler can attribute it.
func (k Kind) typeDescription() string {
	obj := k.prototype()
	if u, ok := obj.(*unstructured.Unstructured); ok {
		return u.GroupVersionKind().String()
	}
	return reflect.TypeOf(obj).String()
}

func hasClusterLabel() labels.Selector {
	selector, err := labels.Parse(ClusterLabel)
	if err != nil {
		panic(err) // a constant selector; unreachable
	}
	return selector
}

func typed[T client.Object](source string, gvk schema.GroupVersionKind, required bool, label labels.Selector, newObj func() T) Kind {
	return Kind{Source: source, GVK: gvk, Required: required, Label: label, prototype: func() client.Object { return newObj() }}
}

func withTransform(kind Kind, transform toolscache.TransformFunc) Kind {
	kind.Transform = transform
	return kind
}

func cnpgGVK(kind string) schema.GroupVersionKind {
	return cnpgv1.SchemeGroupVersion.WithKind(kind)
}

// Kinds returns the informers the console may run, in a fixed order. The
// cluster selector narrows the Cluster informer; children are filtered by
// the read model against the selected Clusters. ClusterImageCatalogs are
// listed only when the opt-in ClusterRole is expected.
// Secrets and ConfigMaps are not here, on purpose: the console caches no
// content of either, and the cache refuses to start an
// informer for a kind it was not given.
func Kinds(clusterSelector labels.Selector, clusterCatalogs bool) []Kind {
	if clusterSelector != nil && clusterSelector.Empty() {
		clusterSelector = nil
	}
	core := corev1.SchemeGroupVersion
	kinds := []Kind{
		withTransform(typed(SourceClusters, cnpgGVK("Cluster"), true, clusterSelector, func() *cnpgv1.Cluster { return &cnpgv1.Cluster{} }), ClusterTransform),
		typed(SourcePods, core.WithKind("Pod"), true, hasClusterLabel(), func() *corev1.Pod { return &corev1.Pod{} }),
		typed(SourceEvents, core.WithKind("Event"), true, nil, func() *corev1.Event { return &corev1.Event{} }),
		typed(SourcePVCs, core.WithKind("PersistentVolumeClaim"), false, hasClusterLabel(), func() *corev1.PersistentVolumeClaim { return &corev1.PersistentVolumeClaim{} }),
		// The rw/ro/r and pooler Services: the addresses the wiring drawing names.
		typed(SourceServices, core.WithKind("Service"), false, hasClusterLabel(), func() *corev1.Service { return &corev1.Service{} }),
		typed(SourcePDBs, policyv1.SchemeGroupVersion.WithKind("PodDisruptionBudget"), false, hasClusterLabel(), func() *policyv1.PodDisruptionBudget { return &policyv1.PodDisruptionBudget{} }),
		// The primary Lease (CNPG 1.30+) is named after its Cluster and
		// carries no cnpg.io/cluster label, so Leases are watched
		// unfiltered in the watched namespaces, managedFields stripped.
		typed(SourceLeases, coordinationv1.SchemeGroupVersion.WithKind("Lease"), false, nil, func() *coordinationv1.Lease { return &coordinationv1.Lease{} }),
		typed(SourceBackups, cnpgGVK("Backup"), false, nil, func() *cnpgv1.Backup { return &cnpgv1.Backup{} }),
		typed(SourceScheduledBackups, cnpgGVK("ScheduledBackup"), false, nil, func() *cnpgv1.ScheduledBackup { return &cnpgv1.ScheduledBackup{} }),
		typed(SourcePoolers, cnpgGVK("Pooler"), false, nil, func() *cnpgv1.Pooler { return &cnpgv1.Pooler{} }),
		typed(SourceDatabases, cnpgGVK("Database"), false, nil, func() *cnpgv1.Database { return &cnpgv1.Database{} }),
		typed(SourceDatabaseRoles, cnpgGVK("DatabaseRole"), false, nil, func() *cnpgv1.DatabaseRole { return &cnpgv1.DatabaseRole{} }),
		typed(SourcePublications, cnpgGVK("Publication"), false, nil, func() *cnpgv1.Publication { return &cnpgv1.Publication{} }),
		typed(SourceSubscriptions, cnpgGVK("Subscription"), false, nil, func() *cnpgv1.Subscription { return &cnpgv1.Subscription{} }),
		typed(SourceImageCatalogs, cnpgGVK("ImageCatalog"), false, nil, func() *cnpgv1.ImageCatalog { return &cnpgv1.ImageCatalog{} }),
		typed(SourceFailoverQuorums, cnpgGVK("FailoverQuorum"), false, nil, func() *cnpgv1.FailoverQuorum { return &cnpgv1.FailoverQuorum{} }),
		typed(SourcePgAdmins, pgtoolboxv1alpha1.GroupVersion.WithKind("PgAdmin"), false, nil, func() *pgtoolboxv1alpha1.PgAdmin { return &pgtoolboxv1alpha1.PgAdmin{} }),
		withTransform(typed(SourceEvidenceAgents, pgtoolboxv1alpha1.GroupVersion.WithKind("EvidenceAgent"), false, nil, func() *pgtoolboxv1alpha1.EvidenceAgent { return &pgtoolboxv1alpha1.EvidenceAgent{} }), evidence.CacheTransform),
		typed(SourceAccessRequests, pgtoolboxv1alpha1.GroupVersion.WithKind("PgAccessRequest"), false, nil, func() *pgtoolboxv1alpha1.PgAccessRequest { return &pgtoolboxv1alpha1.PgAccessRequest{} }),
		{Source: SourceObjectStores, GVK: ObjectStoreGVK, prototype: func() client.Object {
			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(ObjectStoreGVK)
			return u
		}},
		{
			// controller-runtime configures one cache per GVK
			// (pkg/cache/cache.go v0.25.2 delegating.caches[gvk]):
			// byObjectFor merges both views of Clusters into one entry,
			// so they carry the same label and transform, and the
			// transform serves both shapes. They remain two informers
			// (structured and unstructured), two watch streams; the raw
			// one holds only identity and autoGrow.
			Source: SourceClusterStorage, GVK: cnpgGVK("Cluster"), Label: clusterSelector,
			Transform: ClusterTransform,
			prototype: func() client.Object { return NewClusterStorage() },
		},
	}
	if clusterCatalogs {
		k := typed(SourceClusterImageCatalogs, cnpgGVK("ClusterImageCatalog"), false, nil, func() *cnpgv1.ClusterImageCatalog { return &cnpgv1.ClusterImageCatalog{} })
		k.ClusterScoped = true
		kinds = append(kinds, k)
	}
	return kinds
}

// NewClusterStorage is an empty object of the clusterStorage source:
// pass it to Reader().Get to read a Cluster's raw storage fields.
func NewClusterStorage() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(cnpgGVK("Cluster"))
	return u
}

// stripManagedFields is the cache's default transform.
var stripManagedFields = crcache.TransformStripManagedFields()

// ClusterTransform is the transform of both views of Clusters: a typed
// Cluster loses its managedFields like every other kind, a raw one is
// stripped to its storage fields (ClusterStorageTransform).
func ClusterTransform(obj any) (any, error) {
	if _, raw := obj.(*unstructured.Unstructured); raw {
		return ClusterStorageTransform(obj)
	}
	return stripManagedFields(obj)
}

// ClusterStorageTransform keeps of a raw Cluster only its identity and
// the autoGrow stanza of each storage section (spec.storage,
// spec.walStorage, spec.tablespaces[].storage, with the tablespace
// name): the second watch of Clusters then costs a few hundred bytes
// per Cluster, not a second copy of every spec and status.
func ClusterStorageTransform(obj any) (any, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return obj, nil
	}
	kept := &unstructured.Unstructured{Object: map[string]any{}}
	kept.SetAPIVersion(u.GetAPIVersion())
	kept.SetKind(u.GetKind())
	kept.SetName(u.GetName())
	kept.SetNamespace(u.GetNamespace())
	kept.SetUID(u.GetUID())
	kept.SetResourceVersion(u.GetResourceVersion())
	kept.SetGeneration(u.GetGeneration())
	spec := map[string]any{}
	for _, section := range []string{"storage", "walStorage"} {
		if grow, found, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", section, "autoGrow"); found {
			spec[section] = map[string]any{"autoGrow": runtime.DeepCopyJSONValue(grow)}
		}
	}
	if tablespaces, found, _ := unstructured.NestedSlice(u.Object, "spec", "tablespaces"); found {
		var kept []any
		for _, item := range tablespaces {
			tbs, ok := item.(map[string]any)
			if !ok {
				continue
			}
			grow, found, _ := unstructured.NestedFieldNoCopy(tbs, "storage", "autoGrow")
			if !found {
				continue
			}
			kept = append(kept, map[string]any{"name": tbs["name"], "storage": map[string]any{"autoGrow": runtime.DeepCopyJSONValue(grow)}})
		}
		if len(kept) > 0 {
			spec["tablespaces"] = kept
		}
	}
	if len(spec) > 0 {
		kept.Object["spec"] = spec
	}
	return kept, nil
}

// NewScheme registers the typed kinds the console reads.
func NewScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, cnpgv1.AddToScheme, pgtoolboxv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			return nil, err
		}
	}
	return scheme, nil
}

// gvkOf resolves an object's kind through the scheme, falling back to an
// unstructured object's own GVK.
func gvkOf(obj runtime.Object, scheme *runtime.Scheme) (schema.GroupVersionKind, error) {
	if u, ok := obj.(*unstructured.Unstructured); ok {
		return u.GroupVersionKind(), nil
	}
	return apiutil.GVKForObject(obj, scheme)
}
