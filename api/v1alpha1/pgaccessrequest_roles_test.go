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

package v1alpha1_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// TestGrantableRolesCannotDecide: an approved request binds the role its
// capability names, time-boxed. If that role could create RoleBindings,
// update a request's status or bind ClusterRoles, a grantee could bind
// themselves a second, permanent RoleBinding or approve other people's
// requests. Deciding is pgtoolbox-console-approver's, which no capability
// resolves to.
func TestGrantableRolesCannotDecide(t *testing.T) {
	files := map[string]string{
		pgtoolboxv1alpha1.ClusterRoleViewer:   "pgtoolbox_console_viewer_clusterrole.yaml",
		pgtoolboxv1alpha1.ClusterRoleOperator: "pgtoolbox_console_operator_clusterrole.yaml",
	}
	for _, tc := range []struct {
		capability pgtoolboxv1alpha1.AccessCapability
		want       string
	}{
		{pgtoolboxv1alpha1.AccessCapabilityViewer, pgtoolboxv1alpha1.ClusterRoleViewer},
		{pgtoolboxv1alpha1.AccessCapabilityOperator, pgtoolboxv1alpha1.ClusterRoleOperator},
		{"approver", ""},
		{"admin", ""},
		{"", ""},
	} {
		t.Run(string(tc.capability), func(t *testing.T) {
			got := pgtoolboxv1alpha1.ClusterRoleFor(tc.capability)
			if got != tc.want {
				t.Fatalf("ClusterRoleFor(%q) = %q, want %q", tc.capability, got, tc.want)
			}
			if got == "" {
				return
			}
			raw, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", files[got])) // #nosec G304 -- repository manifest
			if err != nil {
				t.Fatal(err)
			}
			var role rbacv1.ClusterRole
			if err := yaml.Unmarshal(raw, &role); err != nil {
				t.Fatal(err)
			}
			for _, rule := range role.Rules {
				for _, forbidden := range []string{"rolebindings", "roles", "clusterroles", "clusterrolebindings", "pgaccessrequests/status", "*"} {
					if slices.Contains(rule.Resources, forbidden) {
						t.Fatalf("%s, which a PgAccessRequest grants, holds %v on %s", got, rule.Verbs, forbidden)
					}
				}
				for _, verb := range []string{"bind", "escalate", "impersonate", "*"} {
					if slices.Contains(rule.Verbs, verb) {
						t.Fatalf("%s, which a PgAccessRequest grants, holds %s on %v", got, verb, rule.Resources)
					}
				}
			}
		})
	}
}
