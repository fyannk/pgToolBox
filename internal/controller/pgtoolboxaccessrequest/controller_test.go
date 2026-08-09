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

package pgtoolboxaccessrequest

import (
	"context"
	"testing"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func requestKey() client.ObjectKey {
	return client.ObjectKey{Namespace: "test", Name: "req-1"}
}

func TestReconcileApprovedCreatesUser(t *testing.T) {
	console := testConsole()
	req := testAccessRequest(pgtoolboxv1alpha1.AccessRequestStateApproved, pgtoolboxv1alpha1.RoleLevelView)
	r, c := newTestReconciler(t, console, req)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: requestKey()}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var user pgtoolboxv1alpha1.PgToolBoxUser
	userKey := client.ObjectKey{Namespace: "test", Name: userNameFor(console, req.Spec.Subject)}
	if err := c.Get(context.Background(), userKey, &user); err != nil {
		t.Fatalf("PgToolBoxUser was not created: %v", err)
	}
	if user.Spec.Subject != req.Spec.Subject ||
		user.Spec.Level != pgtoolboxv1alpha1.RoleLevelView ||
		user.Spec.PgConsoleRef.Name != console.Name {
		t.Fatalf("user spec = %+v", user.Spec)
	}

	var live pgtoolboxv1alpha1.PgToolBoxAccessRequest
	if err := c.Get(context.Background(), requestKey(), &live); err != nil {
		t.Fatalf("read request: %v", err)
	}
	if conditionOf(&live, pgtoolboxv1alpha1.AccessRequestConditionDecided).Status != metav1.ConditionTrue {
		t.Fatalf("Decided = %+v", conditionOf(&live, pgtoolboxv1alpha1.AccessRequestConditionDecided))
	}
	if conditionOf(&live, pgtoolboxv1alpha1.AccessRequestConditionUserReady).Status != metav1.ConditionTrue {
		t.Fatalf("UserReady = %+v", conditionOf(&live, pgtoolboxv1alpha1.AccessRequestConditionUserReady))
	}
}

func TestReconcileApprovedIsIdempotent(t *testing.T) {
	console := testConsole()
	req := testAccessRequest(pgtoolboxv1alpha1.AccessRequestStateApproved, pgtoolboxv1alpha1.RoleLevelView)
	r, c := newTestReconciler(t, console, req)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: requestKey()}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: requestKey()}); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	var users pgtoolboxv1alpha1.PgToolBoxUserList
	if err := c.List(context.Background(), &users, client.InNamespace("test")); err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users.Items) != 1 {
		t.Fatalf("users = %v, want exactly one", users.Items)
	}
}

func TestReconcilePending(t *testing.T) {
	console := testConsole()
	req := testAccessRequest(pgtoolboxv1alpha1.AccessRequestStatePending, pgtoolboxv1alpha1.RoleLevelView)
	r, c := newTestReconciler(t, console, req)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: requestKey()}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var live pgtoolboxv1alpha1.PgToolBoxAccessRequest
	if err := c.Get(context.Background(), requestKey(), &live); err != nil {
		t.Fatalf("read request: %v", err)
	}
	cond := conditionOf(&live, pgtoolboxv1alpha1.AccessRequestConditionDecided)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != pgtoolboxv1alpha1.ReasonPending {
		t.Fatalf("Decided = %+v", cond)
	}
	userCond := conditionOf(&live, pgtoolboxv1alpha1.AccessRequestConditionUserReady)
	if userCond == nil || userCond.Status != metav1.ConditionUnknown {
		t.Fatalf("UserReady = %+v", userCond)
	}

	var users pgtoolboxv1alpha1.PgToolBoxUserList
	if err := c.List(context.Background(), &users, client.InNamespace("test")); err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users.Items) != 0 {
		t.Fatalf("no user may be created for a pending request")
	}
}

