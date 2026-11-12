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

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel/fixtures"
)

// Fixture grant references: one per connected assistant of the fixture user.
const (
	FixtureGrantClaude = "grant-0c1d2e3f405162738495a6b7c8d9eaf0"
	FixtureGrantCodex  = "grant-1f2e3d4c5b6a79881726354453627180"
	FixtureGrantOther  = "grant-9a8b7c6d5e4f30211203f4e5d6c7b8a9"
)

// FixtureGrants is the grant manager of the fixtures: the fixture user has
// three connected assistants (one consented with the write tools on, one
// never used yet); revoking any other reference is ErrNoSuchGrant.
type FixtureGrants struct {
	// None: the user has no connected assistant.
	None bool
}

// Grants implements GrantManager.
func (f FixtureGrants) Grants(context.Context, string) []GrantInfo {
	if f.None {
		return nil
	}
	now := fixtures.Now
	return []GrantInfo{
		{
			ID: FixtureGrantClaude, Client: "Claude Code", Scopes: []string{"mcp:read", "mcp:operate"},
			Created: now.Add(-26 * time.Hour), LastUsed: now.Add(-4 * time.Minute), Expires: now.Add(7*24*time.Hour - 26*time.Hour),
		},
		{
			ID: FixtureGrantCodex, Client: "codex-mcp-client", Scopes: []string{"mcp:read"},
			Created: now.Add(-3 * 24 * time.Hour), LastUsed: now.Add(-2 * 24 * time.Hour), Expires: now.Add(4 * 24 * time.Hour),
		},
		{
			ID: FixtureGrantOther, Client: "opencode", Scopes: []string{"mcp:read"},
			Created: now.Add(-20 * time.Minute), Expires: now.Add(7*24*time.Hour - 20*time.Minute),
		},
	}
}

// RevokeGrant implements GrantManager.
func (f FixtureGrants) RevokeGrant(ctx context.Context, _ auth.Client, _, id string) error {
	if f.None {
		return ErrNoSuchGrant
	}
	for _, g := range f.Grants(ctx, "") {
		if g.ID == id {
			return nil
		}
	}
	return ErrNoSuchGrant
}

// DisconnectAll implements GrantManager.
func (f FixtureGrants) DisconnectAll(ctx context.Context, _ auth.Client, user string) (int, error) {
	return len(f.Grants(ctx, user)), nil
}

type assistantFixtureCase struct {
	file, op, method, path, scenario string
	// served: MCP is on (the grant manager is wired).
	served bool
	grants FixtureGrants
}

// The setup snippets themselves (getAssistantSetup 200, the prompt pack)
// are the golden files of internal/console/assistant, rendered by the real
// templates; the UI copies them from there (ui/scripts/sync-fixtures.mjs).
func assistantFixtureCases() []assistantFixtureCase {
	base := "/api/" + Version
	return []assistantFixtureCase{
		{"listGrants.json", OpListGrants, http.MethodGet, base + "/session/grants", "three connected assistants, one never used, one with the write tools", true, FixtureGrants{}},
		{"listGrants.empty.json", OpListGrants, http.MethodGet, base + "/session/grants", "no connected assistant", true, FixtureGrants{None: true}},
		{"listGrants.notServed.json", OpListGrants, http.MethodGet, base + "/session/grants", "404: MCP is off on this console", false, FixtureGrants{}},
		{"revokeGrant.notFound.json", OpRevokeGrant, http.MethodDelete, base + "/session/grants/grant-00000000000000000000000000000000", "404: not a grant of yours (or already revoked)", true, FixtureGrants{}},
		{"revokeAllGrants.notServed.json", OpRevokeAllGrants, http.MethodDelete, base + "/session/grants", "404: MCP is off on this console", false, FixtureGrants{}},
		{"getAssistantSetup.notServed.json", OpGetAssistantSetup, http.MethodGet, base + "/assistant-setup", "404: MCP is off on this console", false, FixtureGrants{}},
		{"getSession.mcpOff.json", OpGetSession, http.MethodGet, base + "/session", "the session of a console that does not serve MCP (features.mcp false)", false, FixtureGrants{}},
	}
}

func generateAssistantFixtures(out map[string][]byte) ([]Fixture, error) {
	var manifest []Fixture
	for _, c := range assistantFixtureCases() {
		d := Deps{Source: FixtureSource{}, Authz: FixtureAuthorizer{}, Features: FixtureFeatures}
		if c.served {
			d.Grants = c.grants
		} else {
			// What internal/console/server reports without MCP.
			d.Features.MCP, d.Features.MCPWriteTools = false, false
		}
		h, _ := New(d)
		code, body, err := serveFixture(h, c.method, c.path, "", c.method != http.MethodGet)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.file, err)
		}
		out["testdata/fixtures/"+c.file] = body
		manifest = append(manifest, Fixture{File: c.file, OperationID: c.op, Method: c.method, Path: c.path, Status: code, Scenario: c.scenario})
	}
	return manifest, nil
}
