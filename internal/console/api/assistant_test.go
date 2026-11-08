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
	"time"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
)

const testGrant = "grant-0123456789abcdef0123456789abcdef"

type fakeGrants struct {
	revoked      []string
	client       auth.Client
	disconnected []string
}

func (f *fakeGrants) DisconnectAll(_ context.Context, c auth.Client, user string) int {
	f.disconnected, f.client = append(f.disconnected, user), c
	return 1
}

func (f *fakeGrants) Grants(_ context.Context, user string) []GrantInfo {
	if user != "alice" {
		return nil
	}
	return []GrantInfo{{
		ID: testGrant, Client: "Claude Code", Scopes: []string{"mcp:read"},
		Created: time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC), Expires: time.Date(2026, 11, 5, 21, 0, 0, 0, time.UTC),
	}}
}

func (f *fakeGrants) RevokeGrant(_ context.Context, c auth.Client, user, id string) error {
	if user != "alice" || id != testGrant {
		return ErrNoSuchGrant
	}
	f.revoked, f.client = append(f.revoked, id), c
	return nil
}

type fakeSetup struct{}

func (fakeSetup) Setup(origin string) AssistantSetupView {
	return AssistantSetupView{MCPURL: origin + "/mcp", ServerName: "pgtoolbox", Access: "read", Clients: []AssistantClientSetup{}}
}
func (fakeSetup) Skill(origin string) string  { return "---\nname: pgtoolbox\n---\n" + origin }
func (fakeSetup) Agents(origin string) string { return "## console " + origin }

func callAs(h http.Handler, p *auth.Principal, method, path string) *httptest.ResponseRecorder {
	ctx := context.Background()
	if p != nil {
		ctx = auth.WithPrincipal(ctx, p)
	}
	r := httptest.NewRequestWithContext(ctx, method, "http://console.example.com"+path, nil)
	r.RemoteAddr = "192.0.2.7:4242"
	r.Header.Set(auth.RequestHeader, "1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestGrantEndpoints(t *testing.T) {
	g := &fakeGrants{}
	h, _ := New(Deps{Source: FixtureSource{}, Authz: FixtureAuthorizer{}, Grants: g, Assistant: fakeSetup{}})
	alice := &auth.Principal{User: authz.User{Name: "alice"}}
	bob := &auth.Principal{User: authz.User{Name: "bob"}}

	w := callAs(h, alice, http.MethodGet, "/api/v1/session/grants")
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	var list GrantList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].ID != testGrant || list.Items[0].LastUsed != nil {
		t.Errorf("list = %+v", list)
	}
	if w := callAs(h, bob, http.MethodGet, "/api/v1/session/grants"); !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Errorf("bob's list: %s", w.Body)
	}
	if w := callAs(h, nil, http.MethodGet, "/api/v1/session/grants"); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous list: %d", w.Code)
	}

	if w := callAs(h, bob, http.MethodDelete, "/api/v1/session/grants/"+testGrant); w.Code != http.StatusNotFound {
		t.Errorf("bob revoking alice's grant: %d", w.Code)
	}
	if w := callAs(h, alice, http.MethodDelete, "/api/v1/session/grants/not-a-grant"); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("malformed ID: %d", w.Code)
	}
	if w := callAs(h, alice, http.MethodDelete, "/api/v1/session/grants/"+testGrant); w.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	if len(g.revoked) != 1 || g.client.IP == "" {
		t.Errorf("revoked %v by %+v", g.revoked, g.client)
	}

	if w := callAs(h, nil, http.MethodDelete, "/api/v1/session/grants"); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous disconnect-all: %d", w.Code)
	}
	if w := callAs(h, bob, http.MethodDelete, "/api/v1/session/grants"); w.Code != http.StatusNoContent {
		t.Fatalf("disconnect-all: %d %s", w.Code, w.Body)
	}
	if len(g.disconnected) != 1 || g.disconnected[0] != "bob" {
		t.Errorf("disconnected %v", g.disconnected)
	}
}

func TestAssistantEndpoints(t *testing.T) {
	alice := &auth.Principal{User: authz.User{Name: "alice"}}
	h, _ := New(Deps{Source: FixtureSource{}, Authz: FixtureAuthorizer{}, Grants: &fakeGrants{}, Assistant: fakeSetup{}})
	w := callAs(h, alice, http.MethodGet, "/api/v1/assistant-setup")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"mcpURL":"http://console.example.com/mcp"`) {
		t.Errorf("setup: %d %s", w.Code, w.Body)
	}
	w = callAs(h, alice, http.MethodGet, "/api/v1/assistant-setup/skill")
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/markdown") || !strings.HasPrefix(w.Body.String(), "---\nname: pgtoolbox") {
		t.Errorf("skill: %d %q %s", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	w = callAs(h, alice, http.MethodGet, "/api/v1/assistant-setup/agents")
	if w.Code != http.StatusOK || w.Body.String() != "## console http://console.example.com" {
		t.Errorf("agents: %d %s", w.Code, w.Body)
	}
	// Both are attachments under the name they are saved as.
	for p, want := range map[string]string{
		SkillPath: `attachment; filename="SKILL.md"`, AgentsPath: `attachment; filename="AGENTS.md"`,
	} {
		if got := callAs(h, alice, http.MethodGet, p).Header().Get("Content-Disposition"); got != want {
			t.Errorf("%s: Content-Disposition = %q, want %q", p, got, want)
		}
	}

	// The configured public URL wins over the request's Host.
	h, _ = New(Deps{Source: FixtureSource{}, Authz: FixtureAuthorizer{}, Assistant: fakeSetup{}, PublicURL: "https://pg.example.org"})
	if w := callAs(h, alice, http.MethodGet, "/api/v1/assistant-setup"); !strings.Contains(w.Body.String(), "https://pg.example.org/mcp") {
		t.Errorf("setup with PUBLIC_URL: %s", w.Body)
	}

	// Without MCP the routes exist (one OpenAPI document) and answer 404.
	h, _ = New(Deps{Source: FixtureSource{}, Authz: FixtureAuthorizer{}})
	for _, p := range []string{"/api/v1/assistant-setup", "/api/v1/assistant-setup/skill", "/api/v1/session/grants"} {
		if w := callAs(h, alice, http.MethodGet, p); w.Code != http.StatusNotFound {
			t.Errorf("%s without MCP: %d", p, w.Code)
		}
	}
	if w := callAs(h, nil, http.MethodGet, "/api/v1/assistant-setup"); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous setup: %d", w.Code)
	}
}
