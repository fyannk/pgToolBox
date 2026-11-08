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

package ops

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/console/auth"
)

// The access-request write path on a real API server, with the people
// bound to the repository's own pgtoolbox-console-viewer and pgtoolbox-console-operator
// ClusterRoles: token passthrough (no token, no write), the viewer asking
// and refused when deciding, the operator's approval passing the
// RoleBinding escalation check, the decision locked to the version read,
// and an approval retried after its status write failed.
func TestAccessWriterOnEnvtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset; run through make test-envtest-access")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = rbacv1.AddToScheme(scheme)
	_ = pgtoolboxv1alpha1.AddToScheme(scheme)
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	cs := kubernetes.NewForConfigOrDie(cfg)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}))
	for _, file := range []string{"pgtoolbox_console_viewer_clusterrole.yaml", "pgtoolbox_console_operator_clusterrole.yaml"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "rbac", file)) // #nosec G304 -- repository manifest
		must(err)
		role := &rbacv1.ClusterRole{}
		must(yaml.Unmarshal(raw, role))
		must(admin.Create(ctx, role))
	}
	token := func(sa, clusterRole string) string {
		t.Helper()
		must(admin.Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: sa}}))
		must(admin.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: sa},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterRole},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: sa, Namespace: "team-a"}},
		}))
		tr, err := cs.CoreV1().ServiceAccounts("team-a").CreateToken(ctx, sa, &authenticationv1.TokenRequest{
			Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: new(int64(3600))},
		}, metav1.CreateOptions{})
		must(err)
		return tr.Status.Token
	}
	dba, app := token("dba", pgtoolboxv1alpha1.ClusterRoleOperator), token("app", pgtoolboxv1alpha1.ClusterRoleViewer)
	uc, err := NewAccessUserClient(auth.UserConfig(cfg))
	must(err)
	w := NewAccessWriter(uc, 10*time.Second)
	as := func(tok string) context.Context { return auth.WithUserToken(ctx, tok) }
	status := func(err error) int {
		var we *WriteError
		if errors.As(err, &we) {
			return we.Status
		}
		return 0
	}
	newRequest := func() *pgtoolboxv1alpha1.PgAccessRequest {
		return &pgtoolboxv1alpha1.PgAccessRequest{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", GenerateName: "operator-"},
			Spec: pgtoolboxv1alpha1.PgAccessRequestSpec{
				Subject:    pgtoolboxv1alpha1.AccessSubject{Kind: pgtoolboxv1alpha1.AccessSubjectServiceAccount, Name: "app"},
				Capability: pgtoolboxv1alpha1.AccessCapabilityOperator, Justification: "INC-1",
			},
		}
	}

	if _, err := w.CreateAccessRequest(ctx, newRequest()); err == nil {
		t.Fatal("a request without a user token went through")
	}
	created, err := w.CreateAccessRequest(as(app), newRequest())
	if err != nil {
		t.Fatalf("the viewer asks: %v", err)
	}

	t.Run("the viewer cannot decide", func(t *testing.T) {
		r, err := w.ReadAccessRequest(as(app), "team-a", created.Name)
		must(err)
		if err := w.CreateGrant(as(app), r); status(err) != http.StatusForbidden {
			t.Fatalf("viewer grant: %v", err)
		}
		r.Status.Phase = pgtoolboxv1alpha1.AccessPhaseDenied
		r.Status.DecidedBy, r.Status.DecidedAt, r.Status.Note = "system:serviceaccount:team-a:app", &metav1.Time{Time: time.Now()}, "no"
		if err := w.RecordDecision(as(app), r); status(err) != http.StatusForbidden {
			t.Fatalf("viewer decision: %v", err)
		}
	})

	t.Run("the operator approves as themselves", func(t *testing.T) {
		r, err := w.ReadAccessRequest(as(dba), "team-a", created.Name)
		must(err)
		stale := r.DeepCopy()
		must(w.CreateGrant(as(dba), r))
		// Retried after a failed status write: the same grant is success.
		must(w.CreateGrant(as(dba), r))
		r.Status.Phase = pgtoolboxv1alpha1.AccessPhaseApproved
		r.Status.DecidedBy, r.Status.DecidedAt = "system:serviceaccount:team-a:dba", &metav1.Time{Time: time.Now()}
		r.Status.RoleBinding, r.Status.ClusterRole = r.RoleBindingName(), pgtoolboxv1alpha1.ClusterRoleOperator
		must(w.RecordDecision(as(dba), r))

		stale.Status.Phase = pgtoolboxv1alpha1.AccessPhaseDenied
		stale.Status.DecidedBy, stale.Status.DecidedAt, stale.Status.Note = "system:serviceaccount:team-a:dba", &metav1.Time{Time: time.Now()}, "late"
		if err := w.RecordDecision(as(dba), stale); status(err) != http.StatusConflict {
			t.Fatalf("a decision on a stale read: %v", err)
		}
		// The grant is real: app may now decide too.
		if _, err := w.CreateAccessRequest(as(app), newRequest()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a foreign binding under the grant's name", func(t *testing.T) {
		r, err := w.CreateAccessRequest(as(app), newRequest())
		must(err)
		foreign := r.DesiredRoleBinding()
		foreign.OwnerReferences = nil
		foreign.RoleRef.Name = pgtoolboxv1alpha1.ClusterRoleViewer
		must(admin.Create(t.Context(), foreign))
		// An approver who may read RoleBindings is told; one who may not
		// (pgtoolbox-console-operator) proceeds, and the operator reports the
		// grant missing: it never trusts a binding the request does not
		// control.
		reader := token("reader", pgtoolboxv1alpha1.ClusterRoleOperator)
		must(admin.Create(t.Context(), &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "read-bindings"},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{rbacv1.GroupName}, Resources: []string{"rolebindings"}, Verbs: []string{"get"}}},
		}))
		must(admin.Create(t.Context(), &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "read-bindings"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "read-bindings"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "reader", Namespace: "team-a"}},
		}))
		if err := w.CreateGrant(as(reader), r); status(err) != http.StatusConflict {
			t.Fatalf("grant over a foreign binding, as a reader: %v", err)
		}
		if err := w.CreateGrant(as(dba), r); err != nil {
			t.Fatalf("grant over a foreign binding, as pgtoolbox-console-operator: %v", err)
		}
		got := &rbacv1.RoleBinding{}
		must(admin.Get(t.Context(), client.ObjectKeyFromObject(foreign), got))
		if got.RoleRef.Name != pgtoolboxv1alpha1.ClusterRoleViewer || len(got.OwnerReferences) != 0 {
			t.Fatalf("the foreign binding was changed: %+v", got)
		}
	})
}
