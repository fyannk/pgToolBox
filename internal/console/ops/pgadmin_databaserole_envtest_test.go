//go:build envtest

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
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestPersonalDatabaseRoleOnEnvtest: on CloudNativePG 1.30's schema the
// strict create is refused with the unknown spec.password (501, never a
// role without a password); once the schema carries it (1.31) the role is
// created as the user, whose RBAC alone decides; a user without create on
// databaseroles is refused by the API server.
func TestPersonalDatabaseRoleOnEnvtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset; run through make test-envtest-pgadmin-console")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Clean("../../../test/crds/cnpg-1.30")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	scheme, err := NewUserScheme()
	if err != nil {
		t.Fatal(err)
	}
	_ = rbacv1.AddToScheme(scheme)
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}))
	must(admin.Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "roles"},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{"databaseroles"}, Verbs: []string{"create"}}},
	}))
	must(admin.Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "dba"},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "roles"},
		Subjects:   []rbacv1.Subject{{Kind: "User", Name: "dba"}},
	}))
	writer := func(name string) *UserWriter {
		user, err := env.AddUser(envtest.User{Name: name}, nil)
		must(err)
		c, err := NewUserClient(user.Config())
		must(err)
		return NewUserWriter(c, 10*time.Second)
	}
	dba, viewer := writer("dba"), writer("viewer")
	req := PersonalRoleRequest{Namespace: "team-a", Cluster: "orders", Role: "alice", PgAdminUser: "alice@example.com", CreatedBy: "dba"}

	_, err = dba.CreatePersonalDatabaseRole(ctx, req)
	var w *WriteError
	if !errors.As(err, &w) || w.Status != 501 || !errors.Is(err, ErrPasswordGenerationUnavailable) {
		t.Fatalf("on CloudNativePG 1.30: %v, want 501 password generation unavailable", err)
	}

	// The schema gains spec.password (CloudNativePG 1.31).
	crd := &unstructured.Unstructured{}
	crd.SetAPIVersion("apiextensions.k8s.io/v1")
	crd.SetKind("CustomResourceDefinition")
	must(admin.Get(ctx, client.ObjectKey{Name: "databaseroles.postgresql.cnpg.io"}, crd))
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	for i := range versions {
		version := versions[i].(map[string]any)
		must(unstructured.SetNestedField(version, map[string]any{"type": "object", "x-kubernetes-preserve-unknown-fields": true},
			"schema", "openAPIV3Schema", "properties", "spec", "properties", "password"))
		versions[i] = version
	}
	must(unstructured.SetNestedSlice(crd.Object, versions, "spec", "versions"))
	must(admin.Update(ctx, crd))

	deadline := time.Now().Add(30 * time.Second)
	var role PersonalRole
	for {
		role, err = dba.CreatePersonalDatabaseRole(ctx, req)
		if err == nil || !errors.Is(err, ErrPasswordGenerationUnavailable) || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		t.Fatalf("on CloudNativePG 1.31: %v", err)
	}
	created := &unstructured.Unstructured{}
	created.SetAPIVersion("postgresql.cnpg.io/v1")
	created.SetKind("DatabaseRole")
	must(admin.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: role.Name}, created))
	if mode, _, _ := unstructured.NestedString(created.Object, "spec", "password", "mode"); mode != "generate" {
		t.Fatalf("created %v", created.Object["spec"])
	}
	if _, err := dba.CreatePersonalDatabaseRole(ctx, req); !errors.As(err, &w) || w.Status != 409 {
		t.Fatalf("a second create: %v, want 409", err)
	}
	other := req
	other.Role = "bob"
	if _, err := viewer.CreatePersonalDatabaseRole(ctx, other); !errors.As(err, &w) || w.Status != 403 {
		t.Fatalf("a user without create on databaseroles: %v, want 403", err)
	}
}
