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
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestMCP(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		env     map[string]string
		want    MCP
		wantVar string
	}{
		{"off by default", nil, MCP{MaxResultBytes: 256 << 10, CallTimeout: 20 * time.Second, AccessTTL: time.Hour}, ""},
		{"on", map[string]string{EnvMCPEnabled: "true"}, MCP{Enabled: true, MaxResultBytes: 256 << 10, CallTimeout: 20 * time.Second, AccessTTL: time.Hour}, ""},
		{
			"write tools reserved, on with the endpoint",
			map[string]string{EnvMCPEnabled: "true", EnvMCPWriteTools: "true", EnvMCPMaxResultBytes: "65536", EnvMCPCallTimeout: "5s"},
			MCP{Enabled: true, WriteTools: true, MaxResultBytes: 64 << 10, CallTimeout: 5 * time.Second, AccessTTL: time.Hour},
			"",
		},
		{"write tools without the endpoint", map[string]string{EnvMCPWriteTools: "true"}, MCP{}, EnvMCPWriteTools},
		{"bad bool", map[string]string{EnvMCPEnabled: "on"}, MCP{}, EnvMCPEnabled},
		{"cap too small", map[string]string{EnvMCPMaxResultBytes: "1024"}, MCP{}, EnvMCPMaxResultBytes},
		{"cap too large", map[string]string{EnvMCPMaxResultBytes: "104857600"}, MCP{}, EnvMCPMaxResultBytes},
		{"timeout past the write timeout", map[string]string{EnvMCPCallTimeout: "2m"}, MCP{}, EnvMCPCallTimeout},
		{"timeout too short", map[string]string{EnvMCPCallTimeout: "100ms"}, MCP{}, EnvMCPCallTimeout},
		{
			"oauth settings",
			map[string]string{EnvMCPEnabled: "true", EnvMCPOAuthAccessTTL: "20s", EnvMCPOAuthClientMetadata: "true"},
			MCP{Enabled: true, MaxResultBytes: 256 << 10, CallTimeout: 20 * time.Second, AccessTTL: 20 * time.Second, ClientMetadataDocuments: true},
			"",
		},
		{"access ttl too short", map[string]string{EnvMCPOAuthAccessTTL: "1s"}, MCP{}, EnvMCPOAuthAccessTTL},
		{"access ttl past an hour", map[string]string{EnvMCPOAuthAccessTTL: "2h"}, MCP{}, EnvMCPOAuthAccessTTL},
		{"bad metadata switch", map[string]string{EnvMCPOAuthClientMetadata: "yes"}, MCP{}, EnvMCPOAuthClientMetadata},
		{
			"metadata hosts",
			map[string]string{EnvMCPEnabled: "true", EnvMCPOAuthClientMetadata: "true", EnvMCPOAuthClientMetadataHosts: " Claude.AI, chatgpt.com,claude.ai,"},
			MCP{
				Enabled: true, MaxResultBytes: 256 << 10, CallTimeout: 20 * time.Second, AccessTTL: time.Hour, ClientMetadataDocuments: true,
				ClientMetadataHosts: []string{"claude.ai", "chatgpt.com"},
			},
			"",
		},
		{"bad metadata host", map[string]string{EnvMCPOAuthClientMetadataHosts: "https://claude.ai"}, MCP{}, EnvMCPOAuthClientMetadataHosts},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{EnvWatchNamespaces: "team-a"}
			for k, v := range c.env {
				env[k] = v
			}
			cfg, err := LoadConsole(lookupOf(env))
			if c.wantVar == "" {
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(cfg.MCP, c.want) {
					t.Errorf("MCP = %+v, want %+v", cfg.MCP, c.want)
				}
				return
			}
			var varErr *VarError
			for _, e := range unwrapAll(err) {
				if errors.As(e, &varErr) && varErr.Name == c.wantVar {
					return
				}
			}
			t.Errorf("no VarError for %s in %v", c.wantVar, err)
		})
	}
}
