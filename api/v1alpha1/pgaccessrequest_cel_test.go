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

package v1alpha1_test

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// TestPgAccessRequestCEL applies PgAccessRequests to a real API server and
// checks the CRD's admission rules on the spec and on status transitions.
func TestPgAccessRequestCEL(t *testing.T) {
	c := apiServer(t)
	ctx := context.Background()
	valid := func() *pgtoolboxv1alpha1.PgAccessRequest {
		return &pgtoolboxv1alpha1.PgAccessRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "jane-operator", Namespace: "default"},
			Spec: pgtoolboxv1alpha1.PgAccessRequestSpec{
				Subject:       pgtoolboxv1alpha1.AccessSubject{Kind: pgtoolboxv1alpha1.AccessSubjectUser, Name: "oidc:jane@corp.example"},
				Capability:    pgtoolboxv1alpha1.AccessCapabilityOperator,
				Justification: "INC-4211",
			},
		}
	}
	cases := []struct {
		name    string
		mutate  func(*pgtoolboxv1alpha1.PgAccessRequest)
		wantErr string
	}{
		{name: "minimal"},
		{name: "every optional field", mutate: func(r *pgtoolboxv1alpha1.PgAccessRequest) {
			r.Spec.Cluster = "orders"
			r.Spec.ExpiresAt = &metav1.Time{Time: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
		}},
		{name: "a group", mutate: func(r *pgtoolboxv1alpha1.PgAccessRequest) {
			r.Spec.Subject = pgtoolboxv1alpha1.AccessSubject{Kind: pgtoolboxv1alpha1.AccessSubjectGroup, Name: "dba"}
		}},
		{name: "a ServiceAccount", mutate: func(r *pgtoolboxv1alpha1.PgAccessRequest) {
			r.Spec.Subject = pgtoolboxv1alpha1.AccessSubject{Kind: pgtoolboxv1alpha1.AccessSubjectServiceAccount, Name: "ci-runner"}
		}},
		{name: "system:authenticated", mutate: func(r *pgtoolboxv1alpha1.PgAccessRequest) {
			r.Spec.Subject = pgtoolboxv1alpha1.AccessSubject{Kind: pgtoolboxv1alpha1.AccessSubjectGroup, Name: "system:authenticated"}
		}, wantErr: "system: users and groups cannot be granted"},
		{name: "a ServiceAccount spelled as a user", mutate: func(r *pgtoolboxv1alpha1.PgAccessRequest) {
			r.Spec.Subject.Name = "system:serviceaccount:other:admin"
		}, wantErr: "system: users and groups cannot be granted"},
		{name: "a ServiceAccount of another namespace", mutate: func(r *pgtoolboxv1alpha1.PgAccessRequest) {
			r.Spec.Subject = pgtoolboxv1alpha1.AccessSubject{Kind: pgtoolboxv1alpha1.AccessSubjectServiceAccount, Name: "other:admin"}
		}, wantErr: "ServiceAccount in the request's namespace"},
		{name: "a control character in a name", mutate: func(r *pgtoolboxv1alpha1.PgAccessRequest) { r.Spec.Subject.Name = "jane\n" }, wantErr: "spec.subject.name"},
		{name: "no justification", mutate: func(r *pgtoolboxv1alpha1.PgAccessRequest) { r.Spec.Justification = "" }, wantErr: "spec.justification"},
		{name: "a justification too long", mutate: func(r *pgtoolboxv1alpha1.PgAccessRequest) { r.Spec.Justification = strings.Repeat("x", 1025) }, wantErr: "spec.justification"},
		{name: "a cluster that is a path", mutate: func(r *pgtoolboxv1alpha1.PgAccessRequest) { r.Spec.Cluster = "other/orders" }, wantErr: "spec.cluster"},
		{name: "a name too long for a label", mutate: func(r *pgtoolboxv1alpha1.PgAccessRequest) { r.Name = strings.Repeat("a", 64) }, wantErr: "DNS-1123 label of at most 63"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := valid()
			if tc.mutate != nil {
				tc.mutate(obj)
			}
			err := c.Create(ctx, obj)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("create: %v", err)
				}
				_ = c.Delete(ctx, obj)
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				if err == nil {
					_ = c.Delete(ctx, obj)
				}
				t.Fatalf("create error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}

	for _, raw := range []map[string]any{
		{"spec": map[string]any{"subject": map[string]any{"kind": "User", "name": "jane"}, "capability": "admin", "justification": "x"}},
		{"spec": map[string]any{"subject": map[string]any{"kind": "Robot", "name": "jane"}, "capability": "viewer", "justification": "x"}},
		{"spec": map[string]any{"subject": map[string]any{"kind": "User", "name": "jane"}, "justification": "x"}},
	} {
		obj := &unstructured.Unstructured{Object: raw}
		obj.SetAPIVersion(pgtoolboxv1alpha1.GroupVersion.String())
		obj.SetKind("PgAccessRequest")
		obj.SetNamespace("default")
		obj.SetName("raw")
		if err := c.Create(ctx, obj); err == nil {
			_ = c.Delete(ctx, obj)
			t.Fatalf("accepted %v", raw)
		}
	}

	t.Run("spec is immutable", func(t *testing.T) {
		obj := valid()
		obj.Name = "immutable"
		if err := c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Delete(ctx, obj) }()
		obj.Spec.Capability = pgtoolboxv1alpha1.AccessCapabilityViewer
		if err := c.Update(ctx, obj); err == nil || !strings.Contains(err.Error(), "cannot be changed once made") {
			t.Fatalf("update = %v", err)
		}
	})

	t.Run("status transitions", func(t *testing.T) {
		obj := valid()
		obj.Name = "transitions"
		if err := c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Delete(ctx, obj) }()
		now := metav1.Now()
		steps := []struct {
			name    string
			status  pgtoolboxv1alpha1.PgAccessRequestStatus
			wantErr string
		}{
			{"pending", pgtoolboxv1alpha1.PgAccessRequestStatus{Phase: pgtoolboxv1alpha1.AccessPhasePending}, ""},
			{"approved without a decider", pgtoolboxv1alpha1.PgAccessRequestStatus{Phase: pgtoolboxv1alpha1.AccessPhaseApproved, RoleBinding: "pgaccess-transitions", ClusterRole: "pgtoolbox-console-operator"}, "records decidedBy and decidedAt"},
			{"approved without a binding", pgtoolboxv1alpha1.PgAccessRequestStatus{Phase: pgtoolboxv1alpha1.AccessPhaseApproved, DecidedBy: "bob", DecidedAt: &now}, "records its roleBinding"},
			{"denied without a note", pgtoolboxv1alpha1.PgAccessRequestStatus{Phase: pgtoolboxv1alpha1.AccessPhaseDenied, DecidedBy: "bob", DecidedAt: &now}, "records why in note"},
			{"pending to revoked", pgtoolboxv1alpha1.PgAccessRequestStatus{Phase: pgtoolboxv1alpha1.AccessPhaseRevoked, RevokedBy: "bob", RevokedAt: &now}, "phase moves only"},
			{"approved", pgtoolboxv1alpha1.PgAccessRequestStatus{Phase: pgtoolboxv1alpha1.AccessPhaseApproved, DecidedBy: "bob", DecidedAt: &now, RoleBinding: "pgaccess-transitions", ClusterRole: "pgtoolbox-console-operator"}, ""},
			{"another decider", pgtoolboxv1alpha1.PgAccessRequestStatus{Phase: pgtoolboxv1alpha1.AccessPhaseApproved, DecidedBy: "carol", DecidedAt: &now, RoleBinding: "pgaccess-transitions", ClusterRole: "pgtoolbox-console-operator"}, "decidedBy cannot change"},
			{"back to pending", pgtoolboxv1alpha1.PgAccessRequestStatus{Phase: pgtoolboxv1alpha1.AccessPhasePending, DecidedBy: "bob", DecidedAt: &now, RoleBinding: "pgaccess-transitions"}, "phase moves only"},
			{"revoked without a revoker", pgtoolboxv1alpha1.PgAccessRequestStatus{Phase: pgtoolboxv1alpha1.AccessPhaseRevoked, DecidedBy: "bob", DecidedAt: &now, RoleBinding: "pgaccess-transitions", ClusterRole: "pgtoolbox-console-operator"}, "records revokedBy"},
			{"revoked", pgtoolboxv1alpha1.PgAccessRequestStatus{Phase: pgtoolboxv1alpha1.AccessPhaseRevoked, DecidedBy: "bob", DecidedAt: &now, RevokedBy: "carol", RevokedAt: &now, RoleBinding: "pgaccess-transitions", ClusterRole: "pgtoolbox-console-operator"}, ""},
			{"revoked to approved", pgtoolboxv1alpha1.PgAccessRequestStatus{Phase: pgtoolboxv1alpha1.AccessPhaseApproved, DecidedBy: "bob", DecidedAt: &now, RevokedBy: "carol", RevokedAt: &now, RoleBinding: "pgaccess-transitions", ClusterRole: "pgtoolbox-console-operator"}, "phase moves only"},
		}
		for _, step := range steps {
			current := &pgtoolboxv1alpha1.PgAccessRequest{}
			if err := c.Get(ctx, clientKey(obj), current); err != nil {
				t.Fatal(err)
			}
			current.Status = step.status
			err := c.Status().Update(ctx, current)
			switch {
			case step.wantErr == "" && err != nil:
				t.Fatalf("%s: %v", step.name, err)
			case step.wantErr != "" && (err == nil || !strings.Contains(err.Error(), step.wantErr)):
				t.Fatalf("%s: error %v, want %q", step.name, err, step.wantErr)
			}
		}
	})
}

func clientKey(obj *pgtoolboxv1alpha1.PgAccessRequest) client.ObjectKey {
	return client.ObjectKey{Namespace: obj.Namespace, Name: obj.Name}
}
