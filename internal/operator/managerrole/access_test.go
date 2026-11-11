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

	authorizationv1 "k8s.io/api/authorization/v1"
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
		{"resource/*", []authorizationv1.ResourceRule{rule(apps, []string{"deployments/*"}, []string{"update"})}, "apps", "deployments/status", "update", true},
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
	permitted, denied, err := PermittedNamespaces(t.Context(), c, required, []string{"team-a", "team-narrowed", "team-nowatch", "team-c"})
	if err != nil {
		t.Fatal(err)
	}
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
	permitted, denied, err := PermittedNamespaces(t.Context(), c, required, []string{"team-a", "team-b"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(permitted, []string{"team-a"}) || len(denied) != 1 || denied[0].Namespace != "team-b" {
		t.Fatalf("permitted %q, denied %+v; want team-a kept by the access review, team-b refused", permitted, denied)
	}
	slices.Sort(c.accesses)
	if want := []string{"list deployments.apps team-b", "watch deployments.apps team-a", "watch deployments.apps team-b"}; !slices.Equal(c.accesses, want) {
		t.Errorf("access reviews %q, want %q", c.accesses, want)
	}
}

// TestPermittedNamespacesError: a review that cannot be created fails the
// check, naming the namespace; it never keeps or drops one by guess.
func TestPermittedNamespacesError(t *testing.T) {
	c := &reviewer{err: errors.New("connection refused")}
	_, _, err := PermittedNamespaces(t.Context(), c, []Permission{{Resource: "pods", Verbs: []string{"list"}}}, []string{"team-a"})
	if err == nil || !strings.Contains(err.Error(), "team-a") {
		t.Fatalf("err = %v, want the failed review named", err)
	}
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
