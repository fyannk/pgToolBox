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

// Package specs holds the pure helpers every child-object builder of the
// operator uses: the content hash that drives rollouts, the non-negotiable
// security contexts, and image reference resolution. Builders deep-copy
// their inputs and never read the cluster; everything they need is passed in.
package specs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// Hash returns the hex SHA-256 (first 16 characters) of v's JSON encoding.
// encoding/json sorts map keys, so equal values hash equally.
func Hash(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		// Every value hashed here is a Kubernetes API type or a plain
		// struct of strings; failing to encode one is a programming error.
		panic(fmt.Sprintf("specs.Hash: %v", err))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16]
}

// SetHash stores hash in the object's spec-hash annotation.
func SetHash(obj metav1.Object, hash string) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[pgtoolboxv1alpha1.AnnotationSpecHash] = hash
	obj.SetAnnotations(annotations)
}

// HashOf returns the spec-hash annotation of obj, "" when absent.
func HashOf(obj metav1.Object) string {
	return obj.GetAnnotations()[pgtoolboxv1alpha1.AnnotationSpecHash]
}

// ContainerSecurityContext is the restricted Pod Security Standard for one
// container: no privilege escalation, every capability dropped, and a
// read-only root filesystem (pgAdmin's container included since its image
// writes only under /run/pgadmin): whatever a container
// writes goes to a volume the builder declares.
func ContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: new(false),
		ReadOnlyRootFilesystem:   new(true),
		RunAsNonRoot:             new(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// PodSecurityContext is the restricted Pod security context. No UID is ever
// fixed: the image's non-root user runs, or the one OpenShift's SCC
// assigns. fsGroup is the one platform-dependent field: where the platform
// assigns none (no SCC), fsGroup makes the settings volume writable by the
// image's user; on OpenShift it is omitted so restricted-v2 allocates one
// from the namespace's range.
func PodSecurityContext(platformAssignsGroups bool, fsGroup int64) *corev1.PodSecurityContext {
	sc := &corev1.PodSecurityContext{
		RunAsNonRoot:   new(true),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	if !platformAssignsGroups {
		policy := corev1.FSGroupChangeOnRootMismatch
		sc.FSGroup = new(fsGroup)
		sc.FSGroupChangePolicy = &policy
	}
	return sc
}

// ResolvedImage is an image reference ready for a container.
type ResolvedImage struct {
	// Reference is repository[:tag][@digest].
	Reference string
	// PullPolicy is the effective policy.
	PullPolicy corev1.PullPolicy
	// PullSecrets are the image pull secrets.
	PullSecrets []corev1.LocalObjectReference
	// Pinned reports whether the reference carries a digest.
	Pinned bool
}

// ResolveImage returns spec's reference, or fallback (the operator's
// default, normally digest-pinned) when spec is nil. A tag-only reference
// defaults to Always, so a moved tag is at least noticed on restart; a
// digest to IfNotPresent.
func ResolveImage(spec *pgtoolboxv1alpha1.ImageSpec, fallback string) ResolvedImage {
	if spec == nil {
		return ResolvedImage{
			Reference:  fallback,
			PullPolicy: corev1.PullIfNotPresent,
			Pinned:     IsPinned(fallback),
		}
	}
	reference := spec.Repository
	if spec.Tag != "" {
		reference += ":" + spec.Tag
	}
	if spec.Digest != "" {
		reference += "@" + spec.Digest
	}
	policy := spec.PullPolicy
	if policy == "" {
		policy = corev1.PullAlways
		if spec.Digest != "" {
			policy = corev1.PullIfNotPresent
		}
	}
	var secrets []corev1.LocalObjectReference
	for _, secret := range spec.PullSecrets {
		secrets = append(secrets, corev1.LocalObjectReference{Name: secret.Name})
	}
	return ResolvedImage{Reference: reference, PullPolicy: policy, PullSecrets: secrets, Pinned: spec.Digest != ""}
}

// IsPinned reports whether a reference names a digest.
func IsPinned(reference string) bool {
	for i := len(reference) - 1; i >= 0; i-- {
		if reference[i] == '@' {
			return true
		}
		if reference[i] == '/' {
			return false
		}
	}
	return false
}

// ImageAllowed reports whether an image repository a CR names may run:
// it equals an allowed entry, or extends an entry that ends in "/". With
// no list, only the default image's repository is allowed. Whoever may
// create the CR chooses which image receives the namespace's credentials,
// so the choice is bounded by the operator's installer
// (--evidence-agent-image-repositories, --pgadmin-image-repositories).
func ImageAllowed(repository string, allowed []string, defaultImage string) bool {
	if len(allowed) == 0 {
		allowed = []string{ImageRepository(defaultImage)}
	}
	for _, entry := range allowed {
		switch {
		case entry == "":
		case entry == repository:
			return true
		case strings.HasSuffix(entry, "/") && strings.HasPrefix(repository, entry):
			return true
		}
	}
	return false
}

// ImageRepository strips the tag and digest from an image reference.
func ImageRepository(reference string) string {
	reference, _, _ = strings.Cut(reference, "@")
	if slash, colon := strings.LastIndex(reference, "/"), strings.LastIndex(reference, ":"); colon > slash {
		reference = reference[:colon]
	}
	return reference
}
