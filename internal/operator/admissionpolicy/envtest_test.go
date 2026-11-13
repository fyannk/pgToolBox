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
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

const imageOverridePolicy = "pgtoolbox-image-override"

// refusals: a substring of the message of each rule a case names in refusedBy.
var refusals = map[string]string{
	"image":            "spec.image may be set only by",
	"workloadIdentity": "spec.workloadIdentity may be set only by",
	"secretRefs":       "requires get on every Secret it references",
	"credentials":      "offers the credentials of Clusters of its namespace",
	"projectedToken":   "requires create on serviceaccounts/token",
	"exposure":         "require create on the exposure kind",
}

// renderPolicy runs helm template on the operator chart with the admission
// policy on and the values of the cases, and returns its
// ValidatingAdmissionPolicy and binding.
func renderPolicy(t *testing.T, cases policyCases) (*admissionregistrationv1.ValidatingAdmissionPolicy, *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	wi := cases.Values.WorkloadIdentity
	chart := filepath.Join("..", "..", "..", "charts", "pgtoolbox")
	out, err := exec.CommandContext(t.Context(), "helm", "template", "pgtoolbox", chart, "--namespace", "pgtoolbox", // #nosec G204 -- arguments from the test's own table
		"--set", "operator.enabled=true",
		"--set", "operator.admissionPolicy.enabled=true",
		"--set", "operator.admissionPolicy.workloadIdentity.allowedUsers={"+strings.Join(wi.AllowedUsers, ",")+"}",
		"--set", "operator.admissionPolicy.workloadIdentity.allowedGroups={"+strings.Join(wi.AllowedGroups, ",")+"}",
		"--show-only", "templates/operator/admissionpolicy.yaml",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	var policy *admissionregistrationv1.ValidatingAdmissionPolicy
	var binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding
	for _, doc := range bytes.Split(out, []byte("\n---")) {
		kind := struct {
			Kind string `json:"kind"`
		}{}
		if yaml.Unmarshal(doc, &kind) != nil {
			continue
		}
		switch kind.Kind {
		case "ValidatingAdmissionPolicy":
			policy = &admissionregistrationv1.ValidatingAdmissionPolicy{}
			if err := yaml.UnmarshalStrict(doc, policy); err != nil {
				t.Fatalf("policy: %v", err)
			}
		case "ValidatingAdmissionPolicyBinding":
			binding = &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
			if err := yaml.UnmarshalStrict(doc, binding); err != nil {
				t.Fatalf("binding: %v", err)
			}
		}
	}
	if policy == nil || binding == nil || policy.Name != imageOverridePolicy {
		t.Fatalf("the chart rendered no admission policy %s (%v) or binding (%v)", imageOverridePolicy, policy != nil, binding != nil)
	}
	return policy, binding
}

// TestEnvtestAdmissionPolicy evaluates the rendered policy on an API server
// (make test-envtest-admissionpolicy) on every case of testdata/cases.yaml:
// each request is sent as its requester, an UPDATE after a cluster
// administrator created the old object.
func TestEnvtestAdmissionPolicy(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set (make test-envtest-admissionpolicy)")
	}
	cases := loadCases(t)
	policy, binding := renderPolicy(t, cases)

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	admin, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, pgtoolboxv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	root, err := client.New(admin, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	clients := map[string]client.Client{}
	for name, r := range cases.Requesters {
		config := rest.CopyConfig(admin)
		config.Impersonate = rest.ImpersonationConfig{UserName: name, Groups: append([]string{teamGroup}, r.Groups...)}
		if clients[name], err = client.New(config, client.Options{Scheme: scheme}); err != nil {
			t.Fatal(err)
		}
	}

	objects := []client.Object{
		&rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: "team-writes"},
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{pgtoolboxv1alpha1.GroupVersion.Group}, Resources: []string{"evidenceagents", "pgadmins", "pgtoolboxconsoles"},
				Verbs: []string{"get", "list", "create", "update", "patch", "delete"},
			}},
		},
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "team-writes"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "team-writes"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: teamGroup}},
		},
	}
	for name, r := range cases.Requesters {
		if len(r.Rules) == 0 {
			continue
		}
		objects = append(objects,
			&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "requester-" + name, Namespace: namespace}, Rules: r.Rules},
			&rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "requester-" + name, Namespace: namespace},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "requester-" + name},
				Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: name}},
			})
	}
	for _, object := range append(objects, policy, binding) {
		if err := root.Create(t.Context(), object); err != nil {
			t.Fatalf("create %T: %v", object, err)
		}
	}
	// The policy takes effect once the API server has compiled it and its
	// binding: wait until a stranger is refused, so no case races it.
	probe := func() *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion(pgtoolboxv1alpha1.GroupVersion.String())
		u.SetKind("EvidenceAgent")
		u.SetName("probe")
		u.SetNamespace(namespace)
		u.Object["spec"] = map[string]any{"clusterRef": map[string]any{"name": "orders"}, "workloadIdentity": map[string]any{}}
		return u
	}
	deadline := time.Now().Add(time.Minute)
	for {
		err := clients["mallory"].Create(t.Context(), probe())
		if apierrors.IsForbidden(err) && strings.Contains(err.Error(), refusals["workloadIdentity"]) {
			break
		}
		if err == nil {
			_ = root.Delete(t.Context(), probe())
		}
		if time.Now().After(deadline) {
			t.Fatalf("the admission policy never took effect; last create error: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}

	for _, c := range cases.Cases {
		t.Run(c.Name, func(t *testing.T) {
			object := c.object()
			var err error
			t.Cleanup(func() { _ = root.Delete(context.Background(), c.object()) })
			if old := c.oldObject(); old != nil {
				if err := root.Create(t.Context(), old); err != nil {
					t.Fatalf("create the old object: %v", err)
				}
				object.SetResourceVersion(old.GetResourceVersion())
				err = clients[c.Requester].Update(t.Context(), object)
			} else {
				err = clients[c.Requester].Create(t.Context(), object)
			}
			switch {
			case c.Allowed && err != nil:
				t.Fatalf("error = %v, want the request admitted", err)
			case !c.Allowed && (!apierrors.IsForbidden(err) || !strings.Contains(err.Error(), refusals[c.RefusedBy])):
				t.Fatalf("error = %v, want a Forbidden from the %s rule (%q)", err, c.RefusedBy, refusals[c.RefusedBy])
			}
		})
	}
}
