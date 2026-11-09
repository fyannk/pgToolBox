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
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

const (
	drillOwner = "pgtoolbox.fyannk.dev/drill-owner"
	drillOf    = "pgtoolbox.fyannk.dev/restore-drill-of"
	drillSeal  = "pgtoolbox.fyannk.dev/restore-drill-seal"
)

// policyDocs splits rendered or source YAML into the admission objects and
// the ClusterRoles it holds.
func policyDocs(t *testing.T, out []byte) (objects []client.Object) {
	t.Helper()
	for _, doc := range bytes.Split(out, []byte("\n---")) {
		kind := struct {
			Kind string `json:"kind"`
		}{}
		if yaml.Unmarshal(doc, &kind) != nil {
			continue
		}
		var object client.Object
		switch kind.Kind {
		case "ValidatingAdmissionPolicy":
			object = &admissionregistrationv1.ValidatingAdmissionPolicy{}
		case "ValidatingAdmissionPolicyBinding":
			object = &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
		case "ClusterRole":
			object = &rbacv1.ClusterRole{}
		default:
			continue
		}
		if err := yaml.UnmarshalStrict(doc, object); err != nil {
			t.Fatalf("%s: %v", kind.Kind, err)
		}
		objects = append(objects, object)
	}
	return objects
}

func cnpgCluster(namespace, name string, labels, annotations map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "Cluster",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec":       map[string]any{"instances": int64(1), "storage": map[string]any{"size": "1Gi"}},
	}}
	u.SetLabels(labels)
	u.SetAnnotations(annotations)
	return u
}

