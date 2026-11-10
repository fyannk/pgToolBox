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
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// allows reports whether one of the rules grants verb on group/resource,
// with RBAC's wildcards.
func allows(rules []rbacv1.PolicyRule, verb, group, resource string) bool {
	has := func(list []string, v string) bool {
		return slices.Contains(list, v) || slices.Contains(list, rbacv1.ResourceAll)
	}
	for _, r := range rules {
		if has(r.Verbs, verb) && has(r.APIGroups, group) && has(r.Resources, resource) {
			return true
		}
	}
	return false
}

// TestOnlyIntendedRolesOpenPgAdmin: opening pgAdmin (the OpenShift
// oauth-proxy's SubjectAccessReview, the console's link) is reading the
// credentials of every role it offers. Only pgtoolbox-console-operator and
// the share of admin grant it; the roles OLM generates for an owned CRD and
// aggregates into view and edit do not, nor does any other role we ship.
func TestOnlyIntendedRolesOpenPgAdmin(t *testing.T) {
	verb, group := pgtoolboxv1alpha1.PgAdminConnectVerb, pgtoolboxv1alpha1.GroupVersion.Group
	opens := map[string]bool{
		pgtoolboxv1alpha1.ClusterRoleOperator: true,
		"pgtoolbox-aggregate-to-admin":        true,
	}

	files, err := filepath.Glob(filepath.Join("..", "..", "config", "rbac", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(f) // #nosec G304 -- repository manifest
		if err != nil {
			t.Fatal(err)
		}
		for _, doc := range bytes.Split(raw, []byte("\n---")) {
			var role rbacv1.ClusterRole
			if err := yaml.Unmarshal(doc, &role); err != nil || (role.Kind != "ClusterRole" && role.Kind != "Role") {
				continue
			}
			seen[role.Name] = true
			got := allows(role.Rules, verb, group, "pgadmins")
			if got != opens[role.Name] {
				t.Errorf("%s (%s): opens pgAdmin = %v, want %v", role.Name, filepath.Base(f), got, opens[role.Name])
			}
			if got && (role.Labels["rbac.authorization.k8s.io/aggregate-to-view"] == "true" || role.Labels["rbac.authorization.k8s.io/aggregate-to-edit"] == "true") {
				t.Errorf("%s is aggregated to view or edit and opens pgAdmin", role.Name)
			}
		}
	}
	for name := range opens {
		if !seen[name] {
			t.Errorf("%s is not in config/rbac", name)
		}
	}

	// What OLM renders for an owned CRD and aggregates into the built-in
	// roles: <crd>-<version>-view (get, list, watch) into view,
	// <crd>-<version>-edit (create, update, patch, delete) into edit. The
	// built-in view and edit name no pgtoolbox.fyannk.dev rule of their own.
	for name, verbs := range map[string][]string{
		"view": {"get", "list", "watch"},
		"edit": {"create", "update", "patch", "delete", "get", "list", "watch"},
	} {
		rules := []rbacv1.PolicyRule{{APIGroups: []string{group}, Resources: []string{"pgadmins"}, Verbs: verbs}}
		if allows(rules, verb, group, "pgadmins") {
			t.Errorf("OLM's %s role for pgadmins opens pgAdmin", name)
		}
	}
	// The verb must not be one of those, whatever the role.
	if slices.Contains([]string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"}, verb) {
		t.Errorf("PgAdminConnectVerb %q is a verb OLM's view or edit role carries", verb)
	}
}
