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
	"strings"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

var testNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func request(phase pgtoolboxv1alpha1.PgAccessRequestPhase, mutate ...func(*pgtoolboxv1alpha1.PgAccessRequest)) *pgtoolboxv1alpha1.PgAccessRequest {
	r := &pgtoolboxv1alpha1.PgAccessRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "jane-operator", UID: types.UID("req-uid"), Generation: 1},
		Spec: pgtoolboxv1alpha1.PgAccessRequestSpec{
			Subject:       pgtoolboxv1alpha1.AccessSubject{Kind: pgtoolboxv1alpha1.AccessSubjectUser, Name: "oidc:jane@corp.example"},
			Capability:    pgtoolboxv1alpha1.AccessCapabilityOperator,
			Justification: "INC-1",
		},
		Status: pgtoolboxv1alpha1.PgAccessRequestStatus{Phase: phase},
	}
	if phase == pgtoolboxv1alpha1.AccessPhaseApproved {
		r.Status.DecidedBy, r.Status.RoleBinding, r.Status.ClusterRole = "oidc:bob@corp.example", r.RoleBindingName(), pgtoolboxv1alpha1.ClusterRoleOperator
	}
	for _, m := range mutate {
		m(r)
	}
	return r
}

func expiresAt(t time.Time) func(*pgtoolboxv1alpha1.PgAccessRequest) {
	return func(r *pgtoolboxv1alpha1.PgAccessRequest) { r.Spec.ExpiresAt = &metav1.Time{Time: t} }
}

func granted(r *pgtoolboxv1alpha1.PgAccessRequest, created time.Time) rbacv1.RoleBinding {
	b := *r.DesiredRoleBinding()
	b.UID = "rb-uid"
	b.CreationTimestamp = metav1.Time{Time: created}
	return b
}

