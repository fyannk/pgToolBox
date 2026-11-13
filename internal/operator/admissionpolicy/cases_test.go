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

package admissionpolicy

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

const (
	// Every requester is in this group, which RBAC lets write the three
	// kinds: the admission policy, not RBAC, is what is under test.
	teamGroup = "team-members"
	namespace = "default"
)

// requester is who sends a case's request: a user of teamGroup with extra
// groups and the RBAC rules it holds in namespace.
type requester struct {
	Groups []string            `json:"groups,omitempty"`
	Rules  []rbacv1.PolicyRule `json:"rules,omitempty"`
}

// policyCase is one request and the policy's answer.
type policyCase struct {
	Name      string         `json:"name"`
	Requester string         `json:"requester"`
	Object    map[string]any `json:"object"`
	OldObject map[string]any `json:"oldObject,omitempty"`
	Allowed   bool           `json:"allowed"`
	RefusedBy string         `json:"refusedBy,omitempty"`
}

// object and oldObject return the case's objects, defaults applied
// (apiVersion, namespace; the old object's kind and name are the object's);
// oldObject is nil for a CREATE.
func (c policyCase) object() *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: c.Object}
	u = u.DeepCopy()
	if u.GetAPIVersion() == "" {
		u.SetAPIVersion(pgtoolboxv1alpha1.GroupVersion.String())
	}
	if u.GetNamespace() == "" {
		u.SetNamespace(namespace)
	}
	return u
}

func (c policyCase) oldObject() *unstructured.Unstructured {
	if c.OldObject == nil {
		return nil
	}
	u := (&unstructured.Unstructured{Object: c.OldObject}).DeepCopy()
	o := c.object()
	if u.GetKind() == "" {
		u.SetKind(o.GetKind())
	}
	if u.GetAPIVersion() == "" {
		u.SetAPIVersion(o.GetAPIVersion())
	}
	if u.GetName() == "" {
		u.SetName(o.GetName())
	}
	if u.GetNamespace() == "" {
		u.SetNamespace(o.GetNamespace())
	}
	return u
}

// policyCases is testdata/cases.yaml.
type policyCases struct {
	Values struct {
		WorkloadIdentity struct {
			AllowedUsers  []string `json:"allowedUsers"`
			AllowedGroups []string `json:"allowedGroups"`
		} `json:"workloadIdentity"`
	} `json:"values"`
	Requesters map[string]requester `json:"requesters"`
	Cases      []policyCase         `json:"cases"`
}

// refusingRules are the rules a case may name in refusedBy.
var refusingRules = []string{"image", "workloadIdentity", "secretRefs", "credentials", "projectedToken", "exposure"}

// loadCases reads testdata/cases.yaml, the table every implementation of
// the operator's admission rules runs, and checks its shape.
func loadCases(t *testing.T) policyCases {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "cases.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cases policyCases
	if err := yaml.UnmarshalStrict(raw, &cases); err != nil {
		t.Fatalf("testdata/cases.yaml: %v", err)
	}
	seen := map[string]bool{}
	for _, c := range cases.Cases {
		switch {
		case c.Name == "" || seen[c.Name]:
			t.Errorf("case %q: a name is required and unique", c.Name)
		case c.object().GetKind() == "" || c.object().GetName() == "":
			t.Errorf("case %q: the object needs a kind and a name", c.Name)
		case c.Allowed != (c.RefusedBy == ""):
			t.Errorf("case %q: a refused case names the rule that refuses it (refusedBy), an allowed one none", c.Name)
		case !c.Allowed && !slices.Contains(refusingRules, c.RefusedBy):
			t.Errorf("case %q: refusedBy %q is none of %v", c.Name, c.RefusedBy, refusingRules)
		}
		if _, ok := cases.Requesters[c.Requester]; !ok {
			t.Errorf("case %q: unknown requester %q", c.Name, c.Requester)
		}
		seen[c.Name] = true
	}
	return cases
}

// TestCasesTable checks the shape of the shared table without an API server,
// and that every rule has a case it refuses and one it allows.
func TestCasesTable(t *testing.T) {
	cases := loadCases(t)
	for _, rule := range refusingRules {
		if !slices.ContainsFunc(cases.Cases, func(c policyCase) bool { return c.RefusedBy == rule }) {
			t.Errorf("no case is refused by %s", rule)
		}
	}
	if !slices.ContainsFunc(cases.Cases, func(c policyCase) bool { return c.Allowed && c.OldObject != nil }) {
		t.Error("no allowed UPDATE case")
	}
}
