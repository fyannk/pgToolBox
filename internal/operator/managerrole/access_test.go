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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestAllows(t *testing.T) {
	rule := func(groups, resources, verbs []string, names ...string) authorizationv1.ResourceRule {
		return authorizationv1.ResourceRule{APIGroups: groups, Resources: resources, Verbs: verbs, ResourceNames: names}
	}
	apps := []string{"apps"}
	for _, tt := range []struct {
		name     string
		rules    []authorizationv1.ResourceRule
		group    string
		resource string
		verb     string
		want     bool
	}{
		{"exact", []authorizationv1.ResourceRule{rule(apps, []string{"deployments"}, []string{"list", "watch"})}, "apps", "deployments", "watch", true},
		{"verb missing", []authorizationv1.ResourceRule{rule(apps, []string{"deployments"}, []string{"list"})}, "apps", "deployments", "watch", false},
		{"other group", []authorizationv1.ResourceRule{rule([]string{""}, []string{"deployments"}, []string{"watch"})}, "apps", "deployments", "watch", false},
		{"other resource", []authorizationv1.ResourceRule{rule(apps, []string{"statefulsets"}, []string{"watch"})}, "apps", "deployments", "watch", false},
		{"wildcard verb", []authorizationv1.ResourceRule{rule(apps, []string{"deployments"}, []string{"*"})}, "apps", "deployments", "patch", true},
		{"wildcard group", []authorizationv1.ResourceRule{rule([]string{"*"}, []string{"deployments"}, []string{"get"})}, "apps", "deployments", "get", true},
		{"wildcard resource", []authorizationv1.ResourceRule{rule(apps, []string{"*"}, []string{"get"})}, "apps", "deployments", "get", true},
		{"wildcard resource covers a subresource", []authorizationv1.ResourceRule{rule(apps, []string{"*"}, []string{"update"})}, "apps", "deployments/status", "update", true},
		{"resource does not cover its subresource", []authorizationv1.ResourceRule{rule(apps, []string{"deployments"}, []string{"update"})}, "apps", "deployments/status", "update", false},
		// RBAC compares "deployments/*" literally (ResourceMatches honours
		// only "*", an exact match and "*/sub"): it grants no subresource.
		{"resource/* is no wildcard", []authorizationv1.ResourceRule{rule(apps, []string{"deployments/*"}, []string{"update"})}, "apps", "deployments/status", "update", false},
		{"resource/* does not cover the resource", []authorizationv1.ResourceRule{rule(apps, []string{"deployments/*"}, []string{"get"})}, "apps", "deployments", "get", false},
		{"*/subresource", []authorizationv1.ResourceRule{rule(apps, []string{"*/status"}, []string{"update"})}, "apps", "deployments/status", "update", true},
		{"*/other subresource", []authorizationv1.ResourceRule{rule(apps, []string{"*/scale"}, []string{"update"})}, "apps", "deployments/status", "update", false},
		{"subresource does not cover the resource", []authorizationv1.ResourceRule{rule(apps, []string{"deployments/status"}, []string{"get"})}, "apps", "deployments", "get", false},
		{"named objects only", []authorizationv1.ResourceRule{rule(apps, []string{"deployments"}, []string{"list", "watch"}, "one")}, "apps", "deployments", "list", false},
		{"no rules", nil, "apps", "deployments", "list", false},
		{"verbs split over two rules", []authorizationv1.ResourceRule{
			rule(apps, []string{"deployments"}, []string{"list"}), rule(apps, []string{"deployments"}, []string{"watch"}),
		}, "apps", "deployments", "watch", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := Allows(tt.rules, tt.group, tt.resource, tt.verb); got != tt.want {
				t.Fatalf("Allows(%s %s.%s) = %t, want %t", tt.verb, tt.resource, tt.group, got, tt.want)
			}
		})
	}
}

