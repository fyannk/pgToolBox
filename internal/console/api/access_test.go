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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/ops"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel/fixtures"
)

// spyAccessWriter records every call the API makes on the caller's behalf.
type spyAccessWriter struct {
	FixtureAccessWriter
	mu    sync.Mutex
	calls []string
}

func (s *spyAccessWriter) record(what string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, what)
}

func (s *spyAccessWriter) ReadAccessRequest(ctx context.Context, ns, name string) (*pgtoolboxv1alpha1.PgAccessRequest, error) {
	s.record("read " + name)
	return s.FixtureAccessWriter.ReadAccessRequest(ctx, ns, name)
}

func (s *spyAccessWriter) CreateAccessRequest(ctx context.Context, r *pgtoolboxv1alpha1.PgAccessRequest) (*pgtoolboxv1alpha1.PgAccessRequest, error) {
	s.record("create " + r.GenerateName)
	return s.FixtureAccessWriter.CreateAccessRequest(ctx, r)
}

func (s *spyAccessWriter) CreateGrant(ctx context.Context, r *pgtoolboxv1alpha1.PgAccessRequest) error {
	s.record("grant " + r.RoleBindingName())
	return nil
}

func (s *spyAccessWriter) RecordDecision(ctx context.Context, r *pgtoolboxv1alpha1.PgAccessRequest) error {
	s.record("status " + r.Name + " " + string(r.Status.Phase))
	return s.FixtureAccessWriter.RecordDecision(ctx, r)
}

func accessHandler(az Authorizer, writer AccessWriter) http.Handler {
	store := NewFixtureAccessStore()
	if spy, ok := writer.(*spyAccessWriter); ok {
		spy.Store = store
	}
	access := &Access{Store: store, Writer: writer, Now: func() time.Time { return fixtures.Now }}
	h, _ := New(Deps{Source: FixtureSource{}, Authz: az, Access: access})
	return h
}

func post(t *testing.T, h http.Handler, p *auth.Principal, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(auth.WithPrincipal(context.Background(), p), http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(auth.RequestHeader, "1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// TestAccessDecisionsAskTheAPIServerFirst: a refused or undecided review
// is answered before the request is read or anything written, and the
// writes that follow are exactly the pinned ones, in order.
func TestAccessDecisionsAskTheAPIServerFirst(t *testing.T) {
	t.Parallel()
	ns := "/api/v1/namespaces/team-a/access-requests"
	undecided := accessFixtureAuthorizer{FixtureAuthorizer: FixtureAuthorizer{Unchecked: map[authz.ObjectRef]bool{
		{Namespace: "team-a"}: true, {Namespace: "team-a", Name: "operator-k2x9p"}: true,
	}}}
	cases := []struct {
		name   string
		az     Authorizer
		path   string
		body   string
		status int
		calls  []string
	}{
		{"approve as a requester", accessFixtureAuthorizer{requester: true}, ns + "/operator-k2x9p/approve", `{}`, http.StatusForbidden, nil},
		{"approve undecided", undecided, ns + "/operator-k2x9p/approve", `{}`, http.StatusServiceUnavailable, nil},
		{
			"approve",
			accessFixtureAuthorizer{},
			ns + "/operator-k2x9p/approve", `{}`, http.StatusOK,
			[]string{"read operator-k2x9p", "grant pgaccess-operator-k2x9p", "status operator-k2x9p Approved"},
		},
		{"approve your own", accessFixtureAuthorizer{}, ns + "/operator-m4n7t/approve", `{}`, http.StatusForbidden, []string{"read operator-m4n7t"}},
		{"approve an approved request", accessFixtureAuthorizer{}, ns + "/viewer-q8w3d/approve", `{}`, http.StatusConflict, []string{"read viewer-q8w3d"}},
		{"approve a gone request", accessFixtureAuthorizer{}, ns + "/gone/approve", `{}`, http.StatusNotFound, []string{"read gone"}},
		{"deny without a note", accessFixtureAuthorizer{}, ns + "/operator-k2x9p/deny", `{}`, http.StatusUnprocessableEntity, []string{"read operator-k2x9p"}},
		{"deny", accessFixtureAuthorizer{}, ns + "/operator-k2x9p/deny", `{"note":"no"}`, http.StatusOK, []string{"read operator-k2x9p", "status operator-k2x9p Denied"}},
		{"deny your own", accessFixtureAuthorizer{}, ns + "/operator-m4n7t/deny", `{"note":"withdrawn"}`, http.StatusOK, []string{"read operator-m4n7t", "status operator-m4n7t Denied"}},
		{"revoke pending", accessFixtureAuthorizer{}, ns + "/operator-k2x9p/revoke", `{}`, http.StatusConflict, []string{"read operator-k2x9p"}},
		{"revoke", accessFixtureAuthorizer{}, ns + "/viewer-q8w3d/revoke", `{}`, http.StatusOK, []string{"read viewer-q8w3d", "status viewer-q8w3d Revoked"}},
		{"revoke as a requester", accessFixtureAuthorizer{requester: true}, ns + "/viewer-q8w3d/revoke", `{}`, http.StatusForbidden, nil},
		{"create", accessFixtureAuthorizer{requester: true}, ns, `{"capability":"viewer","justification":"read"}`, http.StatusCreated, []string{"create viewer-"}},
		{"create without the right", accessFixtureAuthorizer{noCreate: true}, ns, `{"capability":"viewer","justification":"read"}`, http.StatusForbidden, nil},
		{"create in an unwatched namespace", accessFixtureAuthorizer{}, "/api/v1/namespaces/elsewhere/access-requests", `{"capability":"viewer","justification":"read"}`, http.StatusNotFound, nil},
		{"create for system:authenticated", accessFixtureAuthorizer{}, ns, `{"capability":"viewer","justification":"x","subject":{"kind":"Group","name":"system:authenticated"}}`, http.StatusUnprocessableEntity, nil},
		{"create with an unknown capability", accessFixtureAuthorizer{}, ns, `{"capability":"admin","justification":"x"}`, http.StatusUnprocessableEntity, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spy := &spyAccessWriter{}
			w := post(t, accessHandler(tc.az, spy), FixturePrincipal(), tc.path, tc.body)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body)
			}
			if strings.Join(spy.calls, "|") != strings.Join(tc.calls, "|") {
				t.Fatalf("writer calls %q, want %q", spy.calls, tc.calls)
			}
		})
	}
}

