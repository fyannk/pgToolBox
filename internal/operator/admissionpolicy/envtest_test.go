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
	"fmt"
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
const imageOverridePolicy = "pgtoolbox-image-override"

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
		// The chart also renders the restore-drill guard's policies; this
		// suite tests the image-override one.
		case "ValidatingAdmissionPolicy":
			p := &admissionregistrationv1.ValidatingAdmissionPolicy{}
			if err := yaml.UnmarshalStrict(doc, p); err != nil {
				t.Fatalf("policy: %v", err)
			}
			if p.Name == imageOverridePolicy {
				policy = p
			}
		case "ValidatingAdmissionPolicyBinding":
			b := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
			if err := yaml.UnmarshalStrict(doc, b); err != nil {
				t.Fatalf("binding: %v", err)
			}
			if b.Name == imageOverridePolicy {
				binding = b
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
		// nsreader may read every Secret of the namespace (what admin and
		// edit grant).
		&rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "read-all", Namespace: namespace},
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"},
			}},
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "read-all", Namespace: namespace},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "read-all"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "nsreader"}},
		},
		// minter may mint tokens of the ServiceAccounts of a few consoles
		// (what the projected Prometheus token rule asks), and read no Secret.
		&rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "mint-console-tokens", Namespace: namespace},
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{""}, Resources: []string{"serviceaccounts/token"}, Verbs: []string{"create"},
				ResourceNames: []string{"pt-ok-console", "pt-console-x", "pt-move-console"},
			}},
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "mint-console-tokens", Namespace: namespace},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "mint-console-tokens"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "minter"}},
		},
		// exposer may create Ingresses (not Routes) and read no Secret.
		&rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "create-ingresses", Namespace: namespace},
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{"networking.k8s.io"}, Resources: []string{"ingresses"}, Verbs: []string{"create"},
			}},
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "create-ingresses", Namespace: namespace},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "create-ingresses"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "exposer"}},
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
  credentials: {application: false, databaseRoles: false}
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
	t.Run("secret destinations", func(t *testing.T) { testSecretDestinations(t, root, as) })
	t.Run("evidence agent allowed secrets", func(t *testing.T) { testAllowedSecrets(t, root, as) })
	t.Run("pgadmin cluster credentials", func(t *testing.T) { testPgAdminCredentials(t, root, as) })
	t.Run("pgadmin sign-in audience", func(t *testing.T) { testPgAdminAudience(t, root, as) })
	t.Run("pgadmin settings database", func(t *testing.T) { testPgAdminConfigDatabase(t, root, as) })
	t.Run("pgadmin proxy session lifetime", func(t *testing.T) { testPgAdminSessionMaxAge(t, root, as) })
	t.Run("exposure annotations", func(t *testing.T) { testExposureAnnotations(t, root, as) })
	t.Run("prometheus projected token", func(t *testing.T) { testPrometheusProjectedToken(t, root, as) })
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

// pgadminWith returns a PgAdmin offering no Cluster credentials (the
// credentials rule has its own cases, pgadminOffering), with the auth and
// extra spec fragments.
func pgadminWith(t *testing.T, name, auth, extra string) *unstructured.Unstructured {
	t.Helper()
	return object(t, `apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgAdmin
metadata: {name: `+name+`, namespace: default}
spec:
  clusterRefs: [{name: orders}]
  credentials: {application: false, databaseRoles: false}
  auth:
`+auth+extra)
}

