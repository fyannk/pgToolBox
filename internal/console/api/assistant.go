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
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/fyannk/pgToolBox/internal/console/auth"
)

// The assistants' side of the session page: the
// person's connected assistants (console grants of the MCP authorization
// server) with a Revoke button, and "Connect an assistant": the setup
// snippets per client and the prompt pack.

// Operation IDs of the assistant endpoints.
const (
	OpListGrants        = "listGrants"
	OpRevokeGrant       = "revokeGrant"
	OpRevokeAllGrants   = "revokeAllGrants"
	OpGetAssistantSetup = "getAssistantSetup"
	OpGetAssistantSkill = "getAssistantSkill"
	OpGetAssistantAgent = "getAssistantAgents"
)

// The prompt pack's documents: where the console serves them, and the file
// name each is downloaded and saved under.
const (
	SkillPath      = "/api/" + Version + "/assistant-setup/skill"
	AgentsPath     = "/api/" + Version + "/assistant-setup/agents"
	SkillFileName  = "SKILL.md"
	AgentsFileName = "AGENTS.md"
)

// GrantInfo is one connected assistant as the grant manager reports it.
type GrantInfo struct {
	ID       string
	Client   string
	Scopes   []string
	Created  time.Time
	LastUsed time.Time
	Expires  time.Time
}

// ErrNoSuchGrant is the grant manager's "not one of yours".
var ErrNoSuchGrant = errors.New("no such grant")

// ErrNotRecorded is the grant manager's "revoked on this replica, not yet
// written for the others": answered 503, retry.
var ErrNotRecorded = errors.New("the disconnection could not be recorded for every replica")

// notRecorded is the answer to ErrNotRecorded: 503 with a Retry-After. The
// revocation stays on the replica that answered (it refuses the grant, and
// writes it with its next batch); a retry writes it again.
func notRecorded() error {
	return huma.ErrorWithHeaders(
		huma.Error503ServiceUnavailable("the disconnection was not recorded for every console replica yet (the Kubernetes API server "+
			"did not take the write); retry"),
		http.Header{"Retry-After": {"2"}})
}

// GrantManager lists and revokes a person's grants (mcpoauth.Server,
// adapted in internal/console/server).
type GrantManager interface {
	Grants(ctx context.Context, user string) []GrantInfo
	// RevokeGrant returns ErrNoSuchGrant for a grant that is not the
	// user's, or no longer stands; ErrNotRecorded when the revocation was
	// made on this replica but could not be written for the others.
	RevokeGrant(ctx context.Context, c auth.Client, user, id string) error
	// DisconnectAll revokes every standing grant of the user and returns
	// how many it revoked; ErrNotRecorded, as RevokeGrant, when the
	// revocations could not be written for every replica.
	DisconnectAll(ctx context.Context, c auth.Client, user string) (int, error)
}

// AssistantSetup renders the setup snippets and the prompt pack for a
// console origin (internal/console/assistant).
type AssistantSetup interface {
	Setup(origin string) AssistantSetupView
	Skill(origin string) string
	Agents(origin string) string
}

// GrantView is one connected assistant.
type GrantView struct {
	ID       string     `json:"id" pattern:"^grant-[0-9a-f]{32}$" doc:"The grant's reference: in the audit log (mcp.grant, mcp.call) and accepted by the revocation list's sessions key. Not a credential."`
	Client   string     `json:"client" doc:"The name the application declared when it registered. Unverified."`
	Scopes   []string   `json:"scopes" doc:"What was consented: mcp:read, and mcp:operate when the write tools were on."`
	Created  time.Time  `json:"createdAt" doc:"When you approved it."`
	LastUsed *time.Time `json:"lastUsedAt,omitempty" doc:"Its last use, to within about five minutes."`
	Expires  time.Time  `json:"expiresAt" doc:"When it ends whatever happens (the session's maximum age after the approval)."`
}

// GrantList is the caller's connected assistants.
type GrantList struct {
	Items []GrantView `json:"items" doc:"Standing grants, newest first."`
}

// ConfigFile is one configuration file form of a snippet.
type ConfigFile struct {
	Path    string `json:"path" doc:"Where the file lives, as the client documents it."`
	Format  string `json:"format" enum:"json,toml" doc:"The file's format."`
	Content string `json:"content" doc:"What to put in it (merge into an existing file)."`
}

// StaticTokenSetup is the break-glass form, with a raw API-server token.
type StaticTokenSetup struct {
	Config ConfigFile `json:"config" doc:"The configuration that sends the token."`
	Env    string     `json:"env,omitempty" doc:"The shell line that puts a fresh token in the environment variable the configuration reads, when it reads one."`
	Note   string     `json:"note" doc:"What the form does and when to use it."`
}

