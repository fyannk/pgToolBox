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

package history

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/config"
	"github.com/fyannk/pgToolBox/internal/console/kube"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/history"
)

func kind(source string) cache.Kind {
	for _, k := range cache.Kinds(labels.Everything(), false) {
		if k.Source == source {
			return k
		}
	}
	panic(source)
}

func cluster(instances int) *cnpgv1.Cluster {
	return &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "orders", UID: "u1", Generation: int64(instances)},
		Spec:       cnpgv1.ClusterSpec{Instances: instances},
	}
}

func TestDisabledHistoryIsNil(t *testing.T) {
	h, err := Open(config.History{Enabled: false}, nil)
	if err != nil || h != nil {
		t.Fatalf("Open = %v, %v", h, err)
	}
	h.Tap(kind(cache.SourceClusters), cluster(1), false) // nil receiver is safe
	if _, ok := h.Snapshot(); ok {
		t.Error("a disabled history has a snapshot")
	}
}

func TestInMemoryHistoryRecordsDefinitionsAndPods(t *testing.T) {
	h, err := Open(config.History{Enabled: true, MaxRevisions: 100, MaxBytes: 1 << 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if h.Persistent() {
		t.Error("in-memory history claims to persist")
	}
	h.Tap(kind(cache.SourceClusters), cluster(1), false)
	h.Tap(kind(cache.SourceClusters), cluster(2), false)
	// Member pods are recorded (the diagnostics count their
	// incarnations) and file under their Cluster; events are not.
	h.Tap(kind(cache.SourcePods), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "p", UID: "p1", Labels: map[string]string{"cnpg.io/cluster": "orders"}}}, false)
	h.Tap(kind(cache.SourceEvents), &corev1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "e", UID: "e1"}}, false)
	snap, ok := h.Snapshot()
	if !ok {
		t.Fatal("no snapshot")
	}
	if len(snap.Entries) != 3 {
		t.Fatalf("entries = %+v", snap.Entries)
	}
	for _, e := range snap.Entries {
		switch e.Kind {
		case "Cluster":
			if e.Group != "postgresql.cnpg.io" {
				t.Errorf("entry kind %s/%s: the GVK was not carried", e.Group, e.Kind)
			}
		case "Pod":
			if o, ok := h.OwnerOf(e.UID, e.Kind, e.Namespace, e.Name); !ok || o.Cluster != "orders" {
				t.Errorf("pod owner %+v %v", o, ok)
			}
		default:
			t.Errorf("unexpected entry %s", e.Kind)
		}
	}
}