// upstreamResourceMatches is Kubernetes' rbacv1helpers.ResourceMatches
// (pkg/apis/rbac/v1/evaluation_helpers.go, Apache-2.0, The Kubernetes
// Authors), transcribed for one rule's resources: what the RBAC authorizer
// matches a request's "resource" or "resource/subresource" with.
func upstreamResourceMatches(ruleResources []string, combinedRequestedResource, requestedSubresource string) bool {
	for _, ruleResource := range ruleResources {
		if ruleResource == "*" {
			return true
		}
		if ruleResource == combinedRequestedResource {
			return true
		}
		if len(requestedSubresource) == 0 {
			continue
		}
		if len(ruleResource) == len(requestedSubresource)+2 &&
			strings.HasPrefix(ruleResource, "*/") &&
			strings.HasSuffix(ruleResource, requestedSubresource) {
			return true
		}
	}
	return false
}

// TestResourceMatchesUpstream pins resourceMatches to the RBAC authorizer's
// matching over every pair of rule entry and requested resource of a set
// that holds each shape (exact, "*", "*/sub", "resource/*", other
// resources and subresources): a namespace kept by the start-up check is
// one whose writes RBAC allows, never one that would 403 them.
func TestResourceMatchesUpstream(t *testing.T) {
	entries := []string{
		"*", "*/status", "*/scale", "*/*", "pgadmins", "pgadmins/*", "pgadmins/status",
		"pgadmins/finalizers", "pgtoolboxconsoles", "pgtoolboxconsoles/*", "status", "", "pgadmins/status/x",
	}
	requested := []string{"pgadmins", "pgadmins/status", "pgadmins/finalizers", "pgadmins/scale", "pgtoolboxconsoles/status", "deployments"}
	for _, entry := range entries {
		for _, resource := range requested {
			_, sub, _ := strings.Cut(resource, "/")
			want := upstreamResourceMatches([]string{entry}, resource, sub)
			if got := resourceMatches([]string{entry}, resource); got != want {
				t.Errorf("rule resource %q, requested %q: matches = %t, RBAC says %t", entry, resource, got, want)
			}
		}
	}
	// The same over multi-entry rules: any entry matching is a match.
	for i := range entries {
		for j := range entries {
			rule := []string{entries[i], entries[j]}
			for _, resource := range requested {
				_, sub, _ := strings.Cut(resource, "/")
				if got, want := resourceMatches(rule, resource), upstreamResourceMatches(rule, resource, sub); got != want {
					t.Errorf("rule resources %q, requested %q: matches = %t, RBAC says %t", rule, resource, got, want)
				}
			}
		}
	}
}

// reviewer answers SelfSubjectRulesReviews with rules per namespace and
// SelfSubjectAccessReviews from allowed ("verb resource.group namespace").
type reviewer struct {
	rules      map[string][]authorizationv1.ResourceRule
	incomplete map[string]bool
	allowed    map[string]bool
	err        error

	mu       sync.Mutex
	accesses []string
}

func (r *reviewer) Create(_ context.Context, obj client.Object, _ ...client.CreateOption) error {
	if r.err != nil {
		return r.err
	}
	switch review := obj.(type) {
	case *authorizationv1.SelfSubjectRulesReview:
		review.Status.ResourceRules = r.rules[review.Spec.Namespace]
		review.Status.Incomplete = r.incomplete[review.Spec.Namespace]
	case *authorizationv1.SelfSubjectAccessReview:
		a := review.Spec.ResourceAttributes
		resource := a.Resource
		if a.Subresource != "" {
			resource += "/" + a.Subresource
		}
		key := a.Verb + " " + resource + "." + a.Group + " " + a.Namespace
		r.mu.Lock()
		r.accesses = append(r.accesses, key)
		r.mu.Unlock()
		review.Status.Allowed = r.allowed[key]
	default:
		return errors.New("unexpected object")
	}
	return nil
}

