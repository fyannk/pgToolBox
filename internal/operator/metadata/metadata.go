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

// Package metadata owns the labels, annotations and names of the objects the
// operator generates: the identity labels its label-filtered caches select
// on, bounded generated names, and the propagation of user metadata under the
// operator's own keys, which always win.
package metadata

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"strings"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// Well-known label keys.
const (
	LabelName      = "app.kubernetes.io/name"
	LabelInstance  = "app.kubernetes.io/instance"
	LabelComponent = "app.kubernetes.io/component"
	LabelPartOf    = "app.kubernetes.io/part-of"
	PartOfValue    = "pgtoolbox"
)

// Identity describes the owner of a generated object: the application name
// (app.kubernetes.io/name), the owning CR's name, and the CR kind's own
// instance label key.
type Identity struct {
	Application      string
	Instance         string
	InstanceLabelKey string
}

// Labels returns the full label set of a generated object of component.
// It always carries managed-by=pgtoolbox, which the manager's informers
// for owned kinds select on (an object without it is invisible to the
// operator), and reconcile=true, which marks it as one the operator's
// controllers own.
func (id Identity) Labels(component string) map[string]string {
	labels := id.SelectorLabels(component)
	labels[LabelPartOf] = PartOfValue
	labels[pgtoolboxv1alpha1.LabelManagedBy] = pgtoolboxv1alpha1.ManagedByValue
	labels[pgtoolboxv1alpha1.LabelReconcile] = pgtoolboxv1alpha1.ReconcileLabelValue
	return labels
}

// SelectorLabels returns the immutable subset used in workload selectors.
func (id Identity) SelectorLabels(component string) map[string]string {
	labels := map[string]string{
		LabelName:           id.Application,
		LabelInstance:       id.Instance,
		id.InstanceLabelKey: id.Instance,
	}
	if component != "" {
		labels[LabelComponent] = component
	}
	return labels
}

// Kubernetes name limits.
const (
	maxNameLength     = 63
	truncatedNameStem = 54
)

// BoundedName joins base and suffix, and when the result exceeds the
// 63-character limit shortens it to a 54-character stem plus a dash and 8
// hex characters of its SHA-256, so distinct long names stay distinct.
func BoundedName(base, suffix string) string {
	full := base + suffix
	if len(full) <= maxNameLength {
		return full
	}
	digest := sha256.Sum256([]byte(full))
	return strings.TrimRight(full[:truncatedNameStem], "-.") + "-" + hex.EncodeToString(digest[:4])
}

// Propagate returns labels and annotations for an object of target: the
// matching propagation rules' metadata (in order, later rules win), with
// reserved keys dropped, then the operator's own labels and annotations on
// top. Admission already rejects reserved keys; dropping them again here
// keeps an object written before that rule from overriding identity.
func Propagate(
	target pgtoolboxv1alpha1.PropagationTarget,
	rules []pgtoolboxv1alpha1.MetadataPropagation,
	ownLabels, ownAnnotations map[string]string,
) (labels, annotations map[string]string) {
	labels = map[string]string{}
	annotations = map[string]string{}
	for _, rule := range rules {
		if rule.Target != pgtoolboxv1alpha1.PropagationTargetAll && rule.Target != target {
			continue
		}
		for key, value := range rule.Labels {
			if reservedLabel(key) {
				continue
			}
			labels[key] = value
		}
		for key, value := range rule.Annotations {
			if strings.HasPrefix(key, pgtoolboxv1alpha1.ReservedKeyPrefix) {
				continue
			}
			annotations[key] = value
		}
	}
	maps.Copy(labels, ownLabels)
	maps.Copy(annotations, ownAnnotations)
	if len(annotations) == 0 {
		annotations = nil
	}
	return labels, annotations
}

// FilteredOverlay returns values without the operator's reserved keys, for
// user metadata copied onto an object outside a propagation rule (exposure
// labels and annotations).
func FilteredOverlay(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	filtered := make(map[string]string, len(values))
	for key, value := range values {
		if strings.HasPrefix(key, pgtoolboxv1alpha1.ReservedKeyPrefix) {
			continue
		}
		filtered[key] = value
	}
	return filtered
}

func reservedLabel(key string) bool {
	return strings.HasPrefix(key, pgtoolboxv1alpha1.ReservedKeyPrefix) || strings.HasPrefix(key, "app.kubernetes.io/")
}
