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
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// olmOwnedCRDRoles models the ClusterRoles operator-lifecycle-manager
// renders for one owned CRD (pkg/controller/operators/olm/operatorgroup.go:
// VerbsForSuffix, AdminVerbs, EditVerbs, ViewVerbs; names namePrefix+suffix
// with namePrefix "<crd name>-<version>-"; label
// rbac.authorization.k8s.io/aggregate-to-<suffix>).
func olmOwnedCRDRoles(crdName, version string) []rbacv1.ClusterRole {
	plural, group, _ := strings.Cut(crdName, ".")
	prefix := crdName + "-" + version + "-"
	role := func(suffix string, rule rbacv1.PolicyRule) rbacv1.ClusterRole {
		agg := suffix
		if suffix == "crdview" {
			agg = "view"
		}
		return rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				Name:   prefix + suffix,
				Labels: map[string]string{"rbac.authorization.k8s.io/aggregate-to-" + agg: "true"},
			},
			Rules: []rbacv1.PolicyRule{rule},
		}
	}
	on := func(verbs ...string) rbacv1.PolicyRule {
		return rbacv1.PolicyRule{APIGroups: []string{group}, Resources: []string{plural}, Verbs: verbs}
	}
	return []rbacv1.ClusterRole{
		role("admin", on(rbacv1.VerbAll)),
		role("edit", on("create", "update", "patch", "delete")),
		role("view", on("get", "list", "watch")),
		role("crdview", rbacv1.PolicyRule{
			APIGroups: []string{"apiextensions.k8s.io"}, Resources: []string{"customresourcedefinitions"},
			ResourceNames: []string{crdName}, Verbs: []string{"get"},
		}),
	}
}

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
// the share of admin grant it, and on OLM the -admin role OLM generates for
// the CRD ("*", aggregated to admin); the -view and -edit roles it
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
	// roles, modelled on operator-lifecycle-manager's
	// pkg/controller/operators/olm/operatorgroup.go (VerbsForSuffix,
	// ensureProvidedAPIClusterRole): <crd>-<version>-admin ("*") into admin,
	// -edit (create, update, patch, delete) into edit, -view (get, list,
	// watch) into view, and -crdview (get on the CRD). admin is intended to
	// open pgAdmin (a namespace admin reads the offered Secrets anyway);
	// view and edit are not.
	olmIntended := map[string]bool{"admin": true, "edit": false, "view": false}
	for _, role := range olmOwnedCRDRoles("pgadmins."+group, pgtoolboxv1alpha1.GroupVersion.Version) {
		suffix := role.Name[strings.LastIndex(role.Name, "-")+1:]
		if strings.HasSuffix(role.Name, "-crdview") {
			if allows(role.Rules, verb, group, "pgadmins") {
				t.Errorf("OLM's %s opens pgAdmin", role.Name)
			}
			continue
		}
		want, known := olmIntended[suffix]
		if !known {
			t.Fatalf("OLM role %s: unknown suffix %q", role.Name, suffix)
		}
		if got := allows(role.Rules, verb, group, "pgadmins"); got != want {
			t.Errorf("OLM's %s (aggregated to %s): opens pgAdmin = %v, want %v", role.Name, suffix, got, want)
		}
		if role.Labels["rbac.authorization.k8s.io/aggregate-to-"+suffix] != "true" {
			t.Errorf("OLM's %s is not modelled as aggregated to %s", role.Name, suffix)
		}
	}
	// The verb must not be one of those, whatever the role.
	if slices.Contains([]string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"}, verb) {
		t.Errorf("PgAdminConnectVerb %q is a verb OLM's view or edit role carries", verb)
	}
}
