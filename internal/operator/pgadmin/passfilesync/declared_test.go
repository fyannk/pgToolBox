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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/declared"
)

// ordersCluster is the Cluster "orders" the declared lists name, with an
// initdb bootstrap (owner shop_owner, database shop) and two instances, so
// its ro Service has a replica behind it.
func ordersCluster(mutate ...func(*cnpgv1.Cluster)) *cnpgv1.Cluster {
	c := &cnpgv1.Cluster{
		TypeMeta:   metav1.TypeMeta{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster"},
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "team"},
		Spec: cnpgv1.ClusterSpec{Instances: 2, Bootstrap: &cnpgv1.BootstrapConfiguration{
			InitDB: &cnpgv1.BootstrapInitDB{Database: "shop", Owner: "shop_owner"},
		}},
	}
	for _, m := range mutate {
		m(c)
	}
	return c
}

func declaredRole(name, cluster, secret, version string, mutate ...func(*cnpgv1.DatabaseRole)) *cnpgv1.DatabaseRole {
	applied := true
	r := &cnpgv1.DatabaseRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "postgresql.cnpg.io/v1", Kind: "DatabaseRole"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team", Generation: 1},
		Spec: cnpgv1.DatabaseRoleSpec{
			ClusterRef:        corev1.LocalObjectReference{Name: cluster},
			RoleConfiguration: cnpgv1.RoleConfiguration{Name: "reporting", PasswordSecret: &cnpgv1.LocalObjectReference{Name: secret}},
		},
		Status: cnpgv1.DatabaseRoleStatus{Applied: &applied, ObservedGeneration: 1, SecretResourceVersion: version},
	}
	for _, m := range mutate {
		m(r)
	}
	return r
}

// ordersPool is the Pooler "orders-pool" of the Cluster "orders".
func ordersPool() *cnpgv1.Pooler {
	return &cnpgv1.Pooler{
		TypeMeta:   metav1.TypeMeta{APIVersion: "postgresql.cnpg.io/v1", Kind: "Pooler"},
		ObjectMeta: metav1.ObjectMeta{Name: "orders-pool", Namespace: "team"},
		Spec:       cnpgv1.PoolerSpec{Cluster: cnpgv1.LocalObjectReference{Name: "orders"}},
	}
}

func liveState() Live {
	return Live{
		Clusters: map[string]*cnpgv1.Cluster{"orders": ordersCluster()},
		DatabaseRoles: map[string]*cnpgv1.DatabaseRole{
			"orders-reporting": declaredRole("orders-reporting", "orders", "reporting-pw", "42"),
		},
		Poolers: map[string]*cnpgv1.Pooler{"orders-pool": ordersPool()},
		Secrets: map[string]*corev1.Secret{
			"orders-ca":        caSecret("orders-ca", caPEM1),
			"orders-app":       secret("orders-app", "5", map[string]string{"username": "shop_owner", "password": "pw-app"}),
			"orders-superuser": secret("orders-superuser", "6", map[string]string{"username": "postgres", "password": "pw-root"}),
			"reporting-pw":     secret("reporting-pw", "42", map[string]string{"username": "reporting", "password": "pw-report"}),
			"billing-owner":    secret("billing-owner", "9", map[string]string{"username": "billing_owner", "password": "pw-billing"}),
			"pgpass-only":      secret("pgpass-only", "9", map[string]string{"pgpass": "h:5432:*:other:nope\nh:5432:*:billing_owner:pw\\:pgpass\n"}),
		},
		CAConfigMaps: map[string][]byte{"orders-ca-crt": []byte(caPEM2)},
	}
}

func app(secret string) declared.Credentials {
	return declared.Credentials{Kind: declared.KindApplication, Secret: secret}
}

