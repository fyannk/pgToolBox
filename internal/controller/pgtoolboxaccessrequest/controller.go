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

// Package pgtoolboxaccessrequest reconciles review decisions: when a
// dba-level user approves a PgToolBoxAccessRequest, the operator materializes
// the corresponding PgToolBoxUser. Denial stays an audit record.
package pgtoolboxaccessrequest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	"github.com/fyannk/pgtoolbox/internal/conditions"
	"github.com/fyannk/pgtoolbox/internal/controller/shared"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// Reconciler reconciles a PgToolBoxAccessRequest resource.
type Reconciler struct {
	shared.Runtime
}

// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgtoolboxaccessrequests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgtoolboxaccessrequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgconsoles,verbs=get;list;watch
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgtoolboxroles,verbs=get;list;watch
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgtoolboxusers,verbs=get;list;watch;create;update;patch

// Reconcile reads the review decision and materializes the PgToolBoxUser for
// approved requests. It never deletes users: de-provisioning happens when the
// PgToolBoxUser itself is deleted.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var request pgtoolboxv1alpha1.PgToolBoxAccessRequest
	if err := r.Get(ctx, req.NamespacedName, &request); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !request.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if request.Annotations[pgtoolboxv1alpha1.ReconcileAnnotation] == "skip" {
		log.Info("reconciliation skipped by annotation")
		return ctrl.Result{}, nil
	}

	before := request.DeepCopy()
	request.Status.ObservedGeneration = request.GetGeneration()

	switch request.Status.State {
	case pgtoolboxv1alpha1.AccessRequestStateApproved:
		conditions.MarkTrue(
			&request,
			pgtoolboxv1alpha1.AccessRequestConditionDecided,
			pgtoolboxv1alpha1.ReasonApproved,
			"access request approved",
		)
		if err := r.reconcileApprovedUser(ctx, &request); err != nil {
			return ctrl.Result{}, err
		}
	case pgtoolboxv1alpha1.AccessRequestStateDenied:
		conditions.MarkTrue(
			&request,
			pgtoolboxv1alpha1.AccessRequestConditionDecided,
			pgtoolboxv1alpha1.ReasonDenied,
			"access request denied",
		)
		conditions.MarkFalse(
			&request,
			pgtoolboxv1alpha1.AccessRequestConditionUserReady,
			pgtoolboxv1alpha1.ReasonDenied,
			"access request denied; no user was created",
		)
	default:
		conditions.MarkFalse(
			&request,
			pgtoolboxv1alpha1.AccessRequestConditionDecided,
			pgtoolboxv1alpha1.ReasonPending,
			"access request is pending review",
		)
		conditions.MarkUnknown(
			&request,
			pgtoolboxv1alpha1.AccessRequestConditionUserReady,
			pgtoolboxv1alpha1.ReasonNotRequested,
			"access request is pending review",
		)
	}

	return ctrl.Result{}, r.updateStatus(ctx, before, &request)
}

// reconcileApprovedUser validates the granted level and ensures the
// PgToolBoxUser exists. A missing console marks the request not ready
// without failing the reconcile.
func (r *Reconciler) reconcileApprovedUser(ctx context.Context, request *pgtoolboxv1alpha1.PgToolBoxAccessRequest) error {
	level := request.Status.RequestedLevel
	if level == "" {
		conditions.MarkFalse(
			request,
			pgtoolboxv1alpha1.AccessRequestConditionUserReady,
			pgtoolboxv1alpha1.ReasonConfigurationInvalid,
			"approved access request grants no level",
		)
		return nil
	}

	console, err := r.resolvePgConsole(ctx, request)
	if err != nil {
		return err
	}
	if console == nil {
		conditions.MarkFalse(
			request,
			pgtoolboxv1alpha1.AccessRequestConditionUserReady,
			pgtoolboxv1alpha1.ReasonPgConsoleNotFound,
			"referenced PgConsole %s was not found",
			request.Spec.PgConsoleRef.Name,
		)
		return nil
	}

	userName := userNameFor(console, request.Spec.Subject)
	var user pgtoolboxv1alpha1.PgToolBoxUser
	key := client.ObjectKey{Namespace: request.Namespace, Name: userName}
	err = r.APIReader.Get(ctx, key, &user)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if apierrors.IsNotFound(err) {
		user = pgtoolboxv1alpha1.PgToolBoxUser{
			ObjectMeta: metav1.ObjectMeta{
				Name:      userName,
				Namespace: request.Namespace,
				Labels:    shared.PgConsoleApplication().CommonLabels(console.Name),
			},
			Spec: pgtoolboxv1alpha1.PgToolBoxUserSpec{
				PgConsoleRef: pgtoolboxv1alpha1.LocalObjectReference{Name: console.Name},
				Subject:      request.Spec.Subject,
				Level:        level,
			},
		}
		if err := r.Create(ctx, &user); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// A concurrent reconcile created it; the next reconcile will
				// reconcile the existing user.
				return nil
			}
			return fmt.Errorf("create PgToolBoxUser %s/%s: %w", key.Namespace, key.Name, err)
		}
	} else if user.Spec.Level != level || user.Spec.PgConsoleRef.Name != console.Name {
		before := user.DeepCopy()
		user.Spec.PgConsoleRef = pgtoolboxv1alpha1.LocalObjectReference{Name: console.Name}
		user.Spec.Level = level
		if err := r.Patch(ctx, &user, client.MergeFrom(before)); err != nil {
			return fmt.Errorf("update PgToolBoxUser %s/%s: %w", key.Namespace, key.Name, err)
		}
	}

	conditions.MarkTrue(
		request,
		pgtoolboxv1alpha1.AccessRequestConditionUserReady,
		pgtoolboxv1alpha1.ReasonAsExpected,
		"PgToolBoxUser %s is ready",
		userName,
	)
	return nil
}

// resolvePgConsole fetches the console referenced by the request. A nil
// result with nil error means the console does not exist.
func (r *Reconciler) resolvePgConsole(ctx context.Context, request *pgtoolboxv1alpha1.PgToolBoxAccessRequest) (*pgtoolboxv1alpha1.PgConsole, error) {
	var console pgtoolboxv1alpha1.PgConsole
	key := client.ObjectKey{Namespace: request.Namespace, Name: request.Spec.PgConsoleRef.Name}
	if err := r.Get(ctx, key, &console); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &console, nil
}

// userNameFor derives a deterministic PgToolBoxUser name from the console and
// subject, so repeated approvals of the same identity converge on one user.
func userNameFor(console *pgtoolboxv1alpha1.PgConsole, subject string) string {
	digest := sha256.Sum256([]byte(console.Name + "\x00" + subject))
	return shared.BoundedName(console.Name, "-pguser-"+hex.EncodeToString(digest[:8]))
}

// updateStatus patches status only when it semantically changed.
func (r *Reconciler) updateStatus(ctx context.Context, before, after *pgtoolboxv1alpha1.PgToolBoxAccessRequest) error {
	if apiequality.Semantic.DeepEqual(before.Status, after.Status) {
		return nil
	}
	return r.Status().Patch(ctx, after, client.MergeFrom(before))
}

// SetupWithManager wires the controller.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&pgtoolboxv1alpha1.PgToolBoxAccessRequest{}).
		Named("pgtoolboxaccessrequest").
		Complete(r)
}
