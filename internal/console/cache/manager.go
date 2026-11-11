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

// Package cache is the console's informer layer: one
// controller-runtime cache.New — not a manager, the console runs no
// controller — over the watched namespaces only, every informer
// label-filtered where a label exists, managedFields stripped, bookmarks
// on, Secrets and ConfigMaps never cached. Optional kinds are probed with
// a scoped List(Limit 1) (pkg/discovery) per namespace before they are
// watched, and probed again periodically: a CRD installed or an RBAC
// grant added after start rebuilds the cache so the new source appears
// without a restart.
//
// The package is read-only by construction: it hands out a client.Reader
// and nothing that writes.
package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/fyannk/pgToolBox/internal/console/config"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/discovery"
)

// ErrNotReady is returned by the Reader before the first cache synced.
var ErrNotReady = errors.New("the console's informer cache has not synced yet")

// syncTimeout bounds how long one epoch waits for its initial lists
// before it is served with the unsynced sources marked pending.
const syncTimeout = 2 * time.Minute

// Options wires a Manager.
type Options struct {
	Config     config.Console
	RestConfig *rest.Config
	Logger     *slog.Logger
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// Prober performs the discovery Lists; an uncached client on
	// RestConfig when nil.
	Prober discovery.Lister
	// Tap, when set, receives every add, update and delete the informers
	// deliver (the history capture). It runs on the
	// informer's goroutine: it must be quick and must not block.
	Tap func(kind Kind, obj client.Object, deleted bool)
	// Seed, when set, receives the complete initial list of every watched
	// source, once per cache epoch, over the namespaces the source is
	// listed in; the initial-list adds then never reach Tap, and Tap
	// deliveries that arrive before the seed are held until after it
	// (recorder). Same constraints as Tap.
	Seed func(kind Kind, namespaces []string, objs []client.Object)
}

// Manager owns the informer cache and the account of every source.
type Manager struct {
	cfg        config.Console
	restConfig *rest.Config
	httpClient *http.Client
	scheme     *runtime.Scheme
	mapper     meta.RESTMapper
	prober     discovery.Lister
	tap        func(kind Kind, obj client.Object, deleted bool)
	seed       func(kind Kind, namespaces []string, objs []client.Object)
	logger     *slog.Logger
	kinds      []Kind
	tracker    *tracker
	hub        hub
	informerID atomic.Int64

	mu      sync.RWMutex
	current *epoch
	plan    plan
}

// epoch is one built cache and the plan it was built from.
type epoch struct {
	cache    crcache.Cache
	plan     plan
	cancel   context.CancelFunc
	recorder *recorder
}

// plan maps a source to the namespaces it may be watched in ("" for a
// cluster-scoped kind). A source with no namespace is not watched.
type plan map[string][]string

func (p plan) equal(o plan) bool {
	return maps.EqualFunc(p, o, slices.Equal[[]string])
}

