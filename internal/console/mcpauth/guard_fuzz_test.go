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

package mcpauth

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fyannk/pgToolBox/internal/console/auth"
)

// sdkReads posts body to the SDK's own streamable HTTP handler (stateless,
// at protocol 2025-06-18, where the SDK refuses a batch it reads with a
// message of its own) and reports what the SDK's batch reader made of it:
// a batch, a payload it could not read, or one message.
func sdkReads(t *testing.T, h http.Handler, body []byte) (batch, malformed bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/mcp", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	out := w.Body.String()
	return w.Code == http.StatusBadRequest && strings.Contains(out, "batching is not supported"),
		w.Code == http.StatusBadRequest && strings.Contains(out, "malformed payload")
}

func sdkHandler() http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: "fuzz", Version: "v0"}, nil)
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, Logger: slog.New(slog.DiscardHandler),
	})
}

// The fuzz target's view of the SDK tells a batch, an unreadable payload
// and one message apart: without it the target proves nothing.
func TestSDKReadsTellsABatch(t *testing.T) {
	h := sdkHandler()
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	for _, c := range []struct {
		body             string
		batch, malformed bool
	}{
		{"[" + call + "," + call + "]", true, false},
		{" \n[" + call + "]", true, false},
		{"\xef\xbb\xbf[" + call + "]", false, true},
		{call, false, false},
	} {
		if batch, malformed := sdkReads(t, h, []byte(c.body)); batch != c.batch || malformed != c.malformed {
			t.Errorf("%q: batch %v malformed %v, want %v %v", c.body, batch, malformed, c.batch, c.malformed)
		}
	}
}

// FuzzGuardSeesTheMessageTheSDKReads pairs the guard's reading of a body
// (oneMessage: refused as a batch, or the method it counts) with the
// SDK's batch reader, the one the SDK runs the body through. Whatever the
// body, a body the SDK reads as a batch is one the guard refused (one
// request never carries several calls for one unit of the budget), and a
// body the guard hands on is one message whose method, when the SDK reads
// it, is the one the guard counted (so no call passes as a listen).
func FuzzGuardSeesTheMessageTheSDKReads(f *testing.F) {
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_clusters","arguments":{}}}`
	listen := `{"jsonrpc":"2.0","id":9,"method":"subscriptions/listen","params":{"notifications":{}}}`
	for _, seed := range []string{
		call, listen, "[" + call + "," + call + "]", " \t[" + call + "]", "\xef\xbb\xbf[" + call + "]",
		"\f[" + call + "]", "\v[" + call + "]", " [" + call + "]", "\x00[" + call + "]",
		listen + " " + call, listen + "[" + call + "]", "[]", "[" + listen + "]",
		`{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","method":"tools/call"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","method":"subscriptions/listen"}`,
		`{"jsonrpc":"2.0","id":1,"Method":"subscriptions/listen","method":"tools/call"}`,
		`{"jsonrpc":"2.0","id":1,"method":"subscriptions\/listen"}`,
		`{"jsonrpc":"2.0","method":"subscriptions/listen"}`,
		`{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen\xff"}`,
		"null", `"x"`, "",
	} {
		f.Add([]byte(seed))
	}
	g := NewGuard(Options{Log: slog.New(slog.DiscardHandler)})
	sdk := sdkHandler()
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) == 0 || len(body) > DefaultMaxBodyBytes {
			return
		}
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, publicURL+PathMCP, strings.NewReader(string(body)))
		method, passed := g.oneMessage(httptest.NewRecorder(), r, auth.Client{})
		batch, malformed := sdkReads(t, sdk, body)
		if batch && passed {
			t.Fatalf("the SDK reads a batch the guard handed on: %q", body)
		}
		if !passed || malformed {
			return
		}
		msg, err := jsonrpc.DecodeMessage(body)
		if err != nil {
			if method != "" {
				t.Fatalf("the guard counts %q for a body the SDK cannot read: %q", method, body)
			}
			return
		}
		sdkMethod := ""
		if req, ok := msg.(*jsonrpc.Request); ok {
			sdkMethod = req.Method
		}
		if sdkMethod != method {
			t.Fatalf("the guard counts %q, the SDK runs %q: %q", method, sdkMethod, body)
		}
	})
}
