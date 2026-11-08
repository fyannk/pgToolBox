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

package ops

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

func TestPersonalRoleName(t *testing.T) {
	for _, tc := range []struct{ cluster, role, want string }{
		{"billing", "alice_ro", "billing-alice-ro"},
		{"billing", "_alice_", "billing-alice"},
		{"billing", "___", ""},
		{strings.Repeat("c", 250), "alice", ""},
	} {
		if got := PersonalRoleName(tc.cluster, tc.role); got != tc.want {
			t.Errorf("PersonalRoleName(%q, %q) = %q, want %q", tc.cluster, tc.role, got, tc.want)
		}
	}
}

func TestCreatePersonalDatabaseRole(t *testing.T) {
	req := PersonalRoleRequest{
		Namespace: "team-a", Cluster: "billing", Role: "alice_ro", PgAdminUser: "alice@example.com",
		InRoles: []string{"reporting"}, CreatedBy: "oidc:alice@example.com",
	}
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{name: "created"},
		{name: "CloudNativePG 1.30 (strict validation)", err: apierrors.NewBadRequest(`DatabaseRole in version "v1" cannot be handled as a DatabaseRole: strict decoding error: unknown field "spec.password"`), status: 501},
		{name: "exists", err: apierrors.NewAlreadyExists(schema.GroupResource{Group: "postgresql.cnpg.io", Resource: "databaseroles"}, "billing-alice-ro"), status: 409},
		{name: "forbidden", err: apierrors.NewForbidden(schema.GroupResource{Group: "postgresql.cnpg.io", Resource: "databaseroles"}, "billing-alice-ro", errors.New(`User "oidc:alice" cannot create resource "databaseroles" in API group "postgresql.cnpg.io" in the namespace "billing"`)), status: 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var created *unstructured.Unstructured
			var strict bool
			scheme, err := NewUserScheme()
			if err != nil {
				t.Fatal(err)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
				Create: func(_ context.Context, _ client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					created = obj.(*unstructured.Unstructured).DeepCopy()
					options := &client.CreateOptions{}
					options.ApplyOptions(opts)
					strict = options.FieldValidation == "Strict"
					return tc.err
				},
			}).Build()
			role, err := NewUserWriter(c, 0).CreatePersonalDatabaseRole(context.Background(), req)
			if tc.status != 0 {
				var w *WriteError
				if !errors.As(err, &w) || w.Status != tc.status {
					t.Fatalf("error %v, want status %d", err, tc.status)
				}
				if tc.status == 501 && !errors.Is(err, ErrPasswordGenerationUnavailable) {
					t.Fatal("a refused spec.password is ErrPasswordGenerationUnavailable")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strict {
				t.Fatal("the create must use strict field validation, or a 1.30 API server drops spec.password silently")
			}
			if role.Name != "billing-alice-ro" || role.PasswordSecret != "billing-alice-ro-password" {
				t.Fatalf("role %+v", role)
			}
			if created.GetLabels()[pgtoolboxv1alpha1.LabelPersonalRole] != "true" ||
				created.GetAnnotations()[pgtoolboxv1alpha1.AnnotationPgAdminUser] != "alice@example.com" ||
				created.GetAnnotations()[AnnotationCreatedBy] != "oidc:alice@example.com" {
				t.Fatalf("metadata %v %v", created.GetLabels(), created.GetAnnotations())
			}
			mode, _, _ := unstructured.NestedString(created.Object, "spec", "password", "mode")
			name, _, _ := unstructured.NestedString(created.Object, "spec", "name")
			login, _, _ := unstructured.NestedBool(created.Object, "spec", "login")
			cluster, _, _ := unstructured.NestedString(created.Object, "spec", "cluster", "name")
			inRoles, _, _ := unstructured.NestedStringSlice(created.Object, "spec", "inRoles")
			if mode != "generate" || name != "alice_ro" || !login || cluster != "billing" || len(inRoles) != 1 {
				t.Fatalf("spec %v", created.Object["spec"])
			}
			if _, has, _ := unstructured.NestedFieldNoCopy(created.Object, "spec", "passwordSecret"); has {
				t.Fatal("no password ever passes through the console")
			}
		})
	}
}
