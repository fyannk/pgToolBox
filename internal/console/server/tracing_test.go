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

package server

import (
	"io"
	"net/http"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/fyannk/pgToolBox/internal/console/sse"
	"github.com/fyannk/pgToolBox/internal/console/tracing"
)

// The main listener makes a server span per request, continuing the
// caller's trace, except for the live streams; /mcp keeps its own name
// and the security headers.
func TestServerSpans(t *testing.T) {
	t.Parallel()
	rec := tracetest.NewSpanRecorder()
	tr := tracing.New(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)), false)
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) })
	h := New(Options{Auth: testAuth(t), API: api, UI: api, Ready: &Readiness{}, Broker: sse.NewBroker(), MCP: mounter{}, Tracing: tr}).Handler()

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	w := get(h, http.MethodPost, "/mcp", map[string]string{"Traceparent": "00-" + traceID + "-00f067aa0ba902b7-01"})
	if w.Code != http.StatusOK || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("POST /mcp = %d %v", w.Code, w.Header())
	}
	get(h, http.MethodGet, PathStream, nil)
	get(h, http.MethodGet, "/api/v1/namespaces/team-a/clusters/orders/instances/orders-1/logs/stream", nil)
	get(h, http.MethodGet, "/clusters/team-a/orders", nil)

	names := map[string]string{}
	for _, s := range rec.Ended() {
		names[s.Name()] = s.SpanContext().TraceID().String()
	}
	if len(names) != 2 {
		t.Errorf("spans %v, want POST /mcp and GET /* only (no stream)", names)
	}
	if names["POST /mcp"] != traceID {
		t.Errorf("POST /mcp trace %q, want the caller's", names["POST /mcp"])
	}
	if _, ok := names["GET /*"]; !ok {
		t.Errorf("the UI route is not GET /*: %v", names)
	}
}