// pgadminOffering returns a PgAdmin that names no Secret (OpenShift
// oauth-proxy, default images) with the sources and credentials fragments.
func pgadminOffering(t *testing.T, name, sources, credentials string) *unstructured.Unstructured {
	t.Helper()
	return object(t, `apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgAdmin
metadata: {name: `+name+`, namespace: default}
spec:
`+sources+credentials+`  auth:
    openshiftOAuthProxy: {}
`)
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
		// The console sends the headers to the endpoint the editor chose.
		"tracing.headersSecretRef":              "  tracing:\n    endpoint: https://collector.attacker.example\n    headersSecretRef: {name: unreadable, key: headers}\n",
		"auth.oidc.grantClient.clientSecretRef": "  auth:\n    provider: oidc\n    oidc:\n      issuerURL: https://idp.example.com\n      clientID: c\n      clientSecretRef: {name: readable}\n      grantClient:\n        clientID: g\n        clientSecretRef: {name: unreadable}\n",
		"restoreDrills.sealKeySecretRef":        "  operations: true\n  restoreDrills:\n    enabled: true\n    sealKeySecretRef: {name: unreadable, key: keys}\n",
	} {
		t.Run("PgToolBoxConsole "+name, func(t *testing.T) {
			base := "c-" + strings.ReplaceAll(strings.ToLower(name), ".", "-")
			// A console name is at most 40 characters (CRD CEL).
			base = strings.TrimSuffix(base[:min(len(base), 36)], "-")
			refused(t, create(t, as("mallory"), consoleWith(t, base, spec)))
			// reader may read every other Secret the object names: the
			// refusal is this field's.
			refused(t, create(t, as("reader"), consoleWith(t, base+"-r", spec)))
			// The same object naming a Secret the user may read is admitted:
			// the refusal above is the Secret rule's, not the CRD's.
			if err := create(t, as("reader"), consoleWith(t, base+"-ok", strings.ReplaceAll(spec, "unreadable", "readable"))); err != nil {
				t.Fatalf("with a readable Secret: %v, want admitted", err)
			}
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

// testSecretDestinations: a reference kept as it was but sent somewhere else
// (the issuer, the Prometheus URL, the mail server, the tracing endpoint, the
// image registry) is a new reference: whoever changes the destination must
// be able to read the Secret.
func testSecretDestinations(t *testing.T, root client.Client, as func(string, ...string) client.Client) {
	for _, tc := range []struct {
		name, kind, spec string
		dest             []string
		to               any
	}{
		{"tracing", "console", "  tracing:\n    endpoint: https://collector.example.com\n    headersSecretRef: {name: SECRET, key: headers}\n", []string{"spec", "tracing", "endpoint"}, "https://collector.attacker.example"},
		{"oidc", "console", "  auth:\n    provider: oidc\n    oidc:\n      issuerURL: https://idp.example.com\n      clientID: c\n      clientSecretRef: {name: SECRET}\n", []string{"spec", "auth", "oidc", "issuerURL"}, "https://idp.attacker.example"},
		{"smtp", "console", "  history: {persistent: true}\n  digest:\n    enabled: true\n    delivery:\n      smtp:\n        address: smtp.example.com:587\n        from: a@example.com\n        to: [a@example.com]\n        username: u\n        passwordSecretRef: {name: SECRET, key: password}\n", []string{"spec", "digest", "delivery", "smtp", "address"}, "smtp.attacker.example:587"},
		{"prometheus", "console", "  prometheus:\n    url: https://prometheus.example.com\n    tokenSecretRef: {name: SECRET, key: token}\n", []string{"spec", "prometheus", "url"}, "https://prometheus.attacker.example"},
		{"registry", "console", "  image:\n    repository: registry.example.com/console\n    tag: v1\n    pullSecrets: [{name: SECRET}]\n", []string{"spec", "image", "repository"}, "registry.attacker.example/console"},
		{"pgadmin-oauth2", "pgadmin", strings.Replace(pgadminOAuth, "%s", "SECRET", 1), []string{"spec", "auth", "oauth2", "issuerURL"}, "https://idp.attacker.example"},
		// The CA a destination is trusted with is part of it: a new CA lets
		// whoever it certifies stand in for the same URL.
		{"oidc-ca", "console", "  auth:\n    provider: oidc\n    oidc:\n      issuerURL: https://idp.example.com\n      clientID: c\n      clientSecretRef: {name: SECRET}\n", []string{"spec", "auth", "oidc", "caConfigMapRef", "name"}, "attacker-ca"},
		{"oidc-grant-ca", "console", "  auth:\n    provider: oidc\n    oidc:\n      issuerURL: https://idp.example.com\n      clientID: c\n      clientSecretRef: {name: readable}\n      grantClient:\n        clientID: g\n        clientSecretRef: {name: SECRET}\n      caConfigMapRef: {name: idp-ca}\n", []string{"spec", "auth", "oidc", "caConfigMapRef", "name"}, "attacker-ca"},
		{"openshift-ca", "console", "  publicURL: https://c.example.com\n  auth:\n    provider: openshift\n    openshift:\n      clientSecretRef: {name: SECRET, key: token}\n", []string{"spec", "auth", "openshift", "caConfigMapRef", "name"}, "attacker-ca"},
		{"prometheus-ca", "console", "  prometheus:\n    url: https://prometheus.example.com\n    tokenSecretRef: {name: SECRET, key: token}\n", []string{"spec", "prometheus", "caConfigMapRef", "name"}, "attacker-ca"},
		{"smtp-tls", "console", "  history: {persistent: true}\n  digest:\n    enabled: true\n    delivery:\n      smtp:\n        address: smtp.example.com:587\n        from: a@example.com\n        to: [a@example.com]\n        username: u\n        passwordSecretRef: {name: SECRET, key: password}\n", []string{"spec", "digest", "delivery", "smtp", "tls"}, "tls"},
		// What decides where an EvidenceAgent sends the Secrets it may read.
		{"agent-cluster", "agent", "  clusterRef: {name: orders}\n  allowedSecrets: [SECRET]\n", []string{"spec", "clusterRef", "name"}, "billing"},
		{"agent-objectstore", "agent", "  clusterRef: {name: orders}\n  allowedSecrets: [SECRET]\n", []string{"spec", "objectStoreRef", "name"}, "attacker-store"},
		{"agent-insecure", "agent", "  clusterRef: {name: orders}\n  allowedSecrets: [SECRET]\n", []string{"spec", "allowInsecureEndpoint"}, true},
		{"agent-pgbackrest-endpoint", "agent", "  clusterRef: {name: orders}\n  allowedSecrets: [SECRET]\n  pgbackrest: {endpoint: s3.example.com}\n", []string{"spec", "pgbackrest", "endpoint"}, "s3.attacker.example"},
		{"agent-pgbackrest-repository", "agent", "  clusterRef: {name: orders}\n  allowedSecrets: [SECRET]\n  pgbackrest: {repository: 1}\n", []string{"spec", "pgbackrest", "repository"}, int64(2)},
		{"agent-pgbackrest-ca", "agent", "  clusterRef: {name: orders}\n  allowedSecrets: [SECRET]\n  pgbackrest: {}\n", []string{"spec", "pgbackrest", "endpointCA"}, map[string]any{"name": "attacker-ca", "key": "ca.crt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			build := func(name, secret string) *unstructured.Unstructured {
				spec := strings.ReplaceAll(tc.spec, "SECRET", secret)
				switch tc.kind {
				case "pgadmin":
					return pgadminWith(t, name, spec, "")
				case "agent":
					return object(t, "apiVersion: pgtoolbox.fyannk.dev/v1alpha1\nkind: EvidenceAgent\nmetadata: {name: "+name+", namespace: default}\nspec:\n"+spec)
				}
				return consoleWith(t, name, spec)
			}
			for _, user := range []string{"mallory", "reader"} {
				// Seeded by the cluster admin with a Secret user may not read.
				u := build("d-"+tc.name+"-"+user, "unreadable")
				t.Cleanup(func() { _ = root.Delete(context.Background(), u) })
				if err := root.Create(t.Context(), u); err != nil {
					t.Fatal(err)
				}
				c := as(user)
				if err := c.Get(t.Context(), client.ObjectKeyFromObject(u), u); err != nil {
					t.Fatal(err)
				}
				if err := unstructured.SetNestedField(u.Object, tc.to, tc.dest...); err != nil {
					t.Fatal(err)
				}
				refused(t, c.Update(t.Context(), u))
			}
			// With a Secret the user may read, the same move is admitted: the
			// refusal above is the Secret rule's.
			u := build("d-"+tc.name+"-ok", "readable")
			t.Cleanup(func() { _ = root.Delete(context.Background(), u) })
			if err := root.Create(t.Context(), u); err != nil {
				t.Fatal(err)
			}
			reader := as("reader")
			if err := reader.Get(t.Context(), client.ObjectKeyFromObject(u), u); err != nil {
				t.Fatal(err)
			}
			if err := unstructured.SetNestedField(u.Object, tc.to, tc.dest...); err != nil {
				t.Fatal(err)
			}
			if err := reader.Update(t.Context(), u); err != nil {
				t.Fatalf("moving a readable Secret: %v, want admitted", err)
			}
		})
	}
}

// testAllowedSecrets: spec.allowedSecrets of an EvidenceAgent is the agent
// editor's consent that the agent read those Secrets; it means something
// only from somebody who may read them.
func testAllowedSecrets(t *testing.T, root client.Client, as func(string, ...string) client.Client) {
	agent := func(name string, secrets ...string) *pgtoolboxv1alpha1.EvidenceAgent {
		a := evidenceAgent(name)
		a.Spec.AllowedSecrets = secrets
		return a
	}
	create := func(t *testing.T, c client.Client, a *pgtoolboxv1alpha1.EvidenceAgent) error {
		t.Helper()
		t.Cleanup(func() { _ = root.Delete(context.Background(), a) })
		return c.Create(t.Context(), a)
	}
	t.Run("a Secret the user may not read is refused", func(t *testing.T) {
		refused(t, create(t, as("mallory"), agent("as-mallory", "unreadable")))
		refused(t, create(t, as("reader"), agent("as-reader-mixed", "readable", "unreadable")))
	})
	t.Run("Secrets the user may read are admitted", func(t *testing.T) {
		if err := create(t, as("reader"), agent("as-reader", "readable")); err != nil {
			t.Fatalf("error = %v, want admitted", err)
		}
		if err := create(t, as("mallory"), agent("as-none")); err != nil {
			t.Fatalf("no allowedSecrets: %v, want admitted", err)
		}
	})
	t.Run("many Secrets stay within the CEL cost budget", func(t *testing.T) {
		// One authorizer check per name exceeded the runtime cost limit
		// at a handful of names (an e2e agent was refused); the
		// namespace-wide check comes first.
		names := make([]string, 0, 16)
		for i := range 16 {
			names = append(names, fmt.Sprintf("s-%02d", i))
		}
		if err := create(t, as("nsreader"), agent("as-nsreader-many", names...)); err != nil {
			t.Fatalf("16 allowedSecrets from a namespace-wide reader: %v, want admitted", err)
		}
		if err := create(t, root, agent("as-root-many", names...)); err != nil {
			t.Fatalf("16 allowedSecrets from the cluster admin: %v, want admitted", err)
		}
		// The longest destination (every pgBackRest field) on 16 names,
		// compared old against new on an update that moves all of them.
		full := agent("as-nsreader-full", names...)
		full.Spec.PgBackRest = &pgtoolboxv1alpha1.EvidenceAgentPgBackRestSpec{
			Repository: 7, Endpoint: strings.Repeat("s", 60) + ".example.com:9000",
			EndpointCA: &pgtoolboxv1alpha1.SecretKeyReference{Name: strings.Repeat("c", 200), Key: strings.Repeat("k", 200)},
		}
		full.Spec.AllowInsecureEndpoint = true
		nsreader := as("nsreader")
		if err := create(t, nsreader, full); err != nil {
			t.Fatalf("16 allowedSecrets with a full pgBackRest destination: %v, want admitted", err)
		}
		full.Spec.PgBackRest.Endpoint = strings.Repeat("t", 60) + ".example.com:9000"
		if err := nsreader.Update(t.Context(), full); err != nil {
			t.Fatalf("moving 16 allowedSecrets: %v, want admitted", err)
		}
		// More than two added at once needs get on every Secret of the
		// namespace: a reader of some names adds them two at a time.
		refused(t, create(t, as("reader"), agent("as-reader-three", "readable", "readable-b", "readable-c")))
	})
	t.Run("on update only an added name is checked", func(t *testing.T) {
		a := agent("as-update", "unreadable")
		if err := create(t, root, a); err != nil {
			t.Fatal(err)
		}
		mallory := as("mallory")
		if err := mallory.Get(t.Context(), client.ObjectKeyFromObject(a), a); err != nil {
			t.Fatal(err)
		}
		a.Spec.RefreshInterval = &metav1.Duration{Duration: 2 * time.Minute}
		if err := mallory.Update(t.Context(), a); err != nil {
			t.Fatalf("an edit that leaves allowedSecrets alone: %v", err)
		}
		a.Spec.AllowedSecrets = append(a.Spec.AllowedSecrets, "other-unreadable")
		refused(t, mallory.Update(t.Context(), a))
	})
}

// credentialsRefused is a Forbidden that names the PgAdmin credentials rule.
func credentialsRefused(t *testing.T, err error) {
	t.Helper()
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "offers the credentials of Clusters of its namespace") {
		t.Fatalf("error = %v, want a Forbidden naming the PgAdmin credentials rule", err)
	}
}

// testPgAdminCredentials: whoever opens a PgAdmin holds the credentials of
// the Clusters it registers, whose Secrets the Cluster names: offering them
// requires get on the Secrets of the namespace. A role with create pgadmins
// and connect but no get secrets cannot reach the superuser that way.
func testPgAdminCredentials(t *testing.T, root client.Client, as func(string, ...string) client.Client) {
	create := func(t *testing.T, c client.Client, u *unstructured.Unstructured) error {
		t.Helper()
		t.Cleanup(func() { _ = root.Delete(context.Background(), u) })
		return c.Create(t.Context(), u)
	}
	admitted := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("error = %v, want admitted", err)
		}
	}
	refs := "  clusterRefs: [{name: orders}]\n"
	none := "  credentials: {application: false, databaseRoles: false}\n"
	superuser := "  credentials: {application: false, databaseRoles: false, superuser: true}\n"

	t.Run("superuser without get on the namespace's Secrets is refused", func(t *testing.T) {
		credentialsRefused(t, create(t, as("mallory"), pgadminOffering(t, "cr-super", refs, superuser)))
		// get on one named Secret is not get on the Cluster's.
		credentialsRefused(t, create(t, as("reader"), pgadminOffering(t, "cr-super-r", refs, superuser)))
	})
	t.Run("the default credentials (application, DatabaseRoles) are refused too", func(t *testing.T) {
		credentialsRefused(t, create(t, as("mallory"), pgadminOffering(t, "cr-default", refs, "")))
		credentialsRefused(t, create(t, as("mallory"), pgadminOffering(t, "cr-selector", "  clusterSelector: {matchLabels: {team: a}}\n", "")))
	})
	t.Run("a PgAdmin offering nothing, or Clusters of other namespaces only, is admitted", func(t *testing.T) {
		admitted(t, create(t, as("mallory"), pgadminOffering(t, "cr-none", refs, none)))
		admitted(t, create(t, as("mallory"), pgadminOffering(t, "cr-cross", "  crossNamespaceClusterRefs: [{namespace: team-b, name: orders}]\n", superuser)))
	})
	t.Run("a user who may read the namespace's Secrets may offer them", func(t *testing.T) {
		admitted(t, create(t, as("nsreader"), pgadminOffering(t, "cr-ns", refs, superuser)))
	})
	t.Run("on update only a wider offer is checked", func(t *testing.T) {
		seed := func(t *testing.T, name, sources, credentials string) *unstructured.Unstructured {
			t.Helper()
			u := pgadminOffering(t, name, sources, credentials)
			if err := create(t, root, u); err != nil {
				t.Fatal(err)
			}
			if err := as("mallory").Get(t.Context(), client.ObjectKeyFromObject(u), u); err != nil {
				t.Fatal(err)
			}
			return u
		}
		mallory := as("mallory")
		u := seed(t, "cr-update", refs, "")
		// An edit that leaves the offer and the audience alone passes.
		if err := unstructured.SetNestedStringSlice(u.Object, []string{"rw", "ro"}, "spec", "endpoints"); err != nil {
			t.Fatal(err)
		}
		admitted(t, mallory.Update(t.Context(), u))
		// Narrowing is no new disclosure.
		if err := unstructured.SetNestedField(u.Object, false, "spec", "credentials", "databaseRoles"); err != nil {
			t.Fatal(err)
		}
		admitted(t, mallory.Update(t.Context(), u))
		if err := unstructured.SetNestedField(u.Object, true, "spec", "credentials", "superuser"); err != nil {
			t.Fatal(err)
		}
		credentialsRefused(t, mallory.Update(t.Context(), u))

		u = seed(t, "cr-update-ref", refs, "")
		if err := unstructured.SetNestedSlice(u.Object, []any{map[string]any{"name": "orders"}, map[string]any{"name": "billing"}}, "spec", "clusterRefs"); err != nil {
			t.Fatal(err)
		}
		credentialsRefused(t, mallory.Update(t.Context(), u))

		u = seed(t, "cr-update-sel", "  clusterSelector: {matchLabels: {team: a}}\n", "")
		if err := unstructured.SetNestedStringMap(u.Object, map[string]string{"team": "b"}, "spec", "clusterSelector", "matchLabels"); err != nil {
			t.Fatal(err)
		}
		credentialsRefused(t, mallory.Update(t.Context(), u))
	})
}

