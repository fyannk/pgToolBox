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

// Package cachemetrics exports pgtoolbox_cached_objects{kind,managed}: on
// every scrape, the number of objects the operator's informers hold per
// kind, split on whether pgtoolbox manages them (app.kubernetes.io/managed-by=pgtoolbox). An owned kind
// with managed="false" series is an informer that escaped its label
// filter; the sum is what the memory request is sized from.
package cachemetrics

import (
	"context"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

var cachedObjectsDescription = prometheus.NewDesc("pgtoolbox_cached_objects",
	"Objects held in the operator's cache, per kind, split on whether pgtoolbox manages them (app.kubernetes.io/managed-by=pgtoolbox).",
	[]string{"kind", "managed"}, nil)

const scrapeTimeout = 5 * time.Second

// Collector counts, when scraped, the objects Reader (the manager's
// cache) holds for Kinds, without copying them. Kinds must be kinds a
// controller already watches: listing a kind the cache has no informer
// for would start one. A typed object counts the typed informer; a
// *metav1.PartialObjectMetadata with its GroupVersionKind set counts the
// metadata-only informer of that kind.
type Collector struct {
	Reader client.Reader
	Scheme *runtime.Scheme
	Kinds  []client.Object
}

// Describe implements prometheus.Collector.
func (c Collector) Describe(descriptions chan<- *prometheus.Desc) {
	descriptions <- cachedObjectsDescription
}

// Collect implements prometheus.Collector. A kind that cannot be listed
// (the cache not started yet, a cancelled scrape) is left out of the
// scrape — absent, not zero — and logged once per scrape at debug level.
func (c Collector) Collect(metrics chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()
	log := logf.Log.WithName("cachemetrics")
	for _, object := range c.Kinds {
		gvk, list, err := c.listFor(object)
		if err != nil {
			log.V(1).Info("kind left out of pgtoolbox_cached_objects", "object", object, "error", err.Error())
			continue
		}
		// Counted only: the cached objects are read in place, never copied.
		if err := c.Reader.List(ctx, list, client.UnsafeDisableDeepCopy); err != nil {
			log.V(1).Info("kind left out of pgtoolbox_cached_objects", "kind", gvk.Kind, "error", err.Error())
			continue
		}
		counts := map[bool]int{true: 0, false: 0}
		_ = meta.EachListItem(list, func(item runtime.Object) error {
			if accessor, err := meta.Accessor(item); err == nil {
				counts[accessor.GetLabels()[pgtoolboxv1alpha1.LabelManagedBy] == pgtoolboxv1alpha1.ManagedByValue]++
			}
			return nil
		})
		for _, managed := range []bool{true, false} {
			metrics <- prometheus.MustNewConstMetric(cachedObjectsDescription, prometheus.GaugeValue,
				float64(counts[managed]), gvk.Kind, strconv.FormatBool(managed))
		}
	}
}

// listFor returns the list type that reads object's informer.
func (c Collector) listFor(object client.Object) (schema.GroupVersionKind, client.ObjectList, error) {
	if partial, ok := object.(*metav1.PartialObjectMetadata); ok {
		gvk := partial.GroupVersionKind()
		list := &metav1.PartialObjectMetadataList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		return gvk, list, nil
	}
	gvk, err := apiutil.GVKForObject(object, c.Scheme)
	if err != nil {
		return gvk, nil, err
	}
	fresh, err := c.Scheme.New(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	if err != nil {
		return gvk, nil, err
	}
	list, ok := fresh.(client.ObjectList)
	if !ok {
		return gvk, nil, runtime.NewNotRegisteredErrForKind(c.Scheme.Name(), gvk)
	}
	return gvk, list, nil
}

// Union merges the kinds several controllers watch, once per kind: a
// kind two controllers watch is one informer, so one series pair. When
// one lists it typed and another metadata-only, both informers exist and
// the first wins the label; Union keeps the order of first appearance.
func Union(scheme *runtime.Scheme, lists ...[]client.Object) []client.Object {
	seen := map[schema.GroupKind]bool{}
	var out []client.Object
	for _, list := range lists {
		for _, object := range list {
			var gvk schema.GroupVersionKind
			if partial, ok := object.(*metav1.PartialObjectMetadata); ok {
				gvk = partial.GroupVersionKind()
			} else if typed, err := apiutil.GVKForObject(object, scheme); err == nil {
				gvk = typed
			} else {
				continue
			}
			if !seen[gvk.GroupKind()] {
				seen[gvk.GroupKind()] = true
				out = append(out, object)
			}
		}
	}
	return out
}

// Metadata returns a metadata-only prototype of gvk for Kinds.
func Metadata(gvk schema.GroupVersionKind) client.Object {
	object := &metav1.PartialObjectMetadata{}
	object.SetGroupVersionKind(gvk)
	return object
}
