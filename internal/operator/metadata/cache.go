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

package metadata

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// cachedSecretLabels are the only Secret labels the operator's
// metadata-only Secret informer keeps: managed-by, which
// pgtoolbox_cached_objects splits on, and the reload label, which marks
// a user Secret whose version rolls the Pods that mount it. Every other
// Secret label, and every Secret annotation, is dropped before the object
// enters the cache: the reconcilers read a Secret's metadata past the
// cache (the APIReader), so the cache only feeds the event handlers,
// which map a Secret by its name, namespace and owner references.
var cachedSecretLabels = []string{pgtoolboxv1alpha1.LabelManagedBy, pgtoolboxv1alpha1.LabelReload}

// TransformSecretMetadata is the cache transform of the Secret informers.
// Annotations are where Secret content leaks into metadata: `kubectl
// apply` records the whole object, data included, in
// kubectl.kubernetes.io/last-applied-configuration, and a metadata-only
// informer would otherwise keep that copy of every Secret of every watched
// namespace in the operator's heap. It also strips managedFields, since a
// per-kind transform replaces the cache's DefaultTransform.
func TransformSecretMetadata() toolscache.TransformFunc {
	return func(in any) (any, error) {
		object, ok := in.(metav1.Object)
		if !ok {
			// A tombstone (DeletedFinalStateUnknown) holds an object
			// already transformed when it was added.
			return in, nil
		}
		object.SetManagedFields(nil)
		object.SetAnnotations(nil)
		object.SetLabels(keepLabels(object.GetLabels(), cachedSecretLabels))
		return in, nil
	}
}

// keepLabels returns the entries of labels whose key is in keys, or nil.
func keepLabels(labels map[string]string, keys []string) map[string]string {
	var kept map[string]string
	for _, key := range keys {
		value, ok := labels[key]
		if !ok {
			continue
		}
		if kept == nil {
			kept = make(map[string]string, len(keys))
		}
		kept[key] = value
	}
	return kept
}
