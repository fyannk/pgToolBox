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
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

func TestKindsNeverIncludeSecretsOrConfigMaps(t *testing.T) {
	for _, catalogs := range []bool{false, true} {
		for _, kind := range Kinds(labels.Everything(), catalogs) {
			if kind.GVK.Group == "" && (kind.GVK.Kind == "Secret" || kind.GVK.Kind == "ConfigMap") {
				t.Errorf("%s is watched", kind.GVK.Kind)
			}
		}
	}
	var hasCatalog bool
	for _, kind := range Kinds(nil, true) {
		if kind.Source == SourceClusterImageCatalogs {
			hasCatalog = kind.ClusterScoped
		}
	}
	if !hasCatalog {
		t.Error("ClusterImageCatalog not cluster-scoped or missing when opted in")
	}
	for _, kind := range Kinds(nil, false) {
		if kind.Source == SourceClusterImageCatalogs {
			t.Error("ClusterImageCatalog watched without the opt-in")
		}
	}
}

func TestKindsLabelFiltering(t *testing.T) {
	selector := labels.SelectorFromSet(labels.Set{"app": "orders"})
	for _, kind := range Kinds(selector, false) {
		switch kind.Source {
		case SourceClusters:
			if kind.Label.String() != "app=orders" {
				t.Errorf("cluster selector = %v", kind.Label)
			}
		case SourcePods, SourcePVCs, SourceServices, SourcePDBs:
			if kind.Label == nil || kind.Label.String() != ClusterLabel {
				t.Errorf("%s label = %v, want %s", kind.Source, kind.Label, ClusterLabel)
			}
		}
	}
	for _, kind := range Kinds(labels.Everything(), false) {
		if kind.Source == SourceClusters && kind.Label != nil {
			t.Error("an empty selector must not become a filter")
		}
	}
}

func TestTrackerTransitions(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	tr := newTracker(clock)
	tr.set("a", SourcePods, viewmodel.SourceLive, "")
	tr.set("b", SourcePods, viewmodel.SourceLive, "")
	first, _ := tr.get("a", SourcePods)

	now = now.Add(time.Minute)
	tr.set("a", SourcePods, viewmodel.SourceLive, "")
	if again, _ := tr.get("a", SourcePods); !again.Since.Equal(*first.Since) {
		t.Error("Since moved without a transition")
	}

	now = now.Add(time.Minute)
	tr.watchedOutcome(SourcePods, 1, errors.New("connection refused"))
	for _, ns := range []string{"a", "b"} {
		got, _ := tr.get(ns, SourcePods)
		if got.State != viewmodel.SourceStale || got.Reason == "" || !got.Since.Equal(now) {
			t.Errorf("%s: %+v, want stale since now", ns, got)
		}
	}
	tr.watchedOutcome(SourcePods, 2, errors.New("again"))
	tr.watchedOutcome(SourcePods, 1, nil)
	if got, _ := tr.get("a", SourcePods); got.State != viewmodel.SourceStale {
		t.Error("live again while informer 2 still fails")
	}
	tr.watchedOutcome(SourcePods, 2, nil)
	if got, _ := tr.get("a", SourcePods); got.State != viewmodel.SourceLive {
		t.Errorf("not live after every informer recovered: %+v", got)
	}

	tr.watchedOutcome(SourcePods, 3, apierrors.NewResourceExpired("too old"))
	if got, _ := tr.get("a", SourcePods); got.State != viewmodel.SourceLive {
		t.Error("an expired resource version made the source stale")
	}

	tr.set("a", SourcePoolers, viewmodel.SourceForbidden, "no")
	tr.watchedOutcome(SourcePoolers, 4, errors.New("x"))
	if got, _ := tr.get("a", SourcePoolers); got.State != viewmodel.SourceForbidden {
		t.Error("a watch fault overwrote a forbidden source")
	}

	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("secret detail"))
	tr.watchedOutcome(SourcePods, 5, forbidden)
	if got, _ := tr.get("a", SourcePods); got.State != viewmodel.SourceStale || got.Reason != "the console ServiceAccount is no longer allowed to watch this source" {
		t.Errorf("revoked RBAC: %+v", got)
	}
}

