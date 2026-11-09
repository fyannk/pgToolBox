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

package accessrequest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// The suite runs on a real API server with RBAC: the operator is a user
// bound to exactly the generated manager ClusterRole, the people are users
// bound per namespace to the console ClusterRoles of config/rbac (the approver
// to pgtoolbox-console-approver alone, the viewer to pgtoolbox-console-viewer). What
// a fake client cannot show is shown here: the escalation check on the
// RoleBinding an approver creates, the verbs each role really holds, the
// status CEL rules, and an operator that can delete a grant but never make
// one.
const (
	envOperator = "system:serviceaccount:pgtoolbox-system:pgtoolbox-operator"
	envApprover = "oidc:alice@corp.example"
	envViewer   = "oidc:victor@corp.example"
	envNS       = "team-a"
	// envApproverRole is config/rbac/pgtoolbox_console_approver_clusterrole.yaml:
	// it holds neither console role, only bind on them.
	envApproverRole = "pgtoolbox-console-approver"
)

type envSuite struct {
	admin, operator, approver, viewer client.Client
	reconciler                        *Reconciler
	now                               time.Time
}

func newEnvSuite(t *testing.T) *envSuite {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set (make test-envtest-access)")
	}
	crds := os.Getenv("CNPG_CRDS")
	if crds == "" {
		crds = filepath.Join("..", "..", "..", "test", "crds", "cnpg-1.30")
	}
	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases"), crds},
		ErrorIfCRDPathMissing: true,
	}
	config, err := environment.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = environment.Stop() })
	scheme := testScheme(t)
	as := func(user string) client.Client {
		cfg := rest.CopyConfig(config)
		if user != "" {
			cfg.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: []string{"system:authenticated"}}
		}
		c, err := client.New(cfg, client.Options{Scheme: scheme})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	admin := as("")
	ctx := context.Background()
	mustEnv(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: envNS}}))
	for _, file := range []string{"role.yaml", "pgtoolbox_console_viewer_clusterrole.yaml", "pgtoolbox_console_operator_clusterrole.yaml", "pgtoolbox_console_approver_clusterrole.yaml"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "rbac", file)) // #nosec G304 -- repository manifest
		if err != nil {
			t.Fatal(err)
		}
		role := &rbacv1.ClusterRole{}
		if err := yaml.Unmarshal(raw, role); err != nil {
			t.Fatal(err)
		}
		mustEnv(t, admin.Create(ctx, role))
	}
	mustEnv(t, admin.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "pgtoolbox-operator"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "manager-role"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: envOperator}},
	}))
	for user, role := range map[string]string{envApprover: envApproverRole, envViewer: pgtoolboxv1alpha1.ClusterRoleViewer} {
		mustEnv(t, admin.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Namespace: envNS, Name: role + "-" + strings.Split(strings.TrimPrefix(user, "oidc:"), "@")[0]},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}},
		}))
	}
	s := &envSuite{admin: admin, operator: as(envOperator), approver: as(envApprover), viewer: as(envViewer), now: time.Now().UTC().Truncate(time.Second)}
	s.reconciler = &Reconciler{Client: s.operator, Recorder: events.NewFakeRecorder(64), Now: func() time.Time { return s.now }}
	return s
}

