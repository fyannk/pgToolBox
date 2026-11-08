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

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type countingMapper struct {
	meta.RESTMapper
	calls int
}

func (c *countingMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	c.calls++
	return c.RESTMapper.RESTMapping(gk, versions...)
}

func TestMemoMapperAsksOncePerKindAndNeverRemembersAMiss(t *testing.T) {
	pods := schema.GroupVersionKind{Version: "v1", Kind: "Pod"}
	base := meta.NewDefaultRESTMapper(nil)
	base.Add(pods, meta.RESTScopeNamespace)
	next := &countingMapper{RESTMapper: base}
	m := newMemoMapper(next)

	for range 3 {
		got, err := m.RESTMapping(pods.GroupKind(), "v1")
		if err != nil || got.Scope.Name() != meta.RESTScopeNameNamespace {
			t.Fatalf("RESTMapping(Pod) = %v, %v", got, err)
		}
	}
	if next.calls != 1 {
		t.Fatalf("the wrapped mapper was asked %d times for Pod, want 1", next.calls)
	}

	// A kind not served yet is asked again: it may be installed later.
	clusters := schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}
	if _, err := m.RESTMapping(clusters.GroupKind(), "v1"); err == nil {
		t.Fatal("RESTMapping(Cluster) found a kind that is not served")
	}
	base.Add(clusters, meta.RESTScopeNamespace)
	if _, err := m.RESTMapping(clusters.GroupKind(), "v1"); err != nil {
		t.Fatalf("RESTMapping(Cluster) after install: %v", err)
	}
}
