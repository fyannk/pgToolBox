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

// Package managerrole is the manager ClusterRole the operator is installed
// with, as the operator itself reads it: role.yaml is a copy of the
// controller-gen output config/rbac/role.yaml (hack/sync-manifests.sh,
// checked by make verify-manifests), so the start-up check of each watched
// namespace (cmd/operator) asks for exactly the rules the packaging grants,
// never for a hand-kept list that can drift from them; and the check
// itself (PermittedNamespaces), kept out of pkg/ because it creates
// access reviews and the console imports pkg/discovery read-only.
package managerrole

import (
	_ "embed"
	"fmt"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// The start-up check reviews the operator's own rules in each watched
// namespace (SelfSubjectRulesReview, then SelfSubjectAccessReview where an
// authorizer cannot list its rules). Kubernetes grants both to every
// authenticated user (system:basic-user); the manager role holds them too,
// so a cluster that narrowed that default still lets the check run.
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=selfsubjectrulesreviews;selfsubjectaccessreviews,verbs=create

//go:embed role.yaml
var roleYAML []byte

// Rules is the manager ClusterRole's rules.
func Rules() ([]rbacv1.PolicyRule, error) {
	var role rbacv1.ClusterRole
	if err := yaml.UnmarshalStrict(roleYAML, &role); err != nil {
		return nil, fmt.Errorf("read the embedded manager role: %w", err)
	}
	if len(role.Rules) == 0 {
		return nil, fmt.Errorf("the embedded manager role %q has no rules", role.Name)
	}
	return role.Rules, nil
}