func mustEnv(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (s *envSuite) reconcile(ctx context.Context, t *testing.T, name string) *pgtoolboxv1alpha1.PgAccessRequest {
	t.Helper()
	key := client.ObjectKey{Namespace: envNS, Name: name}
	if _, err := s.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
	got := &pgtoolboxv1alpha1.PgAccessRequest{}
	mustEnv(t, s.admin.Get(ctx, key, got))
	return got
}

// approve is what the console does as the approver: create the binding,
// then record the decision with an update locked to the version read.
func approve(ctx context.Context, c client.Client, r *pgtoolboxv1alpha1.PgAccessRequest, by string, at time.Time) error {
	if err := c.Create(ctx, r.DesiredRoleBinding()); err != nil {
		return err
	}
	r.Status.Phase = pgtoolboxv1alpha1.AccessPhaseApproved
	r.Status.DecidedBy, r.Status.DecidedAt = by, &metav1.Time{Time: at}
	r.Status.RoleBinding, r.Status.ClusterRole = r.RoleBindingName(), pgtoolboxv1alpha1.ClusterRoleFor(r.Spec.Capability)
	return c.Status().Update(ctx, r)
}

func TestAccessRequestEnvtest(t *testing.T) {
	s := newEnvSuite(t)
	ctx := context.Background()

	// A viewer asks for the operator role for themselves.
	req := &pgtoolboxv1alpha1.PgAccessRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: envNS, Name: "victor-operator"},
		Spec: pgtoolboxv1alpha1.PgAccessRequestSpec{
			Subject:    pgtoolboxv1alpha1.AccessSubject{Kind: pgtoolboxv1alpha1.AccessSubjectUser, Name: envViewer},
			Capability: pgtoolboxv1alpha1.AccessCapabilityOperator, Justification: "INC-4211",
			ExpiresAt: &metav1.Time{Time: s.now.Add(8 * time.Hour)},
		},
	}
	mustEnv(t, s.viewer.Create(ctx, req))
	got := s.reconcile(ctx, t, req.Name)
	if got.Status.Phase != pgtoolboxv1alpha1.AccessPhasePending {
		t.Fatalf("phase = %q", got.Status.Phase)
	}

	t.Run("a viewer cannot approve, not even their own request", func(t *testing.T) {
		mine := got.DeepCopy()
		err := approve(ctx, s.viewer, mine, envViewer, s.now)
		if !apierrors.IsForbidden(err) {
			t.Fatalf("viewer approval = %v, want Forbidden", err)
		}
	})

	t.Run("the operator can delete a grant but never make one", func(t *testing.T) {
		err := s.operator.Create(ctx, got.DesiredRoleBinding())
		if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "not currently held") {
			t.Fatalf("operator creating a pgtoolbox-console-operator binding = %v, want the escalation refusal", err)
		}
	})

	t.Run("an approver cannot bind a role they do not hold", func(t *testing.T) {
		b := got.DesiredRoleBinding()
		b.Name, b.RoleRef.Name = "pgaccess-escalation", "manager-role"
		err := s.approver.Create(ctx, b)
		if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "not currently held") {
			t.Fatalf("approver binding manager-role = %v, want the escalation refusal", err)
		}
	})

	t.Run("the approver approves", func(t *testing.T) {
		mustEnv(t, approve(ctx, s.approver, got, envApprover, s.now))
		got = s.reconcile(ctx, t, req.Name)
		synced := meta.FindStatusCondition(got.Status.Conditions, pgtoolboxv1alpha1.ConditionRoleBindingSynced)
		if got.Status.Phase != pgtoolboxv1alpha1.AccessPhaseApproved || synced == nil || synced.Reason != pgtoolboxv1alpha1.ReasonAccessGranted.String() {
			t.Fatalf("after approval: phase %s, RoleBindingSynced %+v", got.Status.Phase, synced)
		}
		if !meta.IsStatusConditionTrue(got.Status.Conditions, pgtoolboxv1alpha1.ConditionReady) {
			t.Fatalf("Ready is not True: %+v", got.Status.Conditions)
		}
		// The grant is real: the viewer may now patch Clusters here.
		cluster := &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: envNS, Name: "missing"}}
		err := s.viewer.Patch(ctx, cluster, client.RawPatch("application/merge-patch+json", []byte(`{"metadata":{"annotations":{"a":"b"}}}`)))
		if !apierrors.IsNotFound(err) {
			t.Fatalf("the granted viewer's patch = %v, want NotFound (authorized)", err)
		}
	})

	t.Run("a grantee of pgtoolbox-console-operator cannot outlive or extend the grant", func(t *testing.T) {
		// A second, permanent binding to themselves.
		keep := got.DesiredRoleBinding()
		keep.Name, keep.OwnerReferences = "victor-forever", nil
		if err := s.viewer.Create(ctx, keep); !apierrors.IsForbidden(err) {
			t.Fatalf("the grantee creating a RoleBinding = %v, want Forbidden", err)
		}
		// Approving somebody else's request.
		other := &pgtoolboxv1alpha1.PgAccessRequest{
			ObjectMeta: metav1.ObjectMeta{Namespace: envNS, Name: "mallory-operator"},
			Spec: pgtoolboxv1alpha1.PgAccessRequestSpec{
				Subject:    pgtoolboxv1alpha1.AccessSubject{Kind: pgtoolboxv1alpha1.AccessSubjectUser, Name: "oidc:mallory@corp.example"},
				Capability: pgtoolboxv1alpha1.AccessCapabilityOperator, Justification: "INC-4212",
			},
		}
		mustEnv(t, s.admin.Create(ctx, other))
		pending := s.reconcile(ctx, t, other.Name)
		if err := approve(ctx, s.viewer, pending, envViewer, s.now); !apierrors.IsForbidden(err) {
			t.Fatalf("the grantee approving another request = %v, want Forbidden", err)
		}
		pending.Status.Phase = pgtoolboxv1alpha1.AccessPhaseDenied
		pending.Status.DecidedBy, pending.Status.DecidedAt, pending.Status.Note = envViewer, &metav1.Time{Time: s.now}, "no"
		if err := s.viewer.Status().Update(ctx, pending); !apierrors.IsForbidden(err) {
			t.Fatalf("the grantee deciding another request = %v, want Forbidden", err)
		}
	})

	t.Run("status CEL keeps a decision a decision", func(t *testing.T) {
		back := got.DeepCopy()
		back.Status.Phase = pgtoolboxv1alpha1.AccessPhasePending
		if err := s.approver.Status().Update(ctx, back); err == nil || !strings.Contains(err.Error(), "phase moves only") {
			t.Fatalf("Approved -> Pending = %v", err)
		}
		other := got.DeepCopy()
		other.Status.DecidedBy = "someone-else"
		if err := s.approver.Status().Update(ctx, other); err == nil || !strings.Contains(err.Error(), "decidedBy cannot change") {
			t.Fatalf("rewriting decidedBy = %v", err)
		}
		spec := got.DeepCopy()
		spec.Spec.Capability = pgtoolboxv1alpha1.AccessCapabilityViewer
		if err := s.admin.Update(ctx, spec); err == nil || !strings.Contains(err.Error(), "cannot be changed once made") {
			t.Fatalf("changing the spec = %v", err)
		}
	})

	t.Run("expiry deletes the grant", func(t *testing.T) {
		s.now = s.now.Add(8 * time.Hour)
		got = s.reconcile(ctx, t, req.Name)
		if got.Status.Phase != pgtoolboxv1alpha1.AccessPhaseExpired {
			t.Fatalf("phase = %s", got.Status.Phase)
		}
		err := s.admin.Get(ctx, client.ObjectKey{Namespace: envNS, Name: got.RoleBindingName()}, &rbacv1.RoleBinding{})
		if !apierrors.IsNotFound(err) {
			t.Fatalf("RoleBinding after expiry: %v", err)
		}
		// And the access is gone with it.
		cluster := &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: envNS, Name: "missing"}}
		err = s.viewer.Patch(ctx, cluster, client.RawPatch("application/merge-patch+json", []byte(`{"metadata":{"annotations":{"a":"b"}}}`)))
		if !apierrors.IsForbidden(err) {
			t.Fatalf("the expired grantee's patch = %v, want Forbidden", err)
		}
	})

	t.Run("revocation deletes the grant", func(t *testing.T) {
		second := &pgtoolboxv1alpha1.PgAccessRequest{
			ObjectMeta: metav1.ObjectMeta{Namespace: envNS, Name: "victor-viewer-ci"},
			Spec: pgtoolboxv1alpha1.PgAccessRequestSpec{
				Subject:    pgtoolboxv1alpha1.AccessSubject{Kind: pgtoolboxv1alpha1.AccessSubjectServiceAccount, Name: "ci"},
				Capability: pgtoolboxv1alpha1.AccessCapabilityViewer, Justification: "dashboards",
			},
		}
		mustEnv(t, s.viewer.Create(ctx, second))
		r := s.reconcile(ctx, t, second.Name)
		mustEnv(t, approve(ctx, s.approver, r, envApprover, s.now))
		r = s.reconcile(ctx, t, second.Name)
		r.Status.Phase = pgtoolboxv1alpha1.AccessPhaseRevoked
		r.Status.RevokedBy, r.Status.RevokedAt, r.Status.Note = envApprover, &metav1.Time{Time: s.now}, "incident closed"
		mustEnv(t, s.approver.Status().Update(ctx, r))
		r = s.reconcile(ctx, t, second.Name)
		if r.Status.Phase != pgtoolboxv1alpha1.AccessPhaseRevoked || r.Status.RevokedBy != envApprover {
			t.Fatalf("status after revocation: %+v", r.Status)
		}
		err := s.admin.Get(ctx, client.ObjectKey{Namespace: envNS, Name: r.RoleBindingName()}, &rbacv1.RoleBinding{})
		if !apierrors.IsNotFound(err) {
			t.Fatalf("RoleBinding after revocation: %v", err)
		}
	})

	t.Run("a binding with the label that the request does not control is left alone", func(t *testing.T) {
		planted := got.DesiredRoleBinding()
		planted.Name, planted.OwnerReferences = "pgaccess-planted", nil
		planted.RoleRef.Name = pgtoolboxv1alpha1.ClusterRoleViewer
		mustEnv(t, s.admin.Create(ctx, planted))
		s.reconcile(ctx, t, req.Name)
		mustEnv(t, s.admin.Get(ctx, client.ObjectKeyFromObject(planted), &rbacv1.RoleBinding{}))
	})
}
