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

package specs

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// TestGeneratedRoleUpdatesOwnerFinalizers: every kind the operator names as
// a controller owner with blockOwnerDeletion needs `<resource>/finalizers
// update` in the generated manager ClusterRole, or an API server enforcing
// OwnerReferencesPermissionEnforcement (OpenShift) refuses every child. The
// envtest suites run with that plugin; this is the check `make test` sees.
func TestGeneratedRoleUpdatesOwnerFinalizers(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var role rbacv1.ClusterRole
	if err := yaml.Unmarshal(raw, &role); err != nil {
		t.Fatal(err)
	}
	allowed := func(resource string) bool {
		for _, rule := range role.Rules {
			if slices.Contains(rule.APIGroups, "pgtoolbox.fyannk.dev") && slices.Contains(rule.Resources, resource) &&
				len(rule.ResourceNames) == 0 && (slices.Contains(rule.Verbs, "update") || slices.Contains(rule.Verbs, "*")) {
				return true
			}
		}
		return false
	}
	for _, tc := range []struct{ owner, setBy string }{
		{"evidenceagents", "internal/operator/evidenceagent/specs.go ownerReference"},
		{"pgadmins", "internal/operator/pgadmin/builders.go ownerRef"},
		{"pgadminaccesses", "internal/operator/pgadmin/crossnamespace.go NewControllerRef"},
		{"pgtoolboxconsoles", "internal/operator/console/builders.go ownerReference"},
	} {
		t.Run(tc.owner, func(t *testing.T) {
			if !allowed(tc.owner + "/finalizers") {
				t.Fatalf("%s sets blockOwnerDeletion on children, but the generated role cannot update %s/finalizers", tc.setBy, tc.owner)
			}
		})
	}
}
