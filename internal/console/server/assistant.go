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

package server

import (
	"context"
	"errors"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/api"
	"github.com/fyannk/pgToolBox/internal/console/assistant"
	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/mcp"
	"github.com/fyannk/pgToolBox/internal/console/mcpauth"
	"github.com/fyannk/pgToolBox/internal/console/mcpoauth"
	"github.com/fyannk/pgToolBox/internal/console/sharedstate"
)

// GrantIndex is the index of MCP grants and of the single-use values
// (consent answers, codes, the write tools' confirmations, which the
// confirmation page marks and the tool then reads: CodeUsed).
type GrantIndex interface {
	mcpoauth.GrantStore
	CodeUsed(code string) bool
	CodeMarked(code, mark string) bool
	SpendCode(ctx context.Context, code, userHash string, until time.Time) error
	MarkCode(ctx context.Context, code, mark, userHash string) error
}

// grantIndex is the shared index when STATE_CONFIGMAP is set, else this
// replica's memory.
func grantIndex(o AppOptions) GrantIndex {
	if o.Grants != nil {
		return o.Grants
	}
	o.Log.Info("MCP grants and confirmations are kept in this replica's memory (STATE_CONFIGMAP is not set): " +
		"a restart disconnects every assistant, and with more than one replica each refuses the others' grants")
	return sharedstate.New(sharedstate.Options{Logger: o.Log})
}

// newOAuthServer builds the console's authorization server for MCP
// clients over the sign-in layer and the grant index.
// A public URL that cannot be an issuer turns it off (nil, no error): the
// MCP endpoint then accepts raw API-server tokens only.
func newOAuthServer(o AppOptions, h *auth.Handler, store mcpoauth.GrantStore) (*mcpoauth.Server, error) {
	s, err := mcpoauth.New(mcpoauth.Options{
		Sessions: h, Store: store, PublicURL: o.Auth.PublicURL, TrustedProxy: o.Auth.TrustedProxy,
		WriteTools: o.MCP.WriteTools, AccessTTL: o.MCP.AccessTTL, ClientMetadataDocuments: o.MCP.ClientMetadataDocuments,
		ClientMetadataHosts: o.MCP.ClientMetadataHosts, Log: o.Log,
	})
	if errors.Is(err, mcpoauth.ErrIssuer) {
		o.Log.Warn("MCP clients cannot sign in through the console: " + err.Error() + "; the MCP endpoint accepts raw API-server tokens only")
		return nil, nil
	}
	return s, err
}

// mcpAuthenticator is the MCP guard's chain: console grants first (when
// the authorization server is on), then raw API-server tokens.
func mcpAuthenticator(s *mcpoauth.Server, h *auth.Handler) mcpauth.Authenticator {
	if s == nil {
		return mcpauth.Chain{mcpauth.RawToken{Checker: h}}
	}
	return mcpauth.Chain{s, mcpauth.RawToken{Checker: h}}
}

// mcpOAuth is the endpoint's authorization server: a nil *Server must
// not become a non-nil interface.
func mcpOAuth(s *mcpoauth.Server) mcp.OAuthServer {
	if s == nil {
		return nil
	}
	return s
}

// grantManager adapts the authorization server to the API's session page.
type grantManager struct{ s *mcpoauth.Server }

func (g grantManager) Grants(ctx context.Context, user string) []api.GrantInfo {
	gs := g.s.Grants(ctx, user)
	out := make([]api.GrantInfo, 0, len(gs))
	for _, x := range gs {
		out = append(out, api.GrantInfo(x))
	}
	return out
}

func (g grantManager) DisconnectAll(ctx context.Context, c auth.Client, user string) int {
	return g.s.DisconnectAll(ctx, c, user)
}

func (g grantManager) RevokeGrant(ctx context.Context, c auth.Client, user, id string) error {
	err := g.s.RevokeGrant(ctx, c, user, id)
	if errors.Is(err, mcpoauth.ErrNoSuchGrant) {
		return api.ErrNoSuchGrant
	}
	return err
}

// assistantSetup is "Connect an assistant", completed once the MCP
// endpoint (whose tool list it renders) exists: the API, built first,
// holds it from the start.
type assistantSetup struct{ s *assistant.Setup }

func (a *assistantSetup) init(e *mcp.Endpoint, tokenLogin bool) error {
	tools := make([]assistant.Tool, 0, len(e.Tools()))
	for _, t := range e.Tools() {
		tools = append(tools, assistant.Tool(t))
	}
	s, err := assistant.New(assistant.Options{Tools: tools, WriteTools: e.WriteTools(), TokenLogin: tokenLogin})
	if err != nil {
		return err
	}
	a.s = s
	return nil
}

func (a *assistantSetup) Setup(origin string) api.AssistantSetupView { return a.s.Setup(origin) }
func (a *assistantSetup) Skill(origin string) string                 { return a.s.Skill(origin) }
func (a *assistantSetup) Agents(origin string) string                { return a.s.Agents(origin) }
