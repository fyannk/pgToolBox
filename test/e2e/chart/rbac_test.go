//go:build e2echart

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

package chart

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
)

// attr is one question put to the API server's authorizer: may this subject do
// verb on group/resource[/subresource] [named name] in ns ("" is cluster-wide)?
type attr struct {
	ns, group, resource, sub, name, verb string
}

func (a attr) String() string {
	r := a.resource
	if a.sub != "" {
		r += "/" + a.sub
	}
	if a.group != "" {
		r += "." + a.group
	}
	where := "cluster-wide"
	if a.ns != "" {
		where = "ns " + a.ns
	}
	if a.name != "" {
		r += "/" + a.name
	}
	return fmt.Sprintf("%s %s (%s)", a.verb, r, where)
}

// readVerbs are the only verbs the console holds, anywhere.
var readVerbs = []string{"get", "list", "watch"}

// allVerbs is every verb the matrix asks about on every resource.
var allVerbs = []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"}

// wantNamespaced is the independent statement of intent the API
// server's answers are compared with: what a console may do in a namespace it
// watches. It is deliberately not generated from the chart. Anything else, in
// any namespace, is denied.
var wantNamespaced = []struct {
	group     string
	resources []string
	verbs     []string
}{
	{"postgresql.cnpg.io", []string{"clusters", "backups", "scheduledbackups", "poolers", "databases", "databaseroles", "publications", "subscriptions", "failoverquorums", "imagecatalogs"}, readVerbs},
	{"barmancloud.cnpg.io", []string{"objectstores"}, readVerbs},
	// Nothing on pgtoolbox.fyannk.dev: this cluster serves the CNPG kinds only,
	// and console.rbac.operatorKinds: auto grants the operator's kinds only
	// where they are served (a namespace admin could not grant them otherwise).
	{"", []string{"pods", "persistentvolumeclaims", "services"}, readVerbs},
	{"", []string{"events", "resourcequotas"}, []string{"list", "watch"}},
	{"", []string{"pods/log"}, []string{"get"}},
	{"policy", []string{"poddisruptionbudgets"}, readVerbs},
	{"coordination.k8s.io", []string{"leases"}, readVerbs},
	{"batch", []string{"jobs"}, readVerbs},
	{"apps", []string{"replicasets", "deployments"}, []string{"get"}},
}

func namespacedKey(group, resource, sub, verb string) string {
	return group + "|" + resource + "|" + sub + "|" + verb
}

var wantNamespacedSet = func() map[string]bool {
	set := map[string]bool{}
	for _, w := range wantNamespaced {
		for _, r := range w.resources {
			res, sub, _ := strings.Cut(r, "/")
			for _, v := range w.verbs {
				set[namespacedKey(w.group, res, sub, v)] = true
			}
		}
	}
	return set
}()

// basicUser is what Kubernetes grants every authenticated subject through the
// system:basic-user ClusterRole. The strict pass evaluates the ServiceAccount
// with no groups, so none of it applies; the effective pass adds it back.
var basicUser = map[string]bool{
	"authentication.k8s.io|selfsubjectreviews||create":      true,
	"authorization.k8s.io|selfsubjectaccessreviews||create": true,
	"authorization.k8s.io|selfsubjectrulesreviews||create":  true,
}