// TestResolveEachKind: each kind of credential, checked against live
// state, yields the user, database and password the Cluster or the role
// designates, or a reason by name.
func TestResolveEachKind(t *testing.T) {
	enabled := true
	cases := []struct {
		name     string
		server   declared.Server
		mutate   func(*Live)
		user, db string
		password string
		// reason, when set, is a substring of the connection's reason;
		// noSection says whether the server keeps no section at all.
		reason    string
		noSection bool
		// withheld: the Cluster cannot serve the endpoint, so the server
		// is not registered.
		withheld bool
	}{
		{
			name: "application", server: declared.Server{Cluster: "orders", Endpoint: "rw", Credentials: app("orders-app")},
			user: "shop_owner", db: "shop", password: "pw-app",
		},
		{
			name: "application through a pooler", server: declared.Server{Cluster: "orders", Endpoint: "pooler:orders-pool", Credentials: app("orders-app")},
			user: "shop_owner", db: "shop", password: "pw-app",
		},
		{
			name: "application with initdb.secret", server: declared.Server{Cluster: "orders", Endpoint: "rw", Credentials: app("mine")},
			mutate: func(l *Live) {
				l.Clusters["orders"].Spec.Bootstrap.InitDB.Secret = &cnpgv1.LocalObjectReference{Name: "mine"}
				l.Secrets["mine"] = secret("mine", "3", map[string]string{"username": "shop_owner", "password": "pw-mine"})
			},
			user: "shop_owner", db: "shop", password: "pw-mine",
		},
		{
			name: "application Secret not the Cluster's", server: declared.Server{Cluster: "orders", Endpoint: "rw", Credentials: app("billing-owner")},
			user: "shop_owner", db: "shop", reason: "the Cluster's application Secret is orders-app, not billing-owner",
		},
		{
			name: "application recreated with initdb.secret", server: declared.Server{Cluster: "orders", Endpoint: "rw", Credentials: app("orders-app")},
			mutate: func(l *Live) {
				l.Clusters["orders"].Spec.Bootstrap.InitDB.Secret = &cnpgv1.LocalObjectReference{Name: "mine"}
			},
			user: "shop_owner", db: "shop", reason: "application Secret is mine, not orders-app",
		},
		{
			name: "application with another declared user", server: declared.Server{Cluster: "orders", Endpoint: "rw", User: "alice", Credentials: app("orders-app")},
			user: "alice", db: "shop", reason: "names another role than alice",
		},
		{
			name: "superuser disabled", server: declared.Server{Cluster: "orders", Endpoint: "rw", Credentials: declared.Credentials{Kind: declared.KindSuperuser, Secret: "orders-superuser"}},
			user: "postgres", db: "postgres", reason: "does not enable superuser access",
		},
		{
			name: "superuser enabled", server: declared.Server{Cluster: "orders", Endpoint: "rw", Credentials: declared.Credentials{Kind: declared.KindSuperuser, Secret: "orders-superuser"}},
			mutate: func(l *Live) { l.Clusters["orders"].Spec.EnableSuperuserAccess = &enabled },
			user:   "postgres", db: "postgres", password: "pw-root",
		},
		{
			name: "superuser Secret not the Cluster's", server: declared.Server{Cluster: "orders", Endpoint: "rw", Credentials: declared.Credentials{Kind: declared.KindSuperuser, Secret: "orders-app"}},
			mutate: func(l *Live) { l.Clusters["orders"].Spec.EnableSuperuserAccess = &enabled },
			user:   "postgres", db: "postgres", reason: "superuser Secret is orders-superuser, not orders-app",
		},
		{
			name: "databaseRole", server: declared.Server{Cluster: "orders", Endpoint: "ro", Credentials: declared.Credentials{Kind: declared.KindDatabaseRole, Role: "orders-reporting", Secret: "reporting-pw"}},
			user: "reporting", db: "postgres", password: "pw-report",
		},
		{
			name: "databaseRole not applied", server: declared.Server{Cluster: "orders", Endpoint: "ro", Credentials: declared.Credentials{Kind: declared.KindDatabaseRole, Role: "orders-reporting", Secret: "reporting-pw"}},
			mutate: func(l *Live) { l.DatabaseRoles["orders-reporting"].Status.Applied = nil },
			user:   "reporting", db: "postgres", reason: "not applied by CloudNativePG yet",
		},
		{
			name: "databaseRole at an older generation", server: declared.Server{Cluster: "orders", Endpoint: "ro", Credentials: declared.Credentials{Kind: declared.KindDatabaseRole, Role: "orders-reporting", Secret: "reporting-pw"}},
			mutate: func(l *Live) { l.DatabaseRoles["orders-reporting"].Generation = 2 },
			user:   "reporting", db: "postgres", reason: "current generation",
		},
		{
			name: "databaseRole Secret newer than applied", server: declared.Server{Cluster: "orders", Endpoint: "ro", Credentials: declared.Credentials{Kind: declared.KindDatabaseRole, Role: "orders-reporting", Secret: "reporting-pw"}},
			mutate: func(l *Live) { l.Secrets["reporting-pw"].ResourceVersion = "43" },
			user:   "reporting", db: "postgres", reason: "not at the version CloudNativePG applied",
		},
		{
			name: "databaseRole names another Secret", server: declared.Server{Cluster: "orders", Endpoint: "ro", Credentials: declared.Credentials{Kind: declared.KindDatabaseRole, Role: "orders-reporting", Secret: "orders-app"}},
			user: "reporting", db: "postgres", reason: "password Secret is reporting-pw, not orders-app",
		},
		{
			name: "databaseRole of another Cluster", server: declared.Server{Cluster: "orders", Endpoint: "ro", Credentials: declared.Credentials{Kind: declared.KindDatabaseRole, Role: "orders-reporting", Secret: "reporting-pw"}},
			mutate: func(l *Live) { l.DatabaseRoles["orders-reporting"].Spec.ClusterRef.Name = "billing" },
			reason: "belongs to Cluster billing", noSection: true,
		},
		{
			name: "databaseRole absent, user unknown", server: declared.Server{Cluster: "orders", Endpoint: "ro", Credentials: declared.Credentials{Kind: declared.KindDatabaseRole, Role: "gone", Secret: "reporting-pw"}},
			db: "postgres", reason: "DatabaseRole gone is absent", noSection: true,
		},
		{
			name: "databaseRole absent, user declared", server: declared.Server{Cluster: "orders", Endpoint: "ro", User: "reporting", Credentials: declared.Credentials{Kind: declared.KindDatabaseRole, Role: "gone", Secret: "reporting-pw"}},
			user: "reporting", db: "postgres", reason: "DatabaseRole gone is absent",
		},
		{
			name: "a Secret you manage", server: declared.Server{Cluster: "orders", Endpoint: "rw", User: "billing_owner", Database: "billing", Credentials: declared.Credentials{Kind: declared.KindSecret, Secret: "billing-owner"}},
			user: "billing_owner", db: "billing", password: "pw-billing",
		},
		{
			name: "a pgpass Secret you manage", server: declared.Server{Cluster: "orders", Endpoint: "rw", User: "billing_owner", Database: "billing", Credentials: declared.Credentials{Kind: declared.KindSecret, Secret: "pgpass-only"}},
			user: "billing_owner", db: "billing", password: "pw:pgpass",
		},
		{
			name: "a Secret naming another user", server: declared.Server{Cluster: "orders", Endpoint: "rw", User: "alice", Database: "billing", Credentials: declared.Credentials{Kind: declared.KindSecret, Secret: "billing-owner"}},
			user: "alice", db: "billing", reason: "names another role than alice",
		},
		{
			name: "a missing Secret", server: declared.Server{Cluster: "orders", Endpoint: "rw", User: "alice", Database: "app", Credentials: declared.Credentials{Kind: declared.KindSecret, Secret: "nope"}},
			user: "alice", db: "app", reason: "Secret nope is absent or not readable",
		},
		{
			name: "none", server: declared.Server{Cluster: "orders", Endpoint: "rw", User: "alice", Database: "app", Credentials: declared.Credentials{Kind: declared.KindNone}},
			user: "alice", db: "app",
		},
		{
			name: "Cluster absent", server: declared.Server{Cluster: "legacy", Endpoint: "rw", User: "alice", Database: "app", Credentials: declared.Credentials{Kind: declared.KindNone}},
			user: "alice", db: "app", reason: "Cluster legacy is absent", noSection: true,
		},
		{
			name: "Pooler of another Cluster", server: declared.Server{Cluster: "orders", Endpoint: "pooler:orders-pool", Credentials: app("orders-app")},
			mutate: func(l *Live) { l.Poolers["orders-pool"].Spec.Cluster.Name = "billing" },
			reason: "Pooler orders-pool belongs to Cluster billing", noSection: true,
		},
		{
			name: "Pooler absent", server: declared.Server{Cluster: "orders", Endpoint: "pooler:nope", Credentials: app("orders-app")},
			reason: "Pooler nope is absent", noSection: true,
		},
		{
			name: "CA Secret not the Cluster's", server: declared.Server{Cluster: "orders", Endpoint: "rw", Credentials: app("orders-app")},
			mutate: func(l *Live) {
				l.Clusters["orders"].Spec.Certificates = &cnpgv1.CertificatesConfiguration{ServerCASecret: "my-ca"}
			},
			reason: "server CA is in Secret my-ca, not orders-ca", noSection: true,
		},
		{
			name: "ro on a one-instance Cluster", server: declared.Server{Cluster: "orders", Endpoint: "ro", Credentials: app("orders-app")},
			mutate: func(l *Live) { l.Clusters["orders"].Spec.Instances = 1 },
			reason: "no replica: the Cluster has one instance", noSection: true, withheld: true,
		},
		{
			name: "ro on a two-instance Cluster", server: declared.Server{Cluster: "orders", Endpoint: "ro", Credentials: app("orders-app")},
			user: "shop_owner", db: "shop", password: "pw-app",
		},
		{
			name: "r on a one-instance Cluster", server: declared.Server{Cluster: "orders", Endpoint: "r", Credentials: app("orders-app")},
			mutate: func(l *Live) { l.Clusters["orders"].Spec.Instances = 1 },
			user:   "shop_owner", db: "shop", password: "pw-app",
		},
		{
			name: "r Service disabled", server: declared.Server{Cluster: "orders", Endpoint: "r", Credentials: app("orders-app")},
			mutate: func(l *Live) {
				l.Clusters["orders"].Spec.Managed = &cnpgv1.ManagedConfiguration{Services: &cnpgv1.ManagedServices{
					DisabledDefaultServices: []cnpgv1.ServiceSelectorType{cnpgv1.ServiceSelectorTypeR},
				}}
			},
			reason: "the Cluster disabled its r Service", noSection: true, withheld: true,
		},
		{
			name: "ro of an absent Cluster is not withheld", server: declared.Server{Cluster: "gone", Endpoint: "ro", User: "a", Database: "b", Credentials: declared.Credentials{Kind: declared.KindNone}},
			reason: "Cluster gone is absent", noSection: true,
		},
		{
			name: "CA Secret absent", server: declared.Server{Cluster: "orders", Endpoint: "rw", Credentials: app("orders-app")},
			mutate: func(l *Live) { delete(l.Secrets, "orders-ca") },
			reason: "CA Secret orders-ca is absent", noSection: true,
		},
		{
			// The Pod may not read the CA Secret: nothing to compare with.
			name: "CA ConfigMap, no CA Secret readable", server: declared.Server{Cluster: "orders", Endpoint: "rw", CAConfigMap: "orders-ca-crt", Credentials: app("orders-app")},
			mutate: func(l *Live) { delete(l.Secrets, "orders-ca") },
			user:   "shop_owner", db: "shop", password: "pw-app",
		},
		{
			name: "CA ConfigMap with a user-managed serverCASecret", server: declared.Server{Cluster: "orders", Endpoint: "rw", CAConfigMap: "orders-ca-crt", Credentials: app("orders-app")},
			mutate: func(l *Live) {
				l.Clusters["orders"].Spec.Certificates = &cnpgv1.CertificatesConfiguration{ServerCASecret: "my-ca"}
			},
			user: "shop_owner", db: "shop", password: "pw-app",
		},
		{
			name: "CA ConfigMap absent", server: declared.Server{Cluster: "orders", Endpoint: "rw", CAConfigMap: "nope", Credentials: app("orders-app")},
			reason: "CA ConfigMap nope is absent or has no ca.crt", noSection: true,
		},
		{
			name: "CA ConfigMap not PEM", server: declared.Server{Cluster: "orders", Endpoint: "rw", CAConfigMap: "orders-ca-crt", Credentials: app("orders-app")},
			mutate: func(l *Live) { l.CAConfigMaps["orders-ca-crt"] = []byte("not a certificate") },
			reason: "CA ConfigMap orders-ca-crt ca.crt is not PEM", noSection: true,
		},
		{
			name: "CA ConfigMap too large", server: declared.Server{Cluster: "orders", Endpoint: "rw", CAConfigMap: "orders-ca-crt", Credentials: app("orders-app")},
			mutate: func(l *Live) {
				l.CAConfigMaps["orders-ca-crt"] = []byte(caPEM1 + strings.Repeat("x", MaxSecretBytes))
			},
			reason: "ca.crt is too large", noSection: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			live := liveState()
			if tc.mutate != nil {
				tc.mutate(&live)
			}
			got := Resolve(declared.Document{Version: 1, Servers: []declared.Server{tc.server}}, "team", live)[0]
			if !tc.noSection && (got.User != tc.user || got.Database != tc.db) {
				t.Errorf("user %q database %q, want %q %q", got.User, got.Database, tc.user, tc.db)
			}
			if got.Password != tc.password {
				t.Errorf("password %q, want %q", got.Password, tc.password)
			}
			if (got.NoSection != "") != tc.noSection {
				t.Errorf("no section %q, want none: %v", got.NoSection, !tc.noSection)
			}
			if got.Withheld != tc.withheld {
				t.Errorf("withheld %v, want %v", got.Withheld, tc.withheld)
			}
			switch {
			case tc.reason == "" && got.Reason() != "":
				t.Errorf("refused: %s", got.Reason())
			case tc.reason != "" && !strings.Contains(got.Reason(), tc.reason):
				t.Errorf("reason %q, want %q", got.Reason(), tc.reason)
			}
			if tc.password == "" && got.Password != "" {
				t.Errorf("a refused credential carries a password")
			}
		})
	}
}

