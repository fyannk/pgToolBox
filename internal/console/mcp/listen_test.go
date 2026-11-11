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
)

// The SDK in the loop, behind the real guard: a client with list-changed
// handlers (protocol 2026-07-28) opens a subscriptions/listen request for
// its whole session when the server advertises list changes. The console
// advertises none, so five such assistants of one person, each a session
// of its own, all connect and call tools: no session holds one of the
// person's four call slots while it is idle.
func TestListChangedClientsHoldNoCallSlot(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sessions := make([]*mcp.ClientSession, 0, 5)
	for range 5 {
		noop := func(context.Context, *mcp.ClientRequest[*mcp.ToolListChangedParams]) {}
		c := mcp.NewClient(&mcp.Implementation{Name: "listener", Version: "v0"}, &mcp.ClientOptions{
			ToolListChangedHandler:     noop,
			PromptListChangedHandler:   func(context.Context, *mcp.ClientRequest[*mcp.PromptListChangedParams]) {},
			ResourceListChangedHandler: func(context.Context, *mcp.ClientRequest[*mcp.ResourceListChangedParams]) {},
		})
		cs, err := c.Connect(t.Context(), &mcp.StreamableClientTransport{
			Endpoint: h.url + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{"tok-dba"}},
		}, nil)
		if err != nil {
			t.Fatalf("session %d could not connect: %v", len(sessions)+1, err)
		}
		t.Cleanup(func() { _ = cs.Close() })
		caps := cs.InitializeResult().Capabilities
		if caps.Tools == nil || caps.Tools.ListChanged || caps.Prompts == nil || caps.Prompts.ListChanged ||
			caps.Resources == nil || caps.Resources.ListChanged {
			t.Fatalf("capabilities advertise list changes: %+v %+v %+v", caps.Tools, caps.Prompts, caps.Resources)
		}
		sessions = append(sessions, cs)
	}
	for i, cs := range sessions {
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_clusters", Arguments: map[string]any{}})
		if err != nil || res.IsError {
			t.Errorf("session %d: list_clusters: %v %v", i+1, err, res)
		}
	}
}

// A client may open subscriptions/listen although nothing is advertised:
// the SDK then agrees to no subscription, and the guard counts the request
// as an open stream, not as a call, should it park. Four listens of one
// person leave that person's calls their four slots.
func TestListenRequestsDoNotTakeCallSlots(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	listen := `{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true},` +
		`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"raw","version":"v0"},` +
		`"io.modelcontextprotocol/clientCapabilities":{}}}}`
	for i := range 4 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.url+"/mcp", strings.NewReader(listen))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer tok-dba")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
		req.Header.Set("Mcp-Method", "subscriptions/listen")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("listen %d = %d %q: the test needs a parked stream %s", i+1, resp.StatusCode, resp.Header.Get("Content-Type"), b)
		}
	}
	cs := h.connect(t, "tok-dba")
	if res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_clusters", Arguments: map[string]any{}}); err != nil || res.IsError {
		t.Errorf("a call beside four open listens: %v %v", err, res)
	}
}
