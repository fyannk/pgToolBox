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

package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/ops"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel/fixtures"
)

// The access-request fixtures: the namespace team-a holds one request of
// each kind of state, team-b an approved request whose RoleBinding is gone.
// The fixture principal (jane, in group dba) is named by two of them.

// FixtureAccessStore serves the fixture requests.
type FixtureAccessStore struct {
	requests []pgtoolboxv1alpha1.PgAccessRequest
	// NotServed plays a cluster without the operator, where the API
	// server does not serve PgAccessRequest.
	NotServed bool
}

// NewFixtureAccessStore returns the fixture requests.
func NewFixtureAccessStore() *FixtureAccessStore {
	return &FixtureAccessStore{requests: fixtureAccessRequests()}
}

// Namespaces implements AccessStore.
func (*FixtureAccessStore) Namespaces() []string { return []string{"team-a", "team-b"} }

// Readable implements AccessStore.
func (s *FixtureAccessStore) Readable(string) bool { return !s.NotServed }

// Served implements AccessStore.
func (s *FixtureAccessStore) Served() bool { return !s.NotServed }

// AccessRequests implements AccessStore.
func (s *FixtureAccessStore) AccessRequests(_ context.Context, ns string) ([]pgtoolboxv1alpha1.PgAccessRequest, error) {
	var out []pgtoolboxv1alpha1.PgAccessRequest
	for i := range s.requests {
		if s.requests[i].Namespace == ns {
			out = append(out, *s.requests[i].DeepCopy())
		}
	}
	return out, nil
}

// FixtureAccessWriter writes into a FixtureAccessStore, as the API server
// would accept the writes.
type FixtureAccessWriter struct{ Store *FixtureAccessStore }

// ReadAccessRequest implements AccessWriter.
func (w FixtureAccessWriter) ReadAccessRequest(_ context.Context, ns, name string) (*pgtoolboxv1alpha1.PgAccessRequest, error) {
	for i := range w.Store.requests {
		if r := &w.Store.requests[i]; r.Namespace == ns && r.Name == name {
			return r.DeepCopy(), nil
		}
	}
	return nil, &ops.WriteError{Status: http.StatusNotFound, Text: "reading the access request: the object no longer exists"}
}

// CreateAccessRequest implements AccessWriter: the generated name is
// fixed.
func (w FixtureAccessWriter) CreateAccessRequest(_ context.Context, r *pgtoolboxv1alpha1.PgAccessRequest) (*pgtoolboxv1alpha1.PgAccessRequest, error) {
	created := r.DeepCopy()
	created.Name = r.GenerateName + "h7d2c"
	created.UID = types.UID("fixture-" + created.Name)
	created.CreationTimestamp = metav1.Time{Time: fixtures.Now}
	created.Generation = 1
	w.Store.requests = append(w.Store.requests, *created)
	return created, nil
}

// CreateGrant implements AccessWriter.
func (FixtureAccessWriter) CreateGrant(context.Context, *pgtoolboxv1alpha1.PgAccessRequest) error {
	return nil
}

// RecordDecision implements AccessWriter.
func (w FixtureAccessWriter) RecordDecision(_ context.Context, r *pgtoolboxv1alpha1.PgAccessRequest) error {
	for i := range w.Store.requests {
		if w.Store.requests[i].UID == r.UID {
			w.Store.requests[i] = *r.DeepCopy()
		}
	}
	return nil
}

func fixtureReady(status metav1.ConditionStatus, reason pgtoolboxv1alpha1.Reason, message string) []metav1.Condition {
	return []metav1.Condition{{Type: pgtoolboxv1alpha1.ConditionReady, Status: status, Reason: reason.String(), Message: message, ObservedGeneration: 1}}
}