// testPgAdminAudience: who may sign in to a PgAdmin that offers Cluster
// credentials decides who holds them. Widening it (the required claims, the
// username claim, the scopes, the client, pgAdmin's administrators, the
// OpenShift proxy) is checked like widening the offer; the button label is
// not, and neither is a PgAdmin that offers nothing.
func testPgAdminAudience(t *testing.T, root client.Client, as func(string, ...string) client.Client) {
	seed := func(t *testing.T, manifest string) *unstructured.Unstructured {
		t.Helper()
		u := object(t, manifest)
		t.Cleanup(func() { _ = root.Delete(context.Background(), u) })
		if err := root.Create(t.Context(), u); err != nil {
			t.Fatal(err)
		}
		if err := root.Get(t.Context(), client.ObjectKeyFromObject(u), u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	oauth := func(name, credentials string) string {
		return `apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgAdmin
metadata: {name: ` + name + `, namespace: default}
spec:
  clusterRefs: [{name: orders}]
` + credentials + `  auth:
    oauth2:
      issuerURL: https://idp.example.com/realms/corp
      clientID: pgadmin
      clientSecretRef: {name: readable}
      requiredClaims: {groups: [dba]}
`
	}
	superuser := "  credentials: {superuser: true}\n"
	for _, tc := range []struct {
		name  string
		path  []string
		value any
	}{
		{"requiredClaims", []string{"spec", "auth", "oauth2", "requiredClaims"}, map[string]any{"email": []any{"mallory@evil.example"}}},
		{"usernameClaim", []string{"spec", "auth", "oauth2", "usernameClaim"}, "preferred_username"},
		{"scopes", []string{"spec", "auth", "oauth2", "scopes"}, []any{"openid", "email", "profile", "groups"}},
		{"admins", []string{"spec", "admins"}, []any{"mallory@evil.example"}},
		{"clientID", []string{"spec", "auth", "oauth2", "clientID"}, "another-client"},
	} {
		t.Run("changing "+tc.name+" of a PgAdmin offering credentials", func(t *testing.T) {
			name := "au-" + strings.ToLower(tc.name)
			u := seed(t, oauth(name, superuser))
			if err := unstructured.SetNestedField(u.Object, tc.value, tc.path...); err != nil {
				t.Fatal(err)
			}
			// reader may read the client Secret but not every Secret of the
			// namespace: the refusal is the credentials rule's.
			credentialsRefused(t, as("mallory").Update(t.Context(), u.DeepCopy()))
			credentialsRefused(t, as("reader").Update(t.Context(), u.DeepCopy()))
			if err := as("nsreader").Update(t.Context(), u.DeepCopy()); err != nil {
				t.Fatalf("a reader of the namespace's Secrets: %v, want admitted", err)
			}
		})
	}
	t.Run("the default credentials are covered too", func(t *testing.T) {
		u := seed(t, oauth("au-default", ""))
		if err := unstructured.SetNestedField(u.Object, map[string]any{"groups": []any{"everyone"}}, "spec", "auth", "oauth2", "requiredClaims"); err != nil {
			t.Fatal(err)
		}
		credentialsRefused(t, as("mallory").Update(t.Context(), u))
	})
	t.Run("the button label is cosmetic", func(t *testing.T) {
		u := seed(t, oauth("au-display", superuser))
		if err := unstructured.SetNestedField(u.Object, "Sign in with Corp", "spec", "auth", "oauth2", "displayName"); err != nil {
			t.Fatal(err)
		}
		if err := as("mallory").Update(t.Context(), u); err != nil {
			t.Fatalf("changing displayName: %v, want admitted", err)
		}
	})
	t.Run("a PgAdmin offering no credentials is unaffected", func(t *testing.T) {
		u := seed(t, oauth("au-none", "  credentials: {application: false, databaseRoles: false}\n"))
		if err := unstructured.SetNestedField(u.Object, map[string]any{"groups": []any{"everyone"}}, "spec", "auth", "oauth2", "requiredClaims"); err != nil {
			t.Fatal(err)
		}
		if err := unstructured.SetNestedStringSlice(u.Object, []string{"mallory@evil.example"}, "spec", "admins"); err != nil {
			t.Fatal(err)
		}
		if err := as("mallory").Update(t.Context(), u); err != nil {
			t.Fatalf("widening a PgAdmin that offers nothing: %v, want admitted", err)
		}
	})
	t.Run("changing or switching to the OpenShift proxy is checked", func(t *testing.T) {
		u := seed(t, `apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgAdmin
metadata: {name: au-proxy, namespace: default}
spec:
  clusterRefs: [{name: orders}]
  credentials: {superuser: true}
  auth:
    openshiftOAuthProxy: {}
`)
		if err := unstructured.SetNestedField(u.Object, map[string]any{"repository": "registry.attacker.example/proxy", "tag": "v1"}, "spec", "auth", "openshiftOAuthProxy", "image"); err != nil {
			t.Fatal(err)
		}
		credentialsRefused(t, as("mallory").Update(t.Context(), u))

		u = seed(t, oauth("au-switch", superuser))
		if err := unstructured.SetNestedField(u.Object, map[string]any{"openshiftOAuthProxy": map[string]any{}}, "spec", "auth"); err != nil {
			t.Fatal(err)
		}
		credentialsRefused(t, as("mallory").Update(t.Context(), u))
	})
	t.Run("the largest audience change stays within the CEL cost budget", func(t *testing.T) {
		claims := map[string]any{}
		for i := range 8 {
			values := make([]any, 0, 16)
			for j := range 16 {
				values = append(values, fmt.Sprintf("%02d-%02d-%s", i, j, strings.Repeat("v", 240)))
			}
			claims[fmt.Sprintf("claim-%d", i)] = values
		}
		admins := make([]any, 0, 32)
		for i := range 32 {
			admins = append(admins, fmt.Sprintf("admin-%02d@%s.example", i, strings.Repeat("d", 200)))
		}
		u := object(t, oauth("au-cost", superuser))
		if err := unstructured.SetNestedField(u.Object, claims, "spec", "auth", "oauth2", "requiredClaims"); err != nil {
			t.Fatal(err)
		}
		if err := unstructured.SetNestedSlice(u.Object, admins, "spec", "admins"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = root.Delete(context.Background(), u) })
		nsreader := as("nsreader")
		if err := nsreader.Create(t.Context(), u); err != nil {
			t.Fatalf("create: %v, want admitted", err)
		}
		// Same size, a value of every claim changed: compared in full.
		changed := map[string]any{}
		for k, v := range claims {
			values := append([]any(nil), v.([]any)...)
			values[15] = "changed-" + strings.Repeat("w", 240)
			changed[k] = values
		}
		if err := unstructured.SetNestedField(u.Object, changed, "spec", "auth", "oauth2", "requiredClaims"); err != nil {
			t.Fatal(err)
		}
		if err := nsreader.Update(t.Context(), u); err != nil {
			t.Fatalf("update: %v, want admitted", err)
		}
	})
}

// tokenRefused is a Forbidden that names the projected Prometheus token rule.
func tokenRefused(t *testing.T, err error) {
	t.Helper()
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "requires create on serviceaccounts/token") {
		t.Fatalf("error = %v, want a Forbidden naming the projected token rule", err)
	}
}

