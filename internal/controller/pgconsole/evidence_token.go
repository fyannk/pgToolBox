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

package pgconsole

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// The pod-local evidence token, per the contract's request-authentication
// section: 32 cryptographically random bytes as 43-character unpadded
// base64url, held in an operator-owned immutable Secret.
//
// The lifecycle rules are the ones pgtoolbox itself wrote into the contract:
// a no-op reconcile reuses the same Secret; rotation is an explicit
// operation that creates a successor under a NEW name, applied as one
// Pod-template revision carrying both containers; in-place mutation is
// forbidden (and the subPath mount makes it inert anyway); a missing active
// Secret self-heals under the next name; superseded Secrets are
// garbage-collected once the replacement Pods are running.
const (
	evidenceTokenKey = "token"
	// evidenceTokenSuffix separates the rotation counter in the Secret
	// name; the counter is parsed back from status for the successor.
	evidenceTokenSuffix = "-evidence-t"
)

// reconcileEvidenceToken resolves the active token Secret name, creating the
// first generation, rotating on the explicit annotation, and self-healing a
// missing Secret under a fresh identity. The returned name is what the Pod
// template must reference; the caller publishes it to status.
//
// The status-held name alone is not a safe starting point: a rotation acks
// its annotation and patches status in two writes, and a reconcile observing
// the console between them sees neither the annotation nor the successor —
// resolving the stale name would roll the freshly rotated Pod template back
// (and, after a crash in the same window, silently lose the rotation to the
// superseded-token collector). The live Deployment template is the persisted
// truth about which generation is actually deployed, so its referenced
// Secret floors the resolution — but only outside a rotation: the successor
// number stays keyed on the status-held generation, which is stable for the
// rotation's whole duration precisely because status is written last, so
// every observer of one annotation computes the same successor and
// AlreadyExists keeps re-runs idempotent. Flooring the rotate path instead
// would let an observer that still sees the annotation after the template
// moved mint a second successor from a single request.
func (r *Reconciler) reconcileEvidenceToken(
	ctx context.Context,
	console *pgtoolboxv1alpha1.PgConsole,
) (string, error) {
	generation := tokenGeneration(console.Status.Evidence.TokenSecretName)
	rotate := console.Annotations[pgtoolboxv1alpha1.RotateEvidenceTokenAnnotation] == "now"
	if !rotate {
		deployedGeneration, err := r.deployedTokenGeneration(ctx, console)
		if err != nil {
			return "", err
		}
		if deployedGeneration > generation {
			generation = deployedGeneration
		}
	}

	switch {
	case generation == 0:
		generation = 1
	case rotate:
		generation++
	default:
		name := evidenceTokenSecretName(console.Name, generation)
		var existing corev1.Secret
		err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: console.Namespace, Name: name}, &existing)
		if err == nil {
			if len(existing.Data[evidenceTokenKey]) == 0 {
				return "", fmt.Errorf("evidence token secret %s is invalid", name)
			}
			return name, nil
		}
		if !apierrors.IsNotFound(err) {
			return "", err
		}
		// The active Secret is gone. Never regenerate under the same name:
		// same-name recreation propagates to the two containers at
		// different times. A new identity forces one atomic replacement.
		generation++
	}

	name := evidenceTokenSecretName(console.Name, generation)
	if err := r.createEvidenceTokenSecret(ctx, console, name); err != nil {
		return "", err
	}

	if rotate {
		// Clear the annotation only after the successor exists, the same
		// acknowledge-after-success order the credential rotation uses; a
		// crash in between re-runs the rotation idempotently.
		live := console.DeepCopy()
		delete(live.Annotations, pgtoolboxv1alpha1.RotateEvidenceTokenAnnotation)
		if err := r.Patch(ctx, live, client.MergeFrom(console)); err != nil {
			return "", fmt.Errorf("acknowledge token rotation: %w", err)
		}
		delete(console.Annotations, pgtoolboxv1alpha1.RotateEvidenceTokenAnnotation)
	}
	return name, nil
}

