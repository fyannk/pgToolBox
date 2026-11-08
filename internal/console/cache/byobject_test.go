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
	"testing"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// TestOneCacheEntryPerGVK: controller-runtime keeps one configuration per
// GVK, so the typed and raw views of Clusters must reach it as one
// deterministic entry, not two of which one wins at random.
func TestOneCacheEntryPerGVK(t *testing.T) {
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	selector := labels.SelectorFromSet(labels.Set{"team": "a"})
	kinds := Kinds(selector, true)
	p := plan{}
	for _, k := range kinds {
		p[k.Source] = []string{"team-a"}
		if k.Source == SourceClusterStorage {
			p[k.Source] = []string{"team-a", "team-b"}
		}
	}
	byObject, watched, err := byObjectFor(kinds, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(watched) != len(kinds) {
		t.Errorf("watched %d of %d kinds", len(watched), len(kinds))
	}
	seen := map[schema.GroupVersionKind]bool{}
	var cluster *struct {
		label      labels.Selector
		namespaces int
		transform  func(any) (any, error)
	}
	for obj, entry := range byObject {
		gvk, err := apiutil.GVKForObject(obj, scheme)
		if err != nil {
			t.Fatal(err)
		}
		if seen[gvk] {
			t.Errorf("two entries for %s", gvk)
		}
		seen[gvk] = true
		if gvk.Kind == "Cluster" {
			cluster = &struct {
				label      labels.Selector
				namespaces int
				transform  func(any) (any, error)
			}{entry.Label, len(entry.Namespaces), entry.Transform}
		}
	}
	if cluster == nil {
		t.Fatal("no Cluster entry")
	}
	if cluster.label == nil || cluster.label.String() != selector.String() || cluster.namespaces != 2 || cluster.transform == nil {
		t.Fatalf("Cluster entry: label %v, %d namespaces", cluster.label, cluster.namespaces)
	}
	// The one transform serves both shapes.
	typed := &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "orders", ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "x"}}}}
	out, err := cluster.transform(typed)
	if err != nil || len(out.(*cnpgv1.Cluster).ManagedFields) != 0 {
		t.Errorf("typed: %v %+v", err, out)
	}
	raw := NewClusterStorage()
	raw.SetName("orders")
	_ = unstructured.SetNestedField(raw.Object, "secret-ish", "spec", "bootstrap", "initdb", "owner")
	_ = unstructured.SetNestedField(raw.Object, "50Gi", "spec", "storage", "autoGrow", "maxSize")
	out, err = cluster.transform(raw)
	if err != nil {
		t.Fatal(err)
	}
	u := out.(*unstructured.Unstructured)
	if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "bootstrap"); found {
		t.Error("the raw view kept more than the storage fields")
	}
	if v, _, _ := unstructured.NestedString(u.Object, "spec", "storage", "autoGrow", "maxSize"); v != "50Gi" {
		t.Errorf("the raw view lost autoGrow: %v", u.Object)
	}
}

func TestKindsSharingAGVKMustAgree(t *testing.T) {
	a := typed("a", cnpgGVK("Cluster"), false, labels.SelectorFromSet(labels.Set{"x": "1"}), func() *cnpgv1.Cluster { return &cnpgv1.Cluster{} })
	b := typed("b", cnpgGVK("Cluster"), false, labels.SelectorFromSet(labels.Set{"x": "2"}), func() *cnpgv1.Cluster { return &cnpgv1.Cluster{} })
	if _, _, err := byObjectFor([]Kind{a, b}, plan{"a": {"ns"}, "b": {"ns"}}); err == nil {
		t.Error("two labels for one GVK were accepted")
	}
	b.Label = a.Label
	b.Transform = ClusterStorageTransform
	if _, _, err := byObjectFor([]Kind{a, b}, plan{"a": {"ns"}, "b": {"ns"}}); err == nil {
		t.Error("two transforms for one GVK were accepted")
	}
}