// testPrometheusProjectedToken: spec.prometheus.projectedToken sends the
// console ServiceAccount's own token to spec.prometheus.url; setting it, or
// sending it elsewhere, takes the power to mint that token. minter may mint
// the tokens of pt-ok-console, pt-console-x and pt-move-console only.
func testPrometheusProjectedToken(t *testing.T, root client.Client, as func(string, ...string) client.Client) {
	prom := "  prometheus:\n    url: https://prometheus.example.com\n    projectedToken: true\n"
	create := func(t *testing.T, c client.Client, u *unstructured.Unstructured) error {
		t.Helper()
		t.Cleanup(func() { _ = root.Delete(context.Background(), u) })
		return c.Create(t.Context(), u)
	}
	t.Run("a user who may not mint the token cannot project it", func(t *testing.T) {
		tokenRefused(t, create(t, as("mallory"), consoleWith(t, "pt-mallory", "  prometheus:\n    url: https://attacker.example/\n    projectedToken: true\n")))
		// Reading every Secret is not minting a token.
		tokenRefused(t, create(t, as("nsreader"), consoleWith(t, "pt-nsreader", prom)))
		// Minting another console's token does not help.
		tokenRefused(t, create(t, as("minter"), consoleWith(t, "pt-other", prom)))
	})
	t.Run("a user who may mint the console's token can", func(t *testing.T) {
		if err := create(t, as("minter"), consoleWith(t, "pt-ok", prom)); err != nil {
			t.Fatalf("pt-ok (ServiceAccount pt-ok-console): %v, want admitted", err)
		}
		// A name that contains "console" is the ServiceAccount's name.
		if err := create(t, as("minter"), consoleWith(t, "pt-console-x", prom)); err != nil {
			t.Fatalf("pt-console-x (ServiceAccount pt-console-x): %v, want admitted", err)
		}
	})
	t.Run("a console without a projected token is not concerned", func(t *testing.T) {
		if err := create(t, as("mallory"), consoleWith(t, "pt-none", "  prometheus:\n    url: https://prometheus.example.com\n")); err != nil {
			t.Fatalf("error = %v, want admitted", err)
		}
	})
	t.Run("on update, sending it somewhere else is checked again", func(t *testing.T) {
		u := consoleWith(t, "pt-move", prom)
		if err := create(t, root, u); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name  string
			path  []string
			value any
			ok    bool
		}{
			{"an unrelated edit", []string{"spec", "prometheus", "namespaced"}, true, true},
			{"the URL", []string{"spec", "prometheus", "url"}, "https://prometheus.attacker.example", false},
			{"the CA", []string{"spec", "prometheus", "caConfigMapRef"}, map[string]any{"name": "attacker-ca"}, false},
			{"the audience", []string{"spec", "prometheus", "tokenAudience"}, "attacker", false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if err := root.Get(t.Context(), client.ObjectKeyFromObject(u), u); err != nil {
					t.Fatal(err)
				}
				changed := u.DeepCopy()
				if err := unstructured.SetNestedField(changed.Object, tc.value, tc.path...); err != nil {
					t.Fatal(err)
				}
				err := as("mallory").Update(t.Context(), changed.DeepCopy())
				if tc.ok {
					if err != nil {
						t.Fatalf("error = %v, want admitted", err)
					}
					return
				}
				tokenRefused(t, err)
				if err := as("minter").Update(t.Context(), changed); err != nil {
					t.Fatalf("minter: %v, want admitted", err)
				}
			})
		}
	})
}

