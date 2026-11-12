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
	"slices"
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	routev1 "github.com/openshift/api/route/v1"
	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/configuration"
	"github.com/fyannk/pgToolBox/internal/operator/managerrole"
)

// testMapper serves a few kinds, each with its scope, and no others.
func testMapper() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range []schema.GroupVersionKind{
		appsv1.SchemeGroupVersion.WithKind("Deployment"),
		corev1.SchemeGroupVersion.WithKind("Secret"),
		corev1.SchemeGroupVersion.WithKind("Pod"),
		cnpgv1.SchemeGroupVersion.WithKind("Cluster"),
		cnpgv1.SchemeGroupVersion.WithKind("Pooler"),
		routev1.GroupVersion.WithKind("Route"),
		pgtoolboxv1alpha1.GroupVersion.WithKind("PgAdmin"),
		{Group: "discovery.k8s.io", Version: "v1", Kind: "EndpointSlice"},
	} {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Node"), meta.RESTScopeRoot)
	mapper.Add(rbacv1.SchemeGroupVersion.WithKind("ClusterRole"), meta.RESTScopeRoot)
	return mapper
}

func TestRequiredPermissions(t *testing.T) {
	rules, err := managerrole.Rules()
	if err != nil {
		t.Fatal(err)
	}
	required, err := requiredPermissions(testMapper(), scheme, rules, requiredKinds(), optionalGroupKinds())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range required {
		got[p.Group+"/"+p.Resource] = strings.Join(p.Verbs, ",")
	}
	all := "create,delete,get,list,patch,update,watch"
	want := map[string]string{
		"apps/deployments":                         all,
		"/secrets":                                 all,
		"/pods":                                    "get,list,watch",
		"postgresql.cnpg.io/clusters":              "get,list,watch",
		"pgtoolbox.fyannk.dev/pgadmins":            "get,list,patch,update,watch",
		"pgtoolbox.fyannk.dev/pgadmins/status":     "get,patch,update",
		"pgtoolbox.fyannk.dev/pgadmins/finalizers": "update",
	}
	for key, verbs := range want {
		if got[key] != verbs {
			t.Errorf("%s: verbs %q, want %q", key, got[key], verbs)
		}
	}
	// Not asked: cluster-scoped (nodes, clusterroles), optional (poolers,
	// routes and their custom-host subresource), named objects only
	// (endpointslices/kubernetes) and kinds the cluster does not serve.
	for _, key := range []string{
		"/nodes", "rbac.authorization.k8s.io/clusterroles", "postgresql.cnpg.io/poolers",
		"route.openshift.io/routes", "route.openshift.io/routes/custom-host", "discovery.k8s.io/endpointslices",
		"apps/statefulsets", "batch/jobs",
	} {
		if verbs, asked := got[key]; asked {
			t.Errorf("%s asked (%s), want it left out", key, verbs)
		}
	}
	if !slices.IsSortedFunc(required, func(a, b managerrole.Permission) int {
		return strings.Compare(a.Group+"/"+a.Resource, b.Group+"/"+b.Resource)
	}) {
		t.Error("the permissions are not sorted")
	}
}

// TestRequiredPermissionsCoversWatchedKinds: a kind an informer holds is
// asked list and watch even when no rule of the role names it (a watch
// added without its RBAC marker is checked anyway).
func TestRequiredPermissionsCoversWatchedKinds(t *testing.T) {
	required, err := requiredPermissions(testMapper(), scheme, nil, []client.Object{&appsv1.Deployment{}, &corev1.Node{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(required) != 1 || required[0].String() != "list,watch deployments.apps" {
		t.Fatalf("required = %v, want list,watch on deployments only (nodes are cluster-scoped)", required)
	}
}

// rulesClient is a fake client whose SelfSubjectRulesReview answers rules
// per namespace (every other create is refused), with testMapper.
func rulesClient(rules map[string][]authorizationv1.ResourceRule) client.Client {
	return fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(testMapper()).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			review, ok := obj.(*authorizationv1.SelfSubjectRulesReview)
			if !ok {
				return errors.New("only rules reviews are expected")
			}
			review.Status.ResourceRules = rules[review.Spec.Namespace]
			return nil
		},
	}).Build()
}

