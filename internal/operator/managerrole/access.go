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
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// Unreviewed is one namespace whose review failed: an error the API server
// returned after one retry of a transient one, or no answer within the
// namespace's deadline.
type Unreviewed struct {
	Namespace string
	Err       error
}

// Reason is the failure in words, naming the namespace.
func (u Unreviewed) Reason() string {
	return fmt.Sprintf("could not review the operator's permissions in %s: %v", u.Namespace, u.Err)
}

// Review is PermittedNamespaces' answer, each list in the order of the
// namespaces asked.
type Review struct {
	// Permitted grant every permission asked for.
	Permitted []string
	// Denied lack some.
	Denied []Denied
	// Unreviewed could not be reviewed: nothing is known of them.
	Unreviewed []Unreviewed
}

const (
	// NamespaceReviewTimeout bounds one attempt at reviewing one namespace
	// (its rules review and the access reviews it needs): a namespace
	// whose review hangs (a response grown to hundreds of megabytes by
	// tens of thousands of RoleBindings) is left out on its own, within
	// the start-up step, while the others answer.
	NamespaceReviewTimeout = 6 * time.Second
	// accessReviewConcurrency bounds the access reviews in flight per
	// namespace (an incomplete rules review asks one per missing verb).
	accessReviewConcurrency = 4
	// retryPause is the pause before the one retry of a transient error.
	retryPause = 250 * time.Millisecond
)

// PermittedNamespaces splits namespaces into those where the caller holds
// every permission of required, those where it lacks one, and those it
// could not review, with one SelfSubjectRulesReview per namespace
// (concurrently, discovery.ProbeConcurrency at a time): a namespace whose
// administrator bound the caller to a narrower Role (Deployments refused,
// or Clusters listed but not watched) would 403 the caller's informer there
// forever, and a cache that never syncs stops the process for every
// namespace. A review the API server reports incomplete (an authorizer
// that cannot list its rules, such as a webhook) decides nothing alone:
// each permission it does not show is asked again with one
// SelfSubjectAccessReview per verb (accessReviewConcurrency at a time),
// which every authorizer answers. Each namespace is decided by its own
// answers only, each attempt within timeout (NamespaceReviewTimeout when
// zero); a transient failure (a 5xx, a server timeout, throttling, or the
// attempt's deadline) is retried once. A namespace still failing is
// Unreviewed, never kept by guess, and never fails the others. Only ctx
// ending is an error.
func PermittedNamespaces(ctx context.Context, c Creator, required []Permission, namespaces []string, timeout time.Duration) (Review, error) {
	if timeout <= 0 {
		timeout = NamespaceReviewTimeout
	}
	missing := make([][]Permission, len(namespaces))
	failed := make([]error, len(namespaces))
	group := errgroup.Group{}
	group.SetLimit(discovery.ProbeConcurrency)
	for i, namespace := range namespaces {
		group.Go(func() error {
			missing[i], failed[i] = reviewNamespace(ctx, c, required, namespace, timeout)
			return nil
		})
	}
	_ = group.Wait()
	if err := ctx.Err(); err != nil {
		return Review{}, fmt.Errorf("reviewing the operator's permissions: %w", err)
	}
	var review Review
	for i, namespace := range namespaces {
		switch {
		case failed[i] != nil:
			review.Unreviewed = append(review.Unreviewed, Unreviewed{Namespace: namespace, Err: failed[i]})
		case len(missing[i]) == 0:
			review.Permitted = append(review.Permitted, namespace)
		default:
			review.Denied = append(review.Denied, Denied{Namespace: namespace, Missing: missing[i]})
		}
	}
	return review, nil
}

// reviewNamespace is missingIn under its own deadline, retried once on a
// transient failure.
func reviewNamespace(ctx context.Context, c Creator, required []Permission, namespace string, timeout time.Duration) ([]Permission, error) {
	var err error
	for attempt := range 2 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryPause):
			}
		}
		var lacking []Permission
		lacking, err = attemptReview(ctx, c, required, namespace, timeout)
		if err == nil {
			return lacking, nil
		}
		if ctx.Err() != nil || !transient(err) {
			return nil, err
		}
	}
	return nil, err
}

// attemptReview is one missingIn within timeout. A Creator that ignores its
// context still cannot hold the namespace past the deadline: the answer is
// awaited only until then.
func attemptReview(ctx context.Context, c Creator, required []Permission, namespace string, timeout time.Duration) ([]Permission, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type outcome struct {
		missing []Permission
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		lacking, err := missingIn(attemptCtx, c, required, namespace)
		done <- outcome{lacking, err}
	}()
	select {
	case result := <-done:
		if result.err != nil && attemptCtx.Err() != nil && ctx.Err() == nil {
			return nil, fmt.Errorf("reviewing the operator's rules in %s: no answer within %s: %w", namespace, timeout, context.DeadlineExceeded)
		}
		return result.missing, result.err
	case <-attemptCtx.Done():
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("reviewing the operator's rules in %s: no answer within %s: %w", namespace, timeout, context.DeadlineExceeded)
	}
}

// transient reports whether a review failure may pass on its own: a server
// error, a server or gateway timeout, throttling, or no answer in time.
func transient(err error) bool {
	return apierrors.IsInternalError(err) || apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsTooManyRequests(err) || apierrors.IsUnexpectedServerError(err) ||
		errors.Is(err, context.DeadlineExceeded)
}

// missingIn is the permissions of required the caller lacks in namespace,
// each with the verbs it lacks.
func missingIn(ctx context.Context, c Creator, required []Permission, namespace string) ([]Permission, error) {
	review := &authorizationv1.SelfSubjectRulesReview{Spec: authorizationv1.SelfSubjectRulesReviewSpec{Namespace: namespace}}
	if err := c.Create(ctx, review); err != nil {
		return nil, fmt.Errorf("reviewing the operator's rules in %s: %w", namespace, err)
	}
	// lacking[i][j]: required[i].Verbs[j] is not granted. The rules decide
	// what they show; the access reviews of an incomplete review run
	// concurrently, bounded.
	lacking := make([][]bool, len(required))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(accessReviewConcurrency)
	for i, permission := range required {
		lacking[i] = make([]bool, len(permission.Verbs))
		for j, verb := range permission.Verbs {
			if Allows(review.Status.ResourceRules, permission.Group, permission.Resource, verb) {
				continue
			}
			if !review.Status.Incomplete {
				lacking[i][j] = true
				continue
			}
			group.Go(func() error {
				allowed, err := accessAllowed(groupCtx, c, permission.Group, permission.Resource, verb, namespace)
				lacking[i][j] = err == nil && !allowed
				return err
			})
		}
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	var missing []Permission
	for i, permission := range required {
		var verbs []string
		for j, verb := range permission.Verbs {
			if lacking[i][j] {
				verbs = append(verbs, verb)
			}
		}
		if len(verbs) > 0 {
			missing = append(missing, Permission{Group: permission.Group, Resource: permission.Resource, Verbs: verbs})
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
