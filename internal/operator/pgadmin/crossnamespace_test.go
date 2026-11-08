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

package pgadmin

import (
	"context"
	"slices"
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/offer"
)

const remoteNamespace = "team-b"

func remoteCluster(name string) *cnpgv1.Cluster {
	c := testCluster2(name, nil)
	c.Namespace = remoteNamespace
	return &c
}

func grant(name string, pgAdmin types.NamespacedName, clusters ...string) *pgtoolboxv1alpha1.PgAdminAccess {
	g := &pgtoolboxv1alpha1.PgAdminAccess{
		TypeMeta:   metav1.TypeMeta{APIVersion: pgtoolboxv1alpha1.GroupVersion.String(), Kind: "PgAdminAccess"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: remoteNamespace, UID: types.UID("grant-" + name)},
		Spec:       pgtoolboxv1alpha1.PgAdminAccessSpec{PgAdmin: pgtoolboxv1alpha1.PgAdminReference{Namespace: pgAdmin.Namespace, Name: pgAdmin.Name}},
	}
	for _, c := range clusters {
		g.Spec.Clusters = append(g.Spec.Clusters, pgtoolboxv1alpha1.LocalObjectReference{Name: c})
	}
	return g
}

func (h *harness) offerDocument() offer.Document {
	const name = "team"
	h.t.Helper()
	var secret corev1.Secret
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name + "-pgadmin-offer"}, &secret); err != nil {
		h.t.Fatal(err)
	}
	doc, err := offer.Parse(secret.Data[offer.SecretKey])
	if err != nil {
		h.t.Fatal(err)
	}
	return doc
}