// New validates the options and prepares the clients; nothing is
// contacted until Run.
func New(opts Options) (*Manager, error) {
	if opts.RestConfig == nil {
		return nil, errors.New("cache: a rest config is required")
	}
	if len(opts.Config.WatchNamespaces) == 0 {
		return nil, errors.New("cache: no watched namespace (WATCH_NAMESPACES is never empty)")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	scheme, err := NewScheme()
	if err != nil {
		return nil, fmt.Errorf("cache: building the scheme: %w", err)
	}
	httpClient, err := rest.HTTPClientFor(opts.RestConfig)
	if err != nil {
		return nil, fmt.Errorf("cache: building the HTTP client: %w", err)
	}
	dynamic, err := apiutil.NewDynamicRESTMapper(opts.RestConfig, httpClient)
	if err != nil {
		return nil, fmt.Errorf("cache: building the REST mapper: %w", err)
	}
	mapper := newMemoMapper(dynamic)
	prober := opts.Prober
	if prober == nil {
		direct, err := client.New(opts.RestConfig, client.Options{HTTPClient: httpClient, Scheme: scheme, Mapper: mapper})
		if err != nil {
			return nil, fmt.Errorf("cache: building the probe client: %w", err)
		}
		prober = direct
	}
	m := &Manager{
		cfg:        opts.Config,
		restConfig: opts.RestConfig,
		httpClient: httpClient,
		scheme:     scheme,
		mapper:     mapper,
		prober:     prober,
		tap:        opts.Tap,
		seed:       opts.Seed,
		logger:     logger,
		kinds:      Kinds(opts.Config.ClusterSelector, opts.Config.AllowClusterCatalogs),
		tracker:    newTracker(now),
	}
	cachedObjects.set(m)
	return m, nil
}

// Run probes, builds and serves the cache until ctx is done, rebuilding
// it when a periodic re-probe finds the set of usable sources changed.
// A probe error other than Forbidden or not-served on the first round
// fails the start; on later rounds it is logged and the
// current cache keeps serving.
func (m *Manager) Run(ctx context.Context) error {
	initial, err := m.probe(ctx)
	if err != nil {
		return err
	}
	if err := m.build(ctx, initial); err != nil {
		return err
	}
	defer m.stopCurrent()
	ticker := time.NewTicker(m.cfg.ReprobeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		next, err := m.probe(ctx)
		if err != nil {
			m.logger.Warn("re-probe of optional APIs failed; keeping the current cache", slog.String("error", err.Error()))
			continue
		}
		m.mu.RLock()
		same := next.equal(m.plan)
		m.mu.RUnlock()
		if same {
			continue
		}
		m.logger.Info("the usable sources changed; rebuilding the informer cache")
		if err := m.build(ctx, next); err != nil {
			m.logger.Error("rebuilding the informer cache failed; keeping the current one", slog.String("error", err.Error()))
		}
	}
}

// probe lists one object of every kind in every watched namespace. The
// round is atomic: the source states are applied only once every List of
// the round has an outcome, so an error that aborts it (a 401, a timeout)
// leaves every state as the previous round set it, and the cache that
// keeps serving is described as it is.
func (m *Manager) probe(ctx context.Context) (plan, error) {
	type outcome struct {
		namespace, source string
		state             viewmodel.SourceStateKind
		reason            string
	}
	next := plan{}
	var outcomes []outcome
	for _, kind := range m.kinds {
		namespaces := m.cfg.WatchNamespaces
		if kind.ClusterScoped {
			namespaces = []string{""}
		}
		for _, ns := range namespaces {
			pctx, cancel := context.WithTimeout(ctx, m.cfg.APIRequestTimeout)
			result, err := discovery.ProbeListable(pctx, m.prober, kind.GVK, ns)
			cancel()
			if err != nil {
				return nil, fmt.Errorf("cache: %w", err)
			}
			switch result.State {
			case discovery.Available:
				next[kind.Source] = append(next[kind.Source], ns)
				outcomes = append(outcomes, outcome{ns, kind.Source, viewmodel.SourcePending, "initial list in progress"})
			case discovery.Forbidden:
				outcomes = append(outcomes, outcome{ns, kind.Source, viewmodel.SourceForbidden, "the console ServiceAccount is " + result.Reason})
			default:
				outcomes = append(outcomes, outcome{ns, kind.Source, viewmodel.SourceNotInstalled, result.Reason})
			}
		}
	}
	for _, o := range outcomes {
		if o.state == viewmodel.SourcePending {
			// An available source already listed keeps its live or stale
			// state: the informer, not the probe, knows it.
			if state, ok := m.tracker.get(o.namespace, o.source); ok && (state.State == viewmodel.SourceLive || state.State == viewmodel.SourceStale) {
				continue
			}
		}
		m.tracker.set(o.namespace, o.source, o.state, o.reason)
	}
	return next, nil
}

// byObjectFor is the cache's per-kind configuration for the plan: one
// entry per GVK. controller-runtime keeps one configuration per GVK
// (pkg/cache/cache.go v0.25.2: delegating.caches[gvk], filled by ranging
// over ByObject), so two entries for one GVK would be a random pick. Kinds
// that share a GVK (the typed and raw views of Clusters) are merged into
// one entry, over the union of their namespaces, and must agree on the
// label and the transform; a disagreement is a programming error.
//
// The merged entry still serves two informers: controller-runtime keys
// them by GVK and by shape (structured, unstructured), so the typed and
// raw views of Clusters are two LIST/WATCH streams of the same objects.
// The raw one is stripped to identity and the autoGrow stanzas by
// ClusterStorageTransform, well under 1 KB a Cluster.
func byObjectFor(kinds []Kind, p plan) (map[client.Object]crcache.ByObject, []Kind, error) {
	type merged struct {
		prototype client.Object
		entry     crcache.ByObject
		source    string
	}
	byGVK := map[schema.GroupVersionKind]*merged{}
	var order []schema.GroupVersionKind
	var watched []Kind
	for _, kind := range kinds {
		namespaces := p[kind.Source]
		if len(namespaces) == 0 {
			continue
		}
		watched = append(watched, kind)
		m := byGVK[kind.GVK]
		if m == nil {
			m = &merged{prototype: kind.Prototype(), entry: crcache.ByObject{Label: kind.Label, Transform: kind.Transform}, source: kind.Source}
			if !kind.ClusterScoped {
				m.entry.Namespaces = map[string]crcache.Config{}
			}
			byGVK[kind.GVK] = m
			order = append(order, kind.GVK)
		} else if !sameSelector(m.entry.Label, kind.Label) || funcPointer(m.entry.Transform) != funcPointer(kind.Transform) {
			return nil, nil, fmt.Errorf("cache: sources %s and %s share %s with a different label or transform", m.source, kind.Source, kind.GVK)
		}
		if m.entry.Namespaces != nil {
			for _, ns := range namespaces {
				m.entry.Namespaces[ns] = crcache.Config{}
			}
		}
	}
	out := make(map[client.Object]crcache.ByObject, len(order))
	for _, gvk := range order {
		out[byGVK[gvk].prototype] = byGVK[gvk].entry
	}
	return out, watched, nil
}

func sameSelector(a, b labels.Selector) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.String() == b.String()
}

