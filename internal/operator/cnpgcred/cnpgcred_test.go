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

package cnpgcred

import (
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func cluster(bootstrap *cnpgv1.BootstrapConfiguration) *cnpgv1.Cluster {
	return &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "team"},
		Spec:       cnpgv1.ClusterSpec{Bootstrap: bootstrap},
	}
}

func role(name, secret string) *cnpgv1.DatabaseRole {
	applied := true
	return &cnpgv1.DatabaseRole{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team", Generation: 1},
		Spec: cnpgv1.DatabaseRoleSpec{
			ClusterRef:        corev1.LocalObjectReference{Name: "orders"},
			RoleConfiguration: cnpgv1.RoleConfiguration{Name: name, PasswordSecret: &cnpgv1.LocalObjectReference{Name: secret}},
		},
		Status: cnpgv1.DatabaseRoleStatus{Applied: &applied, ObservedGeneration: 1, SecretResourceVersion: "7"},
	}
}

// TestApplicationCredential is the README's table: which Secret, owner and
// database CloudNativePG uses for each bootstrap.
func TestApplicationCredential(t *testing.T) {
	cases := []struct {
		name                    string
		bootstrap               *cnpgv1.BootstrapConfiguration
		secret, owner, database string
	}{
		{"no bootstrap", nil, "orders-app", "app", "app"},
		{"initdb defaults", &cnpgv1.BootstrapConfiguration{InitDB: &cnpgv1.BootstrapInitDB{}}, "orders-app", "app", "app"},
		{"initdb database", &cnpgv1.BootstrapConfiguration{InitDB: &cnpgv1.BootstrapInitDB{Database: "shop"}}, "orders-app", "shop", "shop"},
		{"initdb owner", &cnpgv1.BootstrapConfiguration{InitDB: &cnpgv1.BootstrapInitDB{Database: "shop", Owner: "o"}}, "orders-app", "o", "shop"},
		{"initdb secret", &cnpgv1.BootstrapConfiguration{InitDB: &cnpgv1.BootstrapInitDB{
			Secret: &cnpgv1.LocalObjectReference{Name: "mine"},
		}}, "mine", "app", "app"},
		{"recovery owner", &cnpgv1.BootstrapConfiguration{Recovery: &cnpgv1.BootstrapRecovery{Owner: "r"}}, "orders-app", "r", "app"},
		{"recovery database", &cnpgv1.BootstrapConfiguration{Recovery: &cnpgv1.BootstrapRecovery{Database: "d"}}, "orders-app", "d", "d"},
		{"pg_basebackup database", &cnpgv1.BootstrapConfiguration{PgBaseBackup: &cnpgv1.BootstrapPgBaseBackup{Database: "b"}}, "orders-app", "b", "b"},
		{"recovery secret", &cnpgv1.BootstrapConfiguration{Recovery: &cnpgv1.BootstrapRecovery{
			Secret: &cnpgv1.LocalObjectReference{Name: "restored"},
		}}, "restored", "app", "app"},
		{"pg_basebackup secret", &cnpgv1.BootstrapConfiguration{PgBaseBackup: &cnpgv1.BootstrapPgBaseBackup{
			Secret: &cnpgv1.LocalObjectReference{Name: "cloned"},
		}}, "cloned", "app", "app"},
		// CloudNativePG's order when several are set (the webhook allows one
		// bootstrap method; the order still decides on a hand-made object).
		{"recovery before initdb", &cnpgv1.BootstrapConfiguration{
			Recovery: &cnpgv1.BootstrapRecovery{Secret: &cnpgv1.LocalObjectReference{Name: "restored"}},
			InitDB:   &cnpgv1.BootstrapInitDB{Secret: &cnpgv1.LocalObjectReference{Name: "mine"}},
		}, "restored", "app", "app"},
		{"recovery without secret", &cnpgv1.BootstrapConfiguration{Recovery: &cnpgv1.BootstrapRecovery{
			Secret: &cnpgv1.LocalObjectReference{},
		}}, "orders-app", "app", "app"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := cluster(tc.bootstrap)
			if got := ApplicationSecretName(c); got != tc.secret {
				t.Errorf("ApplicationSecretName() = %q, want %q", got, tc.secret)
			}
			if got := ApplicationOwner(c); got != tc.owner {
				t.Errorf("ApplicationOwner() = %q, want %q", got, tc.owner)
			}
			if got := ApplicationDatabase(c); got != tc.database {
				t.Errorf("ApplicationDatabase() = %q, want %q", got, tc.database)
			}
		})
	}
}

func TestSuperuser(t *testing.T) {
	c := cluster(nil)
	if SuperuserEnabled(c) || SuperuserSecretName(c) != "orders-superuser" {
		t.Fatalf("default: enabled %v, secret %s", SuperuserEnabled(c), SuperuserSecretName(c))
	}
	enabled := true
	c.Spec.EnableSuperuserAccess = &enabled
	c.Spec.SuperuserSecret = &cnpgv1.LocalObjectReference{Name: "root"}
	if !SuperuserEnabled(c) || SuperuserSecretName(c) != "root" {
		t.Fatalf("enabled, named: enabled %v, secret %s", SuperuserEnabled(c), SuperuserSecretName(c))
	}
}

func TestCASecretNameIgnoresStatus(t *testing.T) {
	c := cluster(nil)
	c.Status.Certificates.ServerCASecret = "orders-superuser"
	if got := CASecretName(c); got != "orders-ca" {
		t.Fatalf("the CA Secret came from status: %s", got)
	}
	c.Spec.Certificates = &cnpgv1.CertificatesConfiguration{ServerCASecret: "my-ca"}
	if got := CASecretName(c); got != "my-ca" {
		t.Fatalf("spec CA ignored: %s", got)
	}
}