// expected is what the console is meant to be allowed, for one attribute.
func (c console) expected(a attr, effective bool) bool {
	cluster := a.ns == ""
	switch {
	// The opt-in: ClusterImageCatalog, read-only.
	case cluster && c.CatalogOpt && a.group == "postgresql.cnpg.io" && a.resource == "clusterimagecatalogs" && a.sub == "" && a.name == "" &&
		(a.verb == "get" || a.verb == "list" || a.verb == "watch"):
		return true
	// The revocation list: one ConfigMap, by name, in the release namespace.
	case a.ns == c.Namespace && a.group == "" && a.resource == "configmaps" && a.sub == "" && a.name == c.revocations() &&
		(a.verb == "get" || a.verb == "watch"):
		return true
	case effective && cluster && basicUser[namespacedKey(a.group, a.resource, a.sub, a.verb)]:
		return true
	}
	if a.name == "" && a.ns != "" && contains(c.watched(), a.ns) {
		return wantNamespacedSet[namespacedKey(a.group, a.resource, a.sub, a.verb)]
	}
	return false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// apiResource is one entry of API discovery.
type apiResource struct {
	group, resource, sub string
	namespaced           bool
}

// discover lists every resource and subresource the API server serves,
// including the CNPG CRDs the e2e installed.
func discover(t *testing.T) []apiResource {
	t.Helper()
	lists, err := clientset.Discovery().ServerPreferredResources()
	if err != nil && !discovery.IsGroupDiscoveryFailedError(err) {
		t.Fatalf("API discovery: %v", err)
	}
	seen := map[apiResource]bool{}
	var out []apiResource
	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			t.Fatalf("parse %q: %v", list.GroupVersion, err)
		}
		for _, r := range list.APIResources {
			res, sub, _ := strings.Cut(r.Name, "/")
			ar := apiResource{group: gv.Group, resource: res, sub: sub, namespaced: r.Namespaced}
			if !seen[ar] {
				seen[ar] = true
				out = append(out, ar)
			}
		}
	}
	if len(out) < 50 {
		t.Fatalf("API discovery returned only %d resources; the matrix would prove little", len(out))
	}
	return out
}

// matrix is every question the proof asks for one console: all resources and
// subresources the API server serves, with every verb, cluster-wide, in the
// namespaces it watches, in namespaces it does not, plus the named and exotic
// cases that discovery cannot enumerate.
func (c console) matrix(resources []apiResource, unwatched []string, effective bool) []attr {
	var out []attr
	add := func(ns string, r apiResource) {
		for _, v := range allVerbs {
			out = append(out, attr{ns: ns, group: r.group, resource: r.resource, sub: r.sub, verb: v})
		}
	}
	namespaces := append(append([]string{""}, c.watched()...), unwatched...)
	if effective {
		namespaces = append([]string{""}, c.watched()...)
	}
	for _, r := range resources {
		if !r.namespaced {
			add("", r)
			continue
		}
		for _, ns := range namespaces {
			add(ns, r)
		}
	}
	// The operator's kinds, which this cluster does not serve: a
	// SubjectAccessReview does not need them to exist, so asking proves the
	// Role left them out (console.rbac.operatorKinds: auto).
	for _, ns := range c.watched() {
		for _, res := range []string{"pgadmins", "evidenceagents", "pgaccessrequests"} {
			for _, v := range allVerbs {
				out = append(out, attr{ns: ns, group: "pgtoolbox.fyannk.dev", resource: res, verb: v})
			}
		}
	}
	// The revocation list: allowed by name, denied unnamed and under any other name.
	for _, name := range []string{c.revocations(), "", "pgtoolbox-console-instance-ca", "kube-root-ca.crt"} {
		for _, v := range allVerbs {
			out = append(out, attr{ns: c.Namespace, resource: "configmaps", name: name, verb: v})
		}
	}
	// Verbs that are not CRUD: impersonation, privilege escalation, binding.
	for _, x := range []struct{ group, resource, verb string }{
		{"", "users", "impersonate"},
		{"", "groups", "impersonate"},
		{"", "serviceaccounts", "impersonate"},
		{"", "uids", "impersonate"},
		{"authentication.k8s.io", "userextras/scopes", "impersonate"},
		{"rbac.authorization.k8s.io", "roles", "escalate"},
		{"rbac.authorization.k8s.io", "roles", "bind"},
		{"rbac.authorization.k8s.io", "clusterroles", "escalate"},
		{"rbac.authorization.k8s.io", "clusterroles", "bind"},
		{"certificates.k8s.io", "signers", "approve"},
		{"certificates.k8s.io", "signers", "sign"},
		{"", "pods/exec", "create"},
		{"", "pods/portforward", "create"},
		{"", "pods/proxy", "get"},
		{"", "nodes/proxy", "get"},
		{"", "secrets", "get"},
		{"", "secrets", "list"},
		{"", "secrets", "watch"},
	} {
		res, sub, _ := strings.Cut(x.resource, "/")
		out = append(out, attr{group: x.group, resource: res, sub: sub, verb: x.verb})
		for _, ns := range append(append([]string{}, c.watched()...), unwatched...) {
			out = append(out, attr{ns: ns, group: x.group, resource: res, sub: sub, verb: x.verb})
		}
	}
	return out
}

