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

package assistant

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/fyannk/pgToolBox/internal/console/api"
)

var update = flag.Bool("update", false, "rewrite the golden files")

const origin = "https://console.example.com/"

var tools = []Tool{
	{Name: "list_clusters", Title: "Clusters you may see", Description: "The fleet table:\n  every Cluster you may read."},
	{Name: "cluster_status", Title: "A Cluster's status", Description: "The overview of one Cluster."},
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) // #nosec G304 -- test fixture path
	if err != nil {
		t.Fatalf("%v (run go test -update)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from the golden file (run go test -update to accept):\n%s", name, got)
	}
}

// parse decodes a configuration file form by its declared format.
func parse(t *testing.T, f api.ConfigFile) map[string]any {
	t.Helper()
	out := map[string]any{}
	switch f.Format {
	case "json":
		if err := json.Unmarshal([]byte(f.Content), &out); err != nil {
			t.Fatalf("%s is not JSON: %v\n%s", f.Path, err, f.Content)
		}
	case "toml":
		if _, err := toml.Decode(f.Content, &out); err != nil {
			t.Fatalf("%s is not TOML: %v\n%s", f.Path, err, f.Content)
		}
	default:
		t.Fatalf("%s: unknown format %q", f.Path, f.Format)
	}
	return out
}

// dig walks nested objects.
func dig(t *testing.T, m map[string]any, keys ...string) map[string]any {
	t.Helper()
	cur := m
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			t.Fatalf("no object at %v in %v", keys, m)
		}
		cur = next
	}
	return cur
}

// server finds the server entry of a parsed configuration, wherever the
// client keeps it.
func server(t *testing.T, id string, f api.ConfigFile, m map[string]any) map[string]any {
	t.Helper()
	switch {
	case id == "codex":
		return dig(t, m, "mcp_servers", DefaultServerName)
	case id == "opencode":
		if m["$schema"] != "https://opencode.ai/config.json" {
			t.Errorf("opencode: $schema = %v", m["$schema"])
		}
		return dig(t, m, "mcp", DefaultServerName)
	case f.Path == ".vscode/mcp.json":
		return dig(t, m, "servers", DefaultServerName)
	}
	return dig(t, m, "mcpServers", DefaultServerName)
}

func TestSetupRendersParsableConfigurations(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tokenLogin bool
		write      bool
	}{
		{"oauth-only", false, false},
		{"token-login", true, false},
		{"operate", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := New(Options{Tools: tools, TokenLogin: tc.tokenLogin, WriteTools: tc.write})
			if err != nil {
				t.Fatal(err)
			}
			v := s.Setup(origin)
			if v.MCPURL != "https://console.example.com/mcp" {
				t.Errorf("MCPURL = %q", v.MCPURL)
			}
			if want := map[bool]string{false: "read", true: "readOperate"}[tc.write]; v.Access != want {
				t.Errorf("Access = %q, want %q", v.Access, want)
			}
			if len(v.Clients) != len(Clients) {
				t.Fatalf("%d clients rendered, want %d", len(v.Clients), len(Clients))
			}
			for _, c := range v.Clients {
				if c.Name == "" || !strings.HasPrefix(c.Docs, "https://") {
					t.Errorf("%s: name or docs missing: %+v", c.ID, c)
				}
				// Exactly one of a check command (copied and run) and a check
				// in words (read): the page shows them differently.
				if (c.Check == "") == (c.CheckNote == "") {
					t.Errorf("%s: want exactly one of check %q and checkNote %q", c.ID, c.Check, c.CheckNote)
				}
				if strings.Contains(c.Check, ". ") || strings.HasSuffix(c.Check, ".") {
					t.Errorf("%s: check %q reads as prose, not a command", c.ID, c.Check)
				}
				// The prompt pack: every client reads one, from the console's
				// own path, under the file name it is served with.
				switch p := c.Pack; {
				case p == nil:
					t.Errorf("%s: no prompt pack", c.ID)
				case p.Document == "skill" && (p.Download != api.SkillPath || p.FileName != api.SkillFileName):
					t.Errorf("%s: skill pack %+v", c.ID, p)
				case p.Document == "agents" && (p.Download != api.AgentsPath || p.FileName != api.AgentsFileName):
					t.Errorf("%s: agents pack %+v", c.ID, p)
				case p.ProjectPath == "" || !strings.HasSuffix(p.ProjectPath, p.FileName):
					t.Errorf("%s: project path %q does not name %s", c.ID, p.ProjectPath, p.FileName)
				case p.UserPath != "" && !strings.HasSuffix(p.UserPath, p.FileName):
					t.Errorf("%s: user path %q does not name %s", c.ID, p.UserPath, p.FileName)
				}
				files := []api.ConfigFile{c.UserConfig}
				if c.ProjectConfig != nil {
					files = append(files, *c.ProjectConfig)
				}
				for _, f := range files {
					e := server(t, c.ID, f, parse(t, f))
					if e["url"] != v.MCPURL {
						t.Errorf("%s %s: url = %v", c.ID, f.Path, e["url"])
					}
					if _, ok := e["headers"]; ok {
						t.Errorf("%s %s: the sign-in form sends a header", c.ID, f.Path)
					}
				}
				if (c.StaticToken != nil) != tc.tokenLogin {
					t.Fatalf("%s: static token form offered = %v with token sign-in %v", c.ID, c.StaticToken != nil, tc.tokenLogin)
				}
				if c.StaticToken != nil {
					checkStatic(t, c)
				}
			}
			b, err := json.MarshalIndent(v, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden(t, "setup-"+tc.name+".json", string(b)+"\n")
		})
	}
}

