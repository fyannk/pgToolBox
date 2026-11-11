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

package managerrole

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"golang.org/x/sync/errgroup"
	authorizationv1 "k8s.io/api/authorization/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fyannk/pgToolBox/pkg/discovery"
)

// Permission is what a caller needs on one namespaced resource: the verbs,
// on every object of the resource (no resource names). Resource may name a
// subresource ("pgadmins/status").
type Permission struct {
	Group    string
	Resource string
	Verbs    []string
}

// String is the permission as an operator reads it in a log: "list,watch
// deployments.apps".
func (p Permission) String() string {
	resource := p.Resource
	if p.Group != "" {
		base, sub, found := strings.Cut(p.Resource, "/")
		resource = base + "." + p.Group
		if found {
			resource += "/" + sub
		}
	}
	return strings.Join(p.Verbs, ",") + " " + resource
}

// Creator is the one method an access review needs; a controller-runtime
// client or a fake satisfies it.
type Creator interface {
	Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error
}

// Denied is one namespace where the caller lacks some of the permissions
// asked for: those permissions, each with only the verbs it lacks.
type Denied struct {
	Namespace string
	Missing   []Permission
}

// Reason is the missing permissions in words, naming the namespace (the
// only one this Denied is about).
func (d Denied) Reason() string {
	missing := make([]string, len(d.Missing))
	for i, permission := range d.Missing {
		missing[i] = permission.String()
	}
	return fmt.Sprintf("not allowed to %s in %s", strings.Join(missing, "; "), d.Namespace)
}

// PermittedNamespaces splits namespaces into those where the caller holds
// every permission of required and those where it lacks one, with one
// SelfSubjectRulesReview per namespace (concurrently, discovery.ProbeConcurrency at a
// time): a namespace whose administrator bound the caller to a narrower Role
// (Deployments refused, or Clusters listed but not watched) would 403 the
// caller's informer there forever, and a cache that never syncs stops the
// process for every namespace. A review the API server reports incomplete
// (an authorizer that cannot list its rules, such as a webhook) decides
// nothing alone: each permission it does not show is asked again with one
// SelfSubjectAccessReview per verb, which every authorizer answers. Each
// namespace is decided by its own answers only. Any error is returned (the
// caller fails its start): a review it cannot create is a start that
// cannot tell. Both lists keep the order of namespaces.
func PermittedNamespaces(ctx context.Context, c Creator, required []Permission, namespaces []string) (permitted []string, denied []Denied, err error) {
	missing := make([][]Permission, len(namespaces))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(discovery.ProbeConcurrency)
	for i, namespace := range namespaces {
		group.Go(func() error {
			lacking, err := missingIn(groupCtx, c, required, namespace)
			missing[i] = lacking
			return err
		})
	}
	if err := group.Wait(); err != nil {
		return nil, nil, err
	}
	for i, namespace := range namespaces {
		if len(missing[i]) == 0 {
			permitted = append(permitted, namespace)
			continue
		}
		denied = append(denied, Denied{Namespace: namespace, Missing: missing[i]})
	}
	return permitted, denied, nil
}

// missingIn is the permissions of required the caller lacks in namespace,
// each with the verbs it lacks.
func missingIn(ctx context.Context, c Creator, required []Permission, namespace string) ([]Permission, error) {
	review := &authorizationv1.SelfSubjectRulesReview{Spec: authorizationv1.SelfSubjectRulesReviewSpec{Namespace: namespace}}
	if err := c.Create(ctx, review); err != nil {
		return nil, fmt.Errorf("reviewing the operator's rules in %s: %w", namespace, err)
	}
	var missing []Permission
	for _, permission := range required {
		var lacking []string
		for _, verb := range permission.Verbs {
			if Allows(review.Status.ResourceRules, permission.Group, permission.Resource, verb) {
				continue
			}
			if review.Status.Incomplete {
				allowed, err := accessAllowed(ctx, c, permission.Group, permission.Resource, verb, namespace)
				if err != nil {
					return nil, err
				}
				if allowed {
					continue
				}
			}
			lacking = append(lacking, verb)
		}
		if len(lacking) > 0 {
			missing = append(missing, Permission{Group: permission.Group, Resource: permission.Resource, Verbs: lacking})
		}
	}
	return missing, nil
}

// accessAllowed asks the API server with a SelfSubjectAccessReview whether
// the caller may verb resource (possibly "resource/subresource") in
// namespace.
func accessAllowed(ctx context.Context, c Creator, group, resource, verb, namespace string) (bool, error) {
	base, sub, _ := strings.Cut(resource, "/")
	review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
		ResourceAttributes: &authorizationv1.ResourceAttributes{
			Namespace: namespace, Verb: verb, Group: group, Resource: base, Subresource: sub,
		},
	}}
	if err := c.Create(ctx, review); err != nil {
		return false, fmt.Errorf("reviewing %s %s in %s: %w", verb, Permission{Group: group, Resource: resource}.String(), namespace, err)
	}
	return review.Status.Allowed, nil
}

// Allows reports whether rules (a SelfSubjectRulesReview's) grant verb on
// every object of resource ("resource" or "resource/subresource") in group,
// with the RBAC matching rules of the API server (rbacv1helpers.VerbMatches,
// APIGroupMatches, ResourceMatches): "*" matches any verb, group or resource
// (subresources included), "*/sub" that subresource of any resource, and
// any other entry only the exact resource or "resource/subresource" it
// names. "resource/*" is no wildcard: RBAC compares it literally, so it
// grants no subresource and neither does it here. A rule restricted to
// resource names grants no access to every object, so it never counts.
func Allows(rules []authorizationv1.ResourceRule, group, resource, verb string) bool {
	for _, rule := range rules {
		if len(rule.ResourceNames) > 0 {
			continue
		}
		if matchesAny(rule.Verbs, verb) && matchesAny(rule.APIGroups, group) && resourceMatches(rule.Resources, resource) {
			return true
		}
	}
	return false
}

func matchesAny(values []string, want string) bool {
	return slices.Contains(values, "*") || slices.Contains(values, want)
}

func resourceMatches(ruleResources []string, resource string) bool {
	_, sub, hasSub := strings.Cut(resource, "/")
	for _, rule := range ruleResources {
		switch {
		case rule == "*", rule == resource:
			return true
		case hasSub && rule == "*/"+sub:
			return true
		}
	}
	return false
}
