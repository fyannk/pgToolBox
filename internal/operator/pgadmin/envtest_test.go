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

package pgadmin

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/configuration"
	"github.com/fyannk/pgToolBox/pkg/discovery"
)

// TestEnvtestReconcile runs the reconciler against a real API server with
// the generated CRDs and CloudNativePG's own (vendored under test/crds), as the
// operator's ServiceAccount bound to the generated manager ClusterRole and
// nothing else. It proves what a fake client cannot:
//   - RBAC escalation prevention lets the operator write the pgAdmin Pod's
//     name-scoped Role (it holds every verb it grants);
//   - every rendered object is accepted (schemas, the status subresource,
//     server-side apply of typed objects);
//   - a second reconcile with nothing changed issues no write at all:
//     no child's and no PgAdmin's resourceVersion moves.
//
// Needs KUBEBUILDER_ASSETS (make test-envtest-operator).
func TestEnvtestReconcile(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set (make test-envtest-operator)")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "..", "config", "crd", "bases"),
			cnpgCRDs(),
		},
		ErrorIfCRDPathMissing: true,
	}
	restConfig, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	scheme := testScheme(t)
	admin, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// The operator identity: the generated manager ClusterRole, bound.
	roleYAML, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var managerRole rbacv1.ClusterRole
	if err := yaml.Unmarshal(roleYAML, &managerRole); err != nil {
		t.Fatal(err)
	}
	must(t, admin.Create(ctx, &managerRole))
	must(t, admin.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "operator"},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: managerRole.Name},
		Subjects:   []rbacv1.Subject{{Kind: "User", Name: "pgtoolbox-operator"}},
	}))
	user, err := env.AddUser(envtest.User{Name: "pgtoolbox-operator"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	operator, err := client.New(user.Config(), client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	var nodes corev1.NodeList
	if err := operator.List(ctx, &nodes); err == nil {
		t.Fatal("the operator identity can list nodes: the API server is not enforcing RBAC, the test would prove nothing")
	}

	must(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}))
	for _, name := range []string{"orders", "billing"} {
		cluster := &cnpgv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec:       cnpgv1.ClusterSpec{Instances: 1, StorageConfiguration: cnpgv1.StorageConfiguration{Size: "1Gi"}},
		}
		must(t, admin.Create(ctx, cluster))
	}
	must(t, admin.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pgadmin-oidc", Namespace: testNamespace},
		StringData: map[string]string{"client-secret": "x"},
	}))
	p := oauth2PgAdmin()
	p.ResourceVersion, p.UID, p.Generation = "", "", 0
	p.Spec.Storage.Size = new(resource.MustParse("1Gi"))
	p.Spec.Auth.OAuth2.RequiredClaims = map[string]pgtoolboxv1alpha1.ClaimValues{"groups": {"dba"}}
	// ro on Clusters of one instance: NotOffered until a scale-up (below).
	p.Spec.Endpoints = []pgtoolboxv1alpha1.Endpoint{"rw", "ro"}
	must(t, admin.Create(ctx, p))

	cfg := configuration.Defaults()
	cfg.OperatorImage, cfg.PgAdminImage = testOperatorImage, testPgAdminImage
	r := &Reconciler{
		Client: operator, APIReader: operator, Scheme: scheme, Recorder: events.NewFakeRecorder(100), Config: cfg,
		Features: Features{DatabaseRoles: discovery.Result{State: discovery.Available}, Poolers: discovery.Result{State: discovery.Available}},
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: p.Name}}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("first reconcile as the operator identity: %v", err)
	}

	var got pgtoolboxv1alpha1.PgAdmin
	must(t, admin.Get(ctx, request.NamespacedName, &got))
	if c := condition(&got, "ConfigurationValid"); c.Status != metav1.ConditionTrue {
		t.Fatalf("ConfigurationValid %+v", c)
	}
	if c := condition(&got, "Ready"); c.Status != metav1.ConditionFalse || c.Reason != "RolloutInProgress" {
		t.Fatalf("Ready %+v", c)
	}
	var role rbacv1.Role
	must(t, admin.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "team-pgadmin-credentials"}, &role))
	if len(role.Rules) != 1 || !slices.Contains(role.Rules[0].ResourceNames, "orders-app") || slices.Contains(role.Rules[0].Verbs, "list") {
		t.Fatalf("credentials Role %+v", role.Rules)
	}

	// One settling loop: the API server's admission adds the
	// pvc-protection finalizer under the creating apply's field manager,
	// and the next apply, which does not name it, moves its ownership once
	// (a single managedFields write). From then on a loop is a no-op.
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	must(t, admin.Get(ctx, request.NamespacedName, &got))
	versions := childVersions(t, admin)
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	var again pgtoolboxv1alpha1.PgAdmin
	must(t, admin.Get(ctx, request.NamespacedName, &again))
	if again.ResourceVersion != got.ResourceVersion {
		t.Errorf("a no-op reconcile wrote the PgAdmin status (%s -> %s)", got.ResourceVersion, again.ResourceVersion)
	}
	for key, version := range childVersions(t, admin) {
		if versions[key] != version {
			t.Errorf("a no-op reconcile wrote %s (%s -> %s)", key, versions[key], version)
		}
	}

	// orders has one instance, so its ro Service has no
	// endpoints and is not offered; scaled to two, the next reconcile
	// offers it.
	ordersRO := func() pgtoolboxv1alpha1.PgAdminServerStatus {
		t.Helper()
		var current pgtoolboxv1alpha1.PgAdmin
		must(t, admin.Get(ctx, request.NamespacedName, &current))
		for _, row := range current.Status.Servers {
			if row.Cluster == "orders" && row.Endpoint == "ro" {
				return row
			}
		}
		t.Fatalf("no status row for orders ro: %+v", current.Status.Servers)
		return pgtoolboxv1alpha1.PgAdminServerStatus{}
	}
	if row := ordersRO(); row.State != pgtoolboxv1alpha1.ServerStateNotOffered || row.Message != "no replica: the Cluster has one instance" {
		t.Fatalf("one instance: orders ro %+v", row)
	}
	var orders cnpgv1.Cluster
	must(t, admin.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "orders"}, &orders))
	orders.Spec.Instances = 2
	must(t, admin.Update(ctx, &orders))
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if row := ordersRO(); row.State != pgtoolboxv1alpha1.ServerStatePending {
		t.Fatalf("two instances: orders ro %+v, want offered", row)
	}
}

