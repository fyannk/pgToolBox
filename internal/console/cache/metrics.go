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

// The cache's own metrics (console half): list and watch
// calls per source and outcome (relists and watch restarts show up as
// rising counts), and the objects each source holds, counted on scrape
// from the informers without deep copies (pgtoolbox_console_cached_objects{source}:
// the console's counterpart of the operator's
// pgtoolbox_cached_objects{kind,managed}; the console manages nothing,
// so the label is the source). Labels are the closed source set.

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var informerCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "pgtoolbox_console_cache_informer_calls_total",
	Help: "List and watch calls of the console's informers by source, call and outcome (ok, forbidden, not_found, error).",
}, []string{"source", "call", "outcome"})

func outcomeLabel(err error) string {
	switch {
	case err == nil:
		return "ok"
	case apierrors.IsForbidden(err):
		return "forbidden"
	case apierrors.IsNotFound(err):
		return "not_found"
	default:
		return "error"
	}
}

var cachedObjectsDesc = prometheus.NewDesc("pgtoolbox_console_cached_objects",
	"Objects the console's informer cache holds per source, across the watched namespaces.", []string{"source"}, nil)

// objectCounter counts the current Manager's objects on scrape.
type objectCounter struct {
	mu sync.Mutex
	m  *Manager
}

var cachedObjects = &objectCounter{}

func (c *objectCounter) set(m *Manager) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m = m
}

func (c *objectCounter) Describe(ch chan<- *prometheus.Desc) { ch <- cachedObjectsDesc }

func (c *objectCounter) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	m := c.m
	c.mu.Unlock()
	if m == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for source, n := range m.Counts(ctx) {
		ch <- prometheus.MustNewConstMetric(cachedObjectsDesc, prometheus.GaugeValue, float64(n), source)
	}
}

// Counts returns, per readable source, how many objects the cache
// holds. It lists from the informers' stores without deep copies.
func (m *Manager) Counts(ctx context.Context) map[string]int {
	out := map[string]int{}
	if !m.Ready() {
		return out
	}
	reader := m.Reader()
	for _, kind := range m.kinds {
		list, err := m.listFor(kind)
		if err != nil {
			continue
		}
		namespaces := m.cfg.WatchNamespaces
		if kind.ClusterScoped {
			namespaces = []string{""}
		}
		total, counted := 0, false
		for _, ns := range namespaces {
			if !m.Readable(ns, kind.Source) {
				continue
			}
			opts := []client.ListOption{client.UnsafeDisableDeepCopy}
			if ns != "" {
				opts = append(opts, client.InNamespace(ns))
			}
			if reader.List(ctx, list, opts...) != nil {
				continue
			}
			total += meta.LenList(list)
			counted = true
		}
		if counted {
			out[kind.Source] = total
		}
	}
	return out
}

func (m *Manager) listFor(kind Kind) (client.ObjectList, error) {
	if _, ok := kind.Prototype().(*unstructured.Unstructured); ok {
		l := &unstructured.UnstructuredList{}
		l.SetGroupVersionKind(kind.GVK.GroupVersion().WithKind(kind.GVK.Kind + "List"))
		return l, nil
	}
	obj, err := m.scheme.New(kind.GVK.GroupVersion().WithKind(kind.GVK.Kind + "List"))
	if err != nil {
		return nil, err
	}
	l, ok := obj.(client.ObjectList)
	if !ok {
		return nil, errors.New("cache: " + kind.GVK.Kind + "List is not a list")
	}
	return l, nil
}

func init() { crmetrics.Registry.MustRegister(informerCalls, cachedObjects) }
