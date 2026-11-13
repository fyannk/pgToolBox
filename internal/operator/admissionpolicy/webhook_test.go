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

package admissionpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// rulesAuthorizer answers a SubjectAccessReview as RBAC would from the
// rules each user holds in namespace; system:masters may do anything.
func rulesAuthorizer(t *testing.T, rules map[string][]rbacv1.PolicyRule) func(context.Context, authorizationv1.SubjectAccessReviewSpec) (bool, error) {
	return func(_ context.Context, spec authorizationv1.SubjectAccessReviewSpec) (bool, error) {
		a := spec.ResourceAttributes
		if a == nil || a.Namespace != namespace {
			t.Errorf("review %+v: want resource attributes in %s", spec, namespace)
			return false, nil
		}
		if slices.Contains(spec.Groups, "system:masters") {
			return true, nil
		}
		resource := a.Resource
		if a.Subresource != "" {
			resource += "/" + a.Subresource
		}
		matches := func(values []string, value string) bool {
			return slices.Contains(values, "*") || slices.Contains(values, value)
		}
		return slices.ContainsFunc(rules[spec.User], func(r rbacv1.PolicyRule) bool {
			return matches(r.APIGroups, a.Group) && matches(r.Resources, resource) && matches(r.Verbs, a.Verb) &&
				(len(r.ResourceNames) == 0 || (a.Name != "" && slices.Contains(r.ResourceNames, a.Name)))
		}), nil
	}
}

func raw(t *testing.T, object any) runtime.RawExtension {
	t.Helper()
	if object == nil {
		return runtime.RawExtension{}
	}
	b, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return runtime.RawExtension{Raw: b}
}

// TestWebhookCases runs testdata/cases.yaml, the table the CEL envtest
// runs, against the Go rules.
func TestWebhookCases(t *testing.T) {
	cases := loadCases(t)
	rules := map[string][]rbacv1.PolicyRule{}
	for name, r := range cases.Requesters {
		rules[name] = r.Rules
	}
	v := &Validator{
		ImageGroups:   []string{"system:masters"},
		IdentityUsers: cases.Values.WorkloadIdentity.AllowedUsers, IdentityGroups: cases.Values.WorkloadIdentity.AllowedGroups,
		Authorize: rulesAuthorizer(t, rules),
	}
	handler := Handler(v.CustomResource)
	for _, c := range cases.Cases {
		t.Run(c.Name, func(t *testing.T) {
			object := c.object()
			req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
				Operation: admissionv1.Create,
				Resource:  metav1.GroupVersionResource{Group: object.GroupVersionKind().Group, Version: "v1alpha1", Resource: strings.ToLower(object.GetKind()) + "s"},
				Namespace: object.GetNamespace(), Name: object.GetName(),
				UserInfo: authenticationv1.UserInfo{Username: c.Requester, Groups: append([]string{teamGroup}, cases.Requesters[c.Requester].Groups...)},
				Object:   raw(t, object.Object),
			}}
			if old := c.oldObject(); old != nil {
				req.Operation, req.OldObject = admissionv1.Update, raw(t, old.Object)
			}
			response := handler.Handle(t.Context(), req)
			switch {
			case c.Allowed && !response.Allowed:
				t.Fatalf("refused (%v), want the request admitted", response.Result)
			case !c.Allowed && (response.Allowed || !strings.Contains(response.Result.Message, refusals[c.RefusedBy])):
				t.Fatalf("response %+v, want a refusal from the %s rule (%q)", response.Result, c.RefusedBy, refusals[c.RefusedBy])
			}
		})
	}
}

// TestWebhookSecretRefsMatchPolicy: the Go list of Secret references is the
// one the chart's policy reads (coverage_test.go proves that one complete).
func TestWebhookSecretRefsMatchPolicy(t *testing.T) {
	policy := policyEntries(t)
	for resource, entries := range policy {
		var want []secretRef
		for path, e := range entries {
			want = append(want, secretRef{path: path, list: e.list, bare: e.name})
		}
		got := slices.Clone(secretRefs[resource])
		less := func(a, b secretRef) int { return strings.Compare(a.path, b.path) }
		slices.SortFunc(want, less)
		slices.SortFunc(got, less)
		if !slices.Equal(got, want) {
			t.Errorf("%s: Go reads %v, the policy %v", resource, got, want)
		}
	}
	if len(policy) != len(secretRefs) {
		t.Errorf("the policy reads %d resources, Go %d", len(policy), len(secretRefs))
	}
}

