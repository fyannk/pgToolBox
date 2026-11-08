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

package passfilesync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/declared"
)

// TestEnvtestDeclaredSidecarUnderNameScopedRole runs the declared sidecar
// as an identity holding only the pgtools chart's Role: get and watch on
// the named Clusters, DatabaseRoles, Poolers and Secrets, by resourceNames.
// It proves on a real API server (RBAC, not a fake) that a watch scoped by
// the field selector metadata.name=<name> is authorized against
// resourceNames, that rotation is followed through that watch, and that
// the identity can neither list Secrets nor read one it was not named.
func TestEnvtestDeclaredSidecarUnderNameScopedRole(t *testing.T) {
	ctx := t.Context()
	list := declared.Document{Version: 1, Servers: []declared.Server{
		{Cluster: "orders", Endpoint: "rw", Credentials: app("orders-app")},
	}}
	admin, clientset, dynamicClient := sidecarEnvtest(t, list)

	// What the Role allows and refuses, asked directly.
	secrets := clientset.CoreV1().Secrets("team")
	if _, err := secrets.List(ctx, metav1.ListOptions{}); !apierrors.IsForbidden(err) {
		t.Fatalf("list secrets: %v, want Forbidden", err)
	}
	if _, err := secrets.Get(ctx, "unrelated", metav1.GetOptions{}); !apierrors.IsForbidden(err) {
		t.Fatalf("get an unnamed Secret: %v, want Forbidden", err)
	}
	if _, err := secrets.Watch(ctx, metav1.ListOptions{}); !apierrors.IsForbidden(err) {
		t.Fatalf("watch every Secret: %v, want Forbidden", err)
	}
	scoped, err := secrets.Watch(ctx, metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.name", "orders-app").String()})
	if err != nil {
		t.Fatalf("watch a named Secret by field selector: %v", err)
	}
	scoped.Stop()

	files := Files{Dir: t.TempDir()}
	go func() {
		_ = RunDeclared(ctx, DeclaredOptions{
			Client: clientset, Dynamic: dynamicClient, Namespace: "team", Document: list, Files: files,
			Log: (&capture{}).logger(), Debounce: 20 * time.Millisecond, Backoff: 100 * time.Millisecond,
		})
	}()
	eventually(t, "the first password", func() bool { return fileContains(files.PassFile(), "pw-before") })

	// Rotation reaches the files through the name-scoped watch.
	rotated := &corev1.Secret{}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: "team", Name: "orders-app"}, rotated); err != nil {
		t.Fatal(err)
	}
	rotated.Data["password"] = []byte("pw-after")
	if err := admin.Update(ctx, rotated); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the rotated password", func() bool {
		return fileContains(files.PassFile(), "pw-after") && !fileContains(files.PassFile(), "pw-before")
	})
	if fileContains(files.ServiceFile(), "not-yours") || fileContains(files.PassFile(), "not-yours") {
		t.Fatal("an unnamed Secret reached the files")
	}
}

// TestEnvtestDeclaredSidecarCAFromConfigMap: with caConfigMap the chart's
// Role names no CA Secret, so the identity cannot read CloudNativePG's
// <cluster>-ca (and its ca.key) at all, and the sidecar still offers the
// server from the mounted ca.crt.
func TestEnvtestDeclaredSidecarCAFromConfigMap(t *testing.T) {
	ctx := t.Context()
	list := declared.Document{Version: 1, Servers: []declared.Server{
		{Cluster: "orders", Endpoint: "rw", CAConfigMap: "orders-ca-crt", Credentials: app("orders-app")},
	}}
	_, clientset, dynamicClient := sidecarEnvtest(t, list)
	if _, err := clientset.CoreV1().Secrets("team").Get(ctx, "orders-ca", metav1.GetOptions{}); !apierrors.IsForbidden(err) {
		t.Fatalf("get the CA Secret: %v, want Forbidden", err)
	}

	files := Files{Dir: t.TempDir()}
	caDir := t.TempDir()
	projectCAs(t, caDir, map[string]string{"orders-ca-crt": caPEM2})
	go func() {
		_ = RunDeclared(ctx, DeclaredOptions{
			Client: clientset, Dynamic: dynamicClient, Namespace: "team", Document: list, Files: files, CADir: caDir,
			Log: (&capture{}).logger(), Debounce: 20 * time.Millisecond, Backoff: 100 * time.Millisecond,
		})
	}()
	eventually(t, "the password with the mounted CA", func() bool {
		return fileContains(files.PassFile(), "pw-before") && fileContains(filepath.Join(files.Dir, "ca", "orders.crt"), "CLUSTER-TWO")
	})
}

// sidecarEnvtest starts an API server with CloudNativePG's CRDs, the
// Cluster orders, its CA Secret, its application Secret and an unrelated
// Secret in namespace team, and returns an admin client and the clients of
// an identity bound to the Role the chart renders for list (get and watch
// by resourceNames, from list.Names()).
func sidecarEnvtest(t *testing.T, list declared.Document) (client.Client, kubernetes.Interface, dynamic.Interface) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set (make test-envtest-operator)")
	}
	crds := os.Getenv("CNPG_CRDS")
	if crds == "" {
		crds = filepath.Join("..", "..", "..", "..", "test", "crds", "cnpg-1.30")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{crds}, ErrorIfCRDPathMissing: true}
	adminConfig, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, rbacv1.AddToScheme, cnpgv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	admin, err := client.New(adminConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	create := func(obj client.Object) {
		t.Helper()
		obj.SetResourceVersion("") // the fixtures carry one; the API server assigns it
		if err := admin.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team"}})
	cluster := ordersCluster()
	cluster.Spec.StorageConfiguration = cnpgv1.StorageConfiguration{Size: "1Gi"}
	create(cluster)
	create(caSecret("orders-ca", caPEM1))
	create(secret("orders-app", "", map[string]string{"username": "shop_owner", "password": "pw-before"}))
	create(secret("unrelated", "", map[string]string{"password": "not-yours"}))

	names := list.Names()
	rule := func(group, resource string, resourceNames []string) rbacv1.PolicyRule {
		return rbacv1.PolicyRule{APIGroups: []string{group}, Resources: []string{resource}, ResourceNames: resourceNames, Verbs: []string{"get", "watch"}}
	}
	create(&rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "pgadmin-sidecar", Namespace: "team"},
		Rules: []rbacv1.PolicyRule{
			rule("postgresql.cnpg.io", "clusters", names.Clusters),
			rule("", "secrets", names.Secrets),
		},
	})
	create(&rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "pgadmin-sidecar", Namespace: "team"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "pgadmin-sidecar"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "pgadmin-sidecar"}},
	})
	user, err := env.AddUser(envtest.User{Name: "pgadmin-sidecar"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	clientset, err := kubernetes.NewForConfig(user.Config())
	if err != nil {
		t.Fatal(err)
	}
	dynamicClient, err := dynamic.NewForConfig(user.Config())
	if err != nil {
		t.Fatal(err)
	}
	return admin, clientset, dynamicClient
}
