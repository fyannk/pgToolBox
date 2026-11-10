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
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

// rbacEnv is an API server with the generated CRDs and CloudNativePG 1.30's,
// an admin client and the operator identity (the generated manager
// ClusterRole and nothing else).
type rbacEnv struct {
	admin, operator client.Client
	reconciler      *Reconciler
}

func startRBACEnv(t *testing.T) *rbacEnv {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set (make test-envtest-operator)")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases"), cnpgCRDs()},
		ErrorIfCRDPathMissing: true,
	}
	// OpenShift enables OwnerReferencesPermissionEnforcement: an owner
	// reference with blockOwnerDeletion is admitted only from whoever may
	// update the owner's finalizers. kind does not enable it, so it is
	// enabled here, where the operator identity creates every child.
	env.ControlPlane.GetAPIServer().Configure().Append("enable-admission-plugins", "OwnerReferencesPermissionEnforcement")
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
	roleYAML, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var managerRole rbacv1.ClusterRole
	must(t, yaml.Unmarshal(roleYAML, &managerRole))
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
	for _, namespace := range []string{testNamespace, remoteNamespace} {
		must(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))
	}
	must(t, admin.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pgadmin-oidc", Namespace: testNamespace},
		StringData: map[string]string{"client-secret": "x"},
	}))
	cfg := configuration.Defaults()
	cfg.OperatorImage, cfg.PgAdminImage = testOperatorImage, testPgAdminImage
	return &rbacEnv{admin: admin, operator: operator, reconciler: &Reconciler{
		Client: operator, APIReader: operator, Scheme: scheme, Recorder: events.NewFakeRecorder(100), Config: cfg,
		Features: Features{
			DatabaseRoles: discovery.Result{State: discovery.Available}, Poolers: discovery.Result{State: discovery.Available},
			Databases: discovery.Result{State: discovery.Available},
		},
	}}
}

func (e *rbacEnv) cluster(t *testing.T, namespace, name string) {
	t.Helper()
	must(t, e.admin.Create(context.Background(), &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       cnpgv1.ClusterSpec{Instances: 1, StorageConfiguration: cnpgv1.StorageConfiguration{Size: "1Gi"}},
	}))
}

func (e *rbacEnv) pgAdmin(t *testing.T, mutate func(*pgtoolboxv1alpha1.PgAdmin)) types.NamespacedName {
	t.Helper()
	p := oauth2PgAdmin()
	p.ResourceVersion, p.UID, p.Generation = "", "", 0
	p.Spec.ClusterRefs = []pgtoolboxv1alpha1.LocalObjectReference{{Name: "orders"}}
	p.Spec.Storage.Size = new(resource.MustParse("1Gi"))
	p.Spec.Auth.OAuth2.RequiredClaims = map[string]pgtoolboxv1alpha1.ClaimValues{"groups": {"dba"}}
	mutate(p)
	must(t, e.admin.Create(context.Background(), p))
	return client.ObjectKeyFromObject(p)
}

func (e *rbacEnv) reconcile(t *testing.T, key types.NamespacedName) *pgtoolboxv1alpha1.PgAdmin {
	t.Helper()
	if _, err := e.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile as the operator identity: %v", err)
	}
	var got pgtoolboxv1alpha1.PgAdmin
	must(t, e.admin.Get(context.Background(), key, &got))
	return &got
}

// can asks the API server whether the pgAdmin Pod's ServiceAccount may do
// verb on a Secret of namespace (name "" for the collection).
func (e *rbacEnv) can(t *testing.T, verb, namespace, name string) bool {
	t.Helper()
	review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User:   "system:serviceaccount:" + testNamespace + ":team-pgadmin",
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + testNamespace},
		ResourceAttributes: &authorizationv1.ResourceAttributes{
			Namespace: namespace, Verb: verb, Resource: "secrets", Name: name,
		},
	}}
	must(t, e.admin.Create(context.Background(), review))
	return review.Status.Allowed
}