// TestWebhookClusterDelete is the restore-drill guard's rule.
func TestWebhookClusterDelete(t *testing.T) {
	const console = "system:serviceaccount:" + namespace + ":orders-console"
	drills := []rbacv1.PolicyRule{{APIGroups: []string{"pgtoolbox.fyannk.dev"}, Resources: []string{"restoredrills"}, Verbs: []string{"teardown"}}}
	everything := []rbacv1.PolicyRule{{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}}}
	sealed := map[string]any{
		"labels":      map[string]any{"pgtoolbox.fyannk.dev/drill-owner": namespace, "pgtoolbox.fyannk.dev/restore-drill-of": "orders"},
		"annotations": map[string]any{"pgtoolbox.fyannk.dev/restore-drill-seal": "x"},
	}
	without := func(field, key string) map[string]any {
		m := map[string]any{}
		for f, v := range sealed {
			inner := map[string]any{}
			for k, x := range v.(map[string]any) {
				if f != field || k != key {
					inner[k] = x
				}
			}
			m[f] = inner
		}
		return m
	}
	otherOwner := without("labels", "pgtoolbox.fyannk.dev/drill-owner")
	otherOwner["labels"].(map[string]any)["pgtoolbox.fyannk.dev/drill-owner"] = "team-b"
	for _, tc := range []struct {
		name     string
		user     string
		rules    []rbacv1.PolicyRule
		metadata map[string]any
		allowed  bool
	}{
		{"a person is not concerned", "alice", drills, nil, true},
		{"a ServiceAccount without the drills role is not concerned", console, nil, nil, true},
		{"an administrator ServiceAccount is not concerned", console, everything, nil, true},
		{"the console deletes a sealed drill", console, drills, sealed, true},
		{"the console deletes a production Cluster", console, drills, nil, false},
		{"no seal", console, drills, without("annotations", "pgtoolbox.fyannk.dev/restore-drill-seal"), false},
		{"no drill label", console, drills, without("labels", "pgtoolbox.fyannk.dev/restore-drill-of"), false},
		{"no owner label", console, drills, without("labels", "pgtoolbox.fyannk.dev/drill-owner"), false},
		{"another namespace's drill", console, drills, otherOwner, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := &Validator{Authorize: rulesAuthorizer(t, map[string][]rbacv1.PolicyRule{tc.user: tc.rules})}
			old := map[string]any{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "metadata": map[string]any{"name": "c", "namespace": namespace}}
			for k, v := range tc.metadata {
				old["metadata"].(map[string]any)[k] = v
			}
			response := Handler(v.ClusterDelete).Handle(t.Context(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
				Operation: admissionv1.Delete, Namespace: namespace, Name: "c",
				UserInfo:  authenticationv1.UserInfo{Username: tc.user},
				OldObject: raw(t, old),
			}})
			if response.Allowed != tc.allowed || (!tc.allowed && response.Result.Message != msgDrill) {
				t.Fatalf("response %+v, want allowed=%v", response.Result, tc.allowed)
			}
		})
	}
}

// TestWebhookAuthorizerError: a review that fails refuses the request.
func TestWebhookAuthorizerError(t *testing.T) {
	v := &Validator{Authorize: func(context.Context, authorizationv1.SubjectAccessReviewSpec) (bool, error) {
		return false, errors.New("the API server is unreachable")
	}}
	response := Handler(v.ClusterDelete).Handle(t.Context(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Delete, Namespace: namespace, UserInfo: authenticationv1.UserInfo{Username: "system:serviceaccount:x:y"},
	}})
	if response.Allowed || response.Result.Code != http.StatusInternalServerError {
		t.Fatalf("response %+v, want a refusal with code 500", response.Result)
	}
}
