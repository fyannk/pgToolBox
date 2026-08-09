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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
)

func bootstrapUser(t *testing.T, c client.Client, console *pgtoolboxv1alpha1.PgConsole) pgtoolboxv1alpha1.PgToolBoxUser {
	t.Helper()
	var user pgtoolboxv1alpha1.PgToolBoxUser
	key := client.ObjectKey{Namespace: console.Namespace, Name: bootstrapAdminUserName(console)}
	if err := c.Get(context.Background(), key, &user); err != nil {
		t.Fatalf("read bootstrap admin: %v", err)
	}
	return user
}

func TestBootstrapAdminIsMaterialized(t *testing.T) {
	console := testConsole()
	r, c := newTestReconciler(t, console)

	if err := r.reconcileBootstrapAdmin(context.Background(), console); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	user := bootstrapUser(t, c, console)
	if user.Spec.Subject != "root@corp.example" {
		t.Fatalf("subject = %q", user.Spec.Subject)
	}
	// dba, always: no lower level can approve the first access request,
	// which is the entire reason the object exists.
	if user.Spec.Level != pgtoolboxv1alpha1.RoleLevelDBA {
		t.Fatalf("level = %q, want dba", user.Spec.Level)
	}
	if user.Spec.LocalPasswordSecretRef == nil || user.Spec.LocalPasswordSecretRef.Name != "root-password" {
		t.Fatalf("password ref = %+v", user.Spec.LocalPasswordSecretRef)
	}
	if len(user.OwnerReferences) != 1 || user.OwnerReferences[0].Name != console.Name {
		t.Fatalf("owner references = %+v, want the console", user.OwnerReferences)
	}
}

// Kubernetes has no undeletable object. What it has is an object the
// controller puts back, which makes deleting it a transient state rather
// than a locked-out console.
func TestBootstrapAdminReturnsAfterDeletion(t *testing.T) {
	console := testConsole()
	r, c := newTestReconciler(t, console)
	ctx := context.Background()

	if err := r.reconcileBootstrapAdmin(ctx, console); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	user := bootstrapUser(t, c, console)
	if err := c.Delete(ctx, &user); err != nil {
		t.Fatalf("delete bootstrap admin: %v", err)
	}
	if err := r.reconcileBootstrapAdmin(ctx, console); err != nil {
		t.Fatalf("reconcile after deletion: %v", err)
	}
	if got := bootstrapUser(t, c, console); got.Spec.Subject != console.Spec.Proxy.Authentication.BootstrapAdmin.Subject {
		t.Fatalf("subject after recreation = %q", got.Spec.Subject)
	}
}

// The spec is the authority on who the first administrator is. An ordinary
// object claiming the same subject must not be able to shadow it — that
// would be a way to demote the account by choosing a name.
func TestBootstrapAdminWinsDuplicateSubject(t *testing.T) {
	console := testConsole()
	// Sorts before the bootstrap user's name, so name order alone would
	// have picked this one.
	impostor := &pgtoolboxv1alpha1.PgToolBoxUser{
		ObjectMeta: metav1.ObjectMeta{Name: "aaa-impostor", Namespace: console.Namespace},
		Spec: pgtoolboxv1alpha1.PgToolBoxUserSpec{
			PgConsoleRef: pgtoolboxv1alpha1.LocalObjectReference{Name: console.Name},
			Subject:      "root@corp.example",
			Level:        pgtoolboxv1alpha1.RoleLevelView,
		},
	}
	password := testLocalPasswordSecret("root-password",
		"$2a$04$1MkEYTirgqR9o.t6dMEyzOoRST1ueBQEAgrb3x8I9RRUD3XwpTBDG")
	r, _ := newTestReconciler(t, console, impostor, password)
	ctx := context.Background()

	if err := r.reconcileBootstrapAdmin(ctx, console); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	resolved, err := r.resolveConsoleUsers(ctx, console)
	if err != nil {
		t.Fatalf("resolve users: %v", err)
	}

	var kept []pgtoolboxv1alpha1.RoleLevel
	for _, u := range resolved {
		if u.proxyExcluded {
			if u.user.Name != "aaa-impostor" {
				t.Fatalf("excluded the wrong user: %s (%s)", u.user.Name, u.proxyExcludeReason)
			}
			continue
		}
		kept = append(kept, u.user.Spec.Level)
	}
	if len(kept) != 1 || kept[0] != pgtoolboxv1alpha1.RoleLevelDBA {
		t.Fatalf("kept levels = %v, want [dba]", kept)
	}
}

// With an identity provider enabled the first administrator authenticates
// there like everyone else, so forcing a local password on them would be a
// permanent credential nobody uses.
func TestBootstrapAdminWithoutPassword(t *testing.T) {
	console := testConsole()
	console.Spec.Proxy.Authentication.BootstrapAdmin.PasswordSecretRef = nil
	console.Spec.Proxy.Authentication.Local = nil
	console.Spec.Proxy.Authentication.OIDC = &pgtoolboxv1alpha1.ProxyOIDCSpec{
		IssuerURL:       "https://idp.example.com",
		ClientID:        "pgconsole",
		ClientSecretRef: pgtoolboxv1alpha1.SecretKeyReference{Name: "oidc-client"},
	}
	r, c := newTestReconciler(t, console)

	if err := r.reconcileBootstrapAdmin(context.Background(), console); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if ref := bootstrapUser(t, c, console).Spec.LocalPasswordSecretRef; ref != nil {
		t.Fatalf("password ref = %+v, want none", ref)
	}
}

// A bootstrap admin whose password Secret is missing is a fault to repair.
// It must not be an invitation for another object to inherit the identity
// the console assigned, at whatever level that object happens to name.
func TestBootstrapAdminSubjectIsNotInherited(t *testing.T) {
	console := testConsole()
	impostor := &pgtoolboxv1alpha1.PgToolBoxUser{
		ObjectMeta: metav1.ObjectMeta{Name: "aaa-impostor", Namespace: console.Namespace},
		Spec: pgtoolboxv1alpha1.PgToolBoxUserSpec{
			PgConsoleRef: pgtoolboxv1alpha1.LocalObjectReference{Name: console.Name},
			Subject:      "root@corp.example",
			Level:        pgtoolboxv1alpha1.RoleLevelView,
		},
	}
	// No password Secret: the bootstrap admin cannot be rendered.
	r, _ := newTestReconciler(t, console, impostor)
	ctx := context.Background()

	if err := r.reconcileBootstrapAdmin(ctx, console); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	resolved, err := r.resolveConsoleUsers(ctx, console)
	if err != nil {
		t.Fatalf("resolve users: %v", err)
	}
	for _, u := range resolved {
		if !u.proxyExcluded {
			t.Fatalf("user %s (level %s) was rendered for a subject the console reserved",
				u.user.Name, u.user.Spec.Level)
		}
	}
}
