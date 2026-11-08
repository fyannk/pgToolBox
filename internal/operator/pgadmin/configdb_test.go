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
	"slices"
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/offer"
	"github.com/fyannk/pgToolBox/pkg/discovery"
)

func configPgAdmin() *pgtoolboxv1alpha1.PgAdmin {
	p := oauth2PgAdmin()
	p.Spec.ConfigDatabase = &pgtoolboxv1alpha1.PgAdminConfigDatabaseSpec{ClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: "billing"}}
	p.Spec.Replicas = new(int32(3))
	return p
}

// cnpg131 plays a CloudNativePG 1.31 API server for the DatabaseRole the
// operator applies: the vendored 1.30 types cannot carry spec.password, so
// the apply is recorded and the typed object created from it.
type cnpg131 struct {
	applied *unstructured.Unstructured
	refuse  bool
}

func (c *cnpg131) funcs() *interceptor.Funcs {
	return &interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok || u.GetKind() != "DatabaseRole" {
			return cl.Patch(ctx, obj, patch, opts...)
		}
		if c.refuse {
			return apierrors.NewBadRequest(`strict decoding error: unknown field "spec.password"`)
		}
		c.applied = u.DeepCopy()
		role := &cnpgv1.DatabaseRole{}
		if err := cl.Get(ctx, client.ObjectKeyFromObject(u), role); err == nil {
			return nil
		}
		role.ObjectMeta = metav1.ObjectMeta{Name: u.GetName(), Namespace: u.GetNamespace(), Labels: u.GetLabels(), OwnerReferences: u.GetOwnerReferences(), Generation: 1}
		role.Spec.ClusterRef = corev1.LocalObjectReference{Name: "billing"}
		return cl.Create(ctx, role)
	}}
}