func TestAccessDecisionRecordsTheApprover(t *testing.T) {
	t.Parallel()
	spy := &spyAccessWriter{}
	h := accessHandler(accessFixtureAuthorizer{}, spy)
	w := post(t, h, FixturePrincipal(), "/api/v1/namespaces/team-a/access-requests/operator-k2x9p/approve", `{"note":"ok"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var v viewmodel.AccessRequest
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Phase != "Approved" || v.DecidedBy != FixturePrincipal().User.Name || v.RoleBinding != "pgaccess-operator-k2x9p" || v.Note != "ok" {
		t.Fatalf("approved view = %+v", v)
	}
	if v.Actions.Approve.Allowed || !v.Actions.Revoke.Allowed {
		t.Fatalf("actions after approval = %+v", v.Actions)
	}
}

func TestAccessReadsWithoutWriter(t *testing.T) {
	t.Parallel()
	h := accessHandler(accessFixtureAuthorizer{}, nil)
	if w := call(t, h, FixturePrincipal(), "/api/v1/access-requests"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"writable": false`) && !strings.Contains(w.Body.String(), `"writable":false`) {
		t.Fatalf("list without writer: %d %s", w.Code, w.Body)
	}
	if w := post(t, h, FixturePrincipal(), "/api/v1/namespaces/team-a/access-requests/operator-k2x9p/approve", `{}`); w.Code != http.StatusNotImplemented {
		t.Fatalf("approve without writer: %d", w.Code)
	}
	none, _ := New(Deps{Source: FixtureSource{}, Authz: FixtureAuthorizer{}})
	if w := call(t, none, FixturePrincipal(), "/api/v1/access-requests"); w.Code != http.StatusNotImplemented {
		t.Fatalf("list without access: %d", w.Code)
	}
	if w := call(t, h, nil, "/api/v1/access-requests"); w.Code != http.StatusUnauthorized {
		t.Fatalf("list signed out: %d", w.Code)
	}
}