func TestReconcileDenied(t *testing.T) {
	console := testConsole()
	req := testAccessRequest(pgtoolboxv1alpha1.AccessRequestStateDenied, pgtoolboxv1alpha1.RoleLevelView)
	r, c := newTestReconciler(t, console, req)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: requestKey()}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var live pgtoolboxv1alpha1.PgToolBoxAccessRequest
	if err := c.Get(context.Background(), requestKey(), &live); err != nil {
		t.Fatalf("read request: %v", err)
	}
	cond := conditionOf(&live, pgtoolboxv1alpha1.AccessRequestConditionDecided)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != pgtoolboxv1alpha1.ReasonDenied {
		t.Fatalf("Decided = %+v", cond)
	}
	userCond := conditionOf(&live, pgtoolboxv1alpha1.AccessRequestConditionUserReady)
	if userCond == nil || userCond.Status != metav1.ConditionFalse || userCond.Reason != pgtoolboxv1alpha1.ReasonDenied {
		t.Fatalf("UserReady = %+v", userCond)
	}
}

func TestReconcileApprovedMissingRoleRef(t *testing.T) {
	console := testConsole()
	req := testAccessRequest(pgtoolboxv1alpha1.AccessRequestStateApproved, "")
	r, c := newTestReconciler(t, console, req)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: requestKey()}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var live pgtoolboxv1alpha1.PgToolBoxAccessRequest
	if err := c.Get(context.Background(), requestKey(), &live); err != nil {
		t.Fatalf("read request: %v", err)
	}
	userCond := conditionOf(&live, pgtoolboxv1alpha1.AccessRequestConditionUserReady)
	if userCond == nil || userCond.Status != metav1.ConditionFalse || userCond.Reason != pgtoolboxv1alpha1.ReasonConfigurationInvalid {
		t.Fatalf("UserReady = %+v", userCond)
	}
}

// An approval has to say what it grants. There is no role object to look
// up any more, so an empty level is the whole of the invalid case.
func TestReconcileApprovedWithoutLevel(t *testing.T) {
	console := testConsole()
	req := testAccessRequest(pgtoolboxv1alpha1.AccessRequestStateApproved, "")
	r, c := newTestReconciler(t, console, req)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: requestKey()}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var live pgtoolboxv1alpha1.PgToolBoxAccessRequest
	if err := c.Get(context.Background(), requestKey(), &live); err != nil {
		t.Fatalf("read request: %v", err)
	}
	userCond := conditionOf(&live, pgtoolboxv1alpha1.AccessRequestConditionUserReady)
	if userCond == nil || userCond.Status != metav1.ConditionFalse ||
		userCond.Reason != pgtoolboxv1alpha1.ReasonConfigurationInvalid {
		t.Fatalf("UserReady = %+v", userCond)
	}

	// And nothing was created on the strength of an empty grant.
	var users pgtoolboxv1alpha1.PgToolBoxUserList
	if err := c.List(context.Background(), &users); err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users.Items) != 0 {
		t.Fatalf("an approval with no level created %d user(s)", len(users.Items))
	}
}

func TestReconcileApprovedUpdatesExistingUser(t *testing.T) {
	console := testConsole()
	req := testAccessRequest(pgtoolboxv1alpha1.AccessRequestStateApproved, pgtoolboxv1alpha1.RoleLevelView)
	existing := &pgtoolboxv1alpha1.PgToolBoxUser{
		ObjectMeta: metav1.ObjectMeta{Name: userNameFor(console, req.Spec.Subject), Namespace: "test"},
		Spec: pgtoolboxv1alpha1.PgToolBoxUserSpec{
			PgConsoleRef: pgtoolboxv1alpha1.LocalObjectReference{Name: console.Name},
			Subject:      req.Spec.Subject,
			Level:        pgtoolboxv1alpha1.RoleLevelView,
		},
	}
	r, c := newTestReconciler(t, console, req, existing)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: requestKey()}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var user pgtoolboxv1alpha1.PgToolBoxUser
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(existing), &user); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if user.Spec.Level != pgtoolboxv1alpha1.RoleLevelView {
		t.Fatalf("user level = %q, want the granted level", user.Spec.Level)
	}
}