// ask puts attrs to the authorizer in parallel and returns the allowed set.
func ask(t *testing.T, user string, groups []string, attrs []attr) map[attr]bool {
	t.Helper()
	allowed := make(map[attr]bool, 64)
	var mu sync.Mutex
	var firstErr error
	parallel(len(attrs), 48, func(i int) {
		a := attrs[i]
		review := &authorizationv1.SubjectAccessReview{
			Spec: authorizationv1.SubjectAccessReviewSpec{
				User:   user,
				Groups: groups,
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Namespace: a.ns, Verb: a.verb, Group: a.group,
					Resource: a.resource, Subresource: a.sub, Name: a.name,
				},
			},
		}
		resp, err := clientset.AuthorizationV1().SubjectAccessReviews().Create(ctx(), review, metav1.CreateOptions{})
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", a, err)
			}
			return
		}
		if resp.Status.Allowed {
			allowed[a] = true
		}
	})
	if firstErr != nil {
		t.Fatalf("SubjectAccessReview: %v", firstErr)
	}
	return allowed
}

// proveScope puts the matrix to the API server as the console's ServiceAccount
// and compares every answer with the intent in expected. Two passes:
//
//   - strict: the subject alone, no groups, so only bindings that name the
//     ServiceAccount apply. What the chart grants, exactly.
//   - effective: with the groups every ServiceAccount carries, so the cluster's
//     default bindings apply as well. Nothing beyond system:basic-user may appear.
func (c console) proveScope(t *testing.T, resources []apiResource, unwatched []string) {
	t.Helper()
	passes := []struct {
		name      string
		groups    []string
		effective bool
	}{
		{"strict (the ServiceAccount alone)", nil, false},
		{"effective (with the default groups)", []string{"system:serviceaccounts", "system:serviceaccounts:" + c.Namespace, "system:authenticated"}, true},
	}
	for _, pass := range passes {
		attrs := c.matrix(resources, unwatched, pass.effective)
		allowed := ask(t, c.user(), pass.groups, attrs)

		var unexpected, missing []string
		cluster := map[string]bool{}
		perNS := map[string]int{}
		for _, a := range attrs {
			want := c.expected(a, pass.effective)
			got := allowed[a]
			if got && !want {
				unexpected = append(unexpected, a.String())
			}
			if want && !got {
				missing = append(missing, a.String())
			}
			if got && a.ns == "" {
				cluster[a.String()] = true
			}
			if got && a.ns != "" {
				perNS[a.ns]++
			}
		}
		sort.Strings(unexpected)
		sort.Strings(missing)
		t.Logf("%s, %s: %d questions; allowed cluster-wide: %v; allowed per namespace: %v",
			c.user(), pass.name, len(attrs), slices.Sorted(maps.Keys(cluster)), perNS)
		if len(unexpected) > 0 {
			t.Errorf("%s (%s) holds %d permission(s) it is not meant to:\n  %s", c.user(), pass.name, len(unexpected), strings.Join(first(unexpected, 40), "\n  "))
		}
		if len(missing) > 0 {
			t.Errorf("%s (%s) lacks %d permission(s) it is meant to hold:\n  %s", c.user(), pass.name, len(missing), strings.Join(first(missing, 40), "\n  "))
		}
	}
}

func first(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(append([]string{}, s[:n]...), fmt.Sprintf("... and %d more", len(s)-n))
}