// AssistantClientSetup is the setup of one MCP client.
type AssistantClientSetup struct {
	ID            string            `json:"id" enum:"claude-code,codex,opencode,generic" doc:"The client."`
	Name          string            `json:"name" doc:"Display name."`
	Docs          string            `json:"docs" doc:"The client's own documentation of MCP servers."`
	Add           string            `json:"add,omitempty" doc:"The one-line command that adds this console (user scope)."`
	Login         string            `json:"login,omitempty" doc:"The command that signs in, when the client does not do it on first use."`
	UserConfig    ConfigFile        `json:"userConfig" doc:"The user-scope configuration file form."`
	ProjectConfig *ConfigFile       `json:"projectConfig,omitempty" doc:"The project-scope (repository) configuration file form."`
	StaticToken   *StaticTokenSetup `json:"staticToken,omitempty" doc:"The static-token form; present only while token sign-in is on."`
	Check         string            `json:"check,omitempty" doc:"The command that shows whether the client reaches the console, when the client has one."`
	CheckNote     string            `json:"checkNote,omitempty" doc:"How to check it in words, for a client without a check command."`
	Pack          *PromptPackSetup  `json:"pack,omitempty" doc:"Where this client reads the prompt pack, when it reads one."`
}

// PromptPackSetup is where one client reads the prompt pack.
type PromptPackSetup struct {
	Document    string `json:"document" enum:"skill,agents" doc:"Which document: the Claude Code skill (SKILL.md) or the AGENTS.md section."`
	Download    string `json:"download" doc:"The console path that serves it (an attachment)."`
	FileName    string `json:"fileName" doc:"The file name it is served and saved under."`
	ProjectPath string `json:"projectPath,omitempty" doc:"Where it goes in a repository (merge into an existing AGENTS.md)."`
	UserPath    string `json:"userPath,omitempty" doc:"Where it goes for every project of the person, when the client has such a place."`
}

// AssistantSetupView is "Connect an assistant".
type AssistantSetupView struct {
	MCPURL     string                 `json:"mcpURL" doc:"This console's MCP endpoint."`
	ServerName string                 `json:"serverName" doc:"The name the snippets give the server in each client."`
	Access     string                 `json:"access" enum:"read,readOperate" doc:"What a grant may give: read-only; or, with the write tools on, read and operate for a grant whose user chose it on the consent page (which proposes read-only first)."`
	TokenLogin bool                   `json:"tokenLogin" doc:"Token sign-in is on, so raw API-server tokens are accepted too (break-glass)."`
	Clients    []AssistantClientSetup `json:"clients" doc:"One entry per client."`
	SkillPath  string                 `json:"skillPath" doc:"The Claude Code skill (SKILL.md) of the prompt pack."`
	AgentsPath string                 `json:"agentsPath" doc:"The AGENTS.md fragment of the prompt pack (Codex, OpenCode)."`
}

type (
	grantListOutput struct{ Body GrantList }
	grantPath       struct {
		ID string `path:"id" pattern:"^grant-[0-9a-f]{32}$" doc:"The grant's reference."`
	}
	assistantSetupOutput struct{ Body AssistantSetupView }
	markdownOutput       struct {
		ContentType        string `header:"Content-Type"`
		ContentDisposition string `header:"Content-Disposition"`
		Body               []byte
	}
)

type requestKey struct{}

// withRequest keeps the HTTP request in the operation's context for the
// handlers that need its origin or client address.
func withRequest(ctx huma.Context, next func(huma.Context)) {
	r, _ := humago.Unwrap(ctx)
	next(huma.WithValue(ctx, requestKey{}, r))
}

func requestFrom(ctx context.Context) *http.Request {
	r, _ := ctx.Value(requestKey{}).(*http.Request)
	return r
}

// clientOf is the calling client for the audit record.
func clientOf(ctx context.Context, d Deps) auth.Client {
	r := requestFrom(ctx)
	if r == nil {
		return auth.Client{}
	}
	if d.ClientOf != nil {
		return d.ClientOf(r)
	}
	return auth.Client{IP: r.RemoteAddr, UserAgent: r.UserAgent()}
}