// checkStatic verifies each static-token form sends the token the way its
// client documents.
func checkStatic(t *testing.T, c api.AssistantClientSetup) {
	t.Helper()
	f := c.StaticToken.Config
	e := server(t, c.ID, f, parse(t, f))
	switch c.ID {
	case "claude-code":
		h, _ := e["headersHelper"].(string)
		if !strings.Contains(h, `"Authorization": "Bearer %s"`) || !strings.Contains(h, "create token") {
			t.Errorf("claude-code headersHelper = %q", h)
		}
	case "codex":
		if e["bearer_token_env_var"] != TokenEnv {
			t.Errorf("codex bearer_token_env_var = %v", e["bearer_token_env_var"])
		}
	case "opencode":
		if e["oauth"] != false || dig(t, e, "headers")["Authorization"] != "Bearer {env:"+TokenEnv+"}" {
			t.Errorf("opencode static form = %v", e)
		}
	case "generic":
		if dig(t, e, "headers")["Authorization"] != "Bearer ${env:"+TokenEnv+"}" {
			t.Errorf("generic static form = %v", e)
		}
	}
	if c.ID != "claude-code" && !strings.HasPrefix(c.StaticToken.Env, "export "+TokenEnv+"=") {
		t.Errorf("%s: env line = %q", c.ID, c.StaticToken.Env)
	}
	if c.StaticToken.Note == "" {
		t.Errorf("%s: no note", c.ID)
	}
}

func TestQuotingSurvivesHostileServerNames(t *testing.T) {
	s, err := New(Options{ServerName: "a\"b\\c]d\n"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range s.Setup("https://x.example").Clients {
		m := parse(t, c.UserConfig)
		if len(m) == 0 {
			t.Errorf("%s: nothing parsed", c.ID)
		}
	}
}

func TestPromptPack(t *testing.T) {
	for _, write := range []bool{false, true} {
		s, err := New(Options{Tools: tools, WriteTools: write})
		if err != nil {
			t.Fatal(err)
		}
		skill, agents := s.Skill(origin), s.Agents(origin)
		for _, want := range []string{"`list_clusters`: Clusters you may see. The fleet table: every Cluster you may read.", "https://console.example.com/c/<namespace>/<name>", `{"untrusted": "..."}`} {
			if !strings.Contains(skill, want) || !strings.Contains(agents, want) {
				t.Errorf("the prompt pack lacks %q", want)
			}
		}
		// The skill's front matter is what Claude Code reads first.
		head, _, ok := strings.Cut(strings.TrimPrefix(skill, "---\n"), "\n---\n")
		if !strings.HasPrefix(skill, "---\nname: pgtoolbox\n") || !ok || !strings.Contains(head, "description: ") {
			t.Errorf("skill front matter:\n%s", skill)
		}
		if !strings.HasPrefix(agents, "## ") {
			t.Errorf("AGENTS.md fragment does not start with a section:\n%s", agents)
		}
		suffix := map[bool]string{false: "read", true: "operate"}[write]
		golden(t, "skill-"+suffix+".md", skill)
		golden(t, "agents-"+suffix+".md", agents)
	}
}