// TestEnvtestRestoreDrillGuards evaluates both restore-drill delete guards
// on an API server, as the consoles' ServiceAccounts:
// the operator's (config/admission, parameterless, keyed on the drills
// ClusterRole's marker) and the pgtools chart's opt-in one (literal account,
// owner and prefix). A console may delete its sealed drills and nothing else;
// anybody else is not concerned.
//
//nolint:funlen,gocyclo // one API server, both policies, the cases in order.
func TestEnvtestRestoreDrillGuards(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set (make test-envtest-admissionpolicy)")
	}
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	root := filepath.Join("..", "..", "..")
	source, err := os.ReadFile(filepath.Join(root, "config", "admission", "restore_drill_guard.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	drillsRole, err := os.ReadFile(filepath.Join(root, "config", "rbac", "pgtoolbox_console_restore_drills_clusterrole.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	pgtools, err := exec.CommandContext(t.Context(), "helm", "template", "pgc", filepath.Join(root, "charts", "pgtools"), // #nosec G204 -- fixed arguments
		"--namespace", "team-b", "-s", "templates/console/restoredrills-admissionpolicy.yaml",
		"--set", "console.watchNamespaces={team-b,drills}", "--set", "console.restoreDrills.enabled=true",
		"--set", "console.restoreDrills.namespace=drills", "--set", "console.restoreDrills.admissionPolicy.create=true",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template pgtools: %v\n%s", err, pgtools)
	}
	objects := append(append(policyDocs(t, source), policyDocs(t, drillsRole)...), policyDocs(t, pgtools)...)
	if len(objects) != 7 {
		t.Fatalf("%d policy objects and roles, want the operator's two policies, the pgtools one, their bindings and the drills ClusterRole", len(objects))
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(root, "test", "crds", "cnpg-1.30"), filepath.Join(root, "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	admin, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(admin, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	as := func(namespace, account string) client.Client {
		config := rest.CopyConfig(admin)
		config.Impersonate = rest.ImpersonationConfig{
			UserName: "system:serviceaccount:" + namespace + ":" + account,
			Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + namespace, "system:authenticated"},
		}
		sa, err := client.New(config, client.Options{Scheme: scheme})
		if err != nil {
			t.Fatal(err)
		}
		return sa
	}
	saSubject := func(namespace, name string) []rbacv1.Subject {
		return []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Namespace: namespace, Name: name}}
	}
	plainDelete := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "clusters-delete"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{"clusters"},
			Verbs: []string{"get", "list", "watch", "delete"},
		}},
	}
	objects = append(objects, plainDelete)
	for _, ns := range []string{"team-a", "team-b", "drills"} {
		objects = append(objects, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	}
	objects = append(objects,
		// The operator path: the console team-a/orders-console is bound to
		// the drills ClusterRole; team-a/backup-console deletes Clusters
		// through another role and holds no marker.
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "orders-console-restore-drills", Namespace: "team-a"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "pgtoolbox-console-restore-drills"},
			Subjects:   saSubject("team-a", "orders-console"),
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "backup-console", Namespace: "team-a"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "clusters-delete"},
			Subjects:   saSubject("team-a", "backup-console"),
		},
		// billing-console (the PgToolBoxConsole "billing", drill prefix
		// bill-) and drill-bot, a ServiceAccount whose name says nothing,
		// hold the marker too.
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "billing-console-restore-drills", Namespace: "team-a"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "pgtoolbox-console-restore-drills"},
			Subjects:   saSubject("team-a", "billing-console"),
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "drill-bot", Namespace: "team-a"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "pgtoolbox-console-restore-drills"},
			Subjects:   saSubject("team-a", "drill-bot"),
		},
		// The pgtools path: release pgc in team-b, drills in drills.
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "pgc-console-restore-drills", Namespace: "drills"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "clusters-delete"},
			Subjects:   saSubject("team-b", "pgc-console"),
		},
	)
	// The prefix policy's parameters: the namespace's PgToolBoxConsoles.
	for name, prefix := range map[string]string{"orders": "", "billing": "bill-"} {
		console := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "pgtoolbox.fyannk.dev/v1alpha1", "kind": "PgToolBoxConsole",
			"metadata": map[string]any{"name": name, "namespace": "team-a"},
			"spec": map[string]any{
				"auth": map[string]any{"provider": "oidc", "oidc": map[string]any{
					"issuerURL": "https://idp.example.com", "clientID": name, "clientSecretRef": map[string]any{"name": name + "-oidc"},
				}},
			},
		}}
		if prefix != "" {
			_ = unstructured.SetNestedField(console.Object, map[string]any{"enabled": true, "prefix": prefix}, "spec", "restoreDrills")
		}
		objects = append(objects, console)
	}
	for _, object := range objects {
		if err := c.Create(t.Context(), object); err != nil {
			t.Fatalf("create %T %s: %v", object, object.GetName(), err)
		}
	}

	cluster := func(t *testing.T, namespace, name string, labels, annotations map[string]string) *unstructured.Unstructured {
		t.Helper()
		u := cnpgCluster(namespace, name, labels, annotations)
		if err := c.Create(t.Context(), u); err != nil {
			t.Fatalf("create Cluster %s/%s: %v", namespace, name, err)
		}
		t.Cleanup(func() { _ = c.Delete(t.Context(), cnpgCluster(namespace, name, nil, nil)) })
		return u
	}
	// Both policies take effect once compiled: wait until each console is
	// refused an unlabelled Cluster.
	for _, probe := range []struct{ namespace, account string }{{"team-a", "orders-console"}, {"drills", "pgc-console"}} {
		owner := probe.namespace
		if probe.namespace == "drills" {
			owner = "team-b"
		}
		deadline := time.Now().Add(time.Minute)
		for {
			u := cnpgCluster(probe.namespace, "probe", nil, nil)
			if err := c.Create(t.Context(), u); err != nil && !apierrors.IsAlreadyExists(err) {
				t.Fatalf("create probe: %v", err)
			}
			err := as(owner, probe.account).Delete(t.Context(), u)
			if apierrors.IsForbidden(err) && strings.Contains(err.Error(), "restore drill Clusters it created") {
				_ = c.Delete(t.Context(), u)
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the %s guard never took effect; last delete error: %v", probe.namespace, err)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}

	// The prefix policy reads its parameters through an informer: wait
	// until billing-console is refused a sealed drill named drill-.
	deadline := time.Now().Add(time.Minute)
	for {
		u := cnpgCluster("team-a", "drill-probe", map[string]string{drillOwner: "team-a", drillOf: "orders"}, map[string]string{drillSeal: "v1.x"})
		if err := c.Create(t.Context(), u); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create probe: %v", err)
		}
		err := as("team-a", "billing-console").Delete(t.Context(), u)
		if apierrors.IsForbidden(err) && strings.Contains(err.Error(), "named with its restore drill prefix") {
			_ = c.Delete(t.Context(), u)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the prefix guard never took effect; last delete error: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}

	deniedPrefix := func(t *testing.T, err error) {
		t.Helper()
		if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "named with its restore drill prefix") {
			t.Fatalf("error = %v, want the prefix guard's Forbidden", err)
		}
	}
	denied := func(t *testing.T, err error) {
		t.Helper()
		if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "restore drill Clusters it created") {
			t.Fatalf("error = %v, want the guard's Forbidden", err)
		}
	}
	allowed := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("error = %v, want the delete admitted", err)
		}
	}
	sealed := map[string]string{drillSeal: "v1.x"}

	t.Run("operator: a console deletes its sealed drill", func(t *testing.T) {
		u := cluster(t, "team-a", "drill-orders-1", map[string]string{drillOwner: "team-a", drillOf: "orders"}, sealed)
		allowed(t, as("team-a", "orders-console").Delete(t.Context(), u))
	})
	t.Run("operator: a console cannot delete a production Cluster", func(t *testing.T) {
		u := cluster(t, "team-a", "orders", nil, nil)
		denied(t, as("team-a", "orders-console").Delete(t.Context(), u))
	})
	t.Run("operator: nor a drill without the seal", func(t *testing.T) {
		u := cluster(t, "team-a", "drill-orders-2", map[string]string{drillOwner: "team-a", drillOf: "orders"}, nil)
		denied(t, as("team-a", "orders-console").Delete(t.Context(), u))
	})
	t.Run("operator: nor another namespace's drill", func(t *testing.T) {
		u := cluster(t, "team-a", "drill-orders-3", map[string]string{drillOwner: "team-b", drillOf: "orders"}, sealed)
		denied(t, as("team-a", "orders-console").Delete(t.Context(), u))
	})
	t.Run("operator: a console deletes its sealed drill under its own prefix", func(t *testing.T) {
		u := cluster(t, "team-a", "bill-orders-1", map[string]string{drillOwner: "team-a", drillOf: "orders"}, sealed)
		allowed(t, as("team-a", "billing-console").Delete(t.Context(), u))
	})
	t.Run("operator: nor a sealed drill outside its prefix", func(t *testing.T) {
		u := cluster(t, "team-a", "drill-orders-4", map[string]string{drillOwner: "team-a", drillOf: "orders"}, sealed)
		deniedPrefix(t, as("team-a", "billing-console").Delete(t.Context(), u))
	})
	t.Run("operator: the default prefix holds for a console that sets none", func(t *testing.T) {
		u := cluster(t, "team-a", "bill-orders-2", map[string]string{drillOwner: "team-a", drillOf: "orders"}, sealed)
		deniedPrefix(t, as("team-a", "orders-console").Delete(t.Context(), u))
	})
	t.Run("operator: a marker holder is guarded whatever its name", func(t *testing.T) {
		u := cluster(t, "team-a", "payments", nil, nil)
		denied(t, as("team-a", "drill-bot").Delete(t.Context(), u))
	})
	t.Run("operator: a ServiceAccount without the drills role is not concerned", func(t *testing.T) {
		u := cluster(t, "team-a", "orders-old", nil, nil)
		allowed(t, as("team-a", "backup-console").Delete(t.Context(), u))
	})
	t.Run("operator: an administrator is not concerned", func(t *testing.T) {
		u := cluster(t, "team-a", "orders-older", nil, nil)
		allowed(t, c.Delete(t.Context(), u))
	})

	t.Run("pgtools: the console deletes its sealed, prefixed drill", func(t *testing.T) {
		u := cluster(t, "drills", "drill-orders-1", map[string]string{drillOwner: "team-b", drillOf: "orders"}, sealed)
		allowed(t, as("team-b", "pgc-console").Delete(t.Context(), u))
	})
	t.Run("pgtools: it cannot delete a production Cluster", func(t *testing.T) {
		u := cluster(t, "drills", "orders", nil, nil)
		denied(t, as("team-b", "pgc-console").Delete(t.Context(), u))
	})
	t.Run("pgtools: nor a sealed drill without its prefix", func(t *testing.T) {
		u := cluster(t, "drills", "orders-copy", map[string]string{drillOwner: "team-b", drillOf: "orders"}, sealed)
		denied(t, as("team-b", "pgc-console").Delete(t.Context(), u))
	})
	t.Run("pgtools: nor another console's drill", func(t *testing.T) {
		u := cluster(t, "drills", "drill-orders-2", map[string]string{drillOwner: "team-c", drillOf: "orders"}, sealed)
		denied(t, as("team-b", "pgc-console").Delete(t.Context(), u))
	})
	t.Run("pgtools: an administrator is not concerned", func(t *testing.T) {
		u := cluster(t, "drills", "scratch", nil, nil)
		allowed(t, c.Delete(t.Context(), u))
	})
}
