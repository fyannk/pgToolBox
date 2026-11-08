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
	"net/http"
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/internal/console/api"
)

// A JSON-RPC batch, which the SDK still accepts from a client that sends
// no MCP-Protocol-Version, never reaches it: one request is one call.
func TestBatchesAreRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	calls := make([]string, 0, 50)
	for range 50 {
		calls = append(calls, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_clusters","arguments":{}}}`)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.url+"/mcp", strings.NewReader("["+strings.Join(calls, ",")+"]"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok-ops")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a batch = %d", resp.StatusCode)
	}
	if strings.Contains(h.log.String(), `"audit":"mcp.call"`) {
		t.Error("a call of the batch ran")
	}
}

func TestThePublicURLIsRequired(t *testing.T) {
	t.Parallel()
	_, _, reads := api.NewWithReads(api.Deps{Source: api.FixtureSource{}, Authz: api.FixtureAuthorizer{}})
	if _, err := New(Options{Reads: reads, Authenticator: tokens()}); err == nil || !strings.Contains(err.Error(), "PUBLIC_URL") {
		t.Errorf("New without a public URL: %v", err)
	}
}