// TestEnvtestCrossNamespaceGrant: the grant is what lets the pgAdmin Pod
// read another namespace's offered credentials, as the API server's RBAC
// decides (and the operator may write that Role: escalation prevention).
func TestEnvtestCrossNamespaceGrant(t *testing.T) {
	e := startRBACEnv(t)
	ctx := context.Background()
	e.cluster(t, testNamespace, "orders")
	e.cluster(t, remoteNamespace, "payments")
	key := e.pgAdmin(t, func(p *pgtoolboxv1alpha1.PgAdmin) {
		p.Spec.CrossNamespaceClusterRefs = []pgtoolboxv1alpha1.PgAdminClusterReference{{Namespace: remoteNamespace, Name: "payments"}}
	})

	got := e.reconcile(t, key)
	if c := condition(got, "ConfigurationValid"); c.Reason != string(pgtoolboxv1alpha1.ReasonGrantMissing) {
		t.Fatalf("ConfigurationValid %+v", c)
	}
	if e.can(t, "get", remoteNamespace, "payments-app") {
		t.Fatal("without a grant the pgAdmin ServiceAccount must not read the other namespace's Secrets")
	}

	// CEL on the grant.
	invalid := grant("invalid", key)
	invalid.ResourceVersion, invalid.UID = "", ""
	if err := e.admin.Create(ctx, invalid); err == nil {
		t.Fatal("a grant without Clusters must be refused")
	}
	g := grant("pgadmin-team", key, "payments")
	g.ResourceVersion, g.UID = "", ""
	must(t, e.admin.Create(ctx, g))

	got = e.reconcile(t, key)
	if c := condition(got, "ConfigurationValid"); c.Status != metav1.ConditionTrue {
		t.Fatalf("ConfigurationValid %+v", c)
	}
	if !e.can(t, "get", remoteNamespace, "payments-app") || !e.can(t, "watch", remoteNamespace, "payments-ca") {
		t.Fatal("the grant must let the pgAdmin ServiceAccount get and watch the granted Cluster's offered Secrets")
	}
	for _, denied := range [][3]string{
		{"get", remoteNamespace, "payments-superuser"}, {"list", remoteNamespace, ""}, {"get", remoteNamespace, "other"},
	} {
		if e.can(t, denied[0], denied[1], denied[2]) {
			t.Fatalf("the grant allows %v", denied)
		}
	}

	// A no-op loop writes nothing in the grant's namespace either.
	var role, again rbacv1.Role
	roleKey := client.ObjectKey{Namespace: remoteNamespace, Name: "pgadmin-team-pgadmin-access"}
	must(t, e.admin.Get(ctx, roleKey, &role))
	e.reconcile(t, key)
	e.reconcile(t, key)
	must(t, e.admin.Get(ctx, roleKey, &again))
	if again.ResourceVersion != role.ResourceVersion {
		t.Errorf("a no-op reconcile rewrote the access Role (%s -> %s)", role.ResourceVersion, again.ResourceVersion)
	}
}

// TestEnvtestDeletedPgAdminRevokesItsRemoteAccess: the access Role and
// RoleBinding are controlled by the grant, so nothing would delete them
// with the PgAdmin (envtest runs no garbage collector either): the
// operator, as its generated ClusterRole, deletes them itself, and the API
// server then denies the read.
func TestEnvtestDeletedPgAdminRevokesItsRemoteAccess(t *testing.T) {
	e := startRBACEnv(t)
	ctx := context.Background()
	e.cluster(t, testNamespace, "orders")
	e.cluster(t, remoteNamespace, "payments")
	key := e.pgAdmin(t, func(p *pgtoolboxv1alpha1.PgAdmin) {
		p.Spec.CrossNamespaceClusterRefs = []pgtoolboxv1alpha1.PgAdminClusterReference{{Namespace: remoteNamespace, Name: "payments"}}
	})
	g := grant("pgadmin-team", key, "payments")
	g.ResourceVersion, g.UID = "", ""
	must(t, e.admin.Create(ctx, g))

	e.reconcile(t, key)
	accessKey := client.ObjectKey{Namespace: remoteNamespace, Name: "pgadmin-team-pgadmin-access"}
	var role rbacv1.Role
	var binding rbacv1.RoleBinding
	must(t, e.admin.Get(ctx, accessKey, &role))
	must(t, e.admin.Get(ctx, accessKey, &binding))
	if !e.can(t, "get", remoteNamespace, "payments-app") {
		t.Fatal("precondition: the grant must let the pgAdmin ServiceAccount read the offered Secret")
	}

	var p pgtoolboxv1alpha1.PgAdmin
	must(t, e.admin.Get(ctx, key, &p))
	must(t, e.admin.Delete(ctx, &p))
	if _, err := e.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile of the deleted PgAdmin as the operator identity: %v", err)
	}
	if err := e.admin.Get(ctx, accessKey, &role); !apierrors.IsNotFound(err) {
		t.Fatalf("the deleted PgAdmin's Role is still there (%v): %v", err, role.Rules)
	}
	if err := e.admin.Get(ctx, accessKey, &binding); !apierrors.IsNotFound(err) {
		t.Fatalf("the deleted PgAdmin's RoleBinding is still there: %v", err)
	}
	if e.can(t, "get", remoteNamespace, "payments-app") {
		t.Fatal("a re-created ServiceAccount of that name would still read the granted Secret")
	}
	var kept pgtoolboxv1alpha1.PgAdminAccess
	must(t, e.admin.Get(ctx, client.ObjectKeyFromObject(g), &kept))
}

