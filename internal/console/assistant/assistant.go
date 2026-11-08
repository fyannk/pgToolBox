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

// Package assistant renders "Connect an assistant":
// the setup of this console's MCP endpoint for each supported client, and
// the prompt pack (a Claude Code skill and an AGENTS.md fragment)
// generated from the endpoint's tool list.
//
// Each client is one text/template under templates/, whose comment cites
// the client's own documentation of what it reads. The templates are the
// single place a client's format lives; the golden tests parse what they
// render (JSON and TOML), so a template that stops producing a valid file
// fails the build rather than a person's paste.
package assistant

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"

	"github.com/fyannk/pgToolBox/internal/console/api"
)

//go:embed templates/*.tmpl
var templates embed.FS

// Clients are the supported clients, in the order the page offers them.
var Clients = []string{"claude-code", "codex", "opencode", "generic"}

const (
	// DefaultServerName is the name the snippets give the server.
	DefaultServerName = "pgtoolbox"
	// TokenEnv is the environment variable the static-token forms read.
	TokenEnv = "PGTOOLBOX_TOKEN"
	// tokenCommand prints a short-lived token of a ServiceAccount: the
	// break-glass identity of the static-token forms (the person replaces
	// the placeholders).
	tokenCommand = "kubectl -n <namespace> create token <serviceaccount> --duration=1h" // #nosec G101 -- a command line with placeholders
)

// Tool is one MCP tool, as the prompt pack lists it.
type Tool struct {
	Name, Title, Description string
}

// Options configure the renderer.
type Options struct {
	// Tools of the endpoint, in its order (mcp.Endpoint.Tools).
	Tools []Tool
	// WriteTools: the endpoint's operating tools are on.
	WriteTools bool
	// TokenLogin: raw API-server tokens are accepted (AUTH_TOKEN_LOGIN), so
	// the static-token forms are offered.
	TokenLogin bool
	// ServerName overrides DefaultServerName.
	ServerName string
}

// Setup renders the snippets; it implements api.AssistantSetup.
type Setup struct {
	o       Options
	clients map[string]*template.Template
	pack    *template.Template
}

var _ api.AssistantSetup = (*Setup)(nil)

// data is what every template reads.
type data struct {
	Server       string
	URL          string
	Origin       string
	TokenEnv     string
	TokenCommand string
	WriteTools   bool
	Tools        []Tool
}

var funcs = template.FuncMap{
	// q quotes a string for JSON, which is also a valid TOML basic
	// string (TOML 1.0 accepts every JSON escape, \uXXXX included). <, >
	// and & stay as they are: the snippets are pasted, not embedded in
	// HTML.
	"q": func(s string) (string, error) {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(s); err != nil {
			return "", err
		}
		return strings.TrimSuffix(b.String(), "\n"), nil
	},
	"oneLine": func(s string) string { return strings.Join(strings.Fields(s), " ") },
}

// New parses the templates.
func New(o Options) (*Setup, error) {
	if o.ServerName == "" {
		o.ServerName = DefaultServerName
	}
	s := &Setup{o: o, clients: map[string]*template.Template{}}
	for _, c := range Clients {
		t, err := template.New(c).Funcs(funcs).ParseFS(templates, "templates/"+c+".tmpl")
		if err != nil {
			return nil, fmt.Errorf("assistant template %s: %w", c, err)
		}
		s.clients[c] = t
	}
	pack, err := template.New("pack").Funcs(funcs).ParseFS(templates, "templates/body.tmpl", "templates/skill.tmpl", "templates/agents.tmpl")
	if err != nil {
		return nil, fmt.Errorf("assistant prompt pack: %w", err)
	}
	s.pack = pack
	return s, nil
}

func (s *Setup) data(origin string) data {
	origin = strings.TrimRight(origin, "/")
	return data{
		Server: s.o.ServerName, URL: origin + "/mcp", Origin: origin,
		TokenEnv: TokenEnv, TokenCommand: tokenCommand, WriteTools: s.o.WriteTools, Tools: s.o.Tools,
	}
}

// Setup is "Connect an assistant" for a console reached at origin.
func (s *Setup) Setup(origin string) api.AssistantSetupView {
	d := s.data(origin)
	v := api.AssistantSetupView{
		MCPURL: d.URL, ServerName: d.Server, Access: "read", TokenLogin: s.o.TokenLogin,
		Clients:   make([]api.AssistantClientSetup, 0, len(Clients)),
		SkillPath: api.SkillPath, AgentsPath: api.AgentsPath,
	}
	if s.o.WriteTools {
		v.Access = "readOperate"
	}
	for _, c := range Clients {
		cs, err := s.client(c, d)
		if err != nil {
			// The golden tests render every template; an error here is a
			// template bug, not an input: skip the client rather than fail
			// the page.
			continue
		}
		v.Clients = append(v.Clients, cs)
	}
	return v
}

// client renders one client's setup.
func (s *Setup) client(id string, d data) (api.AssistantClientSetup, error) {
	t := s.clients[id]
	var firstErr error
	r := func(name string) string {
		var b bytes.Buffer
		if err := t.ExecuteTemplate(&b, name, d); err != nil && firstErr == nil {
			firstErr = err
		}
		return b.String()
	}
	// opt renders a block a client template may leave out.
	opt := func(name string) string {
		if t.Lookup(name) == nil {
			return ""
		}
		return r(name)
	}
	file := func(prefix string) api.ConfigFile {
		return api.ConfigFile{Path: r(prefix + ".path"), Format: r(prefix + ".format"), Content: r(prefix)}
	}
	cs := api.AssistantClientSetup{
		ID: id, Name: r("name"), Docs: r("docs"), Add: r("add"), Login: r("login"),
		Check: opt("check"), CheckNote: opt("check.note"),
		UserConfig: file("user"),
	}
	switch doc := opt("pack.document"); doc {
	case "skill", "agents":
		p := &api.PromptPackSetup{
			Document: doc, Download: api.SkillPath, FileName: api.SkillFileName,
			ProjectPath: opt("pack.project"), UserPath: opt("pack.user"),
		}
		if doc == "agents" {
			p.Download, p.FileName = api.AgentsPath, api.AgentsFileName
		}
		cs.Pack = p
	case "":
	default:
		if firstErr == nil {
			firstErr = fmt.Errorf("client %s: unknown prompt pack document %q", id, doc)
		}
	}
	if p := file("project"); p.Path != "" {
		cs.ProjectConfig = &p
	}
	if s.o.TokenLogin {
		cs.StaticToken = &api.StaticTokenSetup{Config: file("static"), Env: r("static.env"), Note: r("static.note")}
	}
	return cs, firstErr
}

// Skill is the Claude Code SKILL.md of the prompt pack.
func (s *Setup) Skill(origin string) string { return s.render("skill.tmpl", origin) }

// Agents is the AGENTS.md fragment of the prompt pack.
func (s *Setup) Agents(origin string) string { return s.render("agents.tmpl", origin) }

func (s *Setup) render(name, origin string) string {
	var b bytes.Buffer
	if err := s.pack.ExecuteTemplate(&b, name, s.data(origin)); err != nil {
		return ""
	}
	return b.String()
}