func funcPointer(f toolscache.TransformFunc) uintptr {
	if f == nil {
		return 0
	}
	return reflect.ValueOf(f).Pointer()
}

// build creates, starts and syncs one cache for the plan, then swaps it
// in and stops the previous one.
func (m *Manager) build(ctx context.Context, p plan) error {
	byObject, watched, err := byObjectFor(m.kinds, p)
	if err != nil {
		return err
	}
	defaults := map[string]crcache.Config{}
	for _, ns := range m.cfg.WatchNamespaces {
		defaults[ns] = crcache.Config{}
	}
	resync := m.cfg.ResyncPeriod
	bookmarks := true
	c, err := crcache.New(m.restConfig, crcache.Options{
		HTTPClient:        m.httpClient,
		Scheme:            m.scheme,
		Mapper:            m.mapper,
		SyncPeriod:        &resync,
		DefaultNamespaces: defaults,
		DefaultTransform:  crcache.TransformStripManagedFields(),
		// Bookmarks are the client-go default; stated so a reader of this
		// file need not know it.
		DefaultEnableWatchBookmarks: &bookmarks,
		ByObject:                    byObject,
		// A Get or List of a kind the console was not built for (a Secret,
		// a ConfigMap) is an error, never a lazily started informer.
		ReaderFailOnMissingInformer: true,
		NewInformer:                 m.newInformer,
		DefaultWatchErrorHandler:    m.watchError,
	})
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	watchedSources := map[string]bool{}
	for _, kind := range watched {
		watchedSources[kind.Source] = true
	}
	if err := registerIndexes(ctx, c, watchedSources); err != nil {
		return err
	}
	epochCtx, cancel := context.WithCancel(ctx)
	informers := map[string]crcache.Informer{}
	// planned is this epoch's typed informers by Go type, complete before
	// the cache starts and never written after: the Event handler asks it
	// whether a kind it attributes through is planned and synced, and
	// never asks the cache, whose GetInformer would start an unplanned,
	// unfiltered informer for a kind the plan left out.
	planned := plannedInformers{}
	rec := newRecorder(m.tap, m.seed)
	seeds := map[*sourceRecorder]toolscache.ResourceEventHandlerRegistration{}
	for _, kind := range watched {
		informer, err := c.GetInformer(epochCtx, kind.Prototype())
		if err != nil {
			cancel()
			return fmt.Errorf("cache: informer for %s: %w", kind.Source, err)
		}
		source := rec.source(kind, p[kind.Source])
		registration, err := informer.AddEventHandler(m.handler(epochCtx, c, planned.synced, source))
		if err != nil {
			cancel()
			return fmt.Errorf("cache: event handler for %s: %w", kind.Source, err)
		}
		if source != nil {
			seeds[source] = registration
		}
		informers[kind.Source] = informer
		planned.add(kind.Prototype(), informer)
	}
	// From here on this epoch's lists and watches account for every
	// change: the previous epoch stops recording (it keeps serving reads
	// until the swap).
	m.mu.RLock()
	if m.current != nil {
		m.current.recorder.mute()
	}
	m.mu.RUnlock()
	for source, registration := range seeds {
		go func() {
			// The registration is done once the handler received the whole
			// initial list: the seed is complete. A list that never
			// completes (the sync timeout) seeds whenever it does.
			select {
			case <-registration.HasSyncedChecker().Done():
				source.flush()
			case <-epochCtx.Done():
			}
		}()
	}
	go func() {
		if err := c.Start(epochCtx); err != nil {
			m.logger.Error("informer cache stopped", slog.String("error", err.Error()))
		}
	}()
	syncCtx, syncCancel := context.WithTimeout(epochCtx, syncTimeout)
	c.WaitForCacheSync(syncCtx)
	syncCancel()
	if ctx.Err() != nil {
		cancel()
		return nil //nolint:nilerr // cancelled while syncing: the epoch is torn down and the caller is already stopping
	}
	for source, informer := range informers {
		state, reason := viewmodel.SourceLive, ""
		if !informer.HasSynced() {
			state, reason = viewmodel.SourcePending, "initial list not complete after "+syncTimeout.String()
		}
		for _, ns := range p[source] {
			if current, ok := m.tracker.get(ns, source); ok && current.State == viewmodel.SourceStale {
				continue // the tracked list/watch outcome decides
			}
			m.tracker.set(ns, source, state, reason)
		}
	}

	m.mu.Lock()
	previous := m.current
	m.current = &epoch{cache: c, plan: p, cancel: cancel, recorder: rec}
	m.plan = p
	m.mu.Unlock()
	if previous != nil {
		previous.cancel()
	}
	for _, ns := range m.cfg.WatchNamespaces {
		m.hub.publish(ClusterKey{Namespace: ns})
	}
	return nil
}