func TestPasswordSecretName(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*cnpgv1.DatabaseRole)
		want   string
	}{
		{"named", nil, "r-pw"},
		{"generated", func(r *cnpgv1.DatabaseRole) { r.Spec.PasswordSecret = nil }, "r-password"},
		{"not applied yet", func(r *cnpgv1.DatabaseRole) { r.Spec.PasswordSecret, r.Status.SecretResourceVersion = nil, "" }, ""},
		{"password disabled", func(r *cnpgv1.DatabaseRole) { r.Spec.PasswordSecret, r.Spec.DisablePassword = nil, true }, ""},
		{"certificate", func(r *cnpgv1.DatabaseRole) {
			r.Spec.PasswordSecret, r.Spec.ClientCertificate = nil, &cnpgv1.ClientCertificateConfiguration{}
		}, ""},
	}
	for _, tc := range cases {
		r := role("r", "r-pw")
		if tc.mutate != nil {
			tc.mutate(r)
		}
		if got := PasswordSecretName(r); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDatabaseRoleName(t *testing.T) {
	r := role("k8s-name", "pw")
	r.Spec.Name = ""
	if got := DatabaseRoleName(r); got != "k8s-name" {
		t.Fatalf("without spec.name: %s", got)
	}
	r.Spec.Name = "reporting"
	if got := DatabaseRoleName(r); got != "reporting" {
		t.Fatalf("with spec.name: %s", got)
	}
}

// TestRoleReadiness: a DatabaseRole's password is usable only once
// CloudNativePG applied the current generation and recorded the version of
// the Secret it applied, and only at that version.
func TestRoleReadiness(t *testing.T) {
	enabled := true
	cases := []struct {
		name   string
		mutate func(*cnpgv1.DatabaseRole)
		want   string
	}{
		{"ready", nil, ""},
		{"not applied", func(r *cnpgv1.DatabaseRole) { r.Status.Applied = nil }, "not applied by CloudNativePG yet"},
		{"applied false", func(r *cnpgv1.DatabaseRole) { f := false; r.Status.Applied = &f }, "not applied by CloudNativePG yet"},
		{"older generation", func(r *cnpgv1.DatabaseRole) { r.Generation = 2 }, "has not applied the current generation"},
		{"certificate", func(r *cnpgv1.DatabaseRole) {
			r.Spec.PasswordSecret = nil
			r.Spec.ClientCertificate = &cnpgv1.ClientCertificateConfiguration{Enabled: &enabled}
		}, "certificate authentication"},
		{"no secret", func(r *cnpgv1.DatabaseRole) { r.Spec.PasswordSecret, r.Status.SecretResourceVersion = nil, "" }, "no password Secret"},
		{"version not recorded", func(r *cnpgv1.DatabaseRole) { r.Status.SecretResourceVersion = "" }, "has not recorded applying"},
	}
	for _, tc := range cases {
		r := role("r", "r-pw")
		if tc.mutate != nil {
			tc.mutate(r)
		}
		got := RoleNotReady(r)
		if (tc.want == "") != (got == "") || (tc.want != "" && !strings.Contains(got, tc.want)) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	r := role("r", "r-pw")
	if !SecretAtAppliedVersion(r, "7") || SecretAtAppliedVersion(r, "8") {
		t.Fatal("only the applied version may be used")
	}
	r.Status.SecretResourceVersion = ""
	if SecretAtAppliedVersion(r, "") {
		t.Fatal("an unrecorded version matches nothing, not even an empty one")
	}
}

// TestEndpointNotServed is the rule both pgAdmin modes apply:
// ro needs a declared replica, ro and r need their Service enabled; rw and
// a Pooler's endpoint are always served. It reads the spec, not readiness.
func TestEndpointNotServed(t *testing.T) {
	withInstances := func(n int, disabled ...cnpgv1.ServiceSelectorType) *cnpgv1.Cluster {
		c := cluster(nil)
		c.Spec.Instances = n
		if disabled != nil {
			c.Spec.Managed = &cnpgv1.ManagedConfiguration{Services: &cnpgv1.ManagedServices{DisabledDefaultServices: disabled}}
		}
		return c
	}
	notReady := withInstances(3)
	notReady.Status.ReadyInstances = 1
	for _, tc := range []struct {
		name     string
		cluster  *cnpgv1.Cluster
		endpoint string
		want     string
	}{
		{"rw on one instance", withInstances(1), "rw", ""},
		{"ro on one instance", withInstances(1), "ro", "no replica: the Cluster has one instance"},
		{"ro on zero declared instances", withInstances(0), "ro", "no replica: the Cluster has one instance"},
		{"r on one instance", withInstances(1), "r", ""},
		{"ro on two instances", withInstances(2), "ro", ""},
		{"ro with replicas not ready", notReady, "ro", ""},
		{"ro disabled", withInstances(3, cnpgv1.ServiceSelectorTypeRO), "ro", "the Cluster disabled its ro Service"},
		{"r disabled", withInstances(3, cnpgv1.ServiceSelectorTypeR), "r", "the Cluster disabled its r Service"},
		{"r disabled, ro served", withInstances(3, cnpgv1.ServiceSelectorTypeR), "ro", ""},
		{"ro disabled on one instance names the Service", withInstances(1, cnpgv1.ServiceSelectorTypeRO), "ro", "the Cluster disabled its ro Service"},
		{"pooler endpoint", withInstances(1, cnpgv1.ServiceSelectorTypeRO), "pooler:orders-ro", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := EndpointNotServed(tc.cluster, tc.endpoint); got != tc.want {
				t.Errorf("EndpointNotServed(%q) = %q, want %q", tc.endpoint, got, tc.want)
			}
		})
	}
}
