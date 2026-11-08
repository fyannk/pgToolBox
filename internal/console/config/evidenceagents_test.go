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

package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func agentsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agents.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConsoleStaticAgents(t *testing.T) {
	json := `[{"namespace":"team-a","cluster":"orders","url":"http://orders-evidence.team-a.svc:8443","tokenFile":"/var/run/pgtools/evidence/token"},
	{"namespace":"team-b","cluster":"billing","url":"https://billing-evidence.team-b.svc:8443","tokenFile":"/var/run/pgtools/evidence/token","caFile":"/var/run/pgtools/evidence-ca/billing/ca.crt"}]`
	yaml := `
- namespace: team-a
  cluster: orders
  url: http://orders-evidence.team-a.svc:8443
  tokenFile: /var/run/pgtools/evidence/token
- namespace: team-b
  cluster: billing
  url: https://billing-evidence.team-b.svc:8443
  tokenFile: /var/run/pgtools/evidence/token
  caFile: /var/run/pgtools/evidence-ca/billing/ca.crt
`
	want := []StaticAgent{
		{Namespace: "team-a", Cluster: "orders", URL: "http://orders-evidence.team-a.svc:8443", TokenFile: "/var/run/pgtools/evidence/token"},
		{Namespace: "team-b", Cluster: "billing", URL: "https://billing-evidence.team-b.svc:8443", TokenFile: "/var/run/pgtools/evidence/token", CAFile: "/var/run/pgtools/evidence-ca/billing/ca.crt"},
	}
	for name, content := range map[string]string{"json": json, "yaml": yaml} {
		t.Run(name, func(t *testing.T) {
			path := agentsFile(t, content)
			cfg, err := LoadConsole(lookupOf(map[string]string{EnvWatchNamespaces: "team-a,team-b", EnvEvidenceAgentsFile: path}))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Evidence.Agents, want) || cfg.Evidence.AgentsFile != path || cfg.Info()[EnvEvidenceAgentsFile] != path {
				t.Fatalf("agents = %+v", cfg.Evidence.Agents)
			}
		})
	}
	// Evidence off: the file is not read at all.
	cfg, err := LoadConsole(lookupOf(map[string]string{EnvWatchNamespaces: "team-a", EnvEvidenceEnabled: "false", EnvEvidenceAgentsFile: "/absent"}))
	if err != nil || cfg.Evidence.Agents != nil {
		t.Fatalf("evidence disabled: %+v, %v", cfg.Evidence, err)
	}
}

func TestLoadConsoleRejectsStaticAgents(t *testing.T) {
	entry := func(fields string) string {
		return `[{"namespace":"team-a","cluster":"orders","tokenFile":"/t"` + fields + `}]`
	}
	tests := map[string]struct {
		content string
		path    string
		want    string
	}{
		"relative path":       {path: "agents.json", want: "absolute, clean path"},
		"missing file":        {path: "/nonexistent/agents.json", want: "readable file"},
		"not a list":          {content: `{"namespace":"team-a"}`, want: "JSON or YAML list"},
		"unknown key":         {content: entry(`,"url":"http://a.team-a.svc","caBundle":"/x"`), want: "without other keys"},
		"empty list":          {content: `[]`, want: "between 1 and"},
		"unwatched namespace": {content: `[{"namespace":"team-z","cluster":"orders","url":"http://a.team-z.svc","tokenFile":"/t"}]`, want: "entry 0: namespace must be one of"},
		"foreign url":         {content: entry(`,"url":"http://a.team-b.svc"`), want: "entry 0: url must be"},
		"url with a path":     {content: entry(`,"url":"http://a.team-a.svc/x"`), want: "entry 0: url must be"},
		"https without CA":    {content: entry(`,"url":"https://a.team-a.svc"`), want: "caFile must be an absolute"},
		"http with CA":        {content: entry(`,"url":"http://a.team-a.svc","caFile":"/ca"`), want: "caFile must be empty"},
		"relative token":      {content: `[{"namespace":"team-a","cluster":"orders","url":"http://a.team-a.svc","tokenFile":"t"}]`, want: "tokenFile must be"},
		"bad cluster":         {content: `[{"namespace":"team-a","cluster":"Orders","url":"http://a.team-a.svc","tokenFile":"/t"}]`, want: "cluster must be"},
		"duplicate cluster": {
			content: `[{"namespace":"team-a","cluster":"orders","url":"http://a.team-a.svc","tokenFile":"/t"},{"namespace":"team-a","cluster":"orders","url":"http://b.team-a.svc","tokenFile":"/t"}]`,
			want:    "entry 1: names the same Cluster",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			path := test.path
			if path == "" {
				path = agentsFile(t, test.content)
			}
			_, err := LoadConsole(lookupOf(map[string]string{EnvWatchNamespaces: "team-a", EnvEvidenceAgentsFile: path}))
			if err == nil || !strings.Contains(err.Error(), EnvEvidenceAgentsFile) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want %q", err, test.want)
			}
			if strings.Contains(err.Error(), "team-b") || strings.Contains(err.Error(), "/t\"") {
				t.Fatalf("err %q echoes a value", err)
			}
		})
	}
}