func (m *Manager) stopCurrent() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != nil {
		m.current.cancel()
	}
}

// newInformer builds the shared informer with its list/watch tracked.
func (m *Manager) newInformer(lw toolscache.ListerWatcher, obj runtime.Object, resync time.Duration, indexers toolscache.Indexers) toolscache.SharedIndexInformer {
	source := m.sourceOf(obj)
	id := int(m.informerID.Add(1))
	tracked := trackedListWatch{
		inner:   toolscache.ToListerWatcherWithContext(lw),
		outcome: func(err error) { m.tracker.watchedOutcome(source, id, err) },
		source:  source,
	}
	return toolscache.NewSharedIndexInformer(tracked, obj, resync, indexers)
}

// watchError marks the source stale when an established watch drops
// with an error; the next successful list makes it live again.
func (m *Manager) watchError(_ context.Context, r *toolscache.Reflector, err error) {
	description := r.TypeDescription()
	for _, kind := range m.kinds {
		if kind.typeDescription() == description {
			m.tracker.watchedOutcome(kind.Source, -1, err)
			return
		}
	}
}

func (m *Manager) sourceOf(obj runtime.Object) string {
	gvk, err := gvkOf(obj, m.scheme)
	if err != nil {
		return ""
	}
	// Clusters are watched twice, typed and unstructured
	// (clusterStorage): the object's own shape tells the two apart.
	_, raw := obj.(*unstructured.Unstructured)
	for _, kind := range m.kinds {
		_, rawKind := kind.Prototype().(*unstructured.Unstructured)
		if kind.GVK == gvk && raw == rawKind {
			return kind.Source
		}
	}
	return ""
}

