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

package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/fyannk/pgToolBox/internal/operator/accessrequest"
	"github.com/fyannk/pgToolBox/internal/operator/console"
	"github.com/fyannk/pgToolBox/internal/operator/evidenceagent"
	"github.com/fyannk/pgToolBox/internal/operator/managerrole"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin"
	"github.com/fyannk/pgToolBox/pkg/discovery"
)

// startupAPI is what the start-up reads need of the API: reads and probe
// Lists, the access reviews of the permission check, and the RESTMapper
// that tells a namespaced resource from a cluster-scoped one. The start-up
// client (startupClient) and a fake client satisfy it.
type startupAPI interface {
	client.Reader
	managerrole.Creator
	RESTMapper() meta.RESTMapper
}

// requiredKinds is every kind the controllers' informers hold whatever the
// optional APIs (CachedKinds with every optional feature off): the kinds
// whose informer would never sync in a namespace that refuses to list or
// watch them.
func requiredKinds() []client.Object {
	var kinds []client.Object
	kinds = append(kinds, pgadmin.CachedKinds(pgadmin.Features{})...)
	kinds = append(kinds, console.CachedKinds(console.Features{})...)
	kinds = append(kinds, evidenceagent.CachedKinds(evidenceagent.OptionalAPIs{})...)
	return append(kinds, accessrequest.CachedKinds()...)
}

// requiredPermissions is what the operator needs in every watched
// namespace: each rule of the manager role on a namespaced resource
// (subresources included: pgadmins/status) with all its verbs, and list and
// watch on each namespaced kind of kinds (what the informers hold; the role
// grants them, this only makes sure a watch added without its rule is
// checked too). Left out: the optional kinds (probed one by one, and off in
// a namespace that refuses them, not a reason to drop it), cluster-scoped
// resources (granted cluster-wide, not by a namespace's administrator),
// rules restricted to resource names (the operator does not watch them) and
// resources the cluster does not serve. Sorted, so a log of what is missing
// reads the same at every start.
func requiredPermissions(mapper meta.RESTMapper, objectScheme *runtime.Scheme, rules []rbacv1.PolicyRule, kinds []client.Object, optional []schema.GroupKind) ([]managerrole.Permission, error) {
	verbs := map[schema.GroupResource][]string{}
	add := func(group, resource string, more []string) {
		key := schema.GroupResource{Group: group, Resource: resource}
		for _, verb := range more {
			if !slices.Contains(verbs[key], verb) {
				verbs[key] = append(verbs[key], verb)
			}
		}
	}
	namespaced := func(gvk schema.GroupVersionKind) (string, bool, error) {
		if slices.Contains(optional, gvk.GroupKind()) {
			return "", false, nil
		}
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		switch {
		case discovery.IsNotServed(err):
			return "", false, nil
		case err != nil:
			return "", false, fmt.Errorf("map %s: %w", gvk.Kind, err)
		}
		return mapping.Resource.Resource, mapping.Scope.Name() == meta.RESTScopeNameNamespace, nil
	}
	for _, rule := range rules {
		if len(rule.ResourceNames) > 0 || len(rule.NonResourceURLs) > 0 {
			continue
		}
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				base, _, _ := strings.Cut(resource, "/")
				gvk, err := mapper.KindFor(schema.GroupVersionResource{Group: group, Resource: base})
				switch {
				case discovery.IsNotServed(err):
					continue
				case err != nil:
					return nil, fmt.Errorf("map %s: %w", managerrole.Permission{Group: group, Resource: base}.String(), err)
				}
				if _, ok, err := namespaced(gvk); err != nil {
					return nil, err
				} else if ok {
					add(group, resource, rule.Verbs)
				}
			}
		}
	}
	for _, object := range kinds {
		gvk, err := apiutil.GVKForObject(object, objectScheme)
		if err != nil {
			return nil, err
		}
		resource, ok, err := namespaced(gvk)
		if err != nil {
			return nil, err
		}
		if ok {
			add(gvk.Group, resource, []string{"list", "watch"})
		}
	}
	permissions := make([]managerrole.Permission, 0, len(verbs))
	for key, granted := range verbs {
		slices.Sort(granted)
		permissions = append(permissions, managerrole.Permission{Group: key.Group, Resource: key.Resource, Verbs: granted})
	}
	slices.SortFunc(permissions, func(a, b managerrole.Permission) int {
		return strings.Compare(a.Group+"/"+a.Resource, b.Group+"/"+b.Resource)
	})
	return permissions, nil
}