// TestAccessWithoutTheOperator: where the API server does not serve
// PgAccessRequest (a team install without the operator),
// nobody is offered to request or review, and every write answers 501
// naming the operator, before any review or write: not the 403 that
// would send the caller after a role that changes nothing.
func TestAccessWithoutTheOperator(t *testing.T) {
	t.Parallel()
	const want = "access requests need the pgtoolbox operator"
	ns := "/api/v1/namespaces/team-a/access-requests"
	writes := map[string]string{
		"create":  ns,
		"approve": ns + "/operator-k2x9p/approve",
		"deny":    ns + "/operator-k2x9p/deny",
		"revoke":  ns + "/viewer-q8w3d/revoke",
	}
	for name, tc := range map[string]struct {
		az     Authorizer
		writer bool
	}{
		"allowed by RBAC":          {accessFixtureAuthorizer{}, true},
		"refused by RBAC":          {accessFixtureAuthorizer{requester: true, noCreate: true}, true},
		"undecided review":         {accessFixtureAuthorizer{FixtureAuthorizer: FixtureAuthorizer{Unchecked: map[authz.ObjectRef]bool{{Namespace: "team-a"}: true}}}, true},
		"console serves no writes": {accessFixtureAuthorizer{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			spy := &spyAccessWriter{}
			store := &FixtureAccessStore{NotServed: true}
			spy.Store = store
			access := &Access{Store: store, Now: func() time.Time { return fixtures.Now }}
			if tc.writer {
				access.Writer = spy
			}
			h, _ := New(Deps{Source: FixtureSource{}, Authz: tc.az, Access: access})

			w := call(t, h, FixturePrincipal(), "/api/v1/access-requests")
			if w.Code != http.StatusOK {
				t.Fatalf("list: %d %s", w.Code, w.Body)
			}
			var v viewmodel.AccessRequestList
			if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
				t.Fatal(err)
			}
			for _, n := range v.Namespaces {
				if n.CanRequest || n.CanReview || n.Readable {
					t.Errorf("namespace %+v: nobody can request or review where the kind is not served", n)
				}
			}
			for op, path := range writes {
				body := `{"note":"n"}`
				if op == "create" {
					body = `{"capability":"viewer","justification":"read"}`
				}
				w := post(t, h, FixturePrincipal(), path, body)
				if w.Code != http.StatusNotImplemented || !strings.Contains(w.Body.String(), want) {
					t.Errorf("%s: %d %s, want 501 %q", op, w.Code, w.Body, want)
				}
				if strings.Contains(w.Body.String(), "pgtoolbox-console-viewer") || strings.Contains(w.Body.String(), "pgtoolbox-console-operator") {
					t.Errorf("%s sends the caller after a role: %s", op, w.Body)
				}
			}
			if len(spy.calls) != 0 {
				t.Errorf("writer calls %q, want none", spy.calls)
			}
		})
	}
}

// TestAccessGetIsNoOracle: without get, a request that does not name the
// caller is 403 whether it exists or not.
func TestAccessGetIsNoOracle(t *testing.T) {
	t.Parallel()
	h := accessHandler(accessFixtureAuthorizer{requester: true}, nil)
	for path, want := range map[string]int{
		"/api/v1/namespaces/team-a/access-requests/operator-k2x9p": http.StatusForbidden,
		"/api/v1/namespaces/team-a/access-requests/does-not-exist": http.StatusForbidden,
		"/api/v1/namespaces/elsewhere/access-requests/x":           http.StatusForbidden,
		"/api/v1/namespaces/team-a/access-requests/operator-m4n7t": http.StatusOK,
		"/api/v1/namespaces/team-a/access-requests/viewer-q8w3d":   http.StatusOK,
	} {
		if w := call(t, h, FixturePrincipal(), path); w.Code != want {
			t.Errorf("%s: %d, want %d", path, w.Code, want)
		}
	}
	reviewer := accessHandler(accessFixtureAuthorizer{}, nil)
	if w := call(t, reviewer, FixturePrincipal(), "/api/v1/namespaces/team-a/access-requests/does-not-exist"); w.Code != http.StatusNotFound {
		t.Fatalf("reviewer get of a missing request: %d", w.Code)
	}
}