// testPgAdminConfigDatabase: spec.configDatabase makes the operator create a
// login DatabaseRole on that Cluster whose password the pgAdmin Pod holds
// and offers: it is a credential source, whatever spec.credentials says and
// even when every other Cluster is of another namespace.
func testPgAdminConfigDatabase(t *testing.T, root client.Client, as func(string, ...string) client.Client) {
	create := func(t *testing.T, c client.Client, u *unstructured.Unstructured) error {
		t.Helper()
		t.Cleanup(func() { _ = root.Delete(context.Background(), u) })
		return c.Create(t.Context(), u)
	}
	none := "  credentials: {application: false, databaseRoles: false}\n"
	settings := "  configDatabase: {clusterRef: {name: orders}, database: pgadmin_reporting}\n"
	t.Run("without get on the namespace's Secrets it is refused", func(t *testing.T) {
		credentialsRefused(t, create(t, as("mallory"), pgadminOffering(t, "cd-refs", "  clusterRefs: [{name: orders}]\n", none+settings)))
		credentialsRefused(t, create(t, as("mallory"), pgadminOffering(t, "cd-cross", "  crossNamespaceClusterRefs: [{namespace: team-b, name: orders}]\n", settings)))
		credentialsRefused(t, create(t, as("reader"), pgadminOffering(t, "cd-reader", "  clusterRefs: [{name: orders}]\n", none+settings)))
	})
	t.Run("a reader of the namespace's Secrets may", func(t *testing.T) {
		if err := create(t, as("nsreader"), pgadminOffering(t, "cd-ns", "  clusterRefs: [{name: orders}]\n", none+settings)); err != nil {
			t.Fatalf("error = %v, want admitted", err)
		}
	})
	t.Run("on update, an unrelated edit passes and a wider audience is checked", func(t *testing.T) {
		u := pgadminOffering(t, "cd-update", "  clusterRefs: [{name: orders}]\n", none+settings)
		if err := create(t, root, u); err != nil {
			t.Fatal(err)
		}
		if err := root.Get(t.Context(), client.ObjectKeyFromObject(u), u); err != nil {
			t.Fatal(err)
		}
		edit := u.DeepCopy()
		if err := unstructured.SetNestedStringSlice(edit.Object, []string{"rw", "ro"}, "spec", "endpoints"); err != nil {
			t.Fatal(err)
		}
		if err := as("mallory").Update(t.Context(), edit); err != nil {
			t.Fatalf("unrelated edit: %v, want admitted", err)
		}
		if err := root.Get(t.Context(), client.ObjectKeyFromObject(u), u); err != nil {
			t.Fatal(err)
		}
		if err := unstructured.SetNestedStringSlice(u.Object, []string{"mallory@evil.example"}, "spec", "admins"); err != nil {
			t.Fatal(err)
		}
		credentialsRefused(t, as("mallory").Update(t.Context(), u))
	})
}

