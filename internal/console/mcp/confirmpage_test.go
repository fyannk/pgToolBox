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

package mcp

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fyannk/pgToolBox/internal/console/api"
	"github.com/fyannk/pgToolBox/internal/console/mcpauth"
)

// The page names the credential that asked (a grant: its client and
// callback host), when, and for a promote the primary it moves from.
func TestConfirmPageNamesTheCredential(t *testing.T) {
	t.Parallel()
	h, _ := writeHarness(t, func(o *Options, _ *api.Deps) { o.Authenticator = grantOrRaw(mcpauth.ScopeRead, mcpauth.ScopeOperate) })
	cs, rec := h.elicitClient(t, "tok-grant", answer{action: "cancel"})
	if res, _ := callTool(t, cs, "promote", map[string]any{"namespace": "team-a", "name": "orders", "target": "orders-2"}); !res.IsError {
		t.Fatal("a cancelled promote ran")
	}
	asked := rec.questions()
	if len(asked) != 1 || asked[0].URL == "" {
		t.Fatalf("asked %v", asked)
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, asked[0].URL, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "dba"})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	body := string(b)
	for _, s := range []string{
		"Asked with: the console grant to Claude Code (unverified name), callback host 127.0.0.1:33418, at 12:00:00 UTC.",
		"Primary: <code>orders-1</code> now, <code>orders-2</code> after.",
	} {
		if !strings.Contains(body, s) {
			t.Errorf("the page lacks %q:\n%s", s, body)
		}
	}
}

// The scope check refuses a tools/call it cannot read rather than pass
// it on unchecked.
func TestRequireOperateRefusesAnUnexpectedRequest(t *testing.T) {
	t.Parallel()
	h, _ := writeHarness(t)
	ran := false
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) { ran = true; return nil, nil }
	if _, err := h.e.requireOperate(next)(context.Background(), "tools/call", &mcp.ListToolsRequest{}); err == nil || ran {
		t.Errorf("an unexpected tools/call request: err %v, passed on %v", err, ran)
	}
	if _, err := h.e.requireOperate(next)(context.Background(), "tools/list", &mcp.ListToolsRequest{}); err != nil || !ran {
		t.Errorf("tools/list: err %v, passed on %v", err, ran)
	}
}