// TestEnvtestDatabaseRolesNotServed: on the CloudNativePG 1.30 CRDs without
// the DatabaseRole one (an install that lacks the kind; every supported
// CloudNativePG serves it), the start-time probe says NotInstalled and a
// PgAdmin created with no credentials (stored by the API server with the
// defaulted databaseRoles: true) is ConfigurationValid=True, offering no
// DatabaseRole. Regression: the stored default read as an explicit request.
func TestEnvtestDatabaseRolesNotServed(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set (make test-envtest-operator)")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "..", "config", "crd", "bases"),
			crdsWithout(t, filepath.Join("..", "..", "..", "test", "crds", "cnpg-1.30"), "postgresql.cnpg.io_databaseroles.yaml"),
		},
		ErrorIfCRDPathMissing: true,
	}
	restConfig, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	scheme := testScheme(t)
	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	roles, err := discovery.ProbeNamespaces(ctx, c, cnpgv1.SchemeGroupVersion.WithKind("DatabaseRole"), []string{testNamespace})
	if err != nil {
		t.Fatal(err)
	}
	if roles.State != discovery.NotInstalled {
		t.Fatalf("DatabaseRole probe without its CRD = %+v, want NotInstalled", roles)
	}

	must(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}))
	for _, name := range []string{"orders", "billing"} {
		must(t, c.Create(ctx, &cnpgv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec:       cnpgv1.ClusterSpec{Instances: 1, StorageConfiguration: cnpgv1.StorageConfiguration{Size: "1Gi"}},
		}))
	}
	must(t, c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pgadmin-oidc", Namespace: testNamespace},
		StringData: map[string]string{"client-secret": "x"},
	}))
	p := oauth2PgAdmin()
	p.ResourceVersion, p.UID, p.Generation = "", "", 0
	p.Spec.Storage.Size = new(resource.MustParse("1Gi"))
	p.Spec.Auth.OAuth2.RequiredClaims = map[string]pgtoolboxv1alpha1.ClaimValues{"groups": {"dba"}}
	p.Spec.Credentials = pgtoolboxv1alpha1.PgAdminCredentialsSpec{}
	must(t, c.Create(ctx, p))
	var stored pgtoolboxv1alpha1.PgAdmin
	must(t, c.Get(ctx, client.ObjectKeyFromObject(p), &stored))
	if d := stored.Spec.Credentials.DatabaseRoles; d == nil || !*d {
		t.Fatalf("the API server did not default spec.credentials.databaseRoles to true: %v", d)
	}

	cfg := configuration.Defaults()
	cfg.OperatorImage, cfg.PgAdminImage = testOperatorImage, testPgAdminImage
	r := &Reconciler{
		Client: c, APIReader: c, Scheme: scheme, Recorder: events.NewFakeRecorder(100), Config: cfg,
		Features: Features{DatabaseRoles: roles, Poolers: discovery.Result{State: discovery.Available}},
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(p)}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	var got pgtoolboxv1alpha1.PgAdmin
	must(t, c.Get(ctx, request.NamespacedName, &got))
	if cond := condition(&got, "ConfigurationValid"); cond.Status != metav1.ConditionTrue || !strings.Contains(cond.Message, "no DatabaseRole offered") {
		t.Fatalf("ConfigurationValid %+v", cond)
	}
	if cond := condition(&got, "Ready"); cond.Reason == string(pgtoolboxv1alpha1.ReasonDatabaseRoleAPIUnavailable) {
		t.Fatalf("Ready %+v", cond)
	}
}