// registerAssistant adds the grant and setup operations.
func registerAssistant(a huma.API, d Deps) {
	notServed := func() error {
		return huma.Error404NotFound("this console does not serve MCP (MCP_ENABLED is off)")
	}
	huma.Register(a, huma.Operation{
		OperationID: OpListGrants, Method: http.MethodGet, Path: "/api/" + Version + "/session/grants",
		Summary: "Your connected assistants", Tags: []string{"session"},
		Description: "The MCP clients you approved on this console (console grants), newest first. Each acts as you; revoke one to disconnect it on every replica.",
		Errors:      []int{http.StatusUnauthorized, http.StatusNotFound},
	}, func(ctx context.Context, _ *struct{}) (*grantListOutput, error) {
		p, err := principal(ctx)
		if err != nil {
			return nil, err
		}
		if d.Grants == nil {
			return nil, notServed()
		}
		out := GrantList{Items: []GrantView{}}
		for _, g := range d.Grants.Grants(ctx, p.User.Name) {
			v := GrantView{ID: g.ID, Client: g.Client, Scopes: g.Scopes, Created: g.Created.UTC(), Expires: g.Expires.UTC()}
			if v.Scopes == nil {
				v.Scopes = []string{}
			}
			if !g.LastUsed.IsZero() {
				t := g.LastUsed.UTC()
				v.LastUsed = &t
			}
			out.Items = append(out.Items, v)
		}
		return &grantListOutput{Body: out}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: OpRevokeGrant, Method: http.MethodDelete, Path: "/api/" + Version + "/session/grants/{id}",
		Summary: "Disconnect an assistant", Tags: []string{"session"},
		Description: "Revokes one of your grants: every replica refuses its tokens within about a second. Recorded as mcp.revoke. " +
			"503 (with Retry-After) when the revocation could not be written for the other replicas: this one refuses the grant, retry.",
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable},
	}, func(ctx context.Context, in *grantPath) (*struct{}, error) {
		p, err := principal(ctx)
		if err != nil {
			return nil, err
		}
		if d.Grants == nil {
			return nil, notServed()
		}
		if err := d.Grants.RevokeGrant(ctx, clientOf(ctx, d), p.User.Name, in.ID); err != nil {
			switch {
			case errors.Is(err, ErrNoSuchGrant):
				return nil, huma.Error404NotFound("no such grant of yours")
			case errors.Is(err, ErrNotRecorded):
				return nil, notRecorded()
			}
			return nil, err
		}
		return nil, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: OpRevokeAllGrants, Method: http.MethodDelete, Path: "/api/" + Version + "/session/grants",
		Summary: "Disconnect every assistant", Tags: []string{"session"},
		Description: "Revokes all your grants on this console: every replica refuses their tokens within about a second. Each is recorded as mcp.revoke. Your browser session is untouched (sign out with the disconnect option to end both). " +
			"503 (with Retry-After) when the revocations could not be written for the other replicas: this one refuses the grants, retry.",
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable},
	}, func(ctx context.Context, _ *struct{}) (*struct{}, error) {
		p, err := principal(ctx)
		if err != nil {
			return nil, err
		}
		if d.Grants == nil {
			return nil, notServed()
		}
		if _, err := d.Grants.DisconnectAll(ctx, clientOf(ctx, d), p.User.Name); err != nil {
			if errors.Is(err, ErrNotRecorded) {
				return nil, notRecorded()
			}
			return nil, err
		}
		return nil, nil
	})

	origin := func(ctx context.Context) string {
		if d.PublicURL != "" {
			return d.PublicURL
		}
		if r := requestFrom(ctx); r != nil {
			return auth.ExpectedOrigin(r, "")
		}
		return ""
	}
	huma.Register(a, huma.Operation{
		OperationID: OpGetAssistantSetup, Method: http.MethodGet, Path: "/api/" + Version + "/assistant-setup",
		Summary: "Connect an assistant", Tags: []string{"assistant"},
		Description: "Ready-to-paste setup of this console's MCP endpoint for Claude Code, Codex CLI, OpenCode and other MCP clients: " +
			"the one-line add command, the configuration files, the static-token form when token sign-in is on, and the check command.",
		Errors: []int{http.StatusUnauthorized, http.StatusNotFound},
	}, func(ctx context.Context, _ *struct{}) (*assistantSetupOutput, error) {
		if _, err := principal(ctx); err != nil {
			return nil, err
		}
		if d.Assistant == nil {
			return nil, notServed()
		}
		return &assistantSetupOutput{Body: d.Assistant.Setup(origin(ctx))}, nil
	})
	for _, md := range []struct {
		op, path, file, summary, desc string
		render                        func(string) string
	}{
		{
			OpGetAssistantSkill, SkillPath, SkillFileName, "The Claude Code skill of the prompt pack",
			"SKILL.md for .claude/skills/pgtoolbox/: what the tools are, that log and statement text is untrusted, and to cite the console page of each finding. Generated from the tool list.",
			func(o string) string { return d.Assistant.Skill(o) },
		},
		{
			OpGetAssistantAgent, AgentsPath, AgentsFileName, "The AGENTS.md fragment of the prompt pack",
			"An AGENTS.md section for Codex and OpenCode with the same content as the skill.",
			func(o string) string { return d.Assistant.Agents(o) },
		},
	} {
		huma.Register(a, huma.Operation{
			OperationID: md.op, Method: http.MethodGet, Path: md.path,
			Summary: md.summary, Tags: []string{"assistant"}, Description: md.desc,
			Errors: []int{http.StatusUnauthorized, http.StatusNotFound},
			Responses: map[string]*huma.Response{"200": {
				Description: "Markdown",
				Content:     map[string]*huma.MediaType{"text/markdown": {Schema: &huma.Schema{Type: "string"}}},
			}},
		}, func(ctx context.Context, _ *struct{}) (*markdownOutput, error) {
			if _, err := principal(ctx); err != nil {
				return nil, err
			}
			if d.Assistant == nil {
				return nil, notServed()
			}
			return &markdownOutput{
				ContentType: "text/markdown; charset=utf-8", ContentDisposition: `attachment; filename="` + md.file + `"`,
				Body: []byte(md.render(origin(ctx))),
			}, nil
		})
	}
}