func TestConfigDatabaseLifecycle(t *testing.T) {
	cnpg := &cnpg131{}
	h := newHarness(t, cnpg.funcs(), baseObjects(configPgAdmin())...)
	h.r.Features.Databases = discovery.Result{State: discovery.Available}

	// Loop 1: the role and the database are applied, nothing is ready:
	// no Pod, no settings volume.
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	got := h.pgadmin("team")
	expectCondition(t, got, "ConfigurationValid", metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)
	expectCondition(t, got, "WorkloadReady", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonConfigDatabaseNotReady)
	password, _, _ := unstructured.NestedMap(cnpg.applied.Object, "spec", "password")
	if password["mode"] != "generate" || password["secret"] != "team-pgadmin-config-password" {
		t.Fatalf("the role must ask CloudNativePG to generate its password into a named Secret: %v", password)
	}
	if name, _, _ := unstructured.NestedString(cnpg.applied.Object, "spec", "name"); name != "pgadmin_team" {
		t.Fatalf("role name %q", name)
	}
	var deployment appsv1.Deployment
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "team-pgadmin"}, &deployment); err != nil {
		t.Fatal(err)
	}
	if *deployment.Spec.Replicas != 0 {
		t.Fatalf("pgAdmin must not start before its settings database: %d replicas", *deployment.Spec.Replicas)
	}
	var pvc corev1.PersistentVolumeClaim
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "team-pgadmin"}, &pvc); !apierrors.IsNotFound(err) {
		t.Fatalf("no settings volume with configDatabase: %v", err)
	}
	if evs := strings.Join(h.events(), "\n"); strings.Contains(evs, "Warning ConfigDatabaseNotReady") {
		t.Fatalf("waiting for CloudNativePG is not a Warning: %s", evs)
	}

	// CloudNativePG applies the role (with its generated password) and the
	// database.
	var role cnpgv1.DatabaseRole
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "team-pgadmin-config"}, &role); err != nil {
		t.Fatal(err)
	}
	role.Status = cnpgv1.DatabaseRoleStatus{Applied: new(true), ObservedGeneration: role.Generation, SecretResourceVersion: "81"}
	if err := h.client.Update(context.Background(), &role); err != nil {
		t.Fatal(err)
	}
	var database cnpgv1.Database
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "team-pgadmin-config"}, &database); err != nil {
		t.Fatal(err)
	}
	if database.Spec.Owner != "pgadmin_team" || database.Spec.ClusterRef.Name != "billing" || database.Spec.ReclaimPolicy != cnpgv1.DatabaseReclaimRetain {
		t.Fatalf("database %+v", database.Spec)
	}
	database.Status = cnpgv1.DatabaseStatus{Applied: new(true), ObservedGeneration: database.Generation}
	if err := h.client.Update(context.Background(), &database); err != nil {
		t.Fatal(err)
	}
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	doc := h.offerDocument()
	if doc.ConfigDatabase == nil || doc.ConfigDatabase.SecretName != "team-pgadmin-config-password" ||
		doc.ConfigDatabase.SecretResourceVersion != "81" || doc.ConfigDatabase.User != "pgadmin_team" {
		t.Fatalf("the offer must carry the settings database connection: %+v", doc.ConfigDatabase)
	}
	for _, s := range doc.Servers {
		if s.User == "pgadmin_team" {
			t.Fatalf("the settings database role is never a server: %+v", s)
		}
	}
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "team-pgadmin"}, &deployment); err != nil {
		t.Fatal(err)
	}
	pod := deployment.Spec.Template.Spec
	if *deployment.Spec.Replicas != 1 {
		t.Fatalf("one Pod creates the settings schema: %d replicas", *deployment.Spec.Replicas)
	}
	var uri string
	for _, e := range pod.Containers[0].Env {
		if e.Name == "PGADMIN_CONFIG_CONFIG_DATABASE_URI" {
			uri = e.Value
		}
	}
	want := "'postgresql://pgadmin_team@billing-rw.team-a.svc:5432/pgadmin_team?sslmode=verify-full&sslrootcert=%2Frun%2Fpgadmin%2Fcredentials%2Fca%2Fbilling.crt'"
	if uri != want {
		t.Fatalf("CONFIG_DATABASE_URI = %s, want %s (no password in it)", uri, want)
	}
	if len(pod.Containers) != 1 || len(pod.InitContainers) != 2 || pod.InitContainers[1].Name != "passfile-sync" ||
		pod.InitContainers[1].RestartPolicy == nil || *pod.InitContainers[1].RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Fatal("passfile-sync must be a native sidecar that starts before pgAdmin")
	}
	if pod.Volumes[0].Name != settingsVolume || pod.Volumes[0].EmptyDir == nil {
		t.Fatalf("DATA_DIR is a per-Pod emptyDir with configDatabase: %+v", pod.Volumes[0])
	}
	var service corev1.Service
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "team-pgadmin"}, &service); err != nil {
		t.Fatal(err)
	}
	if service.Spec.SessionAffinity != corev1.ServiceAffinityClientIP {
		t.Fatal("several replicas need a sticky Service")
	}
	credentials := h.credentialsRoleNames("team")
	if !slices.Contains(credentials, "team-pgadmin-config-password") || !slices.Contains(credentials, "billing-ca") {
		t.Fatalf("the Pod's Role must allow the settings database's password and CA: %v", credentials)
	}

	// pgAdmin runs and registers once: spec.replicas follow.
	h.markPodReady()
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	job := h.finishJob(componentRegistration, false, `{"pgAdminVersion":"9.18","servers":3}`)
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Command, " ")
	if !strings.Contains(args, "--config-database-uri postgresql://pgadmin_team@") || !strings.Contains(args, "--credentials-dir "+credentialsMountPath) {
		t.Fatalf("the registration Job opens the settings database like pgAdmin: %s", args)
	}
	if job.Spec.Template.Spec.Affinity != nil {
		t.Fatal("no settings volume, no affinity to the pgAdmin Pod")
	}
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "team-pgadmin"}, &deployment); err != nil {
		t.Fatal(err)
	}
	if *deployment.Spec.Replicas != 3 {
		t.Fatalf("spec.replicas once the schema exists: %d", *deployment.Spec.Replicas)
	}
}

func (h *harness) credentialsRoleNames(name string) []string {
	h.t.Helper()
	var role rbacv1.Role
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name + "-pgadmin-credentials"}, &role); err != nil {
		h.t.Fatal(err)
	}
	return role.Rules[0].ResourceNames
}