// fixtureGranted is the operator's report of a grant in place.
func fixtureGranted() []metav1.Condition {
	return append(fixtureReady(metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected, "every aspect is as expected"),
		metav1.Condition{
			Type: pgtoolboxv1alpha1.ConditionRoleBindingSynced, Status: metav1.ConditionTrue, Reason: pgtoolboxv1alpha1.ReasonAccessGranted.String(),
			Message: "RoleBinding pgaccess-viewer-q8w3d binds Group dba to ClusterRole pgtoolbox-console-viewer in namespace team-a: every Cluster of the namespace", ObservedGeneration: 1,
		})
}

func fixtureAccessRequests() []pgtoolboxv1alpha1.PgAccessRequest {
	at := func(d time.Duration) *metav1.Time { return &metav1.Time{Time: fixtures.Now.Add(d)} }
	request := func(ns, name string, kind pgtoolboxv1alpha1.AccessSubjectKind, subject string, capability pgtoolboxv1alpha1.AccessCapability, age time.Duration) pgtoolboxv1alpha1.PgAccessRequest {
		return pgtoolboxv1alpha1.PgAccessRequest{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID("fixture-" + name), Generation: 1, CreationTimestamp: *at(-age)},
			Spec: pgtoolboxv1alpha1.PgAccessRequestSpec{
				Subject: pgtoolboxv1alpha1.AccessSubject{Kind: kind, Name: subject}, Capability: capability,
			},
		}
	}
	pending := request("team-a", "operator-k2x9p", pgtoolboxv1alpha1.AccessSubjectUser, "oidc:victor@corp.example", pgtoolboxv1alpha1.AccessCapabilityOperator, 10*time.Minute)
	pending.Spec.Cluster, pending.Spec.Justification, pending.Spec.ExpiresAt = "orders", "INC-4211: replica lag on orders, need to restart the standby", at(8*time.Hour)
	pending.Status = pgtoolboxv1alpha1.PgAccessRequestStatus{
		Phase: pgtoolboxv1alpha1.AccessPhasePending, ObservedGeneration: 1,
		Conditions: fixtureReady(metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAwaitingDecision, "waiting for a decision; nothing is bound"),
	}

	mine := request("team-a", "operator-m4n7t", pgtoolboxv1alpha1.AccessSubjectUser, "oidc:jane@corp.example", pgtoolboxv1alpha1.AccessCapabilityOperator, 5*time.Minute)
	mine.Spec.Justification, mine.Spec.ExpiresAt = "On call this week: backups and restarts", at(7*24*time.Hour)
	mine.Status = pgtoolboxv1alpha1.PgAccessRequestStatus{
		Phase: pgtoolboxv1alpha1.AccessPhasePending, ObservedGeneration: 1,
		Conditions: fixtureReady(metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAwaitingDecision, "waiting for a decision; nothing is bound"),
	}

	approved := request("team-a", "viewer-q8w3d", pgtoolboxv1alpha1.AccessSubjectGroup, "dba", pgtoolboxv1alpha1.AccessCapabilityViewer, 26*time.Hour)
	approved.Spec.Justification = "The DBA team reads every cluster of team-a"
	approved.Status = pgtoolboxv1alpha1.PgAccessRequestStatus{
		Phase: pgtoolboxv1alpha1.AccessPhaseApproved, ObservedGeneration: 1,
		DecidedBy: "oidc:bob@corp.example", DecidedAt: at(-25 * time.Hour), RoleBinding: "pgaccess-viewer-q8w3d", ClusterRole: pgtoolboxv1alpha1.ClusterRoleViewer,
		GrantedScope: pgtoolboxv1alpha1.AccessGrantScopeNamespace,
		Conditions:   fixtureGranted(),
	}

	refused := request("team-a", "operator-w3e4r", pgtoolboxv1alpha1.AccessSubjectServiceAccount, "nightly-report", pgtoolboxv1alpha1.AccessCapabilityOperator, 50*time.Hour)
	refused.Spec.Justification = "The report job wants to trigger backups"
	refused.Status = pgtoolboxv1alpha1.PgAccessRequestStatus{
		Phase: pgtoolboxv1alpha1.AccessPhaseDenied, ObservedGeneration: 1,
		DecidedBy: "oidc:bob@corp.example", DecidedAt: at(-49 * time.Hour), Note: "Reading is enough for a report: request viewer",
		Conditions: fixtureReady(metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected, "every aspect is as expected"),
	}

	expired := request("team-a", "operator-a9s8d", pgtoolboxv1alpha1.AccessSubjectUser, "oidc:victor@corp.example", pgtoolboxv1alpha1.AccessCapabilityOperator, 9*24*time.Hour)
	expired.Spec.Justification, expired.Spec.ExpiresAt = "Upgrade window", at(-8*24*time.Hour)
	expired.Status = pgtoolboxv1alpha1.PgAccessRequestStatus{
		Phase: pgtoolboxv1alpha1.AccessPhaseExpired, ObservedGeneration: 1,
		DecidedBy: "oidc:bob@corp.example", DecidedAt: at(-9 * 24 * time.Hour), RoleBinding: "pgaccess-operator-a9s8d", ClusterRole: pgtoolboxv1alpha1.ClusterRoleOperator,
		GrantedScope: pgtoolboxv1alpha1.AccessGrantScopeNamespace,
		Conditions:   fixtureReady(metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected, "every aspect is as expected"),
	}

	missing := request("team-b", "viewer-p0o9i", pgtoolboxv1alpha1.AccessSubjectUser, "oidc:carol@corp.example", pgtoolboxv1alpha1.AccessCapabilityViewer, 3*time.Hour)
	missing.Spec.Justification = "Capacity review"
	missing.Status = pgtoolboxv1alpha1.PgAccessRequestStatus{
		Phase: pgtoolboxv1alpha1.AccessPhaseApproved, ObservedGeneration: 1,
		DecidedBy: "oidc:jane@corp.example", DecidedAt: at(-2 * time.Hour), RoleBinding: "pgaccess-viewer-p0o9i", ClusterRole: pgtoolboxv1alpha1.ClusterRoleViewer,
		GrantedScope: pgtoolboxv1alpha1.AccessGrantScopeNamespace,
		Conditions: fixtureReady(metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonRoleBindingMissing,
			"RoleBindingSynced: RoleBinding pgaccess-viewer-p0o9i does not exist; the operator never recreates a grant: revoke the request and ask again"),
	}

	return []pgtoolboxv1alpha1.PgAccessRequest{pending, mine, approved, refused, expired, missing}
}

