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
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/fyannk/pgToolBox/internal/console/api"
	"github.com/fyannk/pgToolBox/internal/console/tracing"
)

// The docs tools are tools like the others; resource reads and prompts get
// a span named by the method, the URI and the prompt name as attributes,
// and a count in pgtoolbox_console_mcp_requests_total.
func TestDocsResourcesAndPromptsAreTraced(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	h := newHarness(t, withTracing(rec, false), func(o *Options, _ *api.Deps) { o.Docs = fakeDocs{} })
	before := testutil.ToFloat64(requests.WithLabelValues("resources/read", "ok"))
	beforeTool := testutil.ToFloat64(calls.WithLabelValues("search_docs", "ok"))
	cs := h.connectTraced(t, "tok-ops", "")
	if res, _ := callTool(t, cs, "search_docs", map[string]any{"query": "tracing"}); res.IsError {
		t.Fatal(text(res))
	}
	if _, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "docs://1.3/console/mcp"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "pgtoolbox://clusters/team-a/orders"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.GetPrompt(context.Background(), &mcp.GetPromptParams{
		Name: "triage_cluster", Arguments: map[string]string{"namespace": "team-a", "name": "orders"},
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(toolSpans(rec, "search_docs")); n != 1 {
		t.Errorf("%d search_docs spans", n)
	}
	if got := testutil.ToFloat64(calls.WithLabelValues("search_docs", "ok")) - beforeTool; got != 1 {
		t.Errorf("search_docs calls grew by %v", got)
	}
	if got := testutil.ToFloat64(requests.WithLabelValues("resources/read", "ok")) - before; got != 2 {
		t.Errorf("resources/read grew by %v, want 2", got)
	}
	var uris []string
	prompts := 0
	for _, s := range rec.Ended() {
		a := attrs(s)
		switch s.Name() {
		case "resources/read":
			uris = append(uris, a[tracing.AttrMCPResourceURI].AsString())
			if a[tracing.AttrEnduserID].AsString() != "oidc:ops@example.test" {
				t.Errorf("resources/read without the person: %v", a)
			}
		case "prompts/get":
			prompts++
			if a[tracing.AttrGenAIPromptName].AsString() != "triage_cluster" {
				t.Errorf("prompt name %v", a[tracing.AttrGenAIPromptName])
			}
		case "tools/call":
			t.Error("tool calls get a second, generic span")
		}
		if strings.Contains(s.Name(), "orders") {
			t.Errorf("span name %q names a Cluster", s.Name())
		}
	}
	if len(uris) != 2 || prompts != 1 {
		t.Errorf("resource spans %v, prompt spans %d", uris, prompts)
	}
}
