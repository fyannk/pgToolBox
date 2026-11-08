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
	"slices"
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/offer"
)

func offeredUsers(result offerResult) []string {
	out := make([]string, 0, len(result.Document.Servers))
	for _, s := range result.Document.Servers {
		out = append(out, s.Cluster+"/"+s.Endpoint+"/"+s.User)
	}
	return out
}

func notOffered(result offerResult) []string {
	var out []string
	for _, row := range result.Rows {
		if row.State != pgtoolboxv1alpha1.ServerStateNotOffered {
			continue
		}
		if row.Endpoint == "*" { // a credential, on every endpoint
			out = append(out, row.Cluster+"/"+row.User+": "+row.Message)
		} else { // one endpoint the Cluster cannot serve
			out = append(out, row.Cluster+"/"+row.Endpoint+"/"+row.User+": "+row.Message)
		}
	}
	return out
}

func TestResolveOfferAllowlist(t *testing.T) {
	cases := []struct {
		name        string
		mutate      func(*pgtoolboxv1alpha1.PgAdmin)
		clusters    func([]cnpgv1.Cluster) // orders, billing
		roles       []cnpgv1.DatabaseRole
		rolesOff    bool
		want        []string
		wantNot     []string
		wantProblem string
	}{
		{
			name:  "default: application and unprivileged applied roles; superuser and privileged refused",
			roles: testRoles(),
			want:  []string{"billing/rw/app", "orders/rw/orders_owner", "orders/rw/reporting"},
			wantNot: []string{
				"orders/postgres: privileged; offered only with spec.credentials.superuser",
				"orders/dbadmin: privileged; offered only with spec.credentials.superuser",
				"orders/pending: not applied by CloudNativePG yet",
			},
		},
		{
			name:   "superuser opt-in offers the superuser where enabled and privileged roles",
			mutate: func(p *pgtoolboxv1alpha1.PgAdmin) { p.Spec.Credentials.Superuser = new(true) },
			roles:  testRoles(),
			want:   []string{"billing/rw/app", "orders/rw/orders_owner", "orders/rw/postgres", "orders/rw/dbadmin", "orders/rw/reporting"},
			wantNot: []string{
				"billing/postgres: the Cluster does not enable superuser access",
				"orders/pending: not applied by CloudNativePG yet",
			},
		},
		{
			name:   "application off",
			mutate: func(p *pgtoolboxv1alpha1.PgAdmin) { p.Spec.Credentials.Application = new(false) },
			roles:  testRoles(),
			want:   []string{"orders/rw/reporting"},
		},
		{
			name:     "DatabaseRole API unavailable offers application credentials only",
			rolesOff: true,
			want:     []string{"billing/rw/app", "orders/rw/orders_owner"},
		},
		{
			name: "role at a stale generation is refused",
			roles: func() []cnpgv1.DatabaseRole {
				r := testRoles()[:1]
				r[0].Generation = 2
				return r
			}(),
			want:    []string{"billing/rw/app", "orders/rw/orders_owner"},
			wantNot: []string{"orders/postgres: privileged; offered only with spec.credentials.superuser", "orders/reporting: CloudNativePG has not applied the current generation yet"},
		},
		{
			name: "certificate-only role is refused",
			roles: func() []cnpgv1.DatabaseRole {
				r := testRoles()[:1]
				r[0].Spec.PasswordSecret = nil
				r[0].Spec.ClientCertificate = &cnpgv1.ClientCertificateConfiguration{Enabled: new(true)}
				return r
			}(),
			want:    []string{"billing/rw/app", "orders/rw/orders_owner"},
			wantNot: []string{"orders/postgres: privileged; offered only with spec.credentials.superuser", "orders/reporting: certificate authentication is not offered"},
		},
		{
			name: "endpoints multiply credentials; a pooler serves its own Cluster only",
			mutate: func(p *pgtoolboxv1alpha1.PgAdmin) {
				p.Spec.Endpoints = []pgtoolboxv1alpha1.Endpoint{"rw", "r", "pooler:orders-pooler"}
				p.Spec.Credentials.DatabaseRoles = new(false)
			},
			want: []string{"billing/rw/app", "billing/r/app", "orders/rw/orders_owner", "orders/r/orders_owner", "orders/pooler:orders-pooler/orders_owner"},
		},
		{
			name:   "ro is offered where the Cluster declares two instances",
			mutate: readEndpoints,
			want: []string{
				"billing/rw/app", "billing/ro/app", "billing/r/app",
				"orders/rw/orders_owner", "orders/ro/orders_owner", "orders/r/orders_owner",
			},
			wantNot: []string{"orders/postgres: privileged; offered only with spec.credentials.superuser"},
		},
		{
			name:     "ro is not offered for a Cluster of one instance: no replica answers",
			mutate:   readEndpoints,
			clusters: func(c []cnpgv1.Cluster) { c[1].Spec.Instances = 1 },
			want: []string{
				"billing/rw/app", "billing/r/app",
				"orders/rw/orders_owner", "orders/ro/orders_owner", "orders/r/orders_owner",
			},
			wantNot: []string{"billing/ro/app: no replica: the Cluster has one instance", "orders/postgres: privileged; offered only with spec.credentials.superuser"},
		},
		{
			name:   "ro is not offered for a Cluster that disabled its ro Service, whatever its instances",
			mutate: readEndpoints,
			clusters: func(c []cnpgv1.Cluster) {
				c[0].Spec.Instances = 3
				c[0].Spec.Managed = &cnpgv1.ManagedConfiguration{Services: &cnpgv1.ManagedServices{
					DisabledDefaultServices: []cnpgv1.ServiceSelectorType{cnpgv1.ServiceSelectorTypeRO},
				}}
			},
			want: []string{
				"billing/rw/app", "billing/ro/app", "billing/r/app",
				"orders/rw/orders_owner", "orders/r/orders_owner",
			},
			wantNot: []string{"orders/postgres: privileged; offered only with spec.credentials.superuser", "orders/ro/orders_owner: the Cluster disabled its ro Service"},
		},
		{
			name:   "r is not offered for a Cluster that disabled its r Service",
			mutate: readEndpoints,
			clusters: func(c []cnpgv1.Cluster) {
				c[0].Spec.Managed = &cnpgv1.ManagedConfiguration{Services: &cnpgv1.ManagedServices{
					DisabledDefaultServices: []cnpgv1.ServiceSelectorType{cnpgv1.ServiceSelectorTypeR},
				}}
			},
			want: []string{
				"billing/rw/app", "billing/ro/app", "billing/r/app",
				"orders/rw/orders_owner", "orders/ro/orders_owner",
			},
			wantNot: []string{"orders/postgres: privileged; offered only with spec.credentials.superuser", "orders/r/orders_owner: the Cluster disabled its r Service"},
		},
		{
			name: "a Cluster of one instance still offers rw, and every credential gets its own ro row",
			mutate: func(p *pgtoolboxv1alpha1.PgAdmin) {
				p.Spec.ClusterRefs = []pgtoolboxv1alpha1.LocalObjectReference{{Name: "orders"}}
				p.Spec.Endpoints = []pgtoolboxv1alpha1.Endpoint{"rw", "ro"}
			},
			clusters: func(c []cnpgv1.Cluster) { c[0].Spec.Instances = 1 },
			roles:    testRoles()[:1],
			want:     []string{"orders/rw/orders_owner", "orders/rw/reporting"},
			wantNot: []string{
				"orders/postgres: privileged; offered only with spec.credentials.superuser",
				"orders/ro/orders_owner: no replica: the Cluster has one instance",
				"orders/ro/reporting: no replica: the Cluster has one instance",
			},
		},
		{
			name:        "a pooler that does not exist is a problem",
			mutate:      func(p *pgtoolboxv1alpha1.PgAdmin) { p.Spec.Endpoints = []pgtoolboxv1alpha1.Endpoint{"pooler:nope"} },
			wantProblem: "no Pooler nope",
		},
		{
			name: "a pooler of an unregistered Cluster is a problem",
			mutate: func(p *pgtoolboxv1alpha1.PgAdmin) {
				p.Spec.ClusterRefs = []pgtoolboxv1alpha1.LocalObjectReference{{Name: "billing"}}
				p.Spec.Endpoints = []pgtoolboxv1alpha1.Endpoint{"rw", "pooler:orders-pooler"}
			},
			want:        []string{"billing/rw/app"},
			wantProblem: "which this PgAdmin does not register",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := oauth2PgAdmin()
			if tc.mutate != nil {
				tc.mutate(p)
			}
			all := testClusters()
			if tc.clusters != nil {
				tc.clusters(all)
			}
			clusters, _, _ := selectClusters(p, all)
			result := resolveOffer(offerInputs{
				PgAdmin: p, Clusters: clusters, Roles: tc.roles, RolesAvailable: !tc.rolesOff,
				Poolers: testPoolers(), PoolersAvailable: true,
			})
			if tc.want != nil && !slices.Equal(offeredUsers(result), tc.want) {
				t.Errorf("offered %v, want %v", offeredUsers(result), tc.want)
			}
			if tc.wantNot != nil && !slices.Equal(notOffered(result), tc.wantNot) {
				t.Errorf("not offered %q, want %q", notOffered(result), tc.wantNot)
			}
			joined := strings.Join(result.Problems, "; ")
			if (tc.wantProblem == "") != (joined == "") || !strings.Contains(joined, tc.wantProblem) {
				t.Errorf("problems %q, want %q", joined, tc.wantProblem)
			}
			if err := result.Document.Validate(); err != nil {
				t.Fatalf("the offer does not validate: %v", err)
			}
		})
	}
}