func TestPermittedNamespaces(t *testing.T) {
	required := []Permission{
		{Group: "postgresql.cnpg.io", Resource: "clusters", Verbs: []string{"get", "list", "watch"}},
		{Group: "apps", Resource: "deployments", Verbs: []string{"list", "watch", "patch"}},
		{Group: "pgtoolbox.fyannk.dev", Resource: "pgadmins/status", Verbs: []string{"patch"}},
	}
	full := []authorizationv1.ResourceRule{
		{APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{"clusters"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"*"}},
		{APIGroups: []string{"pgtoolbox.fyannk.dev"}, Resources: []string{"pgadmins/status"}, Verbs: []string{"patch"}},
	}
	clustersOnly := full[:1]
	noWatch := []authorizationv1.ResourceRule{
		{APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{"clusters"}, Verbs: []string{"get", "list"}},
		full[1], full[2],
	}
	c := &reviewer{rules: map[string][]authorizationv1.ResourceRule{
		"team-a": full, "team-narrowed": clustersOnly, "team-nowatch": noWatch, "team-c": full,
	}}
	review, err := PermittedNamespaces(t.Context(), c, required, []string{"team-a", "team-narrowed", "team-nowatch", "team-c"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	permitted, denied := review.Permitted, review.Denied
	if !slices.Equal(permitted, []string{"team-a", "team-c"}) {
		t.Fatalf("permitted = %q, want team-a and team-c", permitted)
	}
	if len(denied) != 2 || denied[0].Namespace != "team-narrowed" || denied[1].Namespace != "team-nowatch" {
		t.Fatalf("denied = %+v", denied)
	}
	if got, want := denied[0].Reason(), "not allowed to list,watch,patch deployments.apps; patch pgadmins.pgtoolbox.fyannk.dev/status in team-narrowed"; got != want {
		t.Errorf("reason = %q, want %q", got, want)
	}
	if got, want := denied[1].Reason(), "not allowed to watch clusters.postgresql.cnpg.io in team-nowatch"; got != want {
		t.Errorf("reason = %q, want %q", got, want)
	}
	// A namespace's reason names that namespace and no other.
	for _, d := range denied {
		for _, other := range []string{"team-a", "team-narrowed", "team-nowatch", "team-c"} {
			if other != d.Namespace && strings.Contains(d.Reason(), other) {
				t.Errorf("the reason of %s names %s: %q", d.Namespace, other, d.Reason())
			}
		}
	}
	if len(c.accesses) != 0 {
		t.Errorf("complete rules reviews asked access reviews: %q", c.accesses)
	}
}

// TestPermittedNamespacesIncompleteRules: an authorizer that cannot list
// its rules (a webhook) makes the rules review incomplete; what it does not
// show is asked verb by verb, and that answer decides.
func TestPermittedNamespacesIncompleteRules(t *testing.T) {
	required := []Permission{{Group: "apps", Resource: "deployments", Verbs: []string{"list", "watch"}}}
	c := &reviewer{
		rules:      map[string][]authorizationv1.ResourceRule{"team-a": {{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"list"}}}},
		incomplete: map[string]bool{"team-a": true, "team-b": true},
		allowed:    map[string]bool{"watch deployments.apps team-a": true},
	}
	review, err := PermittedNamespaces(t.Context(), c, required, []string{"team-a", "team-b"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	permitted, denied := review.Permitted, review.Denied
	if !slices.Equal(permitted, []string{"team-a"}) || len(denied) != 1 || denied[0].Namespace != "team-b" {
		t.Fatalf("permitted %q, denied %+v; want team-a kept by the access review, team-b refused", permitted, denied)
	}
	slices.Sort(c.accesses)
	if want := []string{"list deployments.apps team-b", "watch deployments.apps team-a", "watch deployments.apps team-b"}; !slices.Equal(c.accesses, want) {
		t.Errorf("access reviews %q, want %q", c.accesses, want)
	}
}

// TestPermittedNamespacesError: a review that cannot be created leaves
// that namespace out as unreviewed, naming it; it never keeps it by guess,
// and a permanent error is not retried.
func TestPermittedNamespacesError(t *testing.T) {
	c := &reviewer{err: errors.New("connection refused")}
	review, err := PermittedNamespaces(t.Context(), c, []Permission{{Resource: "pods", Verbs: []string{"list"}}}, []string{"team-a"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(review.Permitted) != 0 || len(review.Denied) != 0 || len(review.Unreviewed) != 1 || review.Unreviewed[0].Namespace != "team-a" {
		t.Fatalf("review = %+v, want team-a unreviewed only", review)
	}
	if reason := review.Unreviewed[0].Reason(); !strings.Contains(reason, "team-a") || !strings.Contains(reason, "connection refused") {
		t.Errorf("reason = %q, want the namespace and the error named", reason)
	}
}

// flakyReviewer answers rules reviews with full rules, except: in hang it
// never answers (until its context ends, or ever when ignoreContext), and
// in flaky it fails its first fails reviews with err.
type flakyReviewer struct {
	rules         []authorizationv1.ResourceRule
	hang          string
	ignoreContext bool
	flaky         string
	fails         int
	err           error

	mu    sync.Mutex
	calls map[string]int
}

func (r *flakyReviewer) callsIn(namespace string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[namespace]
}

func (r *flakyReviewer) Create(ctx context.Context, obj client.Object, _ ...client.CreateOption) error {
	review, ok := obj.(*authorizationv1.SelfSubjectRulesReview)
	if !ok {
		return errors.New("unexpected object")
	}
	namespace := review.Spec.Namespace
	r.mu.Lock()
	if r.calls == nil {
		r.calls = map[string]int{}
	}
	r.calls[namespace]++
	call := r.calls[namespace]
	r.mu.Unlock()
	switch {
	case namespace == r.hang && r.ignoreContext:
		select {} //nolint:revive // a request that never returns, whatever its context
	case namespace == r.hang:
		<-ctx.Done()
		return ctx.Err()
	case namespace == r.flaky && call <= r.fails:
		return r.err
	}
	review.Status.ResourceRules = r.rules
	return nil
}

// TestPermittedNamespacesOneHangs: round 15, K8S P3-4. One namespace's
// review never answers (a response grown huge, a stuck request); it is
// left out on its own, past its deadline, while the others are kept, and
// the start is not failed.
func TestPermittedNamespacesOneHangs(t *testing.T) {
	required := []Permission{{Group: "apps", Resource: "deployments", Verbs: []string{"list"}}}
	rules := []authorizationv1.ResourceRule{{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"list"}}}
	for _, ignoreContext := range []bool{false, true} {
		c := &flakyReviewer{rules: rules, hang: "team-stuck", ignoreContext: ignoreContext}
		start := time.Now()
		review, err := PermittedNamespaces(t.Context(), c, required, []string{"team-a", "team-stuck", "team-b"}, 50*time.Millisecond)
		if err != nil {
			t.Fatalf("one namespace hanging failed the check: %v", err)
		}
		if !slices.Equal(review.Permitted, []string{"team-a", "team-b"}) || len(review.Unreviewed) != 1 || review.Unreviewed[0].Namespace != "team-stuck" {
			t.Fatalf("review = %+v, want team-a and team-b kept, team-stuck unreviewed", review)
		}
		if !errors.Is(review.Unreviewed[0].Err, context.DeadlineExceeded) {
			t.Errorf("team-stuck's error = %v, want its deadline", review.Unreviewed[0].Err)
		}
		if calls := c.callsIn("team-stuck"); calls != 2 {
			t.Errorf("team-stuck reviewed %d times, want 2 (no answer in time is retried once)", calls)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("the check took %s; each attempt is bounded by its deadline", elapsed)
		}
	}
}

// TestPermittedNamespacesTransientRetried: round 15, GO plan. A 5xx (or a
// throttled request) is retried once: one failure then an answer keeps the
// namespace; two failures leave it out, alone. A permanent error is not
// retried.
func TestPermittedNamespacesTransientRetried(t *testing.T) {
	required := []Permission{{Group: "apps", Resource: "deployments", Verbs: []string{"list"}}}
	rules := []authorizationv1.ResourceRule{{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"list"}}}
	for _, tt := range []struct {
		name          string
		err           error
		fails         int
		wantPermitted []string
		wantCalls     int
	}{
		{"one 5xx then an answer", apierrors.NewInternalError(errors.New("etcd leader changed")), 1, []string{"team-a", "team-flaky"}, 2},
		{"one 503 then an answer", apierrors.NewServiceUnavailable("apiserver shutting down"), 1, []string{"team-a", "team-flaky"}, 2},
		{"one 429 then an answer", apierrors.NewTooManyRequests("slow down", 1), 1, []string{"team-a", "team-flaky"}, 2},
		{"two 5xx", apierrors.NewInternalError(errors.New("etcd down")), 2, []string{"team-a"}, 2},
		{"a permanent error", apierrors.NewForbidden(authorizationv1.Resource("selfsubjectrulesreviews"), "", errors.New("no")), 1, []string{"team-a"}, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := &flakyReviewer{rules: rules, flaky: "team-flaky", fails: tt.fails, err: tt.err}
			review, err := PermittedNamespaces(t.Context(), c, required, []string{"team-a", "team-flaky"}, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(review.Permitted, tt.wantPermitted) {
				t.Fatalf("permitted = %q, want %q (%+v)", review.Permitted, tt.wantPermitted, review)
			}
			if calls := c.callsIn("team-flaky"); calls != tt.wantCalls {
				t.Errorf("team-flaky reviewed %d times, want %d", calls, tt.wantCalls)
			}
		})
	}
}

// TestPermittedNamespacesCancelled: the caller's own context ending is an
// error (the start-up step ran out), never a namespace left out.
func TestPermittedNamespacesCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c := &flakyReviewer{hang: "team-a"}
	if _, err := PermittedNamespaces(ctx, c, []Permission{{Resource: "pods", Verbs: []string{"list"}}}, []string{"team-a"}, time.Second); err == nil {
		t.Fatal("a cancelled check returned no error")
	}
}

// TestPermittedNamespacesAccessReviewsConcurrent: round 15, GO plan. The
// access reviews an incomplete rules review needs in one namespace run
// concurrently, at most accessReviewConcurrency at a time.
func TestPermittedNamespacesAccessReviewsConcurrent(t *testing.T) {
	required := make([]Permission, 0, 4)
	for _, resource := range []string{"deployments", "statefulsets", "replicasets", "daemonsets"} {
		required = append(required, Permission{Group: "apps", Resource: resource, Verbs: []string{"get", "list", "watch"}})
	}
	c := &concurrentReviewer{release: make(chan struct{})}
	done := make(chan Review, 1)
	go func() {
		review, err := PermittedNamespaces(t.Context(), c, required, []string{"team-a"}, 5*time.Second)
		if err != nil {
			t.Error(err)
		}
		done <- review
	}()
	deadline := time.After(2 * time.Second)
	for c.inFlight() < accessReviewConcurrency {
		select {
		case <-deadline:
			t.Fatalf("%d access reviews in flight at once, want %d", c.inFlight(), accessReviewConcurrency)
		case <-time.After(time.Millisecond):
		}
	}
	close(c.release)
	review := <-done
	if !slices.Equal(review.Permitted, []string{"team-a"}) {
		t.Fatalf("review = %+v, want team-a kept by its access reviews", review)
	}
	c.mu.Lock()
	peak := c.peak
	c.mu.Unlock()
	if peak > accessReviewConcurrency {
		t.Errorf("%d access reviews in flight at once, want at most %d", peak, accessReviewConcurrency)
	}
}

// concurrentReviewer reports incomplete rules reviews and allows every
// access review once release is closed, counting those in flight.
type concurrentReviewer struct {
	release chan struct{}

	mu      sync.Mutex
	current int
	peak    int
}

func (r *concurrentReviewer) inFlight() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current
}

func (r *concurrentReviewer) Create(ctx context.Context, obj client.Object, _ ...client.CreateOption) error {
	switch review := obj.(type) {
	case *authorizationv1.SelfSubjectRulesReview:
		review.Status.Incomplete = true
		return nil
	case *authorizationv1.SelfSubjectAccessReview:
		r.mu.Lock()
		r.current++
		r.peak = max(r.peak, r.current)
		r.mu.Unlock()
		defer func() {
			r.mu.Lock()
			r.current--
			r.mu.Unlock()
		}()
		select {
		case <-r.release:
		case <-ctx.Done():
			return ctx.Err()
		}
		review.Status.Allowed = true
		return nil
	}
	return errors.New("unexpected object")
}

func TestPermissionString(t *testing.T) {
	for _, tt := range []struct {
		p    Permission
		want string
	}{
		{Permission{Resource: "secrets", Verbs: []string{"get", "list"}}, "get,list secrets"},
		{Permission{Group: "apps", Resource: "deployments", Verbs: []string{"watch"}}, "watch deployments.apps"},
		{Permission{Group: "pgtoolbox.fyannk.dev", Resource: "pgadmins/status", Verbs: []string{"patch"}}, "patch pgadmins.pgtoolbox.fyannk.dev/status"},
	} {
		if got := tt.p.String(); got != tt.want {
			t.Errorf("%+v = %q, want %q", tt.p, got, tt.want)
		}
	}
}