func TestCrossNamespaceNeedsAGrant(t *testing.T) {
	p := oauth2PgAdmin()
	p.Spec.CrossNamespaceClusterRefs = []pgtoolboxv1alpha1.PgAdminClusterReference{{Namespace: remoteNamespace, Name: "payments"}}
	team := types.NamespacedName{Namespace: testNamespace, Name: "team"}
	objs := append(baseObjects(p), remoteCluster("payments"),
		// A grant for another PgAdmin, and one for this PgAdmin of another
		// Cluster: neither grants payments.
		grant("other", types.NamespacedName{Namespace: testNamespace, Name: "ops"}, "payments"),
		grant("ledger-only", team, "ledger"),
	)
	h := newHarness(t, nil, objs...)

	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	got := h.pgadmin("team")
	expectCondition(t, got, "ConfigurationValid", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonGrantMissing)
	if c := condition(got, "ConfigurationValid"); !strings.Contains(c.Message, "team-b/payments") {
		t.Fatalf("the message must name the ungranted Cluster: %s", c.Message)
	}
	for _, s := range h.offerDocument().Servers {
		if s.Namespace != "" {
			t.Fatalf("an ungranted Cluster is offered: %+v", s)
		}
	}
	// The grant for another Cluster gets its Role, with no rule: an empty
	// resourceNames would allow every Secret of the namespace.
	var role rbacv1.Role
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: remoteNamespace, Name: "ledger-only-pgadmin-access"}, &role); err != nil {
		t.Fatal(err)
	}
	if len(role.Rules) != 0 {
		t.Fatalf("a grant with nothing offered must have no rule: %+v", role.Rules)
	}
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: remoteNamespace, Name: "other-pgadmin-access"}, &role); !apierrors.IsNotFound(err) {
		t.Fatalf("another PgAdmin's grant must not produce a Role for this one: %v", err)
	}
	if evs := strings.Join(h.events(), "\n"); !strings.Contains(evs, "Warning GrantMissing") {
		t.Fatalf("GrantMissing is a Warning on its transition: %s", evs)
	}

	// The namespace owner grants payments.
	if err := h.client.Create(context.Background(), grant("pgadmin-team", team, "payments")); err != nil {
		t.Fatal(err)
	}
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	got = h.pgadmin("team")
	expectCondition(t, got, "ConfigurationValid", metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)
	var remote []offer.Server
	for _, s := range h.offerDocument().Servers {
		if s.Namespace == remoteNamespace {
			remote = append(remote, s)
		}
	}
	if len(remote) != 1 || remote[0].Name != "team-b/payments rw (app)" || remote[0].Host != "payments-rw.team-b.svc" ||
		remote[0].SecretName != "payments-app" || remote[0].CASecretName != "payments-ca" || remote[0].CAKey() != "team-b.payments" {
		t.Fatalf("granted Cluster offered as %+v", remote)
	}
	if !slices.ContainsFunc(got.Status.Servers, func(s pgtoolboxv1alpha1.PgAdminServerStatus) bool { return s.Cluster == "team-b/payments" }) {
		t.Fatalf("status rows must name the namespace: %+v", got.Status.Servers)
	}
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: remoteNamespace, Name: "pgadmin-team-pgadmin-access"}, &role); err != nil {
		t.Fatal(err)
	}
	if len(role.Rules) != 1 || !slices.Equal(role.Rules[0].ResourceNames, []string{"payments-app", "payments-ca"}) ||
		!slices.Equal(role.Rules[0].Verbs, []string{"get", "watch"}) {
		t.Fatalf("the grant's Role must allow get/watch on exactly the granted Cluster's offered Secrets: %+v", role.Rules)
	}
	if owner := metav1.GetControllerOf(&role); owner == nil || owner.Kind != "PgAdminAccess" || owner.Name != "pgadmin-team" {
		t.Fatalf("the grant owns its Role (deleting it revokes): %+v", role.OwnerReferences)
	}
	var binding rbacv1.RoleBinding
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: remoteNamespace, Name: "pgadmin-team-pgadmin-access"}, &binding); err != nil {
		t.Fatal(err)
	}
	if len(binding.Subjects) != 1 || binding.Subjects[0] != (rbacv1.Subject{Kind: "ServiceAccount", Name: "team-pgadmin", Namespace: testNamespace}) {
		t.Fatalf("the RoleBinding names the PgAdmin's ServiceAccount: %+v", binding.Subjects)
	}
	if requests := byAccessLabels(context.Background(), &binding); len(requests) != 1 || requests[0].NamespacedName != team {
		t.Fatalf("the access RoleBinding maps to %v", requests)
	}
	if requests := h.r.forCluster(context.Background(), remoteCluster("payments")); !slices.ContainsFunc(requests, func(r ctrlreconcile.Request) bool { return r.NamespacedName == team }) {
		t.Fatalf("a Cluster of a referenced namespace maps to %v", requests)
	}

	// The local credentials Role never names the other namespace's Secrets.
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "team-pgadmin-credentials"}, &role); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(role.Rules[0].ResourceNames, "payments-app") {
		t.Fatalf("the local Role names a granted namespace's Secret: %v", role.Rules[0].ResourceNames)
	}

	// The PgAdmin stops naming team-b: the grants' Role and RoleBinding go.
	got.Spec.CrossNamespaceClusterRefs = nil
	got.Generation++
	if err := h.client.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pgadmin-team-pgadmin-access", "ledger-only-pgadmin-access"} {
		if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: remoteNamespace, Name: name}, &binding); !apierrors.IsNotFound(err) {
			t.Fatalf("RoleBinding %s must be deleted once the PgAdmin no longer names its namespace: %v", name, err)
		}
	}
}

func TestCrossNamespaceRefToOwnNamespaceIsInvalid(t *testing.T) {
	p := oauth2PgAdmin()
	p.Spec.CrossNamespaceClusterRefs = []pgtoolboxv1alpha1.PgAdminClusterReference{{Namespace: testNamespace, Name: "orders"}}
	h := newHarness(t, nil, baseObjects(p)...)
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	got := h.pgadmin("team")
	expectCondition(t, got, "ConfigurationValid", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonConfigurationInvalid)
}