// readEndpoints asks for the three Services of every Cluster, application
// credentials only.
func readEndpoints(p *pgtoolboxv1alpha1.PgAdmin) {
	p.Spec.Endpoints = []pgtoolboxv1alpha1.Endpoint{"rw", "ro", "r"}
	p.Spec.Credentials.DatabaseRoles = new(false)
}

// A skipped endpoint is a NotOffered row naming the endpoint and the
// credential, never a Pending one that becomes Registered.
func TestResolveOfferSkippedEndpointRows(t *testing.T) {
	p := oauth2PgAdmin()
	readEndpoints(p)
	clusters := testClusters()
	clusters[1].Spec.Instances = 1
	result := resolveOffer(offerInputs{PgAdmin: p, Clusters: clusters, PoolersAvailable: true})
	var got []pgtoolboxv1alpha1.PgAdminServerStatus
	for _, row := range serverRows(result.Rows, true) {
		if row.Cluster == "billing" {
			got = append(got, row)
		}
	}
	want := []pgtoolboxv1alpha1.PgAdminServerStatus{
		{Cluster: "billing", Endpoint: "rw", User: "app", State: pgtoolboxv1alpha1.ServerStateRegistered},
		{Cluster: "billing", Endpoint: "ro", User: "app", State: pgtoolboxv1alpha1.ServerStateNotOffered, Message: "no replica: the Cluster has one instance"},
		{Cluster: "billing", Endpoint: "r", User: "app", State: pgtoolboxv1alpha1.ServerStateRegistered},
	}
	if !slices.Equal(got, want) {
		t.Errorf("rows %+v, want %+v", got, want)
	}
}