// handler publishes the Cluster each changed object belongs to.
func (m *Manager) handler(ctx context.Context, c client.Reader, synced syncedKind, rec *sourceRecorder) toolscache.ResourceEventHandler {
	publish := func(obj any, deleted, initial bool) {
		if tombstone, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
			obj = tombstone.Obj
		}
		object, ok := obj.(client.Object)
		if !ok {
			return
		}
		rec.deliver(object, deleted, initial)
		m.hub.publish(ClusterKey{Namespace: object.GetNamespace(), Name: clusterOf(ctx, c, synced, object)})
	}
	return toolscache.ResourceEventHandlerDetailedFuncs{
		AddFunc:    func(obj any, initial bool) { publish(obj, false, initial) },
		UpdateFunc: func(_, obj any) { publish(obj, false, false) },
		DeleteFunc: func(obj any) { publish(obj, true, false) },
	}
}

// clusterOf attributes an object to its Cluster, "" when it cannot.
func clusterOf(ctx context.Context, c client.Reader, synced syncedKind, obj client.Object) string {
	switch o := obj.(type) {
	case *cnpgv1.Cluster:
		return o.Name
	case *cnpgv1.Backup:
		return o.Spec.Cluster.Name
	case *cnpgv1.ScheduledBackup:
		return o.Spec.Cluster.Name
	case *cnpgv1.Pooler:
		return o.Spec.Cluster.Name
	case *cnpgv1.Database:
		return o.Spec.ClusterRef.Name
	case *cnpgv1.DatabaseRole:
		return o.Spec.ClusterRef.Name
	case *cnpgv1.Publication:
		return o.Spec.ClusterRef.Name
	case *cnpgv1.Subscription:
		return o.Spec.ClusterRef.Name
	case *cnpgv1.FailoverQuorum:
		return o.Name
	case *unstructured.Unstructured:
		if o.GroupVersionKind() == cnpgGVK("Cluster") {
			return o.GetName()
		}
	case *corev1.Event:
		return eventCluster(ctx, c, synced, o)
	}
	if name := obj.GetLabels()[ClusterLabel]; name != "" {
		return name
	}
	return ""
}

// attributionTimeout bounds one cache lookup made to attribute an event.
// The lookup runs inside the Event informer's handler, so a wait here
// holds every later event back; it is in memory and answers at once
// when the target informer has synced, and anything slower is treated
// as an unknown attribution.
const attributionTimeout = 100 * time.Millisecond

// syncedKind reports whether the epoch planned an informer for obj's
// kind and it has synced, without waiting and without asking the cache
// (a GetInformer would start an informer the plan left out).
type syncedKind func(obj client.Object) bool

// plannedInformers are an epoch's typed informers by Go type (the
// unstructured Cluster informer is left out: attribution looks up typed
// objects only).
type plannedInformers map[reflect.Type]crcache.Informer

func (p plannedInformers) add(prototype client.Object, informer crcache.Informer) {
	if _, raw := prototype.(*unstructured.Unstructured); !raw {
		p[reflect.TypeOf(prototype)] = informer
	}
}

func (p plannedInformers) synced(obj client.Object) bool {
	informer, ok := p[reflect.TypeOf(obj)]
	return ok && informer.HasSynced()
}

