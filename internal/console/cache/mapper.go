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
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// memoMapper remembers the mappings the console's kinds resolve to.
// controller-runtime's multi-namespace cache asks the mapper whether a
// kind is namespaced on every Get and List; the dynamic mapper answers by
// walking every API group the server serves, which cost a seventh of the
// console's CPU on a list of 1,600 Clusters. Only answers are kept, never
// errors, so a kind installed later (a CRD) is still found; a mapping
// that changes while the console runs (a CRD's version removed) does not
// change the informer already started for it either.
type memoMapper struct {
	meta.RESTMapper
	known sync.Map // mappingKey → *meta.RESTMapping
}

type mappingKey struct {
	gk       schema.GroupKind
	versions string
}

func newMemoMapper(next meta.RESTMapper) *memoMapper { return &memoMapper{RESTMapper: next} }

// RESTMapping implements meta.RESTMapper.
func (m *memoMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	key := mappingKey{gk: gk, versions: strings.Join(versions, ",")}
	if v, ok := m.known.Load(key); ok {
		return v.(*meta.RESTMapping), nil
	}
	mapping, err := m.RESTMapper.RESTMapping(gk, versions...)
	if err != nil {
		return nil, err
	}
	m.known.Store(key, mapping)
	return mapping, nil
}