// TestRenderDeclaredFiles: a verified credential reaches both files; a
// refused one keeps its section without a password and has no pgpass line;
// a server without a section is in neither; the CA's signing key never
// leaves its Secret.
func TestRenderDeclaredFiles(t *testing.T) {
	doc := declared.Document{Version: 1, Servers: []declared.Server{
		{Cluster: "orders", Endpoint: "rw", Credentials: app("orders-app")},
		{Cluster: "orders", Endpoint: "ro", User: "alice", Database: "app", Credentials: declared.Credentials{Kind: declared.KindNone}},
		{Cluster: "orders", Endpoint: "r", Credentials: declared.Credentials{Kind: declared.KindDatabaseRole, Role: "orders-reporting", Secret: "reporting-pw"}},
		{Cluster: "legacy", Endpoint: "rw", User: "bob", Database: "app", Credentials: declared.Credentials{Kind: declared.KindNone}},
	}}
	live := liveState()
	live.DatabaseRoles["orders-reporting"].Status.Applied = nil
	files := Files{Dir: t.TempDir()}
	outcome, err := RenderDeclared(files, Resolve(doc, "team", live))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Offered != 1 || len(outcome.Refused) != 2 {
		t.Fatalf("outcome %+v", outcome)
	}
	service, pass := read(t, files.ServiceFile()), read(t, files.PassFile())
	for _, want := range []string{
		"[pgtoolbox-orders-rw-application]", "host=orders-rw.team.svc", "user=shop_owner", "dbname=shop", "password=pw-app",
		"[pgtoolbox-orders-ro-alice]", "host=orders-ro.team.svc", "user=alice",
		"[pgtoolbox-orders-r-orders-reporting]", "host=orders-r.team.svc", "user=reporting",
		"sslrootcert=" + files.Dir + "/ca/orders.crt",
	} {
		if !strings.Contains(service, want) {
			t.Errorf("service file lacks %q:\n%s", want, service)
		}
	}
	if strings.Contains(service, "legacy") || strings.Contains(service, "pw-report") || strings.Count(service, "password=") != 1 {
		t.Errorf("service file:\n%s", service)
	}
	if pass != "orders-rw.team.svc:5432:*:shop_owner:pw-app\n" {
		t.Errorf("pgpass %q", pass)
	}
	if ca := read(t, files.Dir+"/ca/orders.crt"); ca != caPEM1 || strings.Contains(read(t, files.Dir+"/ca/bundle.crt"), "SIGNING-KEY") {
		t.Errorf("CA file %q", ca)
	}
}

