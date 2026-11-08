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

package selfresolve

import (
	"context"
	"errors"
	"log/slog"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fyannk/pgToolBox/internal/backupresolve"
	"github.com/fyannk/pgToolBox/pkg/discovery"
)

// The watched kinds, addressed directly: the agent needs no discovery.
var (
	ClusterResource     = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}
	ObjectStoreResource = schema.GroupVersionResource{Group: discovery.ObjectStoreGVK.Group, Version: discovery.ObjectStoreGVK.Version, Resource: "objectstores"}
	StanzaResource      = schema.GroupVersionResource{Group: discovery.StanzaGVK.Group, Version: discovery.StanzaGVK.Version, Resource: "stanzas"}
	ArchiveResource     = schema.GroupVersionResource{Group: discovery.ArchiveGVK.Group, Version: discovery.ArchiveGVK.Version, Resource: "archives"}
)

// watchedObject is the second object a resolution read: the ObjectStore,
// or a pgBackRest plugin's Stanza or Archive; ok is false when the Cluster
// names none.
func (o Outcome) watchedObject() (resource schema.GroupVersionResource, gvk schema.GroupVersionKind, name string, ok bool) {
	switch {
	case o.PgBackRest != nil && o.PgBackRest.Kind == backupresolve.PgBackRestKindArchive:
		return ArchiveResource, discovery.ArchiveGVK, o.PgBackRest.ObjectName, true
	case o.PgBackRest != nil:
		return StanzaResource, discovery.StanzaGVK, o.PgBackRest.ObjectName, true
	case o.ObjectStoreName != "":
		return ObjectStoreResource, discovery.ObjectStoreGVK, o.ObjectStoreName, true
	}
	return schema.GroupVersionResource{}, schema.GroupVersionKind{}, "", false
}

// Watcher follows the Cluster and the ObjectStore (or the Stanza or
// Archive) a resolution read, each
// by name (a field selector on metadata.name, which the API server
// authorizes against the Role's resourceNames, so the Role can name the
// one object). An informer rather than a periodic get: changes arrive as
// events, and the reflector re-lists and re-watches by itself.
//
// The Secret values the process was built with are re-read every
// SecretRecheck instead (the Role grants get on them, not watch).
type Watcher struct {
	client   dynamic.Interface
	resolver *Resolver
	logger   *slog.Logger
	// recheck is SecretRecheck; tests shorten it.
	recheck time.Duration
}

// NewWatcher builds a Watcher on a dynamic client.
func NewWatcher(client dynamic.Interface, resolver *Resolver) (*Watcher, error) {
	if client == nil || resolver == nil {
		return nil, errors.New("self-resolution watch configuration is invalid")
	}
	return &Watcher{client: client, resolver: resolver, logger: resolver.logger, recheck: SecretRecheck}, nil
}

// Wait blocks until what the watched objects resolve to differs from
// outcome (or outcome.RetryAfter elapsed) and returns why; it returns ""
// when ctx ends first. The caller then exits so the kubelet restarts the
// agent with a fresh resolution.
func (w *Watcher) Wait(ctx context.Context, outcome Outcome) string {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	changed := make(chan struct{}, 1)
	signal := func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	}
	clusters := w.informer(ClusterResource, w.resolver.cluster, signal)
	go clusters.Run(ctx.Done())
	var objectStores cache.SharedIndexInformer
	resource, gvk, objectName, watchesObject := outcome.watchedObject()
	if watchesObject {
		objectStores = w.informer(resource, objectName, signal)
		go objectStores.Run(ctx.Done())
	}
	// Events delivered before both caches synced are not compared (a
	// half-filled store would resolve to a missing object); one signal
	// once they have synced compares whatever arrived meanwhile.
	synced := []cache.InformerSynced{clusters.HasSynced}
	if objectStores != nil {
		synced = append(synced, objectStores.HasSynced)
	}
	go func() {
		if cache.WaitForCacheSync(ctx.Done(), synced...) {
			signal()
		}
	}()
	var retry <-chan time.Time
	if outcome.RetryAfter > 0 {
		timer := time.NewTimer(outcome.RetryAfter)
		defer timer.Stop()
		retry = timer.C
	}
	var recheck <-chan time.Time
	if outcome.Failure == nil && len(outcome.secrets) > 0 {
		ticker := time.NewTicker(w.recheck)
		defer ticker.Stop()
		recheck = ticker.C
	}
	for {
		select {
		case <-ctx.Done():
			return ""
		case <-retry:
			return "the last resolution failed for a reason the watch cannot see (a Secret, or the API server); trying again"
		case <-recheck:
			changed, err := w.resolver.secretsChanged(ctx, outcome)
			if err != nil {
				// The error names the Secret, never a value.
				w.logger.WarnContext(ctx, "re-reading a Secret the agent was built with failed; retrying", slog.String("error", err.Error()))
			}
			if changed {
				return "a Secret the agent reads (endpoint CA, region, cipher pass or credential) changed"
			}
			continue
		case <-changed:
		}
		if !clusters.HasSynced() || (objectStores != nil && !objectStores.HasSynced()) {
			continue
		}
		reader := storeReader{namespace: w.resolver.namespace, clusters: clusters.GetStore()}
		if objectStores != nil {
			reader.objectStores, reader.objectGVK, reader.objectStoreName, reader.objectResource = objectStores.GetStore(), gvk, objectName, resource
		}
		current, err := w.resolver.locate(ctx, reader)
		if errors.Is(err, errNotWatched) {
			return "the Cluster now names another ObjectStore, Stanza or Archive"
		}
		if err != nil {
			// The stores answer from memory; nothing else can fail.
			continue
		}
		if current.objectStoreName != outcome.ObjectStoreName || !samePgBackRest(current.pgBackRest, outcome.PgBackRest) {
			return "the Cluster now names another ObjectStore, Stanza or Archive"
		}
		if current.hash() != outcome.Hash {
			return "the Cluster or the object it names changed what the agent reads"
		}
	}
}

