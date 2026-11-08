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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// screenSpy records whether the read model, the log reader or the
// operator was consulted.
type screenSpy struct {
	FixtureSource
	reads *int
}

func (s screenSpy) Backups(ctx context.Context, ns, name string) (viewmodel.Backups, error) {
	*s.reads++
	return s.FixtureSource.Backups(ctx, ns, name)
}

func (s screenSpy) RestoreCheck(ctx context.Context, ns, name string, at time.Time) (viewmodel.RestoreCheck, error) {
	*s.reads++
	return s.FixtureSource.RestoreCheck(ctx, ns, name, at)
}

func (s screenSpy) Revision(ctx context.Context, ns, name string, seq int64) (viewmodel.Revision, error) {
	*s.reads++
	return s.FixtureSource.Revision(ctx, ns, name, seq)
}

func (s screenSpy) Compare(ctx context.Context, lns, ln, rns, rn string) (viewmodel.Comparison, error) {
	*s.reads++
	return s.FixtureSource.Compare(ctx, lns, ln, rns, rn)
}

type logSpy struct {
	FixtureLogs
	reads *int
}

func (l logSpy) Tail(ctx context.Context, ns, c, pod string, o LogOptions) (viewmodel.LogTail, error) {
	*l.reads++
	return l.FixtureLogs.Tail(ctx, ns, c, pod, o)
}

type operatorSpy struct {
	FixtureOperator
	reads *int
}