// TestRunDeclaredFollowsRotationAndRoleApplication: the sidecar writes a
// DatabaseRole's password only once CloudNativePG applied it, follows the
// Secret's rotation, and withdraws the password when the Cluster stops
// designating the Secret; it logs names and reasons, never a password.
func TestRunDeclaredFollowsRotationAndRoleApplication(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scheme := runtime.NewScheme()
	if err := cnpgv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	pending := declaredRole("orders-reporting", "orders", "reporting-pw", "42", func(r *cnpgv1.DatabaseRole) { r.Status.Applied = nil })
	dyn := dynamicfake.NewSimpleDynamicClient(scheme, ordersCluster(), pending)
	client := fake.NewClientset(
		caSecret("orders-ca", caPEM1),
		secret("orders-app", "5", map[string]string{"username": "shop_owner", "password": "pw-before"}),
		secret("reporting-pw", "42", map[string]string{"username": "reporting", "password": "pw-report"}),
	)
	doc := declared.Document{Version: 1, Servers: []declared.Server{
		{Cluster: "orders", Endpoint: "rw", Credentials: app("orders-app")},
		{Cluster: "orders", Endpoint: "ro", Credentials: declared.Credentials{Kind: declared.KindDatabaseRole, Role: "orders-reporting", Secret: "reporting-pw"}},
	}}
	files := Files{Dir: t.TempDir()}
	logs := &capture{}
	go func() {
		_ = RunDeclared(ctx, DeclaredOptions{
			Client: client, Dynamic: dyn, Namespace: "team", Document: doc, Files: files,
			Log: logs.logger(), Debounce: 20 * time.Millisecond, Backoff: 50 * time.Millisecond,
		})
	}()

	eventually(t, "the application password", func() bool { return fileContains(files.PassFile(), "pw-before") })
	eventually(t, "the pending role logged", func() bool { return strings.Contains(logs.all(), "not applied by CloudNativePG yet") })
	if fileContains(files.PassFile(), "pw-report") || !fileContains(files.ServiceFile(), "user=reporting") {
		t.Fatal("a role CloudNativePG has not applied must keep its section without a password")
	}

	// CloudNativePG applies the role: its password follows.
	applied := declaredRole("orders-reporting", "orders", "reporting-pw", "42")
	if _, err := dyn.Resource(databaseRolesResource).Namespace("team").Update(ctx, toUnstructured(t, applied), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the applied role's password", func() bool { return fileContains(files.PassFile(), "pw-report") })

	// Rotation: the Secret changes, the files follow without a restart.
	rotated := secret("orders-app", "6", map[string]string{"username": "shop_owner", "password": "pw-after"})
	if _, err := client.CoreV1().Secrets("team").Update(ctx, rotated, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the rotated password", func() bool {
		return fileContains(files.PassFile(), "pw-after") && !fileContains(files.PassFile(), "pw-before")
	})

	// The Cluster now designates another Secret: the password is withdrawn.
	moved := ordersCluster(func(c *cnpgv1.Cluster) {
		c.Spec.Bootstrap.InitDB.Secret = &cnpgv1.LocalObjectReference{Name: "elsewhere"}
	})
	if _, err := dyn.Resource(clustersResource).Namespace("team").Update(ctx, toUnstructured(t, moved), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the withdrawn password", func() bool { return !fileContains(files.PassFile(), "pw-after") })
	all := logs.all()
	for _, secretValue := range []string{"pw-before", "pw-after", "pw-report"} {
		if strings.Contains(all, secretValue) {
			t.Fatalf("a password reached the log:\n%s", all)
		}
	}
	if !strings.Contains(all, "application Secret is elsewhere, not orders-app") {
		t.Fatalf("the withdrawal is not explained:\n%s", all)
	}
}

// TestRunDeclaredWithholdsROUntilAReplica: an ro server of a one-instance
// Cluster gets no section and is listed in the withheld file the
// registration reads, by name and reason; scaled to two instances, the
// Cluster gets it back in both files; back to one, it is withheld again.
func TestRunDeclaredWithholdsROUntilAReplica(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scheme := runtime.NewScheme()
	if err := cnpgv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	single := func(c *cnpgv1.Cluster) { c.Spec.Instances = 1 }
	dyn := dynamicfake.NewSimpleDynamicClient(scheme, ordersCluster(single))
	client := fake.NewClientset(
		caSecret("orders-ca", caPEM1),
		secret("orders-app", "5", map[string]string{"username": "shop_owner", "password": "pw-app"}),
	)
	doc := declared.Document{Version: 1, Servers: []declared.Server{
		{Cluster: "orders", Endpoint: "rw", Credentials: app("orders-app")},
		{Cluster: "orders", Endpoint: "ro", Credentials: app("orders-app")},
	}}
	files := Files{Dir: t.TempDir()}
	withheldFile := filepath.Join(t.TempDir(), declared.WithheldFileName)
	logs := &capture{}
	go func() {
		_ = RunDeclared(ctx, DeclaredOptions{
			Client: client, Dynamic: dyn, Namespace: "team", Document: doc, Files: files, WithheldFile: withheldFile,
			Log: logs.logger(), Debounce: 20 * time.Millisecond, Backoff: 50 * time.Millisecond,
		})
	}()
	withheld := func() []declared.WithheldServer {
		w, err := declared.LoadWithheld(withheldFile)
		if err != nil {
			return nil
		}
		return w.Servers
	}

	eventually(t, "the ro server withheld", func() bool {
		w := withheld()
		return len(w) == 1 && w[0].Name == "orders ro (application)" && w[0].Reason == "no replica: the Cluster has one instance"
	})
	if !fileContains(files.ServiceFile(), "orders-rw.team.svc") || fileContains(files.ServiceFile(), "orders-ro") {
		t.Fatalf("service file:\n%s", read(t, files.ServiceFile()))
	}
	eventually(t, "the withheld server logged", func() bool {
		return strings.Contains(logs.all(), "not registered in pgAdmin") && strings.Contains(logs.all(), "no replica")
	})

	// A replica: the ro server comes back in both files.
	if _, err := dyn.Resource(clustersResource).Namespace("team").Update(ctx, toUnstructured(t, ordersCluster()), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the ro server offered", func() bool {
		raw, err := os.ReadFile(withheldFile) // #nosec G304 -- test path
		return err == nil && len(withheld()) == 0 && strings.Contains(string(raw), `"servers":[]`) &&
			fileContains(files.ServiceFile(), "orders-ro.team.svc")
	})

	// Back to one instance: withheld again.
	if _, err := dyn.Resource(clustersResource).Namespace("team").Update(ctx, toUnstructured(t, ordersCluster(single)), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the ro server withheld again", func() bool {
		return len(withheld()) == 1 && !fileContains(files.ServiceFile(), "orders-ro")
	})
	if strings.Contains(logs.all(), "pw-app") {
		t.Fatalf("a password reached the log:\n%s", logs.all())
	}
}

func toUnstructured(t *testing.T, obj runtime.Object) *unstructured.Unstructured {
	t.Helper()
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		t.Fatal(err)
	}
	return &unstructured.Unstructured{Object: raw}
}