func TestAccessListFiltersPerNamespace(t *testing.T) {
	t.Parallel()
	nobody := accessFixtureAuthorizer{requester: true, noCreate: true}
	h := accessHandler(nobody, nil)
	w := call(t, h, FixturePrincipal(), "/api/v1/access-requests")
	var v viewmodel.AccessRequestList
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	// team-b holds nothing naming jane and she may do nothing there: the
	// namespace is not named at all.
	if len(v.Namespaces) != 1 || v.Namespaces[0].Name != "team-a" {
		t.Fatalf("namespaces = %+v", v.Namespaces)
	}
	for _, item := range v.Items {
		if !item.Mine {
			t.Fatalf("a request not naming the caller is listed: %s", item.Name)
		}
	}
}

func TestNewAccessRequest(t *testing.T) {
	now := fixtures.Now
	future := now.Add(time.Hour)
	cases := []struct {
		name, username, ns string
		body               viewmodel.AccessRequestCreate
		wantErr            string
		wantKind           pgtoolboxv1alpha1.AccessSubjectKind
		wantName           string
	}{
		{"defaults to the user", "oidc:jane", "team-a", viewmodel.AccessRequestCreate{Capability: "viewer", Justification: " read "}, "", "User", "oidc:jane"},
		{"defaults to the namespace's ServiceAccount", "system:serviceaccount:team-a:ci", "team-a", viewmodel.AccessRequestCreate{Capability: "viewer", Justification: "x"}, "", "ServiceAccount", "ci"},
		{"another namespace's ServiceAccount cannot ask", "system:serviceaccount:team-b:ci", "team-a", viewmodel.AccessRequestCreate{Capability: "viewer", Justification: "x"}, "which a request cannot name", "", ""},
		{"explicit group", "oidc:jane", "team-a", viewmodel.AccessRequestCreate{Capability: "operator", Justification: "x", Subject: &viewmodel.AccessSubject{Kind: "Group", Name: "dba"}}, "", "Group", "dba"},
		{"system group", "oidc:jane", "team-a", viewmodel.AccessRequestCreate{Capability: "operator", Justification: "x", Subject: &viewmodel.AccessSubject{Kind: "Group", Name: "system:masters"}}, "system:", "", ""},
		{"ServiceAccount path", "oidc:jane", "team-a", viewmodel.AccessRequestCreate{Capability: "operator", Justification: "x", Subject: &viewmodel.AccessSubject{Kind: "ServiceAccount", Name: "team-b:ci"}}, "ServiceAccount in this namespace", "", ""},
		{"unknown kind", "oidc:jane", "team-a", viewmodel.AccessRequestCreate{Capability: "operator", Justification: "x", Subject: &viewmodel.AccessSubject{Kind: "Robot", Name: "r2"}}, "subject kind", "", ""},
		{"control character", "oidc:jane", "team-a", viewmodel.AccessRequestCreate{Capability: "operator", Justification: "x", Subject: &viewmodel.AccessSubject{Kind: "User", Name: "a\x00b"}}, "control character", "", ""},
		{"blank justification", "oidc:jane", "team-a", viewmodel.AccessRequestCreate{Capability: "viewer", Justification: "   "}, "justification", "", ""},
		{"unknown capability", "oidc:jane", "team-a", viewmodel.AccessRequestCreate{Capability: "admin", Justification: "x"}, "capability", "", ""},
		{"past expiry", "oidc:jane", "team-a", viewmodel.AccessRequestCreate{Capability: "viewer", Justification: "x", ExpiresAt: &now}, "future", "", ""},
		{"future expiry", "oidc:jane", "team-a", viewmodel.AccessRequestCreate{Capability: "viewer", Justification: "x", ExpiresAt: &future}, "", "User", "oidc:jane"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewAccessRequest(tc.ns, tc.username, tc.body, now)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.Spec.Subject.Kind != tc.wantKind || r.Spec.Subject.Name != tc.wantName || r.Namespace != tc.ns || r.GenerateName == "" {
				t.Fatalf("request = %+v", r)
			}
			if strings.TrimSpace(r.Spec.Justification) != r.Spec.Justification {
				t.Fatal("justification not trimmed")
			}
		})
	}
}

