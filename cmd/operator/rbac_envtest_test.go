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
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/fyannk/pgToolBox/internal/operator/configuration"
	"github.com/fyannk/pgToolBox/pkg/discovery"
)

// TestEnvtestWatchNamespacesRBAC applies the RBAC the Helm chart renders
// with operator.config.watchNamespaces (team-a, and team-gone, a namespace
// deleted since the install) to a real API server and acts as the
// operator's ServiceAccount: it reads its own ConfigMap and Secret in the
// release namespace, which is not watched, and takes its Lease; it starts
// with team-gone left out; and it cannot leave its namespaces through RBAC:
// no ClusterRoleBinding to the console roles, no other Secret of its own
// namespace, while the RoleBinding it does create in a watched namespace
// still passes the escalation check.
//
// Needs KUBEBUILDER_ASSETS and helm (make test-envtest-manager).
//
//nolint:funlen // one API server, the operator's identity, the cases in order.
func TestEnvtestWatchNamespacesRBAC(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set (make test-envtest-manager)")
	}
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	root := filepath.Join("..", "..")
	out, err := exec.CommandContext(t.Context(), "helm", "template", "pgtoolbox", filepath.Join(root, "charts", "pgtoolbox"), // #nosec G204 -- fixed arguments
		"--namespace", "pgtoolbox", "--set", "operator.enabled=true",
		"--set", "operator.config.watchNamespaces={team-a,team-gone,team-narrowed}").CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	var rbac []client.Object
	for _, doc := range bytes.Split(out, []byte("\n---")) {
		kind := struct {
			Kind     string            `json:"kind"`
			Metadata metav1.ObjectMeta `json:"metadata"`
		}{}
		if yaml.Unmarshal(doc, &kind) != nil {
			continue
		}
		var object client.Object
		switch kind.Kind {
		case "ClusterRole":
			object = &rbacv1.ClusterRole{}
		case "ClusterRoleBinding":
			object = &rbacv1.ClusterRoleBinding{}
		case "Role":
			object = &rbacv1.Role{}
		case "RoleBinding":
			if kind.Metadata.Namespace == "team-gone" {
				continue // deleted with its namespace
			}
			if kind.Metadata.Namespace == "team-narrowed" {
				continue // replaced by its administrator, below
			}
			object = &rbacv1.RoleBinding{}
		default:
			continue
		}
		if err := yaml.UnmarshalStrict(doc, object); err != nil {
			t.Fatalf("%s: %v", kind.Kind, err)
		}
		rbac = append(rbac, object)
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(root, "test", "crds", "cnpg-1.30")},
		ErrorIfCRDPathMissing: true,
	}
	admin, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx := t.Context()
	c, err := client.New(admin, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	objects := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "pgtoolbox"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-narrowed"}},
		// team-narrowed's administrator bound the operator to a Role of
		// their own: Clusters, nothing else.
		&rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "narrow", Namespace: "team-narrowed"},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{"clusters"}, Verbs: []string{"get", "list", "watch"}}},
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "narrow", Namespace: "team-narrowed"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "narrow"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "pgtoolbox", Namespace: "pgtoolbox"}},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "pgtoolbox-operator-config", Namespace: "pgtoolbox"},
			Data:       map[string]string{"UPGRADE_BACKUPS_KEPT": "7"},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "pgtoolbox-operator-config", Namespace: "pgtoolbox"},
			Data:       map[string][]byte{"MAX_CONCURRENT_RECONCILES": []byte("3")},
		},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "someone-else", Namespace: "pgtoolbox"}},
	}
	for _, object := range append(objects, rbac...) {
		if err := c.Create(ctx, object); err != nil {
			t.Fatalf("create %T %s: %v", object, object.GetName(), err)
		}
	}

	config := rest.CopyConfig(admin)
	config.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:pgtoolbox:pgtoolbox",
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:pgtoolbox", "system:authenticated"},
	}
	operator, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("reads its configuration in its own, unwatched namespace", func(t *testing.T) {
		loaded, err := configuration.Load(ctx, operator, func(key string) (string, bool) {
			if key == "OPERATOR_IMAGE" {
				return "registry.example.org/pgtoolbox-operator:test", true
			}
			return "", false
		}, configuration.Objects{
			Namespace: "pgtoolbox", ConfigMap: "pgtoolbox-operator-config", Secret: "pgtoolbox-operator-config",
		})
		if err != nil {
			t.Fatalf("the operator cannot read its configuration: %v", err)
		}
		if loaded.Config.UpgradeBackupsKept != 7 || loaded.Config.MaxConcurrentReconciles != 3 {
			t.Fatalf("configuration = %+v, want the ConfigMap and Secret values", loaded.Config)
		}
	})
	t.Run("takes its leader-election Lease", func(t *testing.T) {
		lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "pgtoolbox.pgtoolbox.fyannk.dev", Namespace: "pgtoolbox"}}
		if err := operator.Create(ctx, lease); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("starts with the deleted namespace left out", func(t *testing.T) {
		got, err := reachableNamespaces(ctx, operator, []string{"team-a", "team-gone"})
		if err != nil || !slices.Equal(got, []string{"team-a"}) {
			t.Fatalf("watched = %q, %v; want [team-a]", got, err)
		}
		if _, err := probeOptionalAPIs(ctx, operator, got); err != nil {
			t.Fatalf("probeOptionalAPIs: %v", err)
		}
	})
	t.Run("a narrowed namespace is left out, the features stay on", func(t *testing.T) {
		got, err := reachableNamespaces(ctx, operator, []string{"team-a", "team-narrowed"})
		if err != nil || !slices.Equal(got, []string{"team-a", "team-narrowed"}) {
			t.Fatalf("reachable = %q, %v; want both (Clusters are listable in each)", got, err)
		}
		probed, err := probeOptionalAPIs(ctx, operator, got)
		if err != nil {
			t.Fatalf("probeOptionalAPIs: %v", err)
		}
		if !slices.Equal(probed.Namespaces, []string{"team-a"}) {
			t.Fatalf("watched %q, want [team-a]", probed.Namespaces)
		}
		for name, r := range map[string]discovery.Result{
			"databaseRoles": probed.Features.DatabaseRoles, "poolers": probed.Features.Poolers, "databases": probed.Features.Databases,
		} {
			if !r.Usable() {
				t.Errorf("%s turned off by team-narrowed: %+v", name, r)
			}
		}
	})
	t.Run("reads no other Secret of its namespace", func(t *testing.T) {
		err := operator.Get(ctx, client.ObjectKey{Namespace: "pgtoolbox", Name: "someone-else"}, &corev1.Secret{})
		if !apierrors.IsForbidden(err) {
			t.Fatalf("get another Secret: %v, want Forbidden", err)
		}
	})
	t.Run("cannot bind a console role cluster-wide", func(t *testing.T) {
		for _, role := range []string{"pgtoolbox-console-restore-drills", "pgtoolbox-console-pod-logs", "pgtoolbox-console"} {
			err := operator.Create(ctx, &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "escape-" + role},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role},
				Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "pgtoolbox", Namespace: "pgtoolbox"}},
			})
			if !apierrors.IsForbidden(err) {
				t.Fatalf("ClusterRoleBinding to %s: %v, want Forbidden", role, err)
			}
		}
	})
	t.Run("binds the console roles in a watched namespace", func(t *testing.T) {
		err := operator.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "orders-console-restore-drills", Namespace: "team-a"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "pgtoolbox-console-restore-drills"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "orders-console", Namespace: "team-a"}},
		}, client.DryRunAll)
		if err != nil {
			t.Fatalf("RoleBinding in team-a: %v", err)
		}
	})
}