func TestSubscriptionCoalesces(t *testing.T) {
	var h hub
	s := h.subscribe()
	for range 1000 {
		h.publish(ClusterKey{Namespace: "a", Name: "orders"})
	}
	h.publish(ClusterKey{Namespace: "a", Name: "billing"})
	select {
	case <-s.Ready():
	default:
		t.Fatal("not signalled")
	}
	got := s.Drain()
	want := []ClusterKey{{"a", "billing"}, {"a", "orders"}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("drain = %v", got)
	}
	if len(s.Drain()) != 0 {
		t.Error("drain did not clear")
	}
	h.unsubscribe(s)
	h.publish(ClusterKey{Namespace: "a"})
	if len(s.Drain()) != 0 {
		t.Error("an unsubscribed subscription still receives")
	}
}

func TestClusterOf(t *testing.T) {
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "orders-1", Labels: map[string]string{ClusterLabel: "orders"}}}
	backup := &cnpgv1.Backup{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "b1"}, Spec: cnpgv1.BackupSpec{Cluster: cnpgv1.LocalObjectReference{Name: "orders"}}}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod, backup).Build()
	event := func(kind, name string) *corev1.Event {
		return &corev1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "e"}, InvolvedObject: corev1.ObjectReference{Kind: kind, Name: name}}
	}
	tests := []struct {
		name string
		obj  client.Object
		want string
	}{
		{"cluster", &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "orders"}}, "orders"},
		{"labelled pod", pod, "orders"},
		{"backup", backup, "orders"},
		{"database", &cnpgv1.Database{Spec: cnpgv1.DatabaseSpec{ClusterRef: corev1.LocalObjectReference{Name: "orders"}}}, "orders"},
		{"event on the cluster", event("Cluster", "orders"), "orders"},
		{"event on a pod", event("Pod", "orders-1"), "orders"},
		{"event on a backup", event("Backup", "b1"), "orders"},
		{"event on an unknown pod", event("Pod", "ghost"), ""},
		{"event on a node", event("Node", "n1"), ""},
		{"unlabelled pvc", &corev1.PersistentVolumeClaim{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clusterOf(context.Background(), reader, allSynced, tt.obj); got != tt.want {
				t.Errorf("clusterOf = %q, want %q", got, tt.want)
			}
		})
	}
}

// allSynced is an epoch whose every kind is planned and synced.
func allSynced(client.Object) bool { return true }

// gatedReader is a cache reader whose Get may block the way a cache's
// Get does before the target informer has synced: until its context
// ends.
type gatedReader struct {
	client.Reader
	block bool
	gets  int
}

func (r *gatedReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	r.gets++
	if r.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

// TestEventAttributionNeverWaitsForAnInformer is the handler's bound:
// an event on a pod whose informer has not synced, or whose lookup
// would block, is attributed to nobody (the namespace is published
// whole) at once, instead of holding the Event informer's handler until
// the Pod informer syncs.
func TestEventAttributionNeverWaitsForAnInformer(t *testing.T) {
	t.Parallel()
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "orders-1", Labels: map[string]string{ClusterLabel: "orders"}}}
	event := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "e"}, InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "orders-1"}}
	cases := []struct {
		name     string
		synced   bool
		block    bool
		want     string
		wantGets int
	}{
		{"synced informer", true, false, "orders", 1},
		{"informer not synced", false, true, "", 0},
		{"lookup blocks", true, true, "", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reader := &gatedReader{
				Reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod.DeepCopy()).Build(),
				block:  tc.block,
			}
			start := time.Now()
			// No deadline on the handler's context: the bound is the
			// attribution's own.
			got := clusterOf(context.Background(), reader, func(client.Object) bool { return tc.synced }, event)
			if got != tc.want {
				t.Errorf("clusterOf = %q, want %q", got, tc.want)
			}
			if reader.gets != tc.wantGets {
				t.Errorf("Get calls = %d, want %d", reader.gets, tc.wantGets)
			}
			if elapsed := time.Since(start); elapsed > 10*attributionTimeout {
				t.Errorf("attribution took %s", elapsed)
			}
		})
	}
}

