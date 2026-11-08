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
	"encoding/json"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/fyannk/pgToolBox/internal/console/tracing"
	"github.com/fyannk/pgToolBox/pkg/redact"
)

// The tool-call metrics: one series per tool and outcome.
// The outcome is the audit line's: ok, the API status (403, 404, ...),
// timeout, error or unauthenticated, a closed set.
var (
	calls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pgtoolbox_console_mcp_calls_total",
		Help: "MCP tool calls by tool and outcome (ok, an HTTP status, timeout, error, unauthenticated).",
	}, []string{"tool", "outcome"})
	callDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "pgtoolbox_console_mcp_call_duration_seconds",
		Help:    "Duration of MCP tool calls by tool.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 50},
	}, []string{"tool"})
	// resultBytes and truncated show how close the answers run to the
	// response cap: a tool whose results are cut is one a model reads half of.
	resultBytes = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "pgtoolbox_console_mcp_result_bytes",
		Help:    "Size in bytes of the results of successful MCP tool calls, by tool.",
		Buckets: prometheus.ExponentialBuckets(256, 4, 8),
	}, []string{"tool"})
	truncatedResults = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pgtoolbox_console_mcp_result_truncated_total",
		Help: "MCP tool results shortened to fit the response cap, by tool.",
	}, []string{"tool"})
)

func init() { crmetrics.Registry.MustRegister(calls, callDuration, resultBytes, truncatedResults) }

// maxPayloadAttribute caps each payload attribute of a span: a trace
// backend is no place for a 256 KiB result.
const maxPayloadAttribute = 16 << 10

// metaCarrier reads a W3C trace context from a request's _meta, where the
// MCP conventions let a client put it (traceparent, tracestate).
type metaCarrier map[string]any

func (m metaCarrier) Get(key string) string {
	s, _ := m[key].(string)
	return s
}
func (m metaCarrier) Set(string, string) {}
func (m metaCarrier) Keys() []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// startToolSpan starts the execute_tool span of a call (OpenTelemetry
// GenAI conventions), a child of the HTTP request's span. A client that
// put another trace's context in the call's _meta gets the span in that
// trace instead, linked to the request's.
func (e *Endpoint) startToolSpan(ctx context.Context, req *mcp.CallToolRequest, tool string) (context.Context, trace.Span) {
	if !e.o.Tracing.Enabled() {
		return ctx, trace.SpanFromContext(context.Background())
	}
	opts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			tracing.AttrGenAIOperationName.String(tracing.OperationExecuteTool),
			tracing.AttrGenAIToolName.String(tool),
			tracing.AttrGenAIToolType.String("function"),
			tracing.AttrMCPMethodName.String("tools/call"),
		),
	}
	if req != nil {
		if v := req.ProtocolVersion(); v != "" {
			opts = append(opts, trace.WithAttributes(tracing.AttrMCPProtocolVersion.String(v)))
		}
		if req.Params != nil && len(req.Params.Meta) > 0 {
			remote := trace.SpanContextFromContext(propagation.TraceContext{}.Extract(context.Background(), metaCarrier(req.Params.Meta)))
			current := trace.SpanContextFromContext(ctx)
			if remote.IsValid() && remote.TraceID() != current.TraceID() {
				if current.IsValid() {
					opts = append(opts, trace.WithLinks(trace.Link{SpanContext: current}))
				}
				ctx = trace.ContextWithRemoteSpanContext(ctx, remote)
			}
		}
	}
	return e.o.Tracing.Tracer().Start(ctx, tracing.OperationExecuteTool+" "+tool, opts...)
}

// callRecord is what one call ends with.
type callRecord struct {
	tool      string
	user      string
	outcome   string
	size      int
	truncated bool
	took      time.Duration
	args      any
	result    any
}

// finishCall counts the call and completes its span: outcome, size and
// truncation always; the arguments and the result only when
// TRACING_INCLUDE_PAYLOADS is on, redacted and capped.
func (e *Endpoint) finishCall(span trace.Span, r callRecord) {
	calls.WithLabelValues(r.tool, r.outcome).Inc()
	callDuration.WithLabelValues(r.tool).Observe(r.took.Seconds())
	if r.outcome == "ok" {
		resultBytes.WithLabelValues(r.tool).Observe(float64(r.size))
		if r.truncated {
			truncatedResults.WithLabelValues(r.tool).Inc()
		}
	}
	if !span.IsRecording() {
		return
	}
	span.SetAttributes(
		tracing.AttrOutcome.String(r.outcome),
		tracing.AttrResultBytes.Int(r.size),
		tracing.AttrResultTruncated.Bool(r.truncated),
	)
	if r.user != "" {
		span.SetAttributes(tracing.AttrEnduserID.String(r.user))
	}
	if r.outcome != "ok" {
		span.SetAttributes(tracing.AttrErrorType.String(r.outcome))
		span.SetStatus(codes.Error, r.outcome)
	}
	if !e.o.Tracing.IncludePayloads() {
		return
	}
	if v := payload(r.args); v != "" {
		span.SetAttributes(tracing.AttrGenAIToolArguments.String(v))
	}
	if v := payload(r.result); v != "" {
		span.SetAttributes(tracing.AttrGenAIToolResult.String(v))
	}
}

// payload is v's JSON, redacted (pkg/redact) and capped.
func payload(v any) string {
	if v == nil {
		return ""
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	out := string(redact.Payload(raw))
	if len(out) > maxPayloadAttribute {
		cut := maxPayloadAttribute
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut--
		}
		out = out[:cut] + "...(cut)"
	}
	return out
}