func TestAccessActions(t *testing.T) {
	now := fixtures.Now
	jane := authz.User{Name: "oidc:jane", Groups: []string{"dba"}}
	yes, no, unknown := authz.Decision{Allowed: true}, authz.Decision{}, authz.Decision{Err: context.DeadlineExceeded}
	req := func(phase pgtoolboxv1alpha1.PgAccessRequestPhase, subject string, expires *time.Time) *pgtoolboxv1alpha1.PgAccessRequest {
		r := &pgtoolboxv1alpha1.PgAccessRequest{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "r"},
			Spec:       pgtoolboxv1alpha1.PgAccessRequestSpec{Subject: pgtoolboxv1alpha1.AccessSubject{Kind: "User", Name: subject}, Capability: "viewer"},
			Status:     pgtoolboxv1alpha1.PgAccessRequestStatus{Phase: phase},
		}
		if expires != nil {
			r.Spec.ExpiresAt = &metav1.Time{Time: *expires}
		}
		return r
	}
	past := now.Add(-time.Minute)
	cases := []struct {
		name                  string
		r                     *pgtoolboxv1alpha1.PgAccessRequest
		rights                accessRights
		writable              bool
		approve, deny, revoke bool
		approveWhy, revokeWhy string
	}{
		{"reviewer, pending", req("Pending", "bob", nil), accessRights{canReview: yes, canRevoke: yes}, true, true, true, false, "", "not Approved"},
		{"reviewer, own request", req("Pending", "oidc:jane", nil), accessRights{canReview: yes, canRevoke: yes}, true, false, true, false, "names you", "not Approved"},
		{"reviewer, expired pending", req("Pending", "bob", &past), accessRights{canReview: yes, canRevoke: yes}, true, false, true, false, "expired", "not Approved"},
		{"reviewer, approved", req("Approved", "bob", nil), accessRights{canReview: yes, canRevoke: yes}, true, false, false, true, "not Pending", ""},
		{"requester", req("Pending", "bob", nil), accessRights{canReview: no, canRevoke: no}, true, false, false, false, "deciding takes", "you may not update"},
		{"undecided", req("Pending", "bob", nil), accessRights{canReview: unknown, canRevoke: unknown}, true, false, false, false, "could not be asked", "could not be asked"},
		{"read-only console", req("Pending", "bob", nil), accessRights{canReview: yes, canRevoke: yes}, false, false, false, false, "serves no writes", "serves no writes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := accessActions(tc.r, jane, tc.rights, tc.writable, now)
			if a.Approve.Allowed != tc.approve || a.Deny.Allowed != tc.deny || a.Revoke.Allowed != tc.revoke {
				t.Fatalf("actions = %+v", a)
			}
			if !strings.Contains(a.Approve.Why, tc.approveWhy) || !strings.Contains(a.Revoke.Why, tc.revokeWhy) {
				t.Fatalf("why = %q / %q", a.Approve.Why, a.Revoke.Why)
			}
			for _, action := range []viewmodel.AccessAction{a.Approve, a.Deny, a.Revoke} {
				if action.Allowed == (action.Why != "") {
					t.Fatalf("an action is allowed with a reason, or refused without one: %+v", action)
				}
			}
		})
	}
}

// TestAccessWritesCarryTheCallersToken wires the real writer over the
// user client against a stand-in API server: every call it receives
// carries the signed-in person's own bearer token (token passthrough),
// never none and never another.
func TestAccessWritesCarryTheCallersToken(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var seen []string
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"apiVersion":"pgtoolbox.fyannk.dev/v1alpha1","kind":"PgAccessRequest","metadata":{"namespace":"team-a","name":"viewer-abcde"},"spec":{"subject":{"kind":"User","name":"oidc:jane@corp.example"},"capability":"viewer","justification":"read"}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
	}))
	defer apiServer.Close()
	c, err := ops.NewAccessUserClient(auth.UserConfig(&rest.Config{Host: apiServer.URL}))
	if err != nil {
		t.Fatal(err)
	}
	access := &Access{Store: NewFixtureAccessStore(), Writer: ops.NewAccessWriter(c, 5*time.Second), Now: func() time.Time { return fixtures.Now }}
	h, _ := New(Deps{Source: FixtureSource{}, Authz: accessFixtureAuthorizer{}, Access: access})
	p := FixturePrincipal()
	p.Token = "jane-token"
	if w := post(t, h, p, "/api/v1/namespaces/team-a/access-requests", `{"capability":"viewer","justification":"read"}`); w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	if w := post(t, h, p, "/api/v1/namespaces/team-a/access-requests/gone/approve", `{}`); w.Code != http.StatusNotFound {
		t.Fatalf("approve of a missing request: %d %s", w.Code, w.Body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("the API server saw %q", seen)
	}
	for _, call := range seen {
		if !strings.HasSuffix(call, " Bearer jane-token") {
			t.Fatalf("a call without the caller's token: %q", call)
		}
	}
}