func TestResolveOfferDatabaseRoleSecretVersionPinned(t *testing.T) {
	p := oauth2PgAdmin()
	clusters, _, _ := selectClusters(p, testClusters())
	result := resolveOffer(offerInputs{PgAdmin: p, Clusters: clusters, Roles: testRoles(), RolesAvailable: true})
	for _, s := range result.Document.Servers {
		if s.User == "reporting" && (s.SecretName != "reporting-pw" || s.SecretResourceVersion != "7") {
			t.Fatalf("role server %+v must pin the Secret version CNPG applied", s)
		}
		if s.User == "orders_owner" && (s.SecretResourceVersion != "" || s.DBName != "orders" || s.CASecretName != "orders-ca") {
			t.Fatalf("application server %+v", s)
		}
	}
}

func TestResolveOfferIsDeterministic(t *testing.T) {
	p := openshiftPgAdmin()
	clusters, _, _ := selectClusters(p, testClusters())
	roles := testRoles()
	first, _ := offer.Encode(resolveOffer(offerInputs{PgAdmin: p, Clusters: clusters, Roles: roles, RolesAvailable: true, Poolers: testPoolers(), PoolersAvailable: true}).Document)
	slices.Reverse(clusters)
	slices.Reverse(roles)
	second, _ := offer.Encode(resolveOffer(offerInputs{PgAdmin: p, Clusters: clusters, Roles: roles, RolesAvailable: true, Poolers: testPoolers(), PoolersAvailable: true}).Document)
	if string(first) != string(second) {
		t.Fatal("the offer depends on the order of the cache's lists")
	}
}

