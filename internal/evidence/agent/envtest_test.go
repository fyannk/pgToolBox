//go:build envtest

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

package agent

import (
	"context"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/fyannk/pgToolBox/internal/evidence/evidenceapi"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// TestAuthenticatorOnEnvtest runs the agent's TokenReview and
// SubjectAccessReview against a real API server with kubelet-equivalent
// bound tokens (TokenRequest): the agent of team-a accepts the console's
// token of audience pgtoolbox-evidence-agent/team-a only; the console's
// valid team-b token and a bare cluster-wide token are refused as
// audience mismatches, and a subject without the get grant
// is forbidden.
func TestAuthenticatorOnEnvtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset; run through make test-envtest-security")
	}
	environment := &envtest.Environment{}
	restConfig, err := environment.Start()
	if err != nil {
		t.Fatalf("envtest: %v", err)
	}
	t.Cleanup(func() { _ = environment.Stop() })
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, namespace := range []string{"pgtoolbox", "team-a", "team-b"} {
		_, err := clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{})
		check(err)
	}
	for _, account := range []struct{ namespace, name string }{{"pgtoolbox", "pgtoolbox-console"}, {"pgtoolbox", "stranger"}, {"team-a", "orders-evidence"}} {
		_, err := clientset.CoreV1().ServiceAccounts(account.namespace).Create(ctx,
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: account.name}}, metav1.CreateOptions{})
		check(err)
	}
	// The agent's one ClusterRole (internal/operator/evidenceagent).
	_, err = clientset.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "pgtoolbox-evidence-agent"},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"authentication.k8s.io"}, Resources: []string{"tokenreviews"}, Verbs: []string{"create"}},
			{APIGroups: []string{"authorization.k8s.io"}, Resources: []string{"subjectaccessreviews"}, Verbs: []string{"create"}},
		},
	}, metav1.CreateOptions{})
	check(err)
	_, err = clientset.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "pgtoolbox-evidence-agent"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "pgtoolbox-evidence-agent"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Namespace: "team-a", Name: "orders-evidence"}},
	}, metav1.CreateOptions{})
	check(err)
	// The console's Role in each watched namespace: get on EvidenceAgents.
	for _, namespace := range []string{"team-a", "team-b"} {
		_, err := clientset.RbacV1().Roles(namespace).Create(ctx, &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "pgtoolbox-console"},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{"pgtoolbox.fyannk.dev"}, Resources: []string{"evidenceagents"}, Verbs: []string{"get"}}},
		}, metav1.CreateOptions{})
		check(err)
		_, err = clientset.RbacV1().RoleBindings(namespace).Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "pgtoolbox-console"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "pgtoolbox-console"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Namespace: "pgtoolbox", Name: "pgtoolbox-console"}},
		}, metav1.CreateOptions{})
		check(err)
	}

	request := func(namespace, name string, audiences ...string) string {
		t.Helper()
		expiration := int64(evidencev1alpha1.RecommendedTokenExpirationSeconds)
		response, err := clientset.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, name, &authenticationv1.TokenRequest{
			Spec: authenticationv1.TokenRequestSpec{Audiences: audiences, ExpirationSeconds: &expiration},
		}, metav1.CreateOptions{})
		check(err)
		return response.Status.Token
	}
	// The agent Pod's own credential: the API server's default audience.
	agentCredential := filepath.Join(t.TempDir(), "token")
	check(os.WriteFile(agentCredential, []byte(request("team-a", "orders-evidence")), 0o600))
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(restConfig.CAData) {
		t.Fatal("envtest CA")
	}
	reviewer, err := NewAPIServerReviewer(restConfig.Host, roots, agentCredential)
	check(err)
	authenticator, err := NewAuthenticator(AuthenticatorOptions{Reviewer: reviewer, Name: "orders", Namespace: "team-a"})
	check(err)

	audienceA, err := evidencev1alpha1.TokenAudience("team-a")
	check(err)
	audienceB, err := evidencev1alpha1.TokenAudience("team-b")
	check(err)
	for _, tc := range []struct {
		name  string
		token string
		want  evidenceapi.AuthOutcome
	}{
		{"the console's token for team-a", request("pgtoolbox", "pgtoolbox-console", audienceA), evidenceapi.AuthAllowed},
		{"the console's token for team-b", request("pgtoolbox", "pgtoolbox-console", audienceB), evidenceapi.AuthAudienceMismatch},
		{"a bare cluster-wide token", request("pgtoolbox", "pgtoolbox-console", evidencev1alpha1.ClusterWideTokenAudience), evidenceapi.AuthAudienceMismatch},
		{"an API server token", request("pgtoolbox", "pgtoolbox-console"), evidenceapi.AuthUnauthenticated},
		{"team-a's audience, subject without the grant", request("pgtoolbox", "stranger", audienceA), evidenceapi.AuthForbidden},
		{"several audiences including team-a's", request("pgtoolbox", "pgtoolbox-console", audienceB, audienceA), evidenceapi.AuthAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, err := evidencev1alpha1.ParseBearerToken([]byte(tc.token))
			check(err)
			if decision := authenticator.Authenticate(ctx, token); decision.Outcome != tc.want {
				t.Fatalf("outcome = %v, want %v", decision.Outcome, tc.want)
			}
		})
	}
}
