/*
Copyright © contributors to the pgtoolbox project.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

// Package shared holds the controller plumbing every reconciler in the
// pgtoolbox family needs: application identity, generated-object helpers
// and the manager-derived runtime.
package shared

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
)

// reservedKeyPrefix namespaces the label and annotation keys the operator
// generates. User-supplied metadata carrying these keys is dropped rather
// than honoured, so a spec cannot overwrite operator-owned values.
const reservedKeyPrefix = "pgtoolbox.fyannk.dev/"

// FilteredOverlay copies user-supplied metadata while dropping keys in the
// operator's reserved namespace, which holds values the operator generates
// such as the configuration checksum annotation.
func FilteredOverlay(values map[string]string) map[string]string {
	filtered := make(map[string]string, len(values)+1)
	for key, value := range values {
		if !strings.HasPrefix(key, reservedKeyPrefix) {
			filtered[key] = value
		}
	}
	return filtered
}

const (
	// maxResourceNameLength is the Kubernetes limit for the object names
	// this operator generates.
	maxResourceNameLength = 63

	// truncatedNameStem leaves room for the separator and the digest so a
	// truncated name still fits maxResourceNameLength.
	truncatedNameStem = 54
)

// BoundedName joins base and suffix, truncating with a stable digest when the
// result would exceed the Kubernetes object-name limit, so that long instance
// names still produce a deterministic, collision-resistant name.
func BoundedName(base, suffix string) string {
	fullName := base + suffix
	if len(fullName) <= maxResourceNameLength {
		return fullName
	}
	digest := sha256.Sum256([]byte(fullName))
	return fullName[:truncatedNameStem] + "-" + hex.EncodeToString(digest[:4])
}

// Application is the identity of one member of the pgtoolbox family.
//
// Every field is frozen once an application ships: ResourceInfix is baked into
// the names of live objects, the selector labels into an immutable Deployment
// selector, and Finalizer into stored custom resources. Changing any of them
// orphans resources that the operator can no longer find.
type Application struct {
	// Name is the app.kubernetes.io/name value, and half of the immutable
	// workload selector.
	Name string

	// ResourceInfix is inserted between the instance name and every
	// generated-name suffix. It is deliberately not derived from Name:
	// a longer application name would eat into the 63-character budget
	// that instance names have to share.
	ResourceInfix string

	// InstanceLabelKey is the application-keyed label carrying the owning
	// instance name. Holding the constant rather than composing it from
	// Name keeps one source of truth for a key that is API surface.
	//
	// The scheme is keyed per application rather than a generic
	// application/instance pair because one Pod can belong to two members
	// of the family — ObjectStoreViewer runs as an evidence sidecar in the
	// PgConsole Pod — and a single-valued instance key cannot express that.
	InstanceLabelKey string

	// Finalizer names the controller responsible for teardown, so it is
	// per-kind by design rather than shared across the family.
	Finalizer string
}

// PgConsoleApplication returns the frozen identity of the PgConsole
// application. It is a function rather than a package-level variable so the
// value cannot be mutated by one caller for every other.
func PgConsoleApplication() Application {
	return Application{
		Name:             "pgconsole",
		ResourceInfix:    "-pgconsole",
		InstanceLabelKey: pgtoolboxv1alpha1.PgConsoleLabelKey,
		Finalizer:        pgtoolboxv1alpha1.PgConsoleFinalizer,
	}
}

// ResourceName derives the name of a generated resource belonging to instance.
func (a Application) ResourceName(instance, suffix string) string {
	return BoundedName(instance, a.ResourceInfix+suffix)
}

// CommonLabels returns the identifying labels stamped on every resource
// generated for instance, so it can be selected and traced back to its owner.
func (a Application) CommonLabels(instance string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       a.Name,
		"app.kubernetes.io/instance":   instance,
		"app.kubernetes.io/managed-by": pgtoolboxv1alpha1.ManagedByLabelValue,
		a.InstanceLabelKey:             instance,
	}
}

// SelectorLabels returns the label subset used as the Deployment and Service
// selector. It must stay a strict subset of CommonLabels and must never gain
// a key: a Deployment selector is immutable once created, so a new key can
// only be adopted by deleting and recreating every Deployment.
func (a Application) SelectorLabels(instance string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     a.Name,
		"app.kubernetes.io/instance": instance,
	}
}