func TestPlanEqual(t *testing.T) {
	a := plan{SourcePods: {"a", "b"}}
	if !a.equal(plan{SourcePods: {"a", "b"}}) {
		t.Error("equal plans differ")
	}
	if a.equal(plan{SourcePods: {"a"}}) || a.equal(plan{SourcePods: {"a", "b"}, SourcePoolers: {"a"}}) {
		t.Error("different plans compare equal")
	}
}

func TestReaderBeforeSync(t *testing.T) {
	m := &Manager{tracker: newTracker(time.Now)}
	if err := m.Reader().Get(context.Background(), client.ObjectKey{Name: "x"}, &cnpgv1.Cluster{}); !errors.Is(err, ErrNotReady) {
		t.Errorf("Get before sync = %v", err)
	}
	if m.Ready() {
		t.Error("ready before sync")
	}
	if got := m.Source("a", SourcePods); got.State != viewmodel.SourcePending {
		t.Errorf("unprobed source = %+v, want pending", got)
	}
}

func TestGenerationMovesOnEveryChangeToTheClusterOrItsNamespace(t *testing.T) {
	t.Parallel()
	var h hub
	orders := ClusterKey{Namespace: "team-a", Name: "orders"}
	if g := h.generation(orders); g != 0 {
		t.Fatalf("generation before any change = %d", g)
	}
	h.publish(orders)
	first := h.generation(orders)
	h.publish(ClusterKey{Namespace: "team-a", Name: "billing"})
	if g := h.generation(orders); g != first {
		t.Fatalf("a change to billing moved orders to %d", g)
	}
	h.publish(ClusterKey{Namespace: "team-a"})
	if g := h.generation(orders); g <= first {
		t.Fatalf("a namespace-wide change left orders at %d", g)
	}
}

// TestClusterStorageKeepsOnlyAutoGrow: the second, raw view of Clusters
// keeps the identity and the fork's autoGrow stanzas, nothing else (no
// status, no other spec field, no annotation).
func TestClusterStorageKeepsOnlyAutoGrow(t *testing.T) {
	u := NewClusterStorage()
	u.SetName("orders")
	u.SetNamespace("team-a")
	u.SetUID("uid-1")
	u.SetAnnotations(map[string]string{"secret-looking": "value"})
	u.Object["spec"] = map[string]any{
		"instances":  int64(3),
		"storage":    map[string]any{"size": "10Gi", "autoGrow": map[string]any{"maxSize": "100Gi"}},
		"walStorage": map[string]any{"size": "5Gi"},
		"tablespaces": []any{
			map[string]any{"name": "hist", "storage": map[string]any{"size": "1Gi", "autoGrow": map[string]any{"maxSize": "2Gi"}}},
			map[string]any{"name": "tmp", "storage": map[string]any{"size": "1Gi"}},
		},
	}
	u.Object["status"] = map[string]any{"phase": "Cluster in healthy state"}
	out, err := ClusterStorageTransform(u)
	if err != nil {
		t.Fatal(err)
	}
	kept := out.(*unstructured.Unstructured)
	want := map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"name": "orders", "namespace": "team-a", "uid": "uid-1"},
		"spec": map[string]any{
			"storage":     map[string]any{"autoGrow": map[string]any{"maxSize": "100Gi"}},
			"tablespaces": []any{map[string]any{"name": "hist", "storage": map[string]any{"autoGrow": map[string]any{"maxSize": "2Gi"}}}},
		},
	}
	if !reflect.DeepEqual(kept.Object, want) {
		t.Errorf("kept %#v\nwant %#v", kept.Object, want)
	}
	if other, _ := ClusterStorageTransform("not an object"); other != "not an object" {
		t.Error("a non-object was changed")
	}
}