// permittedNamespaces keeps the watched namespaces where the operator holds
// every permission it needs (requiredPermissions) and logs each one it
// lacks one in, naming that namespace and the permissions missing there: a
// namespace whose administrator replaced the chart's RoleBinding by a
// narrower Role (Clusters readable, Deployments or Secrets not; Clusters
// listed but not watched) would 403 the operator's informer there forever,
// the cache would never sync, and the operator would crash-loop for every
// namespace. A namespace whose review fails (an error or no answer within
// managerrole.NamespaceReviewTimeout after the retries, or a response too
// large) is left out too, with its error logged: nothing is known of it,
// and the others must not wait on it. Left out, it is not reconciled until
// it is fixed and the operator restarted; the others are unaffected. It
// returns the namespaces
// kept and those left out, why and (for missing permissions) what the
// review showed. At least one namespace must be kept; empty (every
// namespace) stays empty.
func permittedNamespaces(ctx context.Context, api startupAPI, namespaces []string) ([]string, []droppedNamespace, error) {
	if len(namespaces) == 0 {
		return nil, nil, nil
	}
	rules, err := managerrole.Rules()
	if err != nil {
		return nil, nil, err
	}
	required, err := requiredPermissions(api.RESTMapper(), scheme, rules, requiredKinds(), optionalGroupKinds())
	if err != nil {
		return nil, nil, err
	}
	if len(required) == 0 {
		return namespaces, nil, nil
	}
	review, err := managerrole.PermittedNamespaces(ctx, api, required, namespaces, namespaceReviewTimeout)
	if err != nil {
		return nil, nil, err
	}
	var dropped []droppedNamespace
	for _, d := range review.Denied {
		missing := make([]string, len(d.Missing))
		for i, permission := range d.Missing {
			missing[i] = permission.String()
		}
		setupLog.Error(errors.New(d.Reason()), "watched namespace left out: the operator lacks permissions it needs there (restore the chart's manager RoleBinding, then restart the operator)",
			"namespace", d.Namespace, "missing", missing)
		dropped = append(dropped, droppedNamespace{Namespace: d.Namespace, Cause: dropPermissions, Reason: d.Reason(), required: required, missing: d.Missing})
	}
	for _, u := range review.Unreviewed {
		if u.TooLarge() {
			setupLog.Error(u.Err, "watched namespace left out: the operator's permission review there is too large (its RoleBindings grant the operator far more rules than its manager role; remove the extra bindings, then restart the operator)",
				"namespace", u.Namespace)
			dropped = append(dropped, droppedNamespace{Namespace: u.Namespace, Cause: dropReviewTooLarge, Reason: u.Reason()})
			continue
		}
		setupLog.Error(u.Err, "watched namespace left out: the operator's permissions there could not be reviewed (check the API server and the namespace's RoleBindings, then restart the operator)",
			"namespace", u.Namespace)
		dropped = append(dropped, droppedNamespace{Namespace: u.Namespace, Cause: dropReviewFailed, Reason: u.Reason()})
	}
	if len(review.Permitted) == 0 {
		return nil, dropped, fmt.Errorf("none of the watched namespaces %v grants the operator every permission of its manager role (%d lack one, %d could not be reviewed): restore the chart's manager RoleBindings",
			namespaces, len(review.Denied), len(review.Unreviewed))
	}
	return review.Permitted, dropped, nil
}

// namespaceReviewTimeout bounds each attempt at reviewing one namespace's
// permissions at start (managerrole.NamespaceReviewTimeout; a variable for
// the tests).
var namespaceReviewTimeout = managerrole.NamespaceReviewTimeout