func TestDecide(t *testing.T) {
	old := testNow.Add(-time.Hour)
	cases := []struct {
		name        string
		req         *pgtoolboxv1alpha1.PgAccessRequest
		bindings    func(*pgtoolboxv1alpha1.PgAccessRequest) []rbacv1.RoleBinding
		wantPhase   pgtoolboxv1alpha1.PgAccessRequestPhase
		wantReason  pgtoolboxv1alpha1.Reason
		wantStatus  metav1.ConditionStatus
		wantDeletes int
		wantRequeue time.Time
		wantMessage string
	}{
		{
			name: "new request becomes Pending", req: request(""), wantPhase: pgtoolboxv1alpha1.AccessPhasePending,
			wantReason: pgtoolboxv1alpha1.ReasonAwaitingDecision, wantStatus: metav1.ConditionTrue,
		},
		{
			name: "pending with expiry requeues at it", req: request(pgtoolboxv1alpha1.AccessPhasePending, expiresAt(testNow.Add(time.Hour))),
			wantPhase: pgtoolboxv1alpha1.AccessPhasePending, wantReason: pgtoolboxv1alpha1.ReasonAwaitingDecision, wantStatus: metav1.ConditionTrue,
			wantRequeue: testNow.Add(time.Hour),
		},
		{
			name: "pending past expiry expires", req: request(pgtoolboxv1alpha1.AccessPhasePending, expiresAt(testNow)),
			wantPhase: pgtoolboxv1alpha1.AccessPhaseExpired, wantReason: pgtoolboxv1alpha1.ReasonAccessExpired, wantStatus: metav1.ConditionTrue,
		},
		{
			name: "pending with an approval in flight waits for its grace", req: request(pgtoolboxv1alpha1.AccessPhasePending),
			bindings: func(r *pgtoolboxv1alpha1.PgAccessRequest) []rbacv1.RoleBinding {
				return []rbacv1.RoleBinding{granted(r, testNow.Add(-10*time.Second))}
			},
			wantPhase: pgtoolboxv1alpha1.AccessPhasePending, wantReason: pgtoolboxv1alpha1.ReasonAwaitingDecision, wantStatus: metav1.ConditionTrue,
			wantRequeue: testNow.Add(OrphanGrace - 10*time.Second),
		},
		{
			name: "pending with an orphan past its grace deletes it", req: request(pgtoolboxv1alpha1.AccessPhasePending),
			bindings: func(r *pgtoolboxv1alpha1.PgAccessRequest) []rbacv1.RoleBinding {
				return []rbacv1.RoleBinding{granted(r, old)}
			},
			wantPhase: pgtoolboxv1alpha1.AccessPhasePending, wantReason: pgtoolboxv1alpha1.ReasonRoleBindingOrphaned, wantStatus: metav1.ConditionTrue, wantDeletes: 1,
		},
		{
			name: "approved and bound", req: request(pgtoolboxv1alpha1.AccessPhaseApproved, expiresAt(testNow.Add(8*time.Hour))),
			bindings: func(r *pgtoolboxv1alpha1.PgAccessRequest) []rbacv1.RoleBinding {
				return []rbacv1.RoleBinding{granted(r, old)}
			},
			wantPhase: pgtoolboxv1alpha1.AccessPhaseApproved, wantReason: pgtoolboxv1alpha1.ReasonAccessGranted, wantStatus: metav1.ConditionTrue,
			wantRequeue: testNow.Add(8 * time.Hour), wantMessage: "every Cluster of the namespace",
		},
		{
			name: "approved without its binding is reported, never recreated", req: request(pgtoolboxv1alpha1.AccessPhaseApproved),
			wantPhase: pgtoolboxv1alpha1.AccessPhaseApproved, wantReason: pgtoolboxv1alpha1.ReasonRoleBindingMissing, wantStatus: metav1.ConditionFalse,
		},
		{
			name: "approved with a drifted binding deletes it", req: request(pgtoolboxv1alpha1.AccessPhaseApproved),
			bindings: func(r *pgtoolboxv1alpha1.PgAccessRequest) []rbacv1.RoleBinding {
				b := granted(r, old)
				b.Subjects = append(b.Subjects, rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "mallory"})
				return []rbacv1.RoleBinding{b}
			},
			wantPhase: pgtoolboxv1alpha1.AccessPhaseApproved, wantReason: pgtoolboxv1alpha1.ReasonRoleBindingDrifted, wantStatus: metav1.ConditionFalse, wantDeletes: 1,
		},
		{
			name: "approved with a binding to another ClusterRole deletes it", req: request(pgtoolboxv1alpha1.AccessPhaseApproved),
			bindings: func(r *pgtoolboxv1alpha1.PgAccessRequest) []rbacv1.RoleBinding {
				b := granted(r, old)
				b.RoleRef.Name = "cluster-admin"
				return []rbacv1.RoleBinding{b}
			},
			wantPhase: pgtoolboxv1alpha1.AccessPhaseApproved, wantReason: pgtoolboxv1alpha1.ReasonRoleBindingDrifted, wantStatus: metav1.ConditionFalse, wantDeletes: 1,
		},
		{
			name: "approved past expiry expires and unbinds", req: request(pgtoolboxv1alpha1.AccessPhaseApproved, expiresAt(testNow.Add(-time.Second))),
			bindings: func(r *pgtoolboxv1alpha1.PgAccessRequest) []rbacv1.RoleBinding {
				return []rbacv1.RoleBinding{granted(r, old)}
			},
			wantPhase: pgtoolboxv1alpha1.AccessPhaseExpired, wantReason: pgtoolboxv1alpha1.ReasonAccessExpired, wantStatus: metav1.ConditionTrue, wantDeletes: 1,
		},
		{
			name: "self-approval is revoked", req: request(pgtoolboxv1alpha1.AccessPhaseApproved, func(r *pgtoolboxv1alpha1.PgAccessRequest) {
				r.Status.DecidedBy = r.Spec.Subject.Name
			}),
			bindings: func(r *pgtoolboxv1alpha1.PgAccessRequest) []rbacv1.RoleBinding {
				return []rbacv1.RoleBinding{granted(r, old)}
			},
			wantPhase: pgtoolboxv1alpha1.AccessPhaseRevoked, wantReason: pgtoolboxv1alpha1.ReasonSelfApprovalRefused, wantStatus: metav1.ConditionTrue, wantDeletes: 1,
		},
		{
			name: "self-approval of a ServiceAccount is revoked", req: request(pgtoolboxv1alpha1.AccessPhaseApproved, func(r *pgtoolboxv1alpha1.PgAccessRequest) {
				r.Spec.Subject = pgtoolboxv1alpha1.AccessSubject{Kind: pgtoolboxv1alpha1.AccessSubjectServiceAccount, Name: "ci"}
				r.Status.DecidedBy = "system:serviceaccount:team-a:ci"
			}),
			wantPhase: pgtoolboxv1alpha1.AccessPhaseRevoked, wantReason: pgtoolboxv1alpha1.ReasonSelfApprovalRefused, wantStatus: metav1.ConditionTrue,
		},
		{
			name: "revoked unbinds", req: request(pgtoolboxv1alpha1.AccessPhaseRevoked),
			bindings: func(r *pgtoolboxv1alpha1.PgAccessRequest) []rbacv1.RoleBinding {
				return []rbacv1.RoleBinding{granted(r, old)}
			},
			wantPhase: pgtoolboxv1alpha1.AccessPhaseRevoked, wantReason: pgtoolboxv1alpha1.ReasonAccessRevoked, wantStatus: metav1.ConditionTrue, wantDeletes: 1,
		},
		{
			name: "denied stays unbound", req: request(pgtoolboxv1alpha1.AccessPhaseDenied),
			wantPhase: pgtoolboxv1alpha1.AccessPhaseDenied, wantReason: pgtoolboxv1alpha1.ReasonAccessDenied, wantStatus: metav1.ConditionTrue,
		},
		{
			name: "expired is terminal even with a later expiry", req: request(pgtoolboxv1alpha1.AccessPhaseExpired, expiresAt(testNow.Add(time.Hour))),
			wantPhase: pgtoolboxv1alpha1.AccessPhaseExpired, wantReason: pgtoolboxv1alpha1.ReasonAccessExpired, wantStatus: metav1.ConditionTrue,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var bindings []rbacv1.RoleBinding
			if tc.bindings != nil {
				bindings = tc.bindings(tc.req)
			}
			d := Decide(tc.req, bindings, testNow)
			if d.Phase != tc.wantPhase || d.Reason != tc.wantReason || d.Status != tc.wantStatus {
				t.Fatalf("Decide = phase %s reason %s status %s, want %s %s %s (%s)", d.Phase, d.Reason, d.Status, tc.wantPhase, tc.wantReason, tc.wantStatus, d.Message)
			}
			if len(d.Delete) != tc.wantDeletes {
				t.Fatalf("deletes %d bindings, want %d", len(d.Delete), tc.wantDeletes)
			}
			if !d.RequeueAt.Equal(tc.wantRequeue) {
				t.Fatalf("requeue at %v, want %v", d.RequeueAt, tc.wantRequeue)
			}
			if !strings.Contains(d.Message, tc.wantMessage) {
				t.Fatalf("message %q lacks %q", d.Message, tc.wantMessage)
			}
			if !pgtoolboxv1alpha1.IsKnownReason(d.Reason) {
				t.Fatalf("reason %s is not in the catalogue", d.Reason)
			}
		})
	}
}

// TestDecideNeverAsksToCreate: whatever the input, the decision only ever
// deletes bindings the request controls — the operator grants nothing.
func TestDecideNeverAsksToCreate(t *testing.T) {
	for _, phase := range []pgtoolboxv1alpha1.PgAccessRequestPhase{"", "Pending", "Approved", "Denied", "Expired", "Revoked"} {
		r := request(phase)
		b := granted(r, testNow.Add(-time.Hour))
		d := Decide(r, []rbacv1.RoleBinding{b}, testNow)
		for _, deleted := range d.Delete {
			if deleted.Name != b.Name {
				t.Fatalf("%s: deletes %s, which it was not given", phase, deleted.Name)
			}
		}
	}
}