func TestNetworkPolicyReachesGrantedNamespaces(t *testing.T) {
	p := openshiftPgAdmin()
	in := testInputs(p, true)
	in.RemoteClusters = map[string][]string{remoteNamespace: {"payments", "ledger"}}
	policy := buildNetworkPolicy(p, in)
	var rule *networkingv1.NetworkPolicyEgressRule
	for i := range policy.Spec.Egress {
		if slices.ContainsFunc(policy.Spec.Egress[i].To, func(peer networkingv1.NetworkPolicyPeer) bool {
			return peer.NamespaceSelector != nil && peer.PodSelector != nil
		}) {
			rule = &policy.Spec.Egress[i]
		}
	}
	if rule == nil {
		t.Fatalf("no egress rule for the granted namespace: %+v", policy.Spec.Egress)
	}
	var found bool
	for _, peer := range rule.To {
		if peer.NamespaceSelector != nil && peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == remoteNamespace &&
			slices.Equal(peer.PodSelector.MatchExpressions[0].Values, []string{"ledger", "payments"}) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the granted namespace's Clusters are not a peer: %+v", rule.To)
	}
	if confined, open := egressStatement(policy); confined && len(open) == 0 {
		// The fixture has an open extra rule; the new peers add none.
		t.Fatal("the fixture's extra egress rule should still be reported open")
	}
}

// TestGrantDecidesPrivilegedCredentials: the PgAdmin asking for superuser
// does not widen a grant; the grant's allowPrivileged does.
func TestGrantDecidesPrivilegedCredentials(t *testing.T) {
	team := types.NamespacedName{Namespace: testNamespace, Name: "team"}
	for _, allow := range []bool{false, true} {
		p := oauth2PgAdmin()
		p.Spec.Credentials.Superuser = new(true)
		p.Spec.CrossNamespaceClusterRefs = []pgtoolboxv1alpha1.PgAdminClusterReference{{Namespace: remoteNamespace, Name: "payments"}}
		payments := remoteCluster("payments")
		payments.Spec.EnableSuperuserAccess = new(true)
		g := grant("pgadmin-team", team, "payments")
		g.Spec.AllowPrivileged = new(allow)
		h := newHarness(t, nil, append(baseObjects(p), payments, g)...)
		if err := h.reconcile("team"); err != nil {
			t.Fatal(err)
		}
		var superuser bool
		for _, s := range h.offerDocument().Servers {
			if s.Namespace == remoteNamespace && s.User == "postgres" {
				superuser = true
			}
		}
		if superuser != allow {
			t.Fatalf("allowPrivileged=%v: superuser of the granted Cluster offered = %v", allow, superuser)
		}
		var role rbacv1.Role
		if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: remoteNamespace, Name: "pgadmin-team-pgadmin-access"}, &role); err != nil {
			t.Fatal(err)
		}
		if got := slices.Contains(role.Rules[0].ResourceNames, "payments-superuser"); got != allow {
			t.Fatalf("allowPrivileged=%v: the grant's Role names the superuser Secret = %v", allow, got)
		}
	}
}

func TestCrossNamespaceOutsideTheWatchedNamespaces(t *testing.T) {
	p := oauth2PgAdmin()
	p.Spec.CrossNamespaceClusterRefs = []pgtoolboxv1alpha1.PgAdminClusterReference{{Namespace: remoteNamespace, Name: "payments"}}
	h := newHarness(t, nil, baseObjects(p)...)
	h.r.Config.WatchNamespaces = []string{testNamespace}
	if err := h.reconcile("team"); err != nil {
		t.Fatalf("an unwatched namespace is a configuration problem, not a loop error: %v", err)
	}
	got := h.pgadmin("team")
	expectCondition(t, got, "ConfigurationValid", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonConfigurationInvalid)
	if c := condition(got, "ConfigurationValid"); !strings.Contains(c.Message, "WATCH_NAMESPACES") {
		t.Fatalf("message %s", c.Message)
	}
}