// testPgAdminSessionMaxAge: the OpenShift proxy reviews access at sign-in
// only, so its session lifetime bounds how long a revoked person keeps
// pgAdmin. Raising it (above the 1h default when unset) is checked like
// widening the audience; lowering it is free.
func testPgAdminSessionMaxAge(t *testing.T, root client.Client, as func(string, ...string) client.Client) {
	seed := func(t *testing.T, name, proxy string) *unstructured.Unstructured {
		t.Helper()
		u := object(t, `apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgAdmin
metadata: {name: `+name+`, namespace: default}
spec:
  clusterRefs: [{name: orders}]
  auth:
    openshiftOAuthProxy: `+proxy+`
`)
		t.Cleanup(func() { _ = root.Delete(context.Background(), u) })
		if err := root.Create(t.Context(), u); err != nil {
			t.Fatal(err)
		}
		if err := root.Get(t.Context(), client.ObjectKeyFromObject(u), u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	set := func(t *testing.T, u *unstructured.Unstructured, value string) *unstructured.Unstructured {
		t.Helper()
		changed := u.DeepCopy()
		if err := unstructured.SetNestedField(changed.Object, value, "spec", "auth", "openshiftOAuthProxy", "sessionMaxAge"); err != nil {
			t.Fatal(err)
		}
		return changed
	}
	for _, tc := range []struct {
		name, proxy, value string
		ok                 bool
	}{
		{"unset to 12h", "{}", "12h", false},
		{"unset to 61m", "{}", "61m", false},
		{"unset to 1h", "{}", "1h", true},
		{"unset to 30m", "{}", "30m", true},
		{"30m to 45m", "{sessionMaxAge: 30m}", "45m", false},
		{"8h to 2h", "{sessionMaxAge: 8h}", "2h", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := seed(t, "sm-"+strings.ReplaceAll(strings.ReplaceAll(tc.name, " ", "-"), "unset", "u"), tc.proxy)
			err := as("mallory").Update(t.Context(), set(t, u, tc.value))
			if tc.ok {
				if err != nil {
					t.Fatalf("error = %v, want admitted", err)
				}
				return
			}
			credentialsRefused(t, err)
			if err := as("nsreader").Update(t.Context(), set(t, u, tc.value)); err != nil {
				t.Fatalf("a reader of the namespace's Secrets: %v, want admitted", err)
			}
		})
	}
	t.Run("a PgAdmin offering no credentials is unaffected", func(t *testing.T) {
		u := object(t, `apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgAdmin
metadata: {name: sm-none, namespace: default}
spec:
  clusterRefs: [{name: orders}]
  credentials: {application: false, databaseRoles: false}
  auth:
    openshiftOAuthProxy: {}
`)
		t.Cleanup(func() { _ = root.Delete(context.Background(), u) })
		if err := root.Create(t.Context(), u); err != nil {
			t.Fatal(err)
		}
		if err := root.Get(t.Context(), client.ObjectKeyFromObject(u), u); err != nil {
			t.Fatal(err)
		}
		if err := as("mallory").Update(t.Context(), set(t, u, "12h")); err != nil {
			t.Fatalf("error = %v, want admitted", err)
		}
	})
}

// exposureRefused is a Forbidden that names the exposure annotation rule.
func exposureRefused(t *testing.T, err error) {
	t.Helper()
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "an exposure annotation that an ingress controller reads as configuration") {
		t.Fatalf("error = %v, want a Forbidden naming the exposure annotation rule", err)
	}
}