// TestPersistentHistorySurvivesRestart: one bbolt file per console.
func TestPersistentHistorySurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	cfg := config.History{Enabled: true, Path: path, MaxRevisions: 100, MaxBytes: 1 << 20}
	h, err := Open(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Persistent() {
		t.Fatal("not persistent")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	h.Tap(kind(cache.SourceClusters), cluster(1), false)
	h.Tap(kind(cache.SourceClusters), cluster(3), false)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("journal did not stop")
	}

	again, err := Open(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := again.Snapshot()
	if len(snap.Entries) != 2 {
		t.Errorf("after restart: %d entries", len(snap.Entries))
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	_ = again.Run(ctx2)
}

// recordedObjects is one object of every recorded kind that files under a
// Cluster, all owned by "orders".
func recordedObjects() map[string]client.Object {
	ref := cnpgv1.LocalObjectReference{Name: "orders"}
	meta := func(uid string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Namespace: "a", Name: "child-" + uid, UID: types.UID(uid)}
	}
	pod := &corev1.Pod{ObjectMeta: meta("pod")}
	pod.Labels = map[string]string{"cnpg.io/cluster": "orders"}
	return map[string]client.Object{
		cache.SourceClusters:         cluster(1),
		cache.SourcePods:             pod,
		cache.SourceBackups:          &cnpgv1.Backup{ObjectMeta: meta("backup"), Spec: cnpgv1.BackupSpec{Cluster: ref}},
		cache.SourceScheduledBackups: &cnpgv1.ScheduledBackup{ObjectMeta: meta("scheduled"), Spec: cnpgv1.ScheduledBackupSpec{Cluster: ref, Schedule: "0 0 0 * * *"}},
		cache.SourcePoolers:          &cnpgv1.Pooler{ObjectMeta: meta("pooler"), Spec: cnpgv1.PoolerSpec{Cluster: ref}},
		cache.SourceDatabases:        &cnpgv1.Database{ObjectMeta: meta("database"), Spec: cnpgv1.DatabaseSpec{ClusterRef: corev1.LocalObjectReference{Name: "orders"}, Name: "app", Owner: "app"}},
		cache.SourceDatabaseRoles:    &cnpgv1.DatabaseRole{ObjectMeta: meta("role"), Spec: cnpgv1.DatabaseRoleSpec{ClusterRef: corev1.LocalObjectReference{Name: "orders"}}},
		cache.SourcePublications:     &cnpgv1.Publication{ObjectMeta: meta("publication"), Spec: cnpgv1.PublicationSpec{ClusterRef: corev1.LocalObjectReference{Name: "orders"}, Name: "p", DBName: "app"}},
		cache.SourceSubscriptions:    &cnpgv1.Subscription{ObjectMeta: meta("subscription"), Spec: cnpgv1.SubscriptionSpec{ClusterRef: corev1.LocalObjectReference{Name: "orders"}, Name: "s", DBName: "app", PublicationName: "p", ExternalClusterName: "x"}},
	}
}

// TestDeletedChildrenKeepTheirClusterAcrossRestart is the regression test
// of the persisted-history bug: a child observed then deleted before a
// console restart must still file under its Cluster afterwards, for every
// recorded kind.
func TestDeletedChildrenKeepTheirClusterAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	cfg := config.History{Enabled: true, Path: path, MaxRevisions: 100, MaxBytes: 1 << 20}
	h, err := Open(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	objects := recordedObjects()
	for source, object := range objects {
		h.Tap(kind(source), object, false)
		if source != cache.SourceClusters {
			h.Tap(kind(source), object, true)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	again, err := Open(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = again.Run(ctx)
	}()
	snap, _ := again.Snapshot()
	seen := map[string]bool{}
	for _, entry := range snap.Entries {
		owner, ok := again.OwnerOf(entry.UID, entry.Kind, entry.Namespace, entry.Name)
		if !ok || owner != (Owner{Namespace: "a", Cluster: "orders"}) {
			t.Errorf("%s %s after restart: owner %+v, %v", entry.Kind, entry.Name, owner, ok)
		}
		seen[entry.Kind] = true
	}
	if len(seen) != len(objects) {
		t.Fatalf("kinds after restart = %v, want %d", seen, len(objects))
	}
}

// TestClusterOfManifestMatchesClusterOf holds the manifest derivation used
// after a restart equal to the typed one used while watching.
func TestClusterOfManifestMatchesClusterOf(t *testing.T) {
	for source, object := range recordedObjects() {
		k := kind(source)
		observation, err := kube.Observation(source, k.GVK, object, false)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := clusterOfManifest(k.GVK.Group, k.GVK.Kind, observation.Manifest), clusterOf(object); got != want || want == "" {
			t.Errorf("%s: manifest says %q, typed says %q", source, got, want)
		}
	}
	if clusterOfManifest("", "Pod", []byte("{")) != "" || clusterOfManifest("other.io", "Cluster", []byte(`{"metadata":{"name":"x"}}`)) != "" {
		t.Error("an undecodable manifest or a foreign kind was attributed")
	}
}

// life runs one console process life of a persistent history: open,
// run the journal, feed it, stop it.
func life(t *testing.T, cfg config.History, body func(h *History)) {
	t.Helper()
	h, err := Open(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	body(h)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func memberPod() *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "orders-1", UID: "p1", Labels: map[string]string{"cnpg.io/cluster": "orders"}}}
}

// TestRestartAccountsForChangesWhileDown is the regression test of the
// downtime bug: the first complete listing after a restart is a seed, so a
// pod deleted while the console was down gets its unobserved deletion and
// a Cluster edited meanwhile gets an after-gap spec revision, instead of
// the deletion never appearing and the edit reading as live.
func TestRestartAccountsForChangesWhileDown(t *testing.T) {
	cfg := config.History{Enabled: true, Path: filepath.Join(t.TempDir(), "history.db"), MaxRevisions: 100, MaxBytes: 1 << 20}
	life(t, cfg, func(h *History) {
		h.Tap(kind(cache.SourceClusters), cluster(1), false)
		h.Tap(kind(cache.SourcePods), memberPod(), false)
	})
	life(t, cfg, func(h *History) {
		// The informers' first lists: the Cluster, edited while down; no pod.
		h.Seed(kind(cache.SourceClusters), []string{"a"}, []client.Object{cluster(3)})
		h.Seed(kind(cache.SourcePods), []string{"a"}, nil)
		snap, _ := h.Snapshot()
		var deleted, gapEdit bool
		for _, e := range snap.Entries {
			if e.UID == "p1" && e.Change == history.ChangeDeletedUnobserved {
				deleted = true
				if o, ok := h.OwnerOf(e.UID, e.Kind, e.Namespace, e.Name); !ok || o.Cluster != "orders" {
					t.Errorf("the unobserved deletion files under %+v, %v", o, ok)
				}
			}
			if e.UID == "u1" && e.Change == history.ChangeSpec && e.AfterGap {
				gapEdit = true
			}
		}
		if !deleted {
			t.Errorf("no unobserved deletion for the pod deleted while down: %+v", snap.Entries)
		}
		if !gapEdit {
			t.Errorf("the Cluster edited while down has no after-gap spec revision: %+v", snap.Entries)
		}
	})
	// The deleted pod's classification state left the journal too.
	life(t, cfg, func(h *History) {
		h.Seed(kind(cache.SourceClusters), []string{"a"}, []client.Object{cluster(3)})
		h.Seed(kind(cache.SourcePods), []string{"a"}, nil)
		snap, _ := h.Snapshot()
		count := 0
		for _, e := range snap.Entries {
			if e.UID == "p1" && e.Change == history.ChangeDeletedUnobserved {
				count++
			}
		}
		if count != 1 {
			t.Errorf("the pod's deletion was synthesized %d times across restarts, want 1", count)
		}
	})
}

// TestSeedLeavesUnlistedNamespacesAlone: a seed accounts only for the
// namespaces its listing covered; an object in a namespace the console can
// no longer list is unknown, not deleted.
func TestSeedLeavesUnlistedNamespacesAlone(t *testing.T) {
	h, err := Open(config.History{Enabled: true, MaxRevisions: 100, MaxBytes: 1 << 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.Tap(kind(cache.SourcePods), memberPod(), false)
	h.Seed(kind(cache.SourcePods), []string{"b"}, nil)
	snap, _ := h.Snapshot()
	for _, e := range snap.Entries {
		if e.Change == history.ChangeDeletedUnobserved {
			t.Fatalf("a pod in an unlisted namespace was declared deleted: %+v", e)
		}
	}
	h.Seed(kind(cache.SourceEvents), []string{"a"}, nil) // an unrecorded source is ignored
	var none *History
	none.Seed(kind(cache.SourcePods), []string{"a"}, nil) // nil receiver is safe
}