// deployedTokenGeneration reads the token generation the live Deployment
// template actually references, through APIReader so the answer cannot lag
// the write that moved it. No Deployment or no evidence volume yields zero,
// which floors nothing.
func (r *Reconciler) deployedTokenGeneration(
	ctx context.Context,
	console *pgtoolboxv1alpha1.PgConsole,
) (int, error) {
	var deployment appsv1.Deployment
	err := r.APIReader.Get(ctx, client.ObjectKey{
		Namespace: console.Namespace,
		Name:      application.ResourceName(console.Name, ""),
	}, &deployment)
	if apierrors.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read live deployment for token resolution: %w", err)
	}
	for i := range deployment.Spec.Template.Spec.Volumes {
		volume := &deployment.Spec.Template.Spec.Volumes[i]
		if volume.Name != evidenceTokenVolume || volume.Secret == nil {
			continue
		}
		return tokenGeneration(volume.Secret.SecretName), nil
	}
	return 0, nil
}

// createEvidenceTokenSecret mints one immutable token Secret. Immutability
// covers data only — metadata stays reconcilable — and is load-bearing: the
// API server refuses the in-place mutation the contract forbids.
func (r *Reconciler) createEvidenceTokenSecret(
	ctx context.Context,
	console *pgtoolboxv1alpha1.PgConsole,
	name string,
) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("generate evidence token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	labels := application.CommonLabels(console.Name)
	labels[pgtoolboxv1alpha1.EvidenceTokenLabelKey] = console.Name

	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: console.Namespace,
			Labels:    labels,
		},
		Type:      corev1.SecretTypeOpaque,
		Immutable: ptrTo(true),
		Data:      map[string][]byte{evidenceTokenKey: []byte(token)},
	}
	if err := controllerutil.SetControllerReference(console, secret, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create evidence token secret %s: %w", name, err)
	}
	return nil
}

// collectSupersededTokens deletes every generated token Secret except the
// active one — but only once the Deployment reports the rollout complete,
// so the old token outlives every Pod that still presents it. Over-eager
// deletion would be safe (a missing active Secret self-heals with a new
// identity) but would churn Pods for nothing.
func (r *Reconciler) collectSupersededTokens(
	ctx context.Context,
	console *pgtoolboxv1alpha1.PgConsole,
	activeName string,
	deployment *appsv1.Deployment,
) error {
	if deployment == nil || !rolloutComplete(deployment) {
		return nil
	}

	// Listed as metadata: token content never enters the cache, and
	// selection plus deletion need names and owners only.
	secrets := &metav1.PartialObjectMetadataList{}
	secrets.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("SecretList"))
	if err := r.List(ctx, secrets,
		client.InNamespace(console.Namespace),
		client.MatchingLabels{pgtoolboxv1alpha1.EvidenceTokenLabelKey: console.Name},
	); err != nil {
		return fmt.Errorf("list evidence token secrets: %w", err)
	}
	for i := range secrets.Items {
		item := &secrets.Items[i]
		if item.Name == activeName {
			continue
		}
		owner := metav1.GetControllerOf(item)
		if owner == nil || owner.UID != console.UID {
			continue
		}
		stale := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name:      item.Name,
			Namespace: item.Namespace,
		}}
		if err := r.Delete(ctx, stale); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete superseded token secret %s: %w", item.Name, err)
		}
	}
	return nil
}

// rolloutComplete reports whether every Pod runs the current template. The
// console is a singleton: one replica is the whole rollout.
func rolloutComplete(deployment *appsv1.Deployment) bool {
	return deployment.Status.ObservedGeneration >= deployment.Generation &&
		deployment.Status.UpdatedReplicas == 1 &&
		deployment.Status.ReadyReplicas == 1
}

// evidenceTokenSecretName is deterministic per generation: stable across
// no-op reconciles, different on every rotation.
func evidenceTokenSecretName(consoleName string, generation int) string {
	return application.ResourceName(consoleName, evidenceTokenSuffix+strconv.Itoa(generation))
}

// tokenGeneration parses the rotation counter back out of the status-held
// Secret name; zero means no token exists yet.
func tokenGeneration(secretName string) int {
	index := strings.LastIndex(secretName, evidenceTokenSuffix)
	if index < 0 {
		return 0
	}
	generation, err := strconv.Atoi(secretName[index+len(evidenceTokenSuffix):])
	if err != nil || generation < 1 {
		return 0
	}
	return generation
}