// managerRules is the manager role as a rules review shows it.
func managerRules(t *testing.T) []authorizationv1.ResourceRule {
	t.Helper()
	rules, err := managerrole.Rules()
	if err != nil {
		t.Fatal(err)
	}
	out := make([]authorizationv1.ResourceRule, len(rules))
	for i, r := range rules {
		out[i] = authorizationv1.ResourceRule{Verbs: r.Verbs, APIGroups: r.APIGroups, Resources: r.Resources, ResourceNames: r.ResourceNames}
	}
	return out
}

// TestPrepareDropsNamespaceMissingARequiredPermission: round 14, K8s P2-1.
// team-b's administrator bound the operator to a Role that keeps Clusters
// and drops Deployments; team-c's lets it list Clusters but not watch them.
// Both are left out (an informer refused there would never sync and the
// operator would crash-loop for every namespace), counted in
// pgtoolbox_watch_namespaces_dropped; team-a is watched. A namespace that
// refuses only an optional kind (team-d, no Poolers) stays watched.
func TestPrepareDropsNamespaceMissingARequiredPermission(t *testing.T) {
	t.Setenv("WATCH_NAMESPACES", "team-a,team-b,team-c,team-d")
	t.Setenv("OPERATOR_IMAGE", "registry.example.com/pgtoolbox/operator:test")
	full := managerRules(t)
	noPoolers := make([]authorizationv1.ResourceRule, 0, len(full))
	for _, r := range full {
		if slices.Contains(r.Resources, "poolers") {
			r.Resources = []string{"clusters"}
		}
		noPoolers = append(noPoolers, r)
	}
	clustersRead := authorizationv1.ResourceRule{APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{"clusters"}, Verbs: []string{"get", "list", "watch"}}
	noWatch := make([]authorizationv1.ResourceRule, 0, len(full))
	for _, r := range full {
		if slices.Contains(r.Resources, "clusters") {
			r = authorizationv1.ResourceRule{APIGroups: r.APIGroups, Resources: []string{"clusters"}, Verbs: []string{"get", "list"}}
		}
		noWatch = append(noWatch, r)
	}
	registry := prometheus.NewRegistry()
	prepared, err := preparePgAdminWith(t.Context(), rulesClient(map[string][]authorizationv1.ResourceRule{
		"team-a": full, "team-b": {clustersRead}, "team-c": noWatch, "team-d": noPoolers,
	}), registry, newStartupBudget(time.Minute, time.Minute),
		configuration.Objects{Namespace: "pgtoolbox", ConfigMap: "pgtoolbox-operator-config", Secret: "pgtoolbox-operator-config"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(prepared.Config.WatchNamespaces, []string{"team-a", "team-d"}) {
		t.Fatalf("watched = %q, want [team-a team-d]", prepared.Config.WatchNamespaces)
	}
	if got := gatherGauges(t, registry)[`pgtoolbox_watch_namespaces_dropped{reason="permissions"}`]; got != 2 {
		t.Fatalf("pgtoolbox_watch_namespaces_dropped{reason=permissions} = %v, want 2 (team-b, team-c)", got)
	}
	if len(prepared.Dropped) != 2 || prepared.Dropped[0].Namespace != "team-b" || prepared.Dropped[1].Namespace != "team-c" {
		t.Fatalf("dropped = %+v, want team-b and team-c", prepared.Dropped)
	}

	// Every namespace lacking a permission fails the start, naming the
	// watched namespaces.
	_, err = preparePgAdminWith(t.Context(), rulesClient(map[string][]authorizationv1.ResourceRule{
		"team-a": {clustersRead}, "team-b": {clustersRead}, "team-c": noWatch, "team-d": {clustersRead},
	}), prometheus.NewRegistry(), newStartupBudget(time.Minute, time.Minute),
		configuration.Objects{Namespace: "pgtoolbox", ConfigMap: "pgtoolbox-operator-config", Secret: "pgtoolbox-operator-config"})
	if err == nil || !strings.Contains(err.Error(), "none of the watched namespaces") {
		t.Fatalf("err = %v, want the start failed with every namespace named", err)
	}
}

// TestPermittedNamespacesReviewFailedKeepsWhatToAsk: round 16, K8S P2-2,
// and rounds 17 and 18. A namespace whose review failed is dropped with
// the permissions to ask again in the background (reReviewFailed); one
// whose response was too large, or that got no answer in time at every
// attempt, is dropped under a cause of its own (review-too-large,
// review-too-slow), with nothing to ask again: only the next start reviews
// it. Each fails that namespace only.
func TestPermittedNamespacesReviewFailedKeepsWhatToAsk(t *testing.T) {
	full := managerRules(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(testMapper()).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			review, ok := obj.(*authorizationv1.SelfSubjectRulesReview)
			if !ok {
				return errors.New("only rules reviews are expected")
			}
			switch review.Spec.Namespace {
			case "team-b":
				return apierrors.NewForbidden(schema.GroupResource{Group: "authorization.k8s.io", Resource: "selfsubjectrulesreviews"}, "", errors.New("an admission webhook refused it"))
			case "team-c":
				return managerrole.ErrReviewTooLarge
			case "team-d":
				// No answer before the attempt's deadline, every time.
				<-ctx.Done()
				return ctx.Err()
			}
			review.Status.ResourceRules = full
			return nil
		},
	}).Build()
	timeout := namespaceReviewTimeout
	namespaceReviewTimeout = 50 * time.Millisecond
	t.Cleanup(func() { namespaceReviewTimeout = timeout })
	kept, dropped, err := permittedNamespaces(t.Context(), c, []string{"team-a", "team-b", "team-c", "team-d"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(kept, []string{"team-a"}) || len(dropped) != 3 ||
		dropped[0].Namespace != "team-b" || dropped[0].Cause != dropReviewFailed ||
		dropped[1].Namespace != "team-c" || dropped[1].Cause != dropReviewTooLarge ||
		dropped[2].Namespace != "team-d" || dropped[2].Cause != dropReviewTooSlow {
		t.Fatalf("kept %q, dropped %+v; want team-a kept, team-b review-failed, team-c review-too-large, team-d review-too-slow", kept, dropped)
	}
	if len(dropped[0].required) == 0 {
		t.Error("team-b dropped without the permissions to review again")
	}
	for _, d := range dropped[1:] {
		if len(d.required) != 0 {
			t.Errorf("%s, %s, dropped with permissions to review again in the background", d.Namespace, d.Cause)
		}
	}
	for _, d := range dropped {
		if d.allows("events.k8s.io", "events", "create") {
			t.Errorf("%s, whose review failed, reads as allowing a write", d.Namespace)
		}
	}
}

// TestPermittedNamespacesClusterWide: with no watched namespaces (every
// namespace) nothing is reviewed and nothing is dropped.
func TestPermittedNamespacesClusterWide(t *testing.T) {
	got, dropped, err := permittedNamespaces(t.Context(), rulesClient(nil), nil)
	if err != nil || got != nil || dropped != nil {
		t.Fatalf("permittedNamespaces(every namespace) = %q, %+v, %v", got, dropped, err)
	}
}

func TestCacheSyncHint(t *testing.T) {
	synced := errors.New("failed to wait for pgadmin caches to sync kind source: *v1.Deployment: timed out waiting for cache to be synced for Kind *v1.Deployment")
	if hint := cacheSyncHint(synced, []string{"team-a", "team-b"}); !strings.Contains(hint, "[team-a team-b]") || !strings.Contains(hint, "RoleBinding") {
		t.Errorf("hint = %q, want the watched namespaces and the RoleBinding named", hint)
	}
	if hint := cacheSyncHint(synced, nil); !strings.Contains(hint, "ClusterRoleBinding") {
		t.Errorf("cluster-wide hint = %q", hint)
	}
	if hint := cacheSyncHint(errors.New("leader election lost"), []string{"team-a"}); hint != "" {
		t.Errorf("hint for another error = %q, want none", hint)
	}
}