// TestDeletedPgAdminRevokesItsRemoteAccess: the grant's Role and
// RoleBinding are controlled by the PgAdminAccess, not by the PgAdmin, so
// garbage collection keeps them when the PgAdmin goes. Left behind, they
// would give whoever re-creates that ServiceAccount name read on the
// granted credentials: the operator deletes them itself, whether the
// PgAdmin is already gone or only being deleted (a foreign finalizer
// holds it).
func TestDeletedPgAdminRevokesItsRemoteAccess(t *testing.T) {
	for _, held := range []bool{false, true} {
		name := "gone"
		if held {
			name = "held by a finalizer"
		}
		t.Run(name, func(t *testing.T) {
			p := oauth2PgAdmin()
			p.Spec.CrossNamespaceClusterRefs = []pgtoolboxv1alpha1.PgAdminClusterReference{{Namespace: remoteNamespace, Name: "payments"}}
			if held {
				p.Finalizers = []string{"example.com/hold"}
			}
			team := types.NamespacedName{Namespace: testNamespace, Name: "team"}
			// A grant of another PgAdmin keeps its access.
			h := newHarness(t, nil, append(baseObjects(p), remoteCluster("payments"),
				grant("pgadmin-team", team, "payments"),
				grant("pgadmin-ops", types.NamespacedName{Namespace: testNamespace, Name: "ops"}, "payments"))...)
			if err := h.reconcile("team"); err != nil {
				t.Fatal(err)
			}
			ops := oauth2PgAdmin()
			ops.Name, ops.ResourceVersion = "ops", ""
			ops.Spec.CrossNamespaceClusterRefs = p.Spec.CrossNamespaceClusterRefs
			if err := h.client.Create(context.Background(), ops); err != nil {
				t.Fatal(err)
			}
			if err := h.reconcile("ops"); err != nil {
				t.Fatal(err)
			}
			key := client.ObjectKey{Namespace: remoteNamespace, Name: "pgadmin-team-pgadmin-access"}
			var role rbacv1.Role
			if err := h.client.Get(context.Background(), key, &role); err != nil {
				t.Fatalf("precondition: %v", err)
			}

			if err := h.client.Delete(context.Background(), h.pgadmin("team")); err != nil {
				t.Fatal(err)
			}
			if err := h.reconcile("team"); err != nil {
				t.Fatal(err)
			}
			if err := h.client.Get(context.Background(), key, &role); !apierrors.IsNotFound(err) {
				t.Fatalf("the deleted PgAdmin's Role still grants %v (%v)", role.Rules, err)
			}
			var binding rbacv1.RoleBinding
			if err := h.client.Get(context.Background(), key, &binding); !apierrors.IsNotFound(err) {
				t.Fatalf("the deleted PgAdmin's RoleBinding is still there: %v", err)
			}
			if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: remoteNamespace, Name: "pgadmin-ops-pgadmin-access"}, &role); err != nil {
				t.Fatalf("another PgAdmin's access went with it: %v", err)
			}
		})
	}
}

// TestGrantSideRefusesAPgAdminTheAPIServerNoLongerHas: the cache can still
// hold a PgAdmin the API server deleted; its grants are not rendered.
func TestGrantSideRefusesAPgAdminTheAPIServerNoLongerHas(t *testing.T) {
	p := oauth2PgAdmin()
	p.Spec.CrossNamespaceClusterRefs = []pgtoolboxv1alpha1.PgAdminClusterReference{{Namespace: remoteNamespace, Name: "ledger"}}
	team := types.NamespacedName{Namespace: testNamespace, Name: "team"}
	h := newHarness(t, nil, append(baseObjects(p), remoteCluster("ledger"), grant("pgadmin-team", team, "ledger"))...)
	h.r.APIReader = goneReader{Reader: h.client}
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	var role rbacv1.Role
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: remoteNamespace, Name: "pgadmin-team-pgadmin-access"}, &role); !apierrors.IsNotFound(err) {
		t.Fatalf("a Role was rendered for a PgAdmin the API server does not have: %v", err)
	}
}

// goneReader answers NotFound for every PgAdmin read live.
type goneReader struct{ client.Reader }

func (g goneReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if m, ok := obj.(*metav1.PartialObjectMetadata); ok && m.Kind == "PgAdmin" {
		return apierrors.NewNotFound(pgtoolboxv1alpha1.GroupVersion.WithResource("pgadmins").GroupResource(), key.Name)
	}
	return g.Reader.Get(ctx, key, obj, opts...)
}
