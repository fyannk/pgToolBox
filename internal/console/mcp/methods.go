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

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/fyannk/pgToolBox/internal/console/tracing"
)

// tracedMethods are the MCP methods outside tools/call that get a span and
// a series of their own; any other method name a client sends counts as
// "other", so the label set stays closed.
var tracedMethods = map[string]bool{
	"initialize": true, "ping": true, "tools/list": true,
	"resources/list": true, "resources/templates/list": true, "resources/read": true,
	"prompts/list": true, "prompts/get": true,
}

// requests counts the MCP requests other than tool calls (which
// pgtoolbox_console_mcp_calls_total counts) by method and outcome (ok, error).
var requests = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "pgtoolbox_console_mcp_requests_total",
	Help: "MCP requests other than tool calls (resources, prompts, lists, initialize) by method and outcome.",
}, []string{"method", "outcome"})

func init() { crmetrics.Registry.MustRegister(requests) }

// methodTelemetry is the server's receiving middleware: a span and a count
// per resource read, prompt and list request (tool calls have their own
// execute_tool span and series, made in addTool). Spans are named by the
// method alone; the resource URI (which names a Cluster) and the prompt
// name are attributes.
func (e *Endpoint) methodTelemetry(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == "tools/call" || strings.HasPrefix(method, "notifications/") {
			return next(ctx, method, req)
		}
		label := method
		if !tracedMethods[label] {
			label = "other"
		}
		var span trace.Span = noop.Span{}
		if e.o.Tracing.Enabled() {
			ctx, span = e.o.Tracing.Tracer().Start(ctx, label, trace.WithSpanKind(trace.SpanKindInternal),
				trace.WithAttributes(tracing.AttrMCPMethodName.String(label)))
			switch r := req.(type) {
			case *mcp.ReadResourceRequest:
				if r.Params != nil {
					span.SetAttributes(tracing.AttrMCPResourceURI.String(r.Params.URI))
				}
			case *mcp.GetPromptRequest:
				if r.Params != nil {
					span.SetAttributes(tracing.AttrGenAIPromptName.String(r.Params.Name))
				}
			}
			if extra := req.GetExtra(); extra != nil && extra.TokenInfo != nil && extra.TokenInfo.UserID != "" {
				span.SetAttributes(tracing.AttrEnduserID.String(extra.TokenInfo.UserID))
			}
		}
		defer span.End()
		res, err := next(ctx, method, req)
		outcome := "ok"
		if err != nil {
			outcome = "error"
			span.SetAttributes(tracing.AttrErrorType.String(outcome))
			span.SetStatus(codes.Error, outcome)
		}
		span.SetAttributes(tracing.AttrOutcome.String(outcome))
		requests.WithLabelValues(label, outcome).Inc()
		return res, err
	}
}