// eventCluster resolves an event's involved object through the cache;
// the lookup is in memory and bounded by the event's own fields. It
// never waits for an informer: a cache's Get blocks until the target
// kind has synced, and this runs in another informer's handler, so a
// kind not synced yet, a kind this epoch did not plan (a Pooler without
// its RBAC), or a lookup slower than attributionTimeout is an unknown
// attribution ("": the namespace is published whole).
func eventCluster(ctx context.Context, c client.Reader, synced syncedKind, event *corev1.Event) string {
	ref := event.InvolvedObject
	key := types.NamespacedName{Namespace: event.Namespace, Name: ref.Name}
	var (
		target  client.Object
		cluster func() string
	)
	switch ref.Kind {
	case "Cluster":
		return ref.Name
	case "Pod":
		pod := &corev1.Pod{}
		target, cluster = pod, func() string { return pod.Labels[ClusterLabel] }
	case "PersistentVolumeClaim":
		pvc := &corev1.PersistentVolumeClaim{}
		target, cluster = pvc, func() string { return pvc.Labels[ClusterLabel] }
	case "Backup":
		backup := &cnpgv1.Backup{}
		target, cluster = backup, func() string { return backup.Spec.Cluster.Name }
	case "ScheduledBackup":
		scheduled := &cnpgv1.ScheduledBackup{}
		target, cluster = scheduled, func() string { return scheduled.Spec.Cluster.Name }
	case "Pooler":
		pooler := &cnpgv1.Pooler{}
		target, cluster = pooler, func() string { return pooler.Spec.Cluster.Name }
	default:
		return ""
	}
	if synced == nil || !synced(target) {
		return ""
	}
	lookupCtx, cancel := context.WithTimeout(ctx, attributionTimeout)
	defer cancel()
	if c.Get(lookupCtx, key, target) != nil {
		return ""
	}
	return cluster()
}

// Generation is the stamp of the latest change published for the
// Cluster (or its whole namespace): a reader that memoized something
// derived from the Cluster at generation g may reuse it while the
// generation is still g.
func (m *Manager) Generation(key ClusterKey) uint64 { return m.hub.generation(key) }

// Subscribe returns a coalescing subscription to Cluster changes.
func (m *Manager) Subscribe() *Subscription { return m.hub.subscribe() }

// Unsubscribe stops a subscription.
func (m *Manager) Unsubscribe(s *Subscription) { m.hub.unsubscribe(s) }

// Ready reports whether a cache has been built and synced once: the
// readiness probe's answer.
func (m *Manager) Ready() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current != nil
}

// Namespaces are the watched namespaces.
func (m *Manager) Namespaces() []string { return slices.Clone(m.cfg.WatchNamespaces) }

// Sources lists the state of every source in a namespace, including the
// cluster-scoped ones.
func (m *Manager) Sources(namespace string) []viewmodel.SourceState {
	return m.tracker.forNamespace(namespace)
}

// Source returns one source's state; pending when it was never probed.
func (m *Manager) Source(namespace, source string) viewmodel.SourceState {
	if state, ok := m.tracker.get(namespace, source); ok {
		return state
	}
	if state, ok := m.tracker.get("", source); ok {
		return state
	}
	return viewmodel.SourceState{Source: source, State: viewmodel.SourcePending}
}

// Readable reports whether the cache holds a list of the source in the
// namespace that may be rendered: live, or stale (rendered as such).
func (m *Manager) Readable(namespace, source string) bool {
	switch m.Source(namespace, source).State {
	case viewmodel.SourceLive, viewmodel.SourceStale:
		return true
	default:
		return false
	}
}

// Served reports whether the API server serves a kind in at least one
// watched namespace (the discovery evidence of version detection); nil
// when no probe answered either way.
func (m *Manager) Served(source string) *bool {
	var answered bool
	for _, ns := range append(m.Namespaces(), "") {
		state, ok := m.tracker.get(ns, source)
		if !ok {
			continue
		}
		switch state.State {
		case viewmodel.SourceNotInstalled:
			answered = true
		case viewmodel.SourceForbidden:
		default:
			served := true
			return &served
		}
	}
	if answered {
		served := false
		return &served
	}
	return nil
}

// Reader is the read-only view of the current cache. Before the first
// sync it fails with ErrNotReady; afterwards it always reads the newest
// synced epoch.
func (m *Manager) Reader() client.Reader { return epochReader{m} }

type epochReader struct{ m *Manager }

func (r epochReader) current() (crcache.Cache, error) {
	r.m.mu.RLock()
	defer r.m.mu.RUnlock()
	if r.m.current == nil {
		return nil, ErrNotReady
	}
	return r.m.current.cache, nil
}

func (r epochReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c, err := r.current()
	if err != nil {
		return err
	}
	return c.Get(ctx, key, obj, opts...)
}

func (r epochReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	c, err := r.current()
	if err != nil {
		return err
	}
	return c.List(ctx, list, opts...)
}