func TestConfigDatabaseWithoutPasswordGeneration(t *testing.T) {
	cnpg := &cnpg131{refuse: true}
	h := newHarness(t, cnpg.funcs(), baseObjects(configPgAdmin())...)
	h.r.Features.Databases = discovery.Result{State: discovery.Available}
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	got := h.pgadmin("team")
	expectCondition(t, got, "ConfigurationValid", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonPasswordGenerationUnavailable)
	expectCondition(t, got, "Stalled", metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonPasswordGenerationUnavailable)
}

func TestConfigDatabaseWithoutTheDatabaseAPI(t *testing.T) {
	h := newHarness(t, nil, baseObjects(configPgAdmin())...)
	h.r.Features.Databases = discovery.Result{State: discovery.NotInstalled, Reason: "Database.postgresql.cnpg.io is not served"}
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	expectCondition(t, h.pgadmin("team"), "ConfigurationValid", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonDatabaseAPIUnavailable)
}

func TestDesiredReplicas(t *testing.T) {
	ready := &buildInputs{Image: testInputs(oauth2PgAdmin(), false).Image}
	ready.ConfigDatabase.Server = &offer.Server{}
	cases := []struct {
		name  string
		p     func() *pgtoolboxv1alpha1.PgAdmin
		in    *buildInputs
		wantN int32
	}{
		{"sqlite", oauth2PgAdmin, ready, 1},
		{"config db not ready", configPgAdmin, &buildInputs{}, 0},
		{"first registration pending", configPgAdmin, ready, 1},
		{"upgrade", func() *pgtoolboxv1alpha1.PgAdmin {
			p := configPgAdmin()
			p.Status.Image, p.Status.RegisteredHash = "old", "h"
			return p
		}, ready, 1},
		{"steady", func() *pgtoolboxv1alpha1.PgAdmin {
			p := configPgAdmin()
			p.Status.Image, p.Status.RegisteredHash = ready.Image.Reference, "h"
			return p
		}, ready, 3},
	}
	for _, tc := range cases {
		if got := desiredReplicas(tc.p(), tc.in); got != tc.wantN {
			t.Errorf("%s: %d replicas, want %d", tc.name, got, tc.wantN)
		}
	}
}

func TestConfigDatabaseNeverTakesOverARole(t *testing.T) {
	for name, mutate := range map[string]func(*pgtoolboxv1alpha1.PgAdmin) []client.Object{
		"the application owner": func(p *pgtoolboxv1alpha1.PgAdmin) []client.Object {
			p.Spec.ConfigDatabase.Database = "app"
			return nil
		},
		"another DatabaseRole's role": func(p *pgtoolboxv1alpha1.PgAdmin) []client.Object {
			other := databaseRole("someone", "billing", "x")
			other.Namespace, other.Spec.Name = testNamespace, "pgadmin_team"
			return []client.Object{other}
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := configPgAdmin()
			extra := mutate(p)
			cnpg := &cnpg131{}
			h := newHarness(t, cnpg.funcs(), append(baseObjects(p), extra...)...)
			h.r.Features.Databases = discovery.Result{State: discovery.Available}
			if err := h.reconcile("team"); err != nil {
				t.Fatal(err)
			}
			expectCondition(t, h.pgadmin("team"), "ConfigurationValid", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonConfigurationInvalid)
			if cnpg.applied != nil {
				t.Fatal("no DatabaseRole may be applied over another role")
			}
		})
	}
}

func TestConfigDatabaseURILiteralIsTheOperatorsValue(t *testing.T) {
	const uri = "postgresql://pgadmin:p%40ss@orders-rw:5432/pgadmin?sslmode=verify-full&sslrootcert=system"
	for _, in := range []string{uri, uri + "\n", "  " + uri + "\r\n"} {
		got, err := ConfigDatabaseURILiteral(in)
		if err != nil || got != pyString(uri) {
			t.Fatalf("ConfigDatabaseURILiteral(%q) = %q, %v; want %q", in, got, err, pyString(uri))
		}
	}
	if got, _ := ConfigDatabaseURILiteral("postgresql://u:it's\\x@db/d"); got != `'postgresql://u:it\'s\\x@db/d'` {
		t.Fatalf("quote and backslash must be escaped: %s", got)
	}
	for _, bad := range []string{"", " \n", "postgresql://a\nb", "postgresql://a\x00b"} {
		if _, err := ConfigDatabaseURILiteral(bad); err == nil {
			t.Fatalf("%q must be refused", bad)
		}
	}
}