// accessFixtureAuthorizer answers the access questions: the reviewer may
// do everything; the requester may create requests and nothing else.
type accessFixtureAuthorizer struct {
	FixtureAuthorizer
	requester bool
	noCreate  bool
}

// Allowed implements Authorizer.
func (f accessFixtureAuthorizer) Allowed(ctx context.Context, u authz.User, at authz.Attributes) authz.Decision {
	switch {
	case f.noCreate && at.Resource == accessResource && at.Verb == "create":
		return authz.Decision{}
	case f.requester && (at.Resource == "rolebindings" || (at.Resource == accessResource && at.Verb != "create")):
		return authz.Decision{}
	}
	return f.FixtureAuthorizer.Allowed(ctx, u, at)
}

type accessFixtureCase struct {
	file, op, method, path, body, scenario string
	authz                                  accessFixtureAuthorizer
	readOnly                               bool
}

func accessFixtureCases() []accessFixtureCase {
	base := "/api/" + Version
	ns := base + "/namespaces/team-a/access-requests"
	requester := accessFixtureAuthorizer{requester: true}
	post := http.MethodPost
	future := fixtures.Now.Add(8 * time.Hour).Format(time.RFC3339)
	past := fixtures.Now.Add(-time.Hour).Format(time.RFC3339)
	return []accessFixtureCase{
		{"listAccessRequests.json", OpListAccessRequests, "", base + "/access-requests", "", "a reviewer: every request of team-a and team-b, with the decisions offered", accessFixtureAuthorizer{}, false},
		{"listAccessRequests.requester.json", OpListAccessRequests, "", base + "/access-requests", "", "a requester who may not list: only the requests naming them (their user, their group)", requester, false},
		{"listAccessRequests.readOnly.json", OpListAccessRequests, "", base + "/access-requests?namespace=team-a", "", "a console without writes: requests listed, nothing offered", accessFixtureAuthorizer{}, true},
		{"getAccessRequest.json", OpGetAccessRequest, "", ns + "/operator-k2x9p", "", "a pending request naming a Cluster: the scope says namespace-wide", accessFixtureAuthorizer{}, false},
		{"getAccessRequest.forbidden.json", OpGetAccessRequest, "", ns + "/operator-k2x9p", "", "403: not theirs and no get", requester, false},
		{"createAccessRequest.json", OpCreateAccessRequest, post, ns, `{"capability":"operator","cluster":"orders","justification":"INC-4300: failover drill","expiresAt":"` + future + `"}`, "201: a request for yourself", requester, false},
		{"createAccessRequest.forbidden.json", OpCreateAccessRequest, post, ns, `{"capability":"viewer","justification":"read the dashboards"}`, "403: no create pgaccessrequests here", accessFixtureAuthorizer{noCreate: true}, false},
		{"createAccessRequest.invalid.json", OpCreateAccessRequest, post, ns, `{"capability":"viewer","justification":"read","expiresAt":"` + past + `"}`, "422: an expiry in the past", accessFixtureAuthorizer{}, false},
		{"approveAccessRequest.json", OpApproveAccessRequest, post, ns + "/operator-k2x9p/approve", `{"note":"for INC-4211 only"}`, "200: approved, RoleBinding created as the approver", accessFixtureAuthorizer{}, false},
		{"approveAccessRequest.self.json", OpApproveAccessRequest, post, ns + "/operator-m4n7t/approve", `{}`, "403: the request names the approver", accessFixtureAuthorizer{}, false},
		{"approveAccessRequest.forbidden.json", OpApproveAccessRequest, post, ns + "/operator-k2x9p/approve", `{}`, "403: no create rolebindings here", requester, false},
		{"approveAccessRequest.conflict.json", OpApproveAccessRequest, post, ns + "/operator-w3e4r/approve", `{}`, "409: already denied", accessFixtureAuthorizer{}, false},
		{"denyAccessRequest.json", OpDenyAccessRequest, post, ns + "/operator-k2x9p/deny", `{"note":"Restarting the standby is on-call work: ask the on-call DBA"}`, "200: denied with a note", accessFixtureAuthorizer{}, false},
		{"denyAccessRequest.noNote.json", OpDenyAccessRequest, post, ns + "/operator-k2x9p/deny", `{}`, "422: a denial needs a note", accessFixtureAuthorizer{}, false},
		{"revokeAccessRequest.json", OpRevokeAccessRequest, post, ns + "/viewer-q8w3d/revoke", `{"note":"the DBA team moved to team-c"}`, "200: revoked; the operator deletes the RoleBinding next", accessFixtureAuthorizer{}, false},
	}
}

// generateAccessFixtures renders the access-request fixtures into out.
func generateAccessFixtures(out map[string][]byte) ([]Fixture, error) {
	var manifest []Fixture
	for _, c := range accessFixtureCases() {
		store := NewFixtureAccessStore()
		access := &Access{Store: store, Now: func() time.Time { return fixtures.Now }}
		if !c.readOnly {
			access.Writer = FixtureAccessWriter{Store: store}
		}
		h, _ := New(Deps{Source: FixtureSource{}, Authz: c.authz, Features: FixtureFeatures, Access: access})
		method := c.method
		if method == "" {
			method = http.MethodGet
		}
		code, body, err := serveFixture(h, method, c.path, c.body, c.body != "")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.file, err)
		}
		out["testdata/fixtures/"+c.file] = body
		manifest = append(manifest, Fixture{File: c.file, OperationID: c.op, Method: method, Path: c.path, Status: code, Scenario: c.scenario, Body: c.body})
	}
	return manifest, nil
}