// childVersions returns the resourceVersion of every object the PgAdmin
// owns, by kind/name.
func childVersions(t *testing.T, c client.Client) map[string]string {
	t.Helper()
	out := map[string]string{}
	selector := client.MatchingLabels{pgtoolboxv1alpha1.PgAdminLabelKey: "team"}
	lists := map[string]client.ObjectList{
		"Deployment": &appsv1.DeploymentList{}, "Service": &corev1.ServiceList{}, "ServiceAccount": &corev1.ServiceAccountList{},
		"ConfigMap": &corev1.ConfigMapList{}, "PVC": &corev1.PersistentVolumeClaimList{}, "Secret": &corev1.SecretList{},
		"Role": &rbacv1.RoleList{}, "RoleBinding": &rbacv1.RoleBindingList{},
	}
	for kind, list := range lists {
		must(t, c.List(context.Background(), list, client.InNamespace(testNamespace), selector))
		items := metaItems(t, list)
		for _, item := range items {
			out[kind+"/"+item.GetName()] = item.GetResourceVersion()
		}
	}
	if len(out) < 8 {
		t.Fatalf("expected the PgAdmin's children, found %v", out)
	}
	return out
}

// metaItems returns the metadata of every item of a typed list.
func metaItems(t *testing.T, list client.ObjectList) []metav1.Object {
	t.Helper()
	objects, err := meta.ExtractList(list)
	must(t, err)
	out := make([]metav1.Object, 0, len(objects))
	for _, object := range objects {
		accessor, err := meta.Accessor(object)
		must(t, err)
		out = append(out, accessor)
	}
	return out
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatal(err)
	}
}

// cnpgCRDs is the vendored CloudNativePG CRD directory the suite runs
// against: CNPG_CRDS (one test/crds/cnpg-<minor>, as the console's envtest
// target sets it per minor) or the newest vendored minor.
func cnpgCRDs() string {
	if dir := os.Getenv("CNPG_CRDS"); dir != "" {
		return dir
	}
	return filepath.Join("..", "..", "..", "test", "crds", "cnpg-1.30")
}

// crdsWithout copies the CRD files of dir, except the one named, into a
// temporary directory: a CloudNativePG install that lacks one kind.
func crdsWithout(t *testing.T, dir, omit string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	copied := false
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		if entry.Name() == omit {
			copied = true
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, entry.Name()), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if !copied {
		t.Fatalf("%s has no %s to leave out", dir, omit)
	}
	return out
}
