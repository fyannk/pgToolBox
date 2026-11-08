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
	"testing"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testCluster() *cnpgv1.Cluster {
	return &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "cluster-1", Namespace: "team"}}
}

func databaseRole(name, clusterName, secret string) *cnpgv1.DatabaseRole {
	applied := true
	return &cnpgv1.DatabaseRole{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team", Generation: 1},
		Spec: cnpgv1.DatabaseRoleSpec{
			ClusterRef:        corev1.LocalObjectReference{Name: clusterName},
			RoleConfiguration: cnpgv1.RoleConfiguration{Name: name, PasswordSecret: &cnpgv1.LocalObjectReference{Name: secret}},
		},
		Status: cnpgv1.DatabaseRoleStatus{Applied: &applied, ObservedGeneration: 1, SecretResourceVersion: "7"},
	}
}

func TestRolePrivileges(t *testing.T) {
	cluster := testCluster()
	cluster.Spec.Bootstrap = &cnpgv1.BootstrapConfiguration{InitDB: &cnpgv1.BootstrapInitDB{Database: "shop", Owner: "shop_owner"}}
	cluster.Spec.Managed = &cnpgv1.ManagedConfiguration{Roles: []cnpgv1.RoleConfiguration{
		{Name: "managed_admin", CreateRole: true},
		{Name: "managed_plain"},
	}}
	declared := func(name, clusterName string, mutate func(*cnpgv1.RoleConfiguration)) cnpgv1.DatabaseRole {
		role := databaseRole(name, clusterName, name+"-pw")
		mutate(&role.Spec.RoleConfiguration)
		return *role
	}
	roles := []cnpgv1.DatabaseRole{
		declared("ops", "cluster-1", func(r *cnpgv1.RoleConfiguration) { r.BypassRLS = true }),
		declared("ops_team", "cluster-1", func(r *cnpgv1.RoleConfiguration) { r.InRoles = []string{"ops"} }),
		declared("ops_team_member", "cluster-1", func(r *cnpgv1.RoleConfiguration) { r.InRoles = []string{"readers", "ops_team"} }),
		declared("readers", "cluster-1", func(r *cnpgv1.RoleConfiguration) {}),
		declared("loop_a", "cluster-1", func(r *cnpgv1.RoleConfiguration) { r.InRoles = []string{"loop_b"} }),
		declared("loop_b", "cluster-1", func(r *cnpgv1.RoleConfiguration) { r.InRoles = []string{"loop_a"} }),
		declared("loop_admin", "cluster-1", func(r *cnpgv1.RoleConfiguration) { r.InRoles = []string{"loop_a", "loop_c"} }),
		declared("loop_c", "cluster-1", func(r *cnpgv1.RoleConfiguration) { r.InRoles = []string{"loop_admin", "ops"} }),
		// Same name on another cluster: not this cluster's role.
		declared("foreign", "cluster-2", func(r *cnpgv1.RoleConfiguration) { r.Superuser = true }),
	}
	privileges := newRolePrivileges(cluster, roles)
	cases := []struct {
		name    string
		inRoles []string
		want    bool
	}{
		{"no membership", nil, false},
		{"plain declared role", []string{"readers"}, false},
		{"undeclared role", []string{"made_in_sql"}, false},
		{"predefined role", []string{"pg_read_all_data"}, true},
		{"superuser", []string{"postgres"}, true},
		{"application owner", []string{"shop_owner"}, true},
		{"declared privileged role", []string{"ops"}, true},
		{"transitively privileged", []string{"ops_team_member"}, true},
		{"managed privileged role", []string{"managed_admin"}, true},
		{"managed plain role", []string{"managed_plain"}, false},
		{"cycle without privilege", []string{"loop_a"}, false},
		{"cycle with a privileged exit", []string{"loop_admin"}, true},
		{"another cluster's role", []string{"foreign"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := privileges.privileged(cnpgv1.RoleConfiguration{Name: "subject", InRoles: tc.inRoles})
			if got != tc.want {
				t.Fatalf("privileged(inRoles %v) = %v, want %v", tc.inRoles, got, tc.want)
			}
		})
	}
	if !privileges.privileged(cnpgv1.RoleConfiguration{Name: "r", Replication: true}) {
		t.Fatal("replication is privileged")
	}
}
