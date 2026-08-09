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
	"fmt"
	"sort"
	"strings"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	"github.com/fyannk/pgtoolbox/internal/conditions"
	"github.com/fyannk/pgtoolbox/internal/controller/shared"
	proxyconfig "github.com/fyannk/pgtoolbox/internal/proxy/config"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// resolvedConsoleUser holds the per-user state resolved once per reconcile and
// shared between the proxy config render, pgAdmin sync, and status patching.
type resolvedConsoleUser struct {
	user               pgtoolboxv1alpha1.PgToolBoxUser
	proxyUser          proxyconfig.User
	proxyExcluded      bool
	proxyExcludeReason string
}

// proxyIncluded reports whether this user is rendered into the proxy config.
func (u resolvedConsoleUser) proxyIncluded() bool { return !u.proxyExcluded }

// resolveConsoleUsers lists every PgToolBoxUser attached to this console,
// resolves its role, local password, and postgres credential, and returns the
// result ordered by user name for deterministic rendering. Per-user resolution
// failures degrade that user instead of failing the whole reconcile.
func (r *Reconciler) resolveConsoleUsers(
	ctx context.Context,
	console *pgtoolboxv1alpha1.PgConsole,
) ([]resolvedConsoleUser, error) {
	var list pgtoolboxv1alpha1.PgToolBoxUserList
	if err := r.List(ctx, &list, client.InNamespace(console.Namespace)); err != nil {
		return nil, err
	}

	var result []resolvedConsoleUser
	for i := range list.Items {
		if list.Items[i].Spec.PgConsoleRef.Name != console.Name {
			continue
		}
		resolved, err := r.resolveConsoleUser(ctx, console, &list.Items[i])
		if err != nil {
			return nil, err
		}
		result = append(result, resolved)
	}

	sort.Slice(result, func(i, j int) bool { return result[i].user.Name < result[j].user.Name })

	// Reject duplicate subjects deterministically: the second user is excluded.
	seen := map[string]struct{}{}
	for i := range result {
		if result[i].proxyExcluded {
			continue
		}
		key := strings.ToLower(result[i].proxyUser.Subject)
		if _, dup := seen[key]; dup {
			result[i].proxyExcluded = true
			result[i].proxyExcludeReason = fmt.Sprintf("duplicate subject %q", result[i].proxyUser.Subject)
			continue
		}
		seen[key] = struct{}{}
	}

	return result, nil
}

// resolveConsoleUser resolves one PgToolBoxUser. Missing references are treated
// as degradation; unexpected API errors are returned so the reconcile can
// retry.
func (r *Reconciler) resolveConsoleUser(
	ctx context.Context,
	console *pgtoolboxv1alpha1.PgConsole,
	user *pgtoolboxv1alpha1.PgToolBoxUser,
) (resolvedConsoleUser, error) {
	resolved := resolvedConsoleUser{user: *user}

	// The level is on the user. Admission pins it to the closed set, so a
	// value that fails here predates the schema rather than being a
	// configuration mistake someone can still make.
	level := proxyconfig.Level(user.Spec.Level)
	if !level.Valid() {
		resolved.proxyExcluded = true
		resolved.proxyExcludeReason = fmt.Sprintf("user has invalid level %q", user.Spec.Level)
		return resolved, nil
	}

	proxyUser := proxyconfig.User{
		Subject: user.Spec.Subject,
		Level:   level,
	}

	// A password is what makes a user usable at the local form, and it is
	// optional: with an identity provider enabled alongside, most users
	// authenticate there and carry none. A user with neither a password nor
	// a federated identity simply never signs in, which is a statement the
	// operator has no business second-guessing.
	if ref := user.Spec.LocalPasswordSecretRef; console.Spec.Proxy.Authentication.Local != nil &&
		ref != nil && ref.Name != "" {
		hash, _, err := shared.ReadLocalPasswordHash(
			ctx, r.APIReader,
			client.ObjectKey{Namespace: console.Namespace, Name: ref.Name},
			ref.Key,
		)
		if err != nil {
			resolved.proxyExcluded = true
			if apierrors.IsNotFound(err) {
				resolved.proxyExcludeReason = fmt.Sprintf("local password secret %s was not found", ref.Name)
			} else {
				resolved.proxyExcludeReason = fmt.Sprintf("local password secret %s is invalid", ref.Name)
			}
			return resolved, nil
		}
		proxyUser.LocalPasswordBcrypt = hash
	}
	resolved.proxyUser = proxyUser
	return resolved, nil
}

// proxyUsers returns the slice of proxy config users from the resolved set.
func proxyUsers(resolved []resolvedConsoleUser) []proxyconfig.User {
	var users []proxyconfig.User
	for _, u := range resolved {
		if u.proxyIncluded() {
			users = append(users, u.proxyUser)
		}
	}
	return users
}

// applyUserStatuses patches each PgToolBoxUser status with the outcome of this
// reconcile. It is called after both proxy rendering and pgAdmin sync have run.
func (r *Reconciler) applyUserStatuses(ctx context.Context, resolved []resolvedConsoleUser) error {
	for i := range resolved {
		u := &resolved[i]
		before := u.user.DeepCopy()
		u.user.Status.ObservedGeneration = u.user.GetGeneration()

		if u.proxyExcluded {
			conditions.MarkFalse(
				&u.user,
				pgtoolboxv1alpha1.UserConditionProxySynced,
				pgtoolboxv1alpha1.ReasonSomeDegraded,
				"%s", u.proxyExcludeReason,
			)
			u.user.Status.ProxySynced = false
		} else {
			conditions.MarkTrue(
				&u.user,
				pgtoolboxv1alpha1.UserConditionProxySynced,
				pgtoolboxv1alpha1.ReasonAsExpected,
				"user rendered into proxy configuration",
			)
			u.user.Status.ProxySynced = true
		}

		if err := r.Status().Patch(ctx, &u.user, client.MergeFrom(before)); err != nil {
			return fmt.Errorf("patch PgToolBoxUser %s/%s status: %w", u.user.Namespace, u.user.Name, err)
		}
	}
	return nil
}