// testExposureAnnotations: the operator writes spec.exposure.annotations,
// and the annotations of metadataPropagation rules for All or Exposure, on
// the Ingress, Route or HTTPRoute. Those an ingress controller reads as
// configuration (snippets, external authentication, mirroring, middlewares)
// take create on that kind: mallory may not create Ingresses, exposer may
// create Ingresses only.
func testExposureAnnotations(t *testing.T, root client.Client, as func(string, ...string) client.Client) {
	create := func(t *testing.T, c client.Client, u *unstructured.Unstructured) error {
		t.Helper()
		t.Cleanup(func() { _ = root.Delete(context.Background(), u) })
		return c.Create(t.Context(), u)
	}
	pgadmin := func(name, extra string) *unstructured.Unstructured {
		return pgadminOffering(t, name, "  clusterRefs: [{name: orders}]\n", "  credentials: {application: false, databaseRoles: false}\n"+extra)
	}
	ingress := func(annotations string) string {
		return "  exposure:\n    type: ingress\n    hostname: pgadmin.example.com\n    annotations: " + annotations + "\n"
	}
	for _, key := range []string{
		"nginx.ingress.kubernetes.io/configuration-snippet",
		"nginx.ingress.kubernetes.io/server-snippet",
		"nginx.org/location-snippets",
		"haproxy.org/backend-config-snippet",
		"nginx.ingress.kubernetes.io/auth-url",
		"nginx.ingress.kubernetes.io/auth-tls-secret",
		"nginx.ingress.kubernetes.io/enable-global-auth",
		"nginx.ingress.kubernetes.io/satisfy",
		"nginx.ingress.kubernetes.io/mirror-target",
		"nginx.ingress.kubernetes.io/proxy-ssl-secret",
		"haproxy-ingress.github.io/config-backend",
		"haproxy-ingress.github.io/auth-url",
		"traefik.ingress.kubernetes.io/router.middlewares",
		"konghq.com/plugins",
	} {
		t.Run(key, func(t *testing.T) {
			name := "ea-" + strings.NewReplacer(".", "-", "/", "-").Replace(key)
			if len(name) > 40 {
				name = strings.TrimRight(name[:40], "-")
			}
			exposureRefused(t, create(t, as("mallory"), pgadmin(name, ingress("{"+key+": x}"))))
		})
	}
	// Every entry of pgtoolbox.admission.exposureDenyList, the list the
	// security documentation's table shows (TestExposureDenyListDocumented).
	for i, entry := range exposureDenyList(t) {
		key := entry.example()
		t.Run("deny list "+string(entry), func(t *testing.T) {
			exposureRefused(t, create(t, as("mallory"), pgadmin(fmt.Sprintf("ea-deny-%02d", i), ingress("{"+key+": x}"))))
		})
	}
	t.Run("keys that tune the route to the workload's own Service pass", func(t *testing.T) {
		for i, key := range []string{
			"nginx.ingress.kubernetes.io/rewrite-target",
			"nginx.ingress.kubernetes.io/use-regex",
			"nginx.ingress.kubernetes.io/upstream-vhost",
			"nginx.ingress.kubernetes.io/backend-protocol",
			"nginx.ingress.kubernetes.io/proxy-read-timeout",
			"alb.ingress.kubernetes.io/healthcheck-path",
			"alb.ingress.kubernetes.io/target-type",
			"haproxy.router.openshift.io/timeout",
			"haproxy.router.openshift.io/rewrite-target",
			"traefik.ingress.kubernetes.io/router.priority",
			"konghq.com/strip-path",
			"kubernetes.io/ingress.class",
		} {
			if err := create(t, as("mallory"), pgadmin(fmt.Sprintf("ea-free-%02d", i), ingress("{"+key+": x}"))); err != nil {
				t.Errorf("%s: %v, want admitted", key, err)
			}
		}
	})
	t.Run("metadataPropagation for the Service is checked", func(t *testing.T) {
		expose := "  exposure: {type: ingress, hostname: pgadmin.example.com}\n"
		rule := func(annotations string) string {
			return expose + "  metadataPropagation:\n  - target: Service\n    annotations: " + annotations + "\n"
		}
		// Kong reads plugins on the Service an Ingress routes to, HAProxy
		// its backend configuration.
		exposureRefused(t, create(t, as("mallory"), pgadmin("ea-svc-kong", rule("{konghq.com/plugins: steal}"))))
		exposureRefused(t, create(t, as("mallory"), pgadmin("ea-svc-haproxy", rule("{haproxy.org/backend-config-snippet: x}"))))
		exposureRefused(t, create(t, as("mallory"), consoleWith(t, "ea-svc-console", rule("{konghq.com/plugins: steal}"))))
		if err := create(t, as("mallory"), pgadmin("ea-svc-benign", rule("{traefik.ingress.kubernetes.io/service.sticky.cookie: 'true'}"))); err != nil {
			t.Fatalf("benign Service annotation: %v, want admitted", err)
		}
		if err := create(t, as("exposer"), pgadmin("ea-svc-exposer", rule("{konghq.com/plugins: rate-limit}"))); err != nil {
			t.Fatalf("exposer: %v, want admitted", err)
		}
		// clusterIP: nothing the operator writes routes to the Service.
		if err := create(t, as("mallory"), pgadmin("ea-svc-clusterip", "  metadataPropagation:\n  - target: Service\n    annotations: {konghq.com/plugins: x}\n")); err != nil {
			t.Fatalf("clusterIP: %v, want admitted", err)
		}
	})
	t.Run("a kept Service annotation passes; moved to another target it is checked", func(t *testing.T) {
		u := pgadmin("ea-svc-kept", "  exposure: {type: ingress, hostname: pgadmin.example.com}\n  metadataPropagation:\n  - target: Service\n    annotations: {konghq.com/plugins: rate-limit}\n")
		if err := create(t, root, u); err != nil {
			t.Fatal(err)
		}
		if err := root.Get(t.Context(), client.ObjectKeyFromObject(u), u); err != nil {
			t.Fatal(err)
		}
		edit := u.DeepCopy()
		if err := unstructured.SetNestedStringSlice(edit.Object, []string{"rw", "ro"}, "spec", "endpoints"); err != nil {
			t.Fatal(err)
		}
		if err := as("mallory").Update(t.Context(), edit); err != nil {
			t.Fatalf("unrelated edit: %v, want admitted", err)
		}
		if err := root.Get(t.Context(), client.ObjectKeyFromObject(u), u); err != nil {
			t.Fatal(err)
		}
		rules, _, _ := unstructured.NestedSlice(u.Object, "spec", "metadataPropagation")
		rules[0].(map[string]any)["target"] = "Exposure"
		if err := unstructured.SetNestedSlice(u.Object, rules, "spec", "metadataPropagation"); err != nil {
			t.Fatal(err)
		}
		exposureRefused(t, as("mallory").Update(t.Context(), u))
	})
	t.Run("other annotations pass", func(t *testing.T) {
		if err := create(t, as("mallory"), pgadmin("ea-benign", ingress("{nginx.ingress.kubernetes.io/proxy-body-size: 8m, nginx.ingress.kubernetes.io/affinity: cookie}"))); err != nil {
			t.Fatalf("error = %v, want admitted", err)
		}
	})
	t.Run("a user who may create Ingresses may set them", func(t *testing.T) {
		if err := create(t, as("exposer"), pgadmin("ea-exposer", ingress("{nginx.ingress.kubernetes.io/auth-url: https://auth.example.com}"))); err != nil {
			t.Fatalf("error = %v, want admitted", err)
		}
		// Ingresses are not Routes.
		exposureRefused(t, create(t, as("exposer"), pgadmin("ea-exposer-route",
			"  exposure:\n    type: route\n    hostname: pgadmin.example.com\n    annotations: {haproxy.org/auth-realm: x}\n")))
	})
	t.Run("metadataPropagation for All or Exposure is checked, for other targets not", func(t *testing.T) {
		expose := "  exposure: {type: ingress, hostname: pgadmin.example.com}\n"
		rule := func(target string) string {
			return expose + "  metadataPropagation:\n  - target: " + target + "\n    annotations: {nginx.ingress.kubernetes.io/configuration-snippet: x}\n"
		}
		exposureRefused(t, create(t, as("mallory"), pgadmin("ea-mp-exposure", rule("Exposure"))))
		exposureRefused(t, create(t, as("mallory"), pgadmin("ea-mp-all", rule("All"))))
		if err := create(t, as("mallory"), pgadmin("ea-mp-pod", rule("Pod"))); err != nil {
			t.Fatalf("target Pod: %v, want admitted", err)
		}
	})
	t.Run("a PgToolBoxConsole is checked too", func(t *testing.T) {
		exposureRefused(t, create(t, as("mallory"), consoleWith(t, "ea-console", ingress("{nginx.ingress.kubernetes.io/server-snippet: x}"))))
	})
	t.Run("a clusterIP object renders no exposure object; exposing it later is checked", func(t *testing.T) {
		u := pgadmin("ea-later", "  exposure:\n    annotations: {nginx.ingress.kubernetes.io/configuration-snippet: x}\n")
		if err := create(t, as("mallory"), u); err != nil {
			t.Fatalf("clusterIP: %v, want admitted", err)
		}
		if err := root.Get(t.Context(), client.ObjectKeyFromObject(u), u); err != nil {
			t.Fatal(err)
		}
		if err := unstructured.SetNestedField(u.Object, "ingress", "spec", "exposure", "type"); err != nil {
			t.Fatal(err)
		}
		if err := unstructured.SetNestedField(u.Object, "pgadmin.example.com", "spec", "exposure", "hostname"); err != nil {
			t.Fatal(err)
		}
		exposureRefused(t, as("mallory").Update(t.Context(), u))
	})
	t.Run("on update a kept annotation passes and a changed one is checked", func(t *testing.T) {
		u := pgadmin("ea-update", ingress("{nginx.ingress.kubernetes.io/auth-url: https://auth.example.com}"))
		if err := create(t, root, u); err != nil {
			t.Fatal(err)
		}
		if err := root.Get(t.Context(), client.ObjectKeyFromObject(u), u); err != nil {
			t.Fatal(err)
		}
		edit := u.DeepCopy()
		if err := unstructured.SetNestedStringSlice(edit.Object, []string{"rw", "ro"}, "spec", "endpoints"); err != nil {
			t.Fatal(err)
		}
		if err := as("mallory").Update(t.Context(), edit); err != nil {
			t.Fatalf("unrelated edit: %v, want admitted", err)
		}
		if err := root.Get(t.Context(), client.ObjectKeyFromObject(u), u); err != nil {
			t.Fatal(err)
		}
		if err := unstructured.SetNestedField(u.Object, "https://auth.attacker.example", "spec", "exposure", "annotations", "nginx.ingress.kubernetes.io/auth-url"); err != nil {
			t.Fatal(err)
		}
		exposureRefused(t, as("mallory").Update(t.Context(), u))
	})
	t.Run("the largest annotation set stays within the CEL cost budget", func(t *testing.T) {
		annotations := func(rule int) map[string]any {
			out := map[string]any{}
			for i := range 32 {
				out[fmt.Sprintf("%s.example/%02d-%02d-%s-snippet", strings.Repeat("d", 240), rule, i, strings.Repeat("k", 40))] = strings.Repeat("v", 200)
			}
			return out
		}
		rules := make([]any, 0, 8)
		for r := range 8 {
			rules = append(rules, map[string]any{"target": "All", "annotations": annotations(r)})
		}
		u := pgadmin("ea-cost", "  exposure: {type: ingress, hostname: pgadmin.example.com}\n")
		if err := unstructured.SetNestedSlice(u.Object, rules, "spec", "metadataPropagation"); err != nil {
			t.Fatal(err)
		}
		if err := unstructured.SetNestedField(u.Object, annotations(9), "spec", "exposure", "annotations"); err != nil {
			t.Fatal(err)
		}
		if err := create(t, as("exposer"), u); err != nil {
			t.Fatalf("create: %v, want admitted", err)
		}
		if err := root.Get(t.Context(), client.ObjectKeyFromObject(u), u); err != nil {
			t.Fatal(err)
		}
		// Everything kept: every annotation is compared with every old rule.
		if err := unstructured.SetNestedStringSlice(u.Object, []string{"rw", "ro"}, "spec", "endpoints"); err != nil {
			t.Fatal(err)
		}
		if err := as("mallory").Update(t.Context(), u); err != nil {
			t.Fatalf("update: %v, want admitted", err)
		}
	})
	t.Run("the costliest keys of the deny list stay within the CEL cost budget", func(t *testing.T) {
		// A denied key matched last in the longest controller branch (after
		// the substring scan, the exact keys and every other prefix), and a
		// key of that branch that is denied by nothing.
		annotations := func(rule int, prefix string) map[string]any {
			out := map[string]any{}
			for i := range 32 {
				out[fmt.Sprintf("%s%02d-%02d-%s", prefix, rule, i, strings.Repeat("k", 280))] = strings.Repeat("v", 200)
			}
			return out
		}
		for i, prefix := range []string{"nginx.ingress.kubernetes.io/proxy-redirect-", "nginx.ingress.kubernetes.io/x-", "konghq.com/x-"} {
			t.Run(prefix, func(t *testing.T) {
				rules := make([]any, 0, 8)
				for r := range 8 {
					target := []string{"All", "Exposure", "Service"}[r%3]
					rules = append(rules, map[string]any{"target": target, "annotations": annotations(r, prefix)})
				}
				u := pgadmin(fmt.Sprintf("ea-costliest-%d", i), "  exposure: {type: ingress, hostname: pgadmin.example.com}\n")
				if err := unstructured.SetNestedSlice(u.Object, rules, "spec", "metadataPropagation"); err != nil {
					t.Fatal(err)
				}
				if err := unstructured.SetNestedField(u.Object, annotations(9, prefix), "spec", "exposure", "annotations"); err != nil {
					t.Fatal(err)
				}
				if err := create(t, as("exposer"), u); err != nil {
					t.Fatalf("create: %v, want admitted", err)
				}
				if err := root.Get(t.Context(), client.ObjectKeyFromObject(u), u); err != nil {
					t.Fatal(err)
				}
				if err := unstructured.SetNestedStringSlice(u.Object, []string{"rw", "ro"}, "spec", "endpoints"); err != nil {
					t.Fatal(err)
				}
				if err := as("mallory").Update(t.Context(), u); err != nil {
					t.Fatalf("update: %v, want admitted", err)
				}
			})
		}
	})
}