// TestEnvtestConfigDatabase: CloudNativePG 1.30's DatabaseRole has no
// spec.password, which strict field validation reports, and the operator
// stalls instead of holding a password; once the API carries it (1.31),
// the operator, with its generated Role only, writes the DatabaseRole and
// the Database and holds pgAdmin at zero Pods until CloudNativePG applied
// both.
func TestEnvtestConfigDatabase(t *testing.T) {
	e := startRBACEnv(t)
	ctx := context.Background()
	e.cluster(t, testNamespace, "orders")
	e.cluster(t, testNamespace, "billing")
	key := e.pgAdmin(t, func(p *pgtoolboxv1alpha1.PgAdmin) {
		p.Spec.ConfigDatabase = &pgtoolboxv1alpha1.PgAdminConfigDatabaseSpec{ClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: "billing"}}
		p.Spec.Replicas = new(int32(2))
	})

	got := e.reconcile(t, key)
	if c := condition(got, "ConfigurationValid"); c.Reason != string(pgtoolboxv1alpha1.ReasonPasswordGenerationUnavailable) {
		t.Fatalf("on CloudNativePG 1.30, ConfigurationValid %+v", c)
	}
	if c := condition(got, "Stalled"); c.Status != metav1.ConditionTrue {
		t.Fatalf("Stalled %+v", c)
	}

	// The DatabaseRole API gains spec.password (CloudNativePG 1.31).
	crd := &unstructured.Unstructured{}
	crd.SetAPIVersion("apiextensions.k8s.io/v1")
	crd.SetKind("CustomResourceDefinition")
	must(t, e.admin.Get(ctx, client.ObjectKey{Name: "databaseroles.postgresql.cnpg.io"}, crd))
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	for i := range versions {
		version := versions[i].(map[string]any)
		must(t, unstructured.SetNestedField(version, map[string]any{"type": "object", "x-kubernetes-preserve-unknown-fields": true},
			"schema", "openAPIV3Schema", "properties", "spec", "properties", "password"))
		versions[i] = version
	}
	must(t, unstructured.SetNestedSlice(crd.Object, versions, "spec", "versions"))
	must(t, e.admin.Update(ctx, crd))

	deadline := time.Now().Add(30 * time.Second)
	for {
		got = e.reconcile(t, key)
		if c := condition(got, "ConfigurationValid"); c.Status == metav1.ConditionTrue {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("the operator never applied the DatabaseRole: %+v", c)
		}
		time.Sleep(time.Second)
	}
	if c := condition(got, "WorkloadReady"); c.Reason != string(pgtoolboxv1alpha1.ReasonConfigDatabaseNotReady) {
		t.Fatalf("WorkloadReady %+v", c)
	}
	role := &unstructured.Unstructured{}
	role.SetAPIVersion(cnpgv1.SchemeGroupVersion.String())
	role.SetKind("DatabaseRole")
	must(t, e.admin.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "team-pgadmin-config"}, role))
	if mode, _, _ := unstructured.NestedString(role.Object, "spec", "password", "mode"); mode != "generate" {
		t.Fatalf("the DatabaseRole must ask CloudNativePG for a generated password: %v", role.Object["spec"])
	}
	var database cnpgv1.Database
	must(t, e.admin.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "team-pgadmin-config"}, &database))
	if database.Spec.Owner != "pgadmin_team" {
		t.Fatalf("database %+v", database.Spec)
	}
	var deployment appsv1.Deployment
	must(t, e.admin.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "team-pgadmin"}, &deployment))
	if *deployment.Spec.Replicas != 0 || deployment.Spec.Template.Spec.Volumes[0].EmptyDir == nil {
		t.Fatalf("held at zero Pods, DATA_DIR an emptyDir: replicas %d, %+v", *deployment.Spec.Replicas, deployment.Spec.Template.Spec.Volumes[0])
	}
	var pvcs corev1.PersistentVolumeClaimList
	must(t, e.admin.List(ctx, &pvcs, client.InNamespace(testNamespace)))
	if len(pvcs.Items) != 0 {
		t.Fatalf("no settings volume with configDatabase: %v", pvcs.Items)
	}

	// CEL: configDatabase cannot be removed, replicas need it.
	got.Spec.ConfigDatabase = nil
	if err := e.admin.Update(ctx, got); err == nil {
		t.Fatal("removing configDatabase must be refused")
	}
	sqlite := oauth2PgAdmin()
	sqlite.Name, sqlite.ResourceVersion, sqlite.UID = "solo", "", ""
	sqlite.Spec.Replicas = new(int32(2))
	if err := e.admin.Create(ctx, sqlite); err == nil {
		t.Fatal("replicas above 1 without configDatabase must be refused")
	}
	if !slices.Contains([]int32{2}, *got.Spec.Replicas) {
		t.Fatalf("replicas %d", *got.Spec.Replicas)
	}
}