// samePgBackRest says whether two resolutions name the same Stanza or
// Archive (or neither).
func samePgBackRest(a, b *backupresolve.PgBackRestLocation) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Kind == b.Kind && a.ObjectName == b.ObjectName
}

// informer watches one object by name, with managedFields stripped (the
// agent keeps the two objects' specs, nothing else).
func (w *Watcher) informer(resource schema.GroupVersionResource, name string, signal func()) cache.SharedIndexInformer {
	selector := fields.OneTermEqualSelector("metadata.name", name).String()
	namespaced := w.client.Resource(resource).Namespace(w.resolver.namespace)
	// ToListWatcherWithWatchListSemantics carries whether the client
	// supports streaming lists, as the generated informers do (the fake
	// client of the tests does not).
	informer := cache.NewSharedIndexInformer(cache.ToListWatcherWithWatchListSemantics(&cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
			options.FieldSelector = selector
			return namespaced.List(ctx, options)
		},
		WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
			options.FieldSelector = selector
			return namespaced.Watch(ctx, options)
		},
	}, w.client), &unstructured.Unstructured{}, 0, cache.Indexers{})
	_ = informer.SetTransform(func(object any) (any, error) {
		if typed, ok := object.(*unstructured.Unstructured); ok {
			typed.SetManagedFields(nil)
		}
		return object, nil
	})
	_, _ = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { signal() },
		UpdateFunc: func(any, any) { signal() },
		DeleteFunc: func(any) { signal() },
	})
	_ = informer.SetWatchErrorHandlerWithContext(func(ctx context.Context, _ *cache.Reflector, err error) {
		w.logger.WarnContext(ctx, "watching the Cluster or the object it names failed; retrying", slog.String("resource", resource.Resource), slog.String("error", err.Error()))
	})
	return informer
}

var errNotWatched = errors.New("the object is not watched")

// storeReader answers the resolver's two reads from the informers'
// stores, with the API's NotFound for an absent object, so a resolution
// from the watch and one from the API agree on the same state. The second
// store holds the one watched ObjectStore, Stanza or Archive; a read of
// another kind or name is errNotWatched.
type storeReader struct {
	namespace       string
	clusters        cache.Store
	objectStores    cache.Store
	objectGVK       schema.GroupVersionKind
	objectStoreName string
	objectResource  schema.GroupVersionResource
}

func (r storeReader) Get(_ context.Context, key client.ObjectKey, object client.Object, _ ...client.GetOption) error {
	switch typed := object.(type) {
	case *cnpgv1.Cluster:
		item, err := r.item(r.clusters, key, ClusterResource)
		if err != nil {
			return err
		}
		return runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, typed)
	case *unstructured.Unstructured:
		if r.objectStores == nil || key.Name != r.objectStoreName || typed.GroupVersionKind() != r.objectGVK {
			return errNotWatched
		}
		item, err := r.item(r.objectStores, key, r.objectResource)
		if err != nil {
			return err
		}
		typed.Object = item.DeepCopy().Object
		return nil
	default:
		return errNotWatched
	}
}

func (r storeReader) item(store cache.Store, key client.ObjectKey, resource schema.GroupVersionResource) (*unstructured.Unstructured, error) {
	value, exists, err := store.GetByKey(key.Namespace + "/" + key.Name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, apierrors.NewNotFound(resource.GroupResource(), key.Name)
	}
	item, ok := value.(*unstructured.Unstructured)
	if !ok {
		return nil, errNotWatched
	}
	return item, nil
}

func (storeReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errNotWatched
}
