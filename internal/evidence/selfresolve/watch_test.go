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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// TestWaitReturnsWhenTheObjectStoreChanges drives the watch end to end on a
// fake dynamic client: an unrelated update keeps waiting, a change of what
// the agent reads returns a reason, and a cancelled context returns "".
func TestWaitReturnsWhenTheObjectStoreChanges(t *testing.T) {
	reads := 0
	store := testObjectStore("minio")
	resolver := newResolver(t, reader(t, store, false, &reads, testSecret()), "minio")
	outcome := resolver.Resolve(context.Background())
	if outcome.Failure != nil {
		t.Fatal(outcome.Failure)
	}

	cluster, err := runtime.DefaultUnstructuredConverter.ToUnstructured(testCluster())
	if err != nil {
		t.Fatal(err)
	}
	clusterObject := &unstructured.Unstructured{Object: cluster}
	clusterObject.SetAPIVersion("postgresql.cnpg.io/v1")
	clusterObject.SetKind("Cluster")
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		ClusterResource:     "ClusterList",
		ObjectStoreResource: "ObjectStoreList",
	}, clusterObject, store.DeepCopy())
	watcher, err := NewWatcher(dynamicClient, resolver)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	reasons := make(chan string, 1)
	go func() { reasons <- watcher.Wait(ctx, outcome) }()

	stores := dynamicClient.Resource(ObjectStoreResource).Namespace(namespace)
	// A label change does not change what the agent reads.
	labelled := store.DeepCopy()
	labelled.SetLabels(map[string]string{"team": "a"})
	if _, err := stores.Update(ctx, labelled, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case reason := <-reasons:
		t.Fatalf("Wait returned %q on a change the agent does not read", reason)
	case <-time.After(300 * time.Millisecond):
	}

	changed := labelled.DeepCopy()
	changed.Object["spec"].(map[string]any)["configuration"].(map[string]any)["destinationPath"] = "s3://backups/elsewhere"
	if _, err := stores.Update(ctx, changed, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case reason := <-reasons:
		if reason == "" {
			t.Fatal("Wait returned no reason")
		}
	case <-ctx.Done():
		t.Fatal("Wait did not see the ObjectStore change")
	}

	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if reason := watcher.Wait(cancelled, outcome); reason != "" {
		t.Fatalf("Wait on a cancelled context = %q", reason)
	}
}

func TestWaitRetriesAFailureTheWatchCannotSee(t *testing.T) {
	reads := 0
	resolver := newResolver(t, reader(t, testObjectStore("minio"), false, &reads), "minio")
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		ClusterResource: "ClusterList", ObjectStoreResource: "ObjectStoreList",
	})
	watcher, err := NewWatcher(dynamicClient, resolver)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if reason := watcher.Wait(ctx, Outcome{RetryAfter: 50 * time.Millisecond}); reason == "" {
		t.Fatal("Wait did not retry")
	}
}
