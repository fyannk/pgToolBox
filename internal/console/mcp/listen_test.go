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
	"sync"
	"testing"
	"time"

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
// the SDK then agrees to no subscription and answers at once, so a listen
// parks nothing and holds none of the person's call slots once answered.
// Four listens, each read to its end within a deadline, leave all four
// call slots to the person's calls, made at once. (That the guard counts a
// listen that does park as a stream, not a call, is
// mcpauth.TestGuardOpenStreamsDoNotTakeCallSlots.)
func TestListenRequestsDoNotTakeCallSlots(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	listen := `{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true},` +
		`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"raw","version":"v0"},` +
		`"io.modelcontextprotocol/clientCapabilities":{}}}}`
	for i := range 4 {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url+"/mcp", strings.NewReader(listen))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer tok-dba")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
		req.Header.Set("Mcp-Method", "subscriptions/listen")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		if err != nil {
			t.Fatalf("listen %d did not end: %v (a listen parks: the call slots below prove nothing)", i+1, err)
		}
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"result"`) {
			t.Fatalf("listen %d = %d %s: want an answer agreeing to nothing", i+1, resp.StatusCode, b)
		}
	}
	cs := h.connect(t, "tok-dba")
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			if res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_clusters", Arguments: map[string]any{}}); err != nil || res.IsError {
				t.Errorf("call %d of four at once, after four listens: %v %v", i+1, err, res)
			}
		})
	}
	wg.Wait()
}
