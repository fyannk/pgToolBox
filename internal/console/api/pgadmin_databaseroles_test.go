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
	"testing"

	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/ops"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// roleAuthorizer is the fixture authorizer with create databaseroles
// denied or undecided.
type roleAuthorizer struct {
	FixtureAuthorizer
	deny, undecided bool
	asked           *[]authz.Attributes
}

func (a roleAuthorizer) Allowed(ctx context.Context, u authz.User, at authz.Attributes) authz.Decision {
	if at.Resource == "databaseroles" {
		*a.asked = append(*a.asked, at)
		switch {
		case a.undecided:
			return authz.Decision{Err: context.DeadlineExceeded}
		case a.deny:
			return authz.Decision{}
		}
	}
	return a.FixtureAuthorizer.Allowed(ctx, u, at)
}

type spyCreator struct {
	got []ops.PersonalRoleRequest
	err error
}

func (s *spyCreator) CreatePersonalDatabaseRole(_ context.Context, req ops.PersonalRoleRequest) (ops.PersonalRole, error) {
	s.got = append(s.got, req)
	if s.err != nil {
		return ops.PersonalRole{}, s.err
	}
	return ops.PersonalRole{Name: ops.PersonalRoleName(req.Cluster, req.Role), PasswordSecret: ops.PersonalRoleName(req.Cluster, req.Role) + "-password"}, nil
}

func TestCreateDatabaseRole(t *testing.T) {
	t.Parallel()
	billing := authz.ObjectRef{Namespace: "team-a", Name: "billing"}
	cases := []struct {
		name      string
		az        roleAuthorizer
		creator   *spyCreator
		nilWriter bool
		body      string
		status    int
		created   int
		want      string
	}{
		{name: "created for the caller", body: `{"role":"alice_ro","inRoles":["reporting","reporting"]}`, status: http.StatusCreated, created: 1},
		{name: "created for a named pgAdmin user", body: `{"role":"alice_ro","pgAdminUser":"alice@example.com"}`, status: http.StatusCreated, created: 1},
		{
			name: "may not get the Cluster", az: roleAuthorizer{FixtureAuthorizer: FixtureAuthorizer{Denied: map[authz.ObjectRef]bool{billing: true}}},
			body: `{"role":"alice_ro"}`, status: http.StatusForbidden,
		},
		{name: "may not create databaseroles", az: roleAuthorizer{deny: true}, body: `{"role":"alice_ro"}`, status: http.StatusForbidden, want: "pgtoolbox-console-operator"},
		{name: "API server unreachable", az: roleAuthorizer{undecided: true}, body: `{"role":"alice_ro"}`, status: http.StatusServiceUnavailable},
		{name: "reserved role", body: `{"role":"pg_monitor"}`, status: http.StatusUnprocessableEntity},
		{name: "superuser name", body: `{"role":"postgres"}`, status: http.StatusUnprocessableEntity},
		{name: "upper-case role", body: `{"role":"Alice"}`, status: http.StatusUnprocessableEntity},
		{name: "quoted pgAdmin user", body: `{"role":"alice","pgAdminUser":"a'b"}`, status: http.StatusUnprocessableEntity},
		{name: "member role with a quote", body: `{"role":"alice","inRoles":["x\"y"]}`, status: http.StatusUnprocessableEntity},
		{
			name: "CloudNativePG before 1.31", creator: &spyCreator{err: &ops.WriteError{Status: 501, Text: "not available on this CloudNativePG minor"}},
			body: `{"role":"alice"}`, status: http.StatusNotImplemented, created: 1, want: "not available on this CloudNativePG minor",
		},
		{name: "exists", creator: &spyCreator{err: &ops.WriteError{Status: 409, Text: "already exists"}}, body: `{"role":"alice"}`, status: http.StatusConflict, created: 1},
		{name: "not served", nilWriter: true, body: `{"role":"alice"}`, status: http.StatusNotImplemented},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var asked []authz.Attributes
			tc.az.asked = &asked
			creator := tc.creator
			if creator == nil {
				creator = &spyCreator{}
			}
			mux := http.NewServeMux()
			a := humago.New(mux, Config())
			var writer DatabaseRoleCreator = creator
			if tc.nilWriter {
				writer = nil
			}
			RegisterPgAdminDatabaseRoles(a, Deps{Source: FixtureSource{}, Authz: tc.az}, writer)

			principal := FixturePrincipal()
			ctx := auth.WithPrincipal(context.Background(), principal)
			r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/namespaces/team-a/clusters/billing/database-roles", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d (%s), want %d", w.Code, w.Body, tc.status)
			}
			if len(creator.got) != tc.created {
				t.Fatalf("%d writes, want %d", len(creator.got), tc.created)
			}
			if tc.want != "" && !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("body %s lacks %q", w.Body, tc.want)
			}
			if tc.status != http.StatusCreated {
				return
			}
			if len(asked) != 1 || asked[0].Verb != "create" || asked[0].Group != "postgresql.cnpg.io" || asked[0].Namespace != "team-a" {
				t.Fatalf("SubjectAccessReviews %+v", asked)
			}
			got := creator.got[0]
			wantUser := principal.User.Name
			if strings.Contains(tc.body, "pgAdminUser") {
				wantUser = "alice@example.com"
			}
			if got.PgAdminUser != wantUser || got.CreatedBy != principal.User.Name || got.Cluster != "billing" || got.Namespace != "team-a" {
				t.Fatalf("request %+v", got)
			}
			if strings.Contains(tc.body, "inRoles") && len(got.InRoles) != 1 {
				t.Fatalf("inRoles must be deduplicated: %v", got.InRoles)
			}
			var view viewmodel.PersonalDatabaseRole
			if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			if view.Name != "billing-alice-ro" || view.PasswordSecret != "billing-alice-ro-password" || strings.Contains(w.Body.String(), "password\":\"") {
				t.Fatalf("view %+v", view)
			}
		})
	}
}