// TestEventAttributionStartsNoUnplannedInformer: an Event about a kind
// the plan left out (Poolers without their RBAC) is attributed to
// nobody, and the real informer cache is never asked for that kind's
// informer, which it would create and start, unfiltered, despite
// ReaderFailOnMissingInformer.
func TestEventAttributionStartsNoUnplannedInformer(t *testing.T) {
	t.Parallel()
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	podGVK := corev1.SchemeGroupVersion.WithKind("Pod")
	poolerGVK := cnpgv1.SchemeGroupVersion.WithKind("Pooler")
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(podGVK, meta.RESTScopeNamespace)
	mapper.Add(poolerGVK, meta.RESTScopeNamespace)
	var created sync.Map
	c, err := crcache.New(&rest.Config{Host: "https://127.0.0.1:1"}, crcache.Options{
		Scheme: scheme, Mapper: mapper, ReaderFailOnMissingInformer: true,
		NewInformer: func(lw toolscache.ListerWatcher, obj runtime.Object, resync time.Duration, idx toolscache.Indexers) toolscache.SharedIndexInformer {
			created.Store(reflect.TypeOf(obj).String(), true)
			return toolscache.NewSharedIndexInformer(lw, obj, resync, idx)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The plan: Pods only, as build gets them.
	planned := plannedInformers{}
	podInformer, err := c.GetInformer(t.Context(), &corev1.Pod{})
	if err != nil {
		t.Fatal(err)
	}
	planned.add(&corev1.Pod{}, podInformer)

	for _, kind := range []string{"Pooler", "Pod", "Backup", "ScheduledBackup", "PersistentVolumeClaim"} {
		event := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "e"}, InvolvedObject: corev1.ObjectReference{Kind: kind, Name: "x"}}
		if got := clusterOf(t.Context(), c, planned.synced, event); got != "" {
			t.Errorf("event on a %s attributed to %q", kind, got)
		}
	}
	var kinds []string
	created.Range(func(k, _ any) bool { kinds = append(kinds, k.(string)); return true })
	if len(kinds) != 1 || kinds[0] != "*v1.Pod" {
		t.Fatalf("informers created %v, want only the planned *v1.Pod", kinds)
	}
	// The cache would have started one had it been asked: the path the
	// handler no longer takes.
	if _, err := c.GetInformer(t.Context(), &cnpgv1.Pooler{}, crcache.BlockUntilSynced(false)); err != nil {
		t.Fatal(err)
	}
	if _, ok := created.Load("*v1.Pooler"); !ok {
		t.Fatal("the cache did not create a Pooler informer on GetInformer: the test proves nothing")
	}
}

// TestBackupTransformKeepsOnlyStatusWriteTimes (round 22, PG P3-B): a
// Backup keeps the API server's time of each status-subresource write,
// never a field set or a main-resource entry, so the purge band can bound
// the end of one whose instance's and operator's clocks disagree.
func TestBackupTransformKeepsOnlyStatusWriteTimes(t *testing.T) {
	at := metav1.NewTime(time.Date(2026, 11, 1, 10, 3, 4, 0, time.UTC))
	b := &cnpgv1.Backup{ObjectMeta: metav1.ObjectMeta{Name: "b", ManagedFields: []metav1.ManagedFieldsEntry{
		{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply, Time: &at, FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(`{"f:spec":{}}`)},
		{Manager: "manager", Operation: metav1.ManagedFieldsOperationUpdate, Time: &at, Subresource: "status", FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(`{"f:status":{}}`)},
		{Manager: "untimed", Operation: metav1.ManagedFieldsOperationUpdate, Subresource: "status"},
	}}}
	out, err := BackupTransform(b)
	if err != nil {
		t.Fatal(err)
	}
	got := out.(*cnpgv1.Backup).ManagedFields
	want := []metav1.ManagedFieldsEntry{{Manager: "manager", Operation: metav1.ManagedFieldsOperationUpdate, Time: &at, Subresource: "status"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("kept %+v, want %+v", got, want)
	}
	tomb := toolscache.DeletedFinalStateUnknown{Key: "team-a/b", Obj: b}
	if other, _ := BackupTransform(tomb); other != any(tomb) {
		t.Error("a tombstone was changed")
	}
	var backups *Kind
	for _, k := range Kinds(nil, false) {
		if k.Source == SourceBackups {
			backups = &k
		}
	}
	if backups == nil || backups.Transform == nil {
		t.Fatal("the Backup informer keeps the default transform")
	}
}
