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
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/fyannk/pgToolBox/internal/console/api"
	"github.com/fyannk/pgToolBox/internal/console/tracing"
)

// The caller's trace: a client that traces its own run sends it.
const (
	callerTraceID     = "4bf92f3577b34da6a3ce929d0e0e4736"
	callerParent      = "00f067aa0ba902b7"
	callerTraceparent = "00-" + callerTraceID + "-" + callerParent + "-01"
)

func withTracing(rec *tracetest.SpanRecorder, payloads bool) options {
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	return func(o *Options, _ *api.Deps) { o.Tracing = tracing.New(tp, payloads) }
}

// headerTransport adds the bearer and a traceparent to every request.
type headerTransport struct{ token, traceparent string }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+h.token)
	if h.traceparent != "" {
		r.Header.Set("Traceparent", h.traceparent)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func (h harness) connectTraced(t *testing.T, token, traceparent string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: h.url + "/mcp", HTTPClient: &http.Client{Transport: headerTransport{token, traceparent}},
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func toolSpans(rec *tracetest.SpanRecorder, tool string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == tracing.OperationExecuteTool+" "+tool {
			out = append(out, s)
		}
	}
	return out
}

func attrs(s sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	out := map[attribute.Key]attribute.Value{}
	for _, kv := range s.Attributes() {
		out[kv.Key] = kv.Value
	}
	return out
}

// A tool call is one execute_tool span in the caller's trace, under the
// request's span, with the GenAI attributes, the person, the outcome and
// the size, and none of the payload.
func TestToolCallSpan(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	h := newHarness(t, withTracing(rec, false))
	cs := h.connectTraced(t, "tok-ops", callerTraceparent)
	if res, _ := callTool(t, cs, "cluster_status", map[string]any{"namespace": "team-a", "name": "orders"}); res.IsError {
		t.Fatal(text(res))
	}
	spans := toolSpans(rec, "cluster_status")
	if len(spans) != 1 {
		t.Fatalf("%d execute_tool spans, want 1 (all: %d)", len(spans), len(rec.Ended()))
	}
	s := spans[0]
	if got := s.SpanContext().TraceID().String(); got != callerTraceID {
		t.Errorf("trace %s, want the caller's %s", got, callerTraceID)
	}
	// The parent is the request's server span, itself the caller's child.
	var server sdktrace.ReadOnlySpan
	for _, o := range rec.Ended() {
		if o.SpanContext().SpanID() == s.Parent().SpanID() {
			server = o
		}
	}
	if server == nil || server.SpanKind() != trace.SpanKindServer || server.Name() != "POST /mcp" {
		t.Fatalf("parent %v, want the POST /mcp server span", server)
	}
	if got := server.Parent().SpanID().String(); got != callerParent {
		t.Errorf("server span parent %s, want the caller's %s", got, callerParent)
	}
	a := attrs(s)
	for k, want := range map[attribute.Key]any{
		tracing.AttrGenAIOperationName: "execute_tool",
		tracing.AttrGenAIToolName:      "cluster_status",
		tracing.AttrMCPMethodName:      "tools/call",
		tracing.AttrEnduserID:          "oidc:ops@example.test",
		tracing.AttrOutcome:            "ok",
		tracing.AttrResultTruncated:    false,
	} {
		if got := a[k].AsInterface(); got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
	if a[tracing.AttrResultBytes].AsInt64() <= 0 {
		t.Errorf("result size %v, want the envelope's", a[tracing.AttrResultBytes])
	}
	for _, k := range []attribute.Key{tracing.AttrGenAIToolArguments, tracing.AttrGenAIToolResult} {
		if _, ok := a[k]; ok {
			t.Errorf("%s is set without TRACING_INCLUDE_PAYLOADS", k)
		}
	}
	for _, o := range rec.Ended() {
		for _, kv := range o.Attributes() {
			if strings.Contains(kv.Value.String(), "tok-ops") {
				t.Errorf("span %q carries the bearer in %s", o.Name(), kv.Key)
			}
		}
	}
}

// A refused call is an error span with its outcome, counted.
func TestRefusedToolCallSpanAndMetric(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	h := newHarness(t, withTracing(rec, false))
	before := testutil.ToFloat64(calls.WithLabelValues("cluster_status", "403"))
	cs := h.connectTraced(t, "tok-dba", "")
	res, _ := callTool(t, cs, "cluster_status", map[string]any{"namespace": "team-a", "name": "billing"})
	if !res.IsError {
		t.Fatal("dba read billing")
	}
	if got := testutil.ToFloat64(calls.WithLabelValues("cluster_status", "403")) - before; got != 1 {
		t.Errorf("pgtoolbox_console_mcp_calls_total{cluster_status,403} grew by %v, want 1", got)
	}
	spans := toolSpans(rec, "cluster_status")
	if len(spans) != 1 {
		t.Fatalf("%d spans", len(spans))
	}
	a := attrs(spans[0])
	if a[tracing.AttrOutcome].AsString() != "403" || a[tracing.AttrErrorType].AsString() != "403" || spans[0].Status().Code.String() != "Error" {
		t.Errorf("outcome %v, error.type %v, status %v", a[tracing.AttrOutcome], a[tracing.AttrErrorType], spans[0].Status())
	}
}

// With TRACING_INCLUDE_PAYLOADS the arguments and the result are on the
// span, redacted: a credential-shaped value never is.
func TestToolCallPayloads(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	h := newHarness(t, withTracing(rec, true))
	cs := h.connectTraced(t, "tok-ops", "")
	if res, _ := callTool(t, cs, "cluster_status", map[string]any{"namespace": "team-a", "name": "orders"}); res.IsError {
		t.Fatal(text(res))
	}
	a := attrs(toolSpans(rec, "cluster_status")[0])
	if got := a[tracing.AttrGenAIToolArguments].AsString(); got != `{"name":"orders","namespace":"team-a"}` {
		t.Errorf("arguments %q", got)
	}
	result := a[tracing.AttrGenAIToolResult].AsString()
	if !strings.Contains(result, `"data"`) || !strings.Contains(result, "orders") {
		t.Errorf("result %q is not the envelope", result)
	}
	if len(result) > maxPayloadAttribute+len("...(cut)") {
		t.Errorf("result attribute of %d bytes past the cap", len(result))
	}
}

func TestPayloadIsRedactedAndCapped(t *testing.T) {
	t.Parallel()
	got := payload(map[string]any{"token": "abc", "note": "Bearer abcdefghijklmnop", "big": strings.Repeat("é", maxPayloadAttribute)})
	if strings.Contains(got, "abc\"") || strings.Contains(got, "abcdefghijklmnop") {
		t.Errorf("a credential survived: %.200s", got)
	}
	if !strings.HasSuffix(got, "...(cut)") || len(got) > maxPayloadAttribute+len("...(cut)") {
		t.Errorf("not capped: %d bytes", len(got))
	}
	if !strings.HasPrefix(got, "{") {
		t.Errorf("payload %.40s", got)
	}
}

// Tracing off: no span anywhere, the metrics still count.
func TestNoTracingNoSpans(t *testing.T) {
	h := newHarness(t)
	before := testutil.ToFloat64(calls.WithLabelValues("list_clusters", "ok"))
	res, _ := callTool(t, h.connect(t, "tok-ops"), "list_clusters", map[string]any{})
	if res.IsError {
		t.Fatal(text(res))
	}
	if got := testutil.ToFloat64(calls.WithLabelValues("list_clusters", "ok")) - before; got < 1 {
		t.Errorf("pgtoolbox_console_mcp_calls_total{list_clusters,ok} grew by %v", got)
	}
	if n := testutil.CollectAndCount(callDuration, "pgtoolbox_console_mcp_call_duration_seconds"); n == 0 {
		t.Error("no duration series")
	}
	if n := testutil.CollectAndCount(resultBytes, "pgtoolbox_console_mcp_result_bytes"); n == 0 {
		t.Error("no result size series")
	}
}

// A truncated result counts once, an untruncated one not at all, and a
// failed call is no result.
func TestFinishCallCountsTruncation(t *testing.T) {
	t.Parallel()
	e := &Endpoint{}
	span := trace.SpanFromContext(context.Background())
	before := testutil.ToFloat64(truncatedResults.WithLabelValues("trunc_probe"))
	e.finishCall(span, callRecord{tool: "trunc_probe", outcome: "ok", size: 10, truncated: true})
	e.finishCall(span, callRecord{tool: "trunc_probe", outcome: "ok", size: 10})
	e.finishCall(span, callRecord{tool: "trunc_probe", outcome: "403", truncated: true})
	if got := testutil.ToFloat64(truncatedResults.WithLabelValues("trunc_probe")) - before; got != 1 {
		t.Errorf("pgtoolbox_console_mcp_result_truncated_total grew by %v, want 1", got)
	}
}

// A client may carry its trace in the call's _meta (MCP conventions)
// rather than in the HTTP header: the tool span joins that trace.
func TestMetaTraceparent(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	h := newHarness(t, withTracing(rec, false))
	cs := h.connectTraced(t, "tok-ops", "")
	_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "list_clusters", Arguments: map[string]any{},
		Meta: mcp.Meta{"traceparent": callerTraceparent},
	})
	if err != nil {
		t.Fatal(err)
	}
	spans := toolSpans(rec, "list_clusters")
	if len(spans) != 1 {
		t.Fatalf("%d spans", len(spans))
	}
	if got := spans[0].SpanContext().TraceID().String(); got != callerTraceID {
		t.Errorf("trace %s, want the _meta's %s", got, callerTraceID)
	}
	if len(spans[0].Links()) != 1 {
		t.Errorf("links %v, want the request's span", spans[0].Links())
	}
}