func TestResolveOfferTooMany(t *testing.T) {
	p := oauth2PgAdmin()
	p.Spec.ClusterSelector = &metav1.LabelSelector{}
	p.Spec.ClusterRefs = nil
	p.Spec.Endpoints = []pgtoolboxv1alpha1.Endpoint{"rw", "ro", "r"}
	clusters := make([]cnpgv1.Cluster, 0, 30)
	for i := range 30 {
		clusters = append(clusters, testCluster2("c"+strings.Repeat("x", i), nil))
	}
	result := resolveOffer(offerInputs{PgAdmin: p, Clusters: clusters})
	if !result.TooMany || len(result.Document.Servers) != 90 {
		t.Fatalf("TooMany=%v servers=%d", result.TooMany, len(result.Document.Servers))
	}
}

func TestSelectClusters(t *testing.T) {
	p := oauth2PgAdmin()
	p.Spec.ClusterRefs = append(p.Spec.ClusterRefs, pgtoolboxv1alpha1.LocalObjectReference{Name: "gone"})
	selected, missing, err := selectClusters(p, testClusters())
	if err != nil || len(selected) != 2 || !slices.Equal(missing, []string{"gone"}) {
		t.Fatalf("selected %d, missing %v, err %v", len(selected), missing, err)
	}
	q := openshiftPgAdmin()
	other := testCluster2("other", func(c *cnpgv1.Cluster) { c.Labels = map[string]string{"team": "b"} })
	selected, missing, err = selectClusters(q, append(testClusters(), other))
	if err != nil || len(selected) != 2 || missing != nil {
		t.Fatalf("selector: selected %d, missing %v, err %v", len(selected), missing, err)
	}
}

// A DatabaseRole that names the application owner (or the superuser) is
// privileged by its name, whatever attributes it declares: without
// credentials.superuser it must not be offered, or anyone who may create
// DatabaseRoles and open pgAdmin would get the owner's password.
func TestResolveOfferOwnerByNameIsPrivileged(t *testing.T) {
	p := oauth2PgAdmin()
	p.Spec.Credentials.Application = new(false)
	owner := databaseRole("sneaky", "orders", "orders-app")
	owner.Namespace = testNamespace
	owner.Spec.Name = "orders_owner"
	owner.Spec.Login = true
	postgres := databaseRole("sneaky-su", "orders", "orders-superuser")
	postgres.Namespace = testNamespace
	postgres.Spec.Name = "postgres"
	clusters, _, _ := selectClusters(p, testClusters())
	result := resolveOffer(offerInputs{PgAdmin: p, Clusters: clusters, Roles: []cnpgv1.DatabaseRole{*owner, *postgres}, RolesAvailable: true})
	for _, s := range result.Document.Servers {
		if s.User == "orders_owner" || s.User == "postgres" {
			t.Fatalf("offered %s without credentials.superuser", s.User)
		}
	}
	for _, name := range result.Document.SecretNames("") {
		if name == "orders-app" || name == "orders-superuser" {
			t.Fatalf("the Pod's Role would grant %s", name)
		}
	}
	p.Spec.Credentials.Superuser = new(true)
	result = resolveOffer(offerInputs{PgAdmin: p, Clusters: clusters, Roles: []cnpgv1.DatabaseRole{*owner, *postgres}, RolesAvailable: true})
	users := make([]string, 0, len(result.Document.Servers))
	for _, s := range result.Document.Servers {
		users = append(users, s.User)
	}
	if !slices.Contains(users, "orders_owner") || countOf(users, "postgres") != 1 {
		t.Fatalf("with superuser: the owner role is offered, the superuser only once through its own Secret: %v", users)
	}
}

func countOf(values []string, want string) int {
	n := 0
	for _, v := range values {
		if v == want {
			n++
		}
	}
	return n
}