func (o operatorSpy) Request(ctx context.Context, p *auth.Principal, ns, name string, req viewmodel.OperationRequest) (viewmodel.OperationAccepted, error) {
	*o.reads++
	return o.FixtureOperator.Request(ctx, p, ns, name, req)
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	ctx := auth.WithPrincipal(context.Background(), FixturePrincipal())
	if body != "" {
		r = httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set(auth.RequestHeader, "1")
	} else {
		r = httptest.NewRequestWithContext(ctx, method, path, nil)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestScreenAuthorizationComesFirst(t *testing.T) {
	t.Parallel()
	orders := authz.ObjectRef{Namespace: "team-a", Name: "orders"}
	base := "/api/v1/namespaces/team-a/clusters/orders"
	gone := authz.ObjectRef{Namespace: "team-a", Name: "gone"}
	compare := func(left, right string) string {
		return "/api/v1/compare?leftNamespace=team-a&leftName=" + left + "&rightNamespace=team-a&rightName=" + right
	}
	cases := []struct {
		name, method, path, body string
		az                       FixtureAuthorizer
		features                 Features
		status, reads            int
	}{
		{"backups allowed", http.MethodGet, base + "/backups", "", FixtureAuthorizer{}, FixtureFeatures, 200, 1},
		{"backups denied", http.MethodGet, base + "/backups", "", FixtureAuthorizer{Denied: map[authz.ObjectRef]bool{orders: true}}, FixtureFeatures, 403, 0},
		{"restore check allowed", http.MethodGet, base + "/backups/restore-check?at=2026-10-29T10:00:00Z", "", FixtureAuthorizer{}, FixtureFeatures, 200, 1},
		{"restore check denied", http.MethodGet, base + "/backups/restore-check?at=2026-10-29T10:00:00Z", "", FixtureAuthorizer{Denied: map[authz.ObjectRef]bool{orders: true}}, FixtureFeatures, 403, 0},
		{"restore check without a time", http.MethodGet, base + "/backups/restore-check", "", FixtureAuthorizer{}, FixtureFeatures, 422, 0},
		{"restore check of a malformed time", http.MethodGet, base + "/backups/restore-check?at=yesterday", "", FixtureAuthorizer{}, FixtureFeatures, 422, 0},
		{"revision denied", http.MethodGet, base + "/history/42", "", FixtureAuthorizer{Denied: map[authz.ObjectRef]bool{orders: true}}, FixtureFeatures, 403, 0},
		{"revision of another object", http.MethodGet, base + "/history/7", "", FixtureAuthorizer{}, FixtureFeatures, 404, 1},
		{"logs allowed", http.MethodGet, base + "/instances/orders-1/logs", "", FixtureAuthorizer{}, FixtureFeatures, 200, 1},
		{"logs denied by pods/log", http.MethodGet, base + "/instances/orders-1/logs", "", FixtureAuthorizer{DeniedLogs: true}, FixtureFeatures, 403, 0},
		{"logs switched off", http.MethodGet, base + "/instances/orders-1/logs", "", FixtureAuthorizer{}, Features{}, 501, 0},
		{"logs of a bad pod name", http.MethodGet, base + "/instances/..%2Fx/logs", "", FixtureAuthorizer{}, FixtureFeatures, 422, 0},
		{"request allowed", http.MethodPost, base + "/operations", `{"kind":"backup","confirmation":"orders"}`, FixtureAuthorizer{}, FixtureFeatures, 202, 1},
		{"request on a denied cluster", http.MethodPost, base + "/operations", `{"kind":"backup","confirmation":"orders"}`, FixtureAuthorizer{Denied: map[authz.ObjectRef]bool{orders: true}}, FixtureFeatures, 403, 0},
		{"request of an unknown kind", http.MethodPost, base + "/operations", `{"kind":"delete","confirmation":"orders"}`, FixtureAuthorizer{}, FixtureFeatures, 422, 0},
		// Compare: a refusal on either side comes before any read, so a
		// denied Cluster that does not exist answers 403, never 404 (which
		// would tell its absence to someone who may not see it).
		{"compare allowed", http.MethodGet, compare("orders", "billing"), "", FixtureAuthorizer{}, FixtureFeatures, 200, 1},
		{"compare allowed, one missing", http.MethodGet, compare("orders", "gone"), "", FixtureAuthorizer{}, FixtureFeatures, 404, 1},
		{"compare denied and missing", http.MethodGet, compare("orders", "gone"), "", FixtureAuthorizer{Denied: map[authz.ObjectRef]bool{gone: true}}, FixtureFeatures, 403, 0},
		{"compare, left denied", http.MethodGet, compare("orders", "billing"), "", FixtureAuthorizer{Denied: map[authz.ObjectRef]bool{orders: true}}, FixtureFeatures, 403, 0},
		{"request with an unknown field", http.MethodPost, base + "/operations", `{"kind":"backup","confirmation":"orders","spec":{}}`, FixtureAuthorizer{}, FixtureFeatures, 422, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reads := 0
			h, _ := New(Deps{
				Source: screenSpy{reads: &reads}, Authz: tc.az, Logs: logSpy{reads: &reads},
				Operator: operatorSpy{reads: &reads}, Features: tc.features,
			})
			w := do(t, h, tc.method, tc.path, tc.body)
			if w.Code != tc.status {
				t.Fatalf("%s %s = %d %s, want %d", tc.method, tc.path, w.Code, w.Body, tc.status)
			}
			if reads != tc.reads {
				t.Fatalf("consulted %d times, want %d", reads, tc.reads)
			}
		})
	}
}

func TestOperationLookupAuthorizesTheNamedCluster(t *testing.T) {
	t.Parallel()
	id := fixtureOperationID()
	denied := FixtureAuthorizer{Denied: map[authz.ObjectRef]bool{{Namespace: "team-a", Name: "orders"}: true}}
	h, _ := New(Deps{Source: FixtureSource{}, Authz: denied, Operator: FixtureOperator{}, Features: FixtureFeatures})
	if w := do(t, h, http.MethodGet, "/api/v1/operations/"+id, ""); w.Code != http.StatusForbidden {
		t.Fatalf("lookup on a denied cluster = %d", w.Code)
	}
	h, _ = New(Deps{Source: FixtureSource{}, Authz: FixtureAuthorizer{}})
	if w := do(t, h, http.MethodGet, "/api/v1/operations/"+id, ""); w.Code != http.StatusNotImplemented {
		t.Fatalf("lookup without an operator = %d", w.Code)
	}
}

func TestPgAdminLinksFollowAccess(t *testing.T) {
	t.Parallel()
	deny := FixtureAuthorizer{Denied: map[authz.ObjectRef]bool{{Namespace: "team-a", Name: "team-a"}: true}}
	for name, tc := range map[string]struct {
		az   FixtureAuthorizer
		want int
	}{"may open": {FixtureAuthorizer{}, 1}, "may not open": {deny, 0}} {
		h, _ := New(Deps{Source: FixtureSource{}, Authz: tc.az, Features: FixtureFeatures})
		w := do(t, h, http.MethodGet, "/api/v1/namespaces/team-a/clusters/orders", "")
		var v ClusterOverviewView
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		if len(v.PgAdmins) != tc.want {
			t.Errorf("%s: %d links", name, len(v.PgAdmins))
		}
	}
}

// failingLinks is the fixture source whose PgAdmin list fails.
type failingLinks struct{ FixtureSource }

func (failingLinks) PgAdminLinks(context.Context, string, string) ([]viewmodel.PgAdminLink, error) {
	return nil, errors.New("listing PgAdmins: cache torn down")
}

// TestPgAdminLinksUnread: a failed PgAdmin list still serves the view,
// without links, and says they were not read instead of "no pgAdmin".
func TestPgAdminLinksUnread(t *testing.T) {
	t.Parallel()
	h, _ := New(Deps{Source: failingLinks{}, Authz: FixtureAuthorizer{}, Features: FixtureFeatures})
	for _, path := range []string{
		"/api/v1/namespaces/team-a/clusters/orders",
		"/api/v1/namespaces/team-a/clusters/orders/insight",
		"/api/v1/namespaces/team-a/clusters/orders/insight/blocking",
	} {
		w := do(t, h, http.MethodGet, path, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d", path, w.Code)
		}
		var v struct {
			PgAdmins       []viewmodel.PgAdminLink `json:"pgAdmins"`
			PgAdminsUnread bool                    `json:"pgAdminsUnread"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		if !v.PgAdminsUnread || len(v.PgAdmins) != 0 {
			t.Errorf("%s: unread %v, %d links", path, v.PgAdminsUnread, len(v.PgAdmins))
		}
	}
	read, _ := New(Deps{Source: FixtureSource{}, Authz: FixtureAuthorizer{}, Features: FixtureFeatures})
	w := do(t, read, http.MethodGet, "/api/v1/namespaces/team-a/clusters/orders", "")
	var ok ClusterOverviewView
	if err := json.Unmarshal(w.Body.Bytes(), &ok); err != nil {
		t.Fatal(err)
	}
	if ok.PgAdminsUnread {
		t.Fatal("a read list marked unread")
	}
}

// TestFixedPgAdminLink: a pgAdmin installed without the operator
// (PGADMIN_URL) is offered on every Cluster, after the PgAdmin objects'
// links, whether or not the caller may get a PgAdmin, with its health
// unknown: the console knows nothing else about it.
func TestFixedPgAdminLink(t *testing.T) {
	t.Parallel()
	deny := FixtureAuthorizer{Denied: map[authz.ObjectRef]bool{{Namespace: "team-a", Name: "team-a"}: true}}
	for name, tc := range map[string]struct {
		az   FixtureAuthorizer
		want int
	}{"with a PgAdmin": {FixtureAuthorizer{}, 2}, "without access to the PgAdmin": {deny, 1}} {
		h, _ := New(Deps{Source: FixtureSource{}, Authz: tc.az, Features: FixtureFeatures, PgAdminURL: "https://pgadmin.example.com/"})
		w := do(t, h, http.MethodGet, "/api/v1/namespaces/team-a/clusters/orders", "")
		var v ClusterOverviewView
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		if len(v.PgAdmins) != tc.want {
			t.Fatalf("%s: %d links", name, len(v.PgAdmins))
		}
		fixed := v.PgAdmins[len(v.PgAdmins)-1]
		if fixed.URL != "https://pgadmin.example.com/" || fixed.Health != viewmodel.HealthUnknown || fixed.ServerState != "" {
			t.Errorf("%s: fixed link %+v", name, fixed)
		}
	}
}

func TestOpenAPIDocumentsTheCSRFHeader(t *testing.T) {
	t.Parallel()
	_, a := New(Deps{})
	op := a.OpenAPI().Paths["/api/v1/namespaces/{namespace}/clusters/{name}/operations"].Post
	found := false
	for _, p := range op.Parameters {
		if p.In == "header" && p.Name == auth.RequestHeader && p.Required {
			found = true
		}
	}
	if !found {
		t.Fatal("POST operations does not declare the required " + auth.RequestHeader + " header")
	}
}

type blockingLogs struct{ FixtureLogs }

func (blockingLogs) Follow(ctx context.Context, _, _, _ string, _ LogOptions, emit func(any) error) error {
	if err := emit(viewmodel.LogStreamLine{Text: "one"}); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

type revokedSessions struct{}

func (revokedSessions) StillValid(context.Context, *auth.Principal) bool { return false }

// A follow ends when the session is no longer valid, and never outlives
// the session's expiry.
func TestLogStreamEndsWithTheSession(t *testing.T) { //nolint:paralleltest // it shortens the package's liveness intervals
	logHeartbeat, logRecheck = 20*time.Millisecond, 50*time.Millisecond
	defer func() { logHeartbeat, logRecheck = 15*time.Second, 30*time.Second }()
	path := "/api/v1/namespaces/team-a/clusters/orders/instances/orders-1/logs/stream"
	for name, deps := range map[string]Deps{
		"revoked": {Sessions: revokedSessions{}},
		"expired": {},
	} {
		deps.Source, deps.Authz, deps.Logs, deps.Features = FixtureSource{}, FixtureAuthorizer{}, blockingLogs{}, FixtureFeatures
		h, _ := New(deps)
		p := FixturePrincipal()
		if name == "expired" {
			p.Expiry = time.Now().Add(150 * time.Millisecond)
		} else {
			p.Expiry = time.Now().Add(time.Hour)
		}
		ctx, cancel := context.WithTimeout(auth.WithPrincipal(context.Background(), p), 5*time.Second)
		r := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		outlived := ctx.Err() != nil
		cancel()
		body := w.Body.String()
		if outlived {
			t.Fatalf("%s: the stream outlived the session: %s", name, body)
		}
		if !strings.Contains(body, "event: end") || !strings.Contains(body, `"text":"one"`) {
			t.Errorf("%s: body %s", name, body)
		}
		if name == "revoked" && !strings.Contains(body, ": heartbeat") {
			t.Errorf("no heartbeat: %s", body)
		}
	}
}