// racingWriter refuses the first status writes as stale (the operator's
// own status write won the race), after letting race change the stored
// request the way the competing writer did.
type racingWriter struct {
	spyAccessWriter
	conflicts int
	race      func(*pgtoolboxv1alpha1.PgAccessRequest)
}

func (w *racingWriter) RecordDecision(ctx context.Context, r *pgtoolboxv1alpha1.PgAccessRequest) error {
	if w.conflicts > 0 {
		w.conflicts--
		w.record("stale " + r.Name)
		for i := range w.Store.requests {
			if w.Store.requests[i].UID == r.UID && w.race != nil {
				w.race(&w.Store.requests[i])
			}
		}
		return &ops.WriteError{Status: http.StatusConflict, Text: "recording the decision: conflict"}
	}
	return w.spyAccessWriter.RecordDecision(ctx, r)
}

// TestAccessDecisionSurvivesTheOperatorsStatusWrite: a decision made just
// after the request was created races the operator marking it Pending; the
// stale write is re-judged on a fresh read and written again, while a
// decision someone else recorded in between is never overwritten.
func TestAccessDecisionSurvivesTheOperatorsStatusWrite(t *testing.T) {
	t.Parallel()
	path := "/api/v1/namespaces/team-a/access-requests/operator-k2x9p/approve"
	operatorPending := func(r *pgtoolboxv1alpha1.PgAccessRequest) {
		r.Status.Conditions = append(r.Status.Conditions, metav1.Condition{Type: "Progressing", Status: metav1.ConditionFalse, Reason: "AsExpected"})
	}
	someoneDenied := func(r *pgtoolboxv1alpha1.PgAccessRequest) {
		r.Status.Phase, r.Status.DecidedBy, r.Status.Note = pgtoolboxv1alpha1.AccessPhaseDenied, "oidc:bob@corp.example", "no"
	}
	replaced := func(r *pgtoolboxv1alpha1.PgAccessRequest) {
		r.Spec.Capability = pgtoolboxv1alpha1.AccessCapabilityViewer
	}
	cases := []struct {
		name      string
		conflicts int
		race      func(*pgtoolboxv1alpha1.PgAccessRequest)
		status    int
		calls     string
	}{
		{
			"the operator wrote first", 1, operatorPending, http.StatusOK,
			"read operator-k2x9p|grant pgaccess-operator-k2x9p|stale operator-k2x9p|read operator-k2x9p|status operator-k2x9p Approved",
		},
		{
			"someone else decided first", 1, someoneDenied, http.StatusConflict,
			"read operator-k2x9p|grant pgaccess-operator-k2x9p|stale operator-k2x9p|read operator-k2x9p",
		},
		{
			"the request changed", 1, replaced, http.StatusConflict,
			"read operator-k2x9p|grant pgaccess-operator-k2x9p|stale operator-k2x9p|read operator-k2x9p",
		},
		{
			"a write that never lands is bounded", maxDecisionRetries + 1, operatorPending, http.StatusConflict,
			"read operator-k2x9p|grant pgaccess-operator-k2x9p|stale operator-k2x9p|read operator-k2x9p|stale operator-k2x9p|read operator-k2x9p|stale operator-k2x9p|read operator-k2x9p|stale operator-k2x9p",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := NewFixtureAccessStore()
			w := &racingWriter{conflicts: tc.conflicts, race: tc.race}
			w.Store = store
			access := &Access{Store: store, Writer: w, Now: func() time.Time { return fixtures.Now }}
			h, _ := New(Deps{Source: FixtureSource{}, Authz: accessFixtureAuthorizer{}, Access: access})
			resp := post(t, h, FixturePrincipal(), path, `{}`)
			if resp.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", resp.Code, tc.status, resp.Body)
			}
			if got := strings.Join(w.calls, "|"); got != tc.calls {
				t.Fatalf("calls\n%s\nwant\n%s", got, tc.calls)
			}
			if tc.status == http.StatusOK && !strings.Contains(resp.Body.String(), `"decidedBy":"`+FixturePrincipal().User.Name+`"`) {
				t.Fatalf("the retried decision lost its decider: %s", resp.Body)
			}
		})
	}
}
