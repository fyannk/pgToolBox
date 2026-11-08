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

package console

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/reconcile"
	"github.com/fyannk/pgToolBox/internal/operator/specs"
	"github.com/fyannk/pgToolBox/pkg/lifecycletable"
)

// The lifecycle ConfigMap, the way CloudNativePG treats its default
// monitoring queries: the operator provides it when there is none and does
// not touch one that is someone else's. In between, the one the operator
// created keeps following the operator's table, so an operator upgrade
// brings new end-of-life lines without anyone editing anything, until a
// person takes it over:
//
//   - absent: created with the built-in table, its spec-hash annotation
//     the hash of that data;
//   - present, controlled by this console, not annotated user-managed and
//     its data still hashing to its annotation: the operator's, rewritten
//     when the built-in table differs;
//   - otherwise (edited, annotated user-managed, or not this console's): a
//     person's, never written again.
//
// spec.lifecycleConfigMapRef skips all of this: the console reads the named
// ConfigMap and the operator neither creates nor reads it (checkReferences
// only checks that it exists).

// reconcileLifecycle provides the lifecycle ConfigMap the console reads, as
// above, and reports who keeps it in status.lifecycle.
func (r *Reconciler) reconcileLifecycle(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole) error {
	name := console.LifecycleConfigMapName()
	if console.Spec.LifecycleConfigMapRef != nil {
		console.Status.Lifecycle = &pgtoolboxv1alpha1.PgToolBoxConsoleLifecycleStatus{ConfigMap: name, KeptBy: pgtoolboxv1alpha1.LifecycleReferenced}
		return nil
	}
	desired := buildLifecycle(console)
	operator := &pgtoolboxv1alpha1.PgToolBoxConsoleLifecycleStatus{
		ConfigMap: name, KeptBy: pgtoolboxv1alpha1.LifecycleKeptByOperator, Reviewed: lifecycletable.Reviewed(),
	}
	user := &pgtoolboxv1alpha1.PgToolBoxConsoleLifecycleStatus{ConfigMap: name, KeptBy: pgtoolboxv1alpha1.LifecycleKeptByUser}

	// ConfigMaps are never cached for content: a direct read.
	existing := &corev1.ConfigMap{}
	err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Client.Create(ctx, desired, client.FieldOwner(reconcile.FieldOwner)); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create ConfigMap %s: %w", name, err)
		}
		console.Status.Lifecycle = operator
		return nil
	case err != nil:
		return fmt.Errorf("read ConfigMap %s: %w", name, err)
	}
	if !operatorKeeps(console, existing) {
		console.Status.Lifecycle = user
		return nil
	}
	console.Status.Lifecycle = operator
	if specs.HashOf(existing) == specs.HashOf(desired) {
		return nil
	}
	// The operator brings a newer table: rewrite, under the resourceVersion
	// read above, so an edit made since is never overwritten (Conflict, and
	// the next loop sees the edit).
	updated := existing.DeepCopy()
	updated.Data = desired.Data
	specs.SetHash(updated, specs.HashOf(desired))
	if err := r.Client.Update(ctx, updated, client.FieldOwner(reconcile.FieldOwner)); err != nil {
		return fmt.Errorf("update ConfigMap %s to the operator's table: %w", name, err)
	}
	return nil
}

// operatorKeeps reports whether the lifecycle ConfigMap is still the
// operator's to write: this console controls it, nobody annotated it
// user-managed, and its data is what the operator last wrote.
func operatorKeeps(console *pgtoolboxv1alpha1.PgToolBoxConsole, existing *corev1.ConfigMap) bool {
	owner := metav1.GetControllerOfNoCopy(existing)
	switch {
	case owner == nil || owner.UID != console.UID:
		return false
	case existing.Annotations[pgtoolboxv1alpha1.AnnotationUserManaged] == pgtoolboxv1alpha1.UserManagedValue:
		return false
	}
	hash := specs.HashOf(existing)
	return hash != "" && hash == specs.Hash(existing.Data)
}
