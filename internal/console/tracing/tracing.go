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

// Package tracing is the console's OpenTelemetry instrumentation: a server span per HTTP request named by its route template, a client
// span per Kubernetes API call made while serving one, and the attribute
// names of the MCP tool spans. It depends on the OpenTelemetry API only;
// the SDK and the OTLP exporter are internal/console/tracing/otlp's, which
// only cmd/console links.
//
// A nil *Tracing is tracing turned off: every method is then a no-op that
// adds no handler, no transport wrapper, no goroutine.
package tracing

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"k8s.io/client-go/transport"
)

// ScopeName is the instrumentation scope of the console's own spans.
const ScopeName = "github.com/fyannk/pgToolBox/internal/console"

// Attribute names. The GenAI and MCP ones follow the OpenTelemetry
// semantic conventions (GenAI "execute_tool" spans and the MCP
// conventions, semconv 1.41: the Go module dropped the GenAI group in
// 1.42 when it moved to its own registry, so the names are spelled here);
// the pgtoolbox-console.* ones are the console's.
const (
	AttrGenAIOperationName = attribute.Key("gen_ai.operation.name")
	AttrGenAIToolName      = attribute.Key("gen_ai.tool.name")
	AttrGenAIToolCallID    = attribute.Key("gen_ai.tool.call.id")
	AttrGenAIToolType      = attribute.Key("gen_ai.tool.type")
	AttrGenAIToolArguments = attribute.Key("gen_ai.tool.call.arguments")
	AttrGenAIToolResult    = attribute.Key("gen_ai.tool.call.result")
	AttrMCPMethodName      = attribute.Key("mcp.method.name")
	AttrMCPProtocolVersion = attribute.Key("mcp.protocol.version")
	AttrMCPResourceURI     = attribute.Key("mcp.resource.uri")
	AttrGenAIPromptName    = attribute.Key("gen_ai.prompt.name")
	AttrEnduserID          = attribute.Key("enduser.id")
	AttrErrorType          = attribute.Key("error.type")
	AttrHTTPRoute          = attribute.Key("http.route")
	AttrK8sNamespace       = attribute.Key("k8s.namespace.name")

	AttrOutcome         = attribute.Key("pgtoolbox-console.outcome")
	AttrResultBytes     = attribute.Key("pgtoolbox-console.result.bytes")
	AttrResultTruncated = attribute.Key("pgtoolbox-console.result.truncated")
)

// OperationExecuteTool is gen_ai.operation.name of a tool call.
const OperationExecuteTool = "execute_tool"

// Tracing is tracing turned on: the provider spans are made with, the
// propagator (W3C trace context) and the payload switch.
type Tracing struct {
	provider        trace.TracerProvider
	propagator      propagation.TextMapPropagator
	includePayloads bool
}

// New is tracing over provider. A nil provider is tracing off (nil).
func New(provider trace.TracerProvider, includePayloads bool) *Tracing {
	if provider == nil {
		return nil
	}
	return &Tracing{provider: provider, propagator: propagation.TraceContext{}, includePayloads: includePayloads}
}

// Enabled reports whether spans are made.
func (t *Tracing) Enabled() bool { return t != nil }

// IncludePayloads reports TRACING_INCLUDE_PAYLOADS.
func (t *Tracing) IncludePayloads() bool { return t != nil && t.includePayloads }

// Tracer is the console's tracer; a no-op one when tracing is off.
func (t *Tracing) Tracer() trace.Tracer {
	if t == nil {
		return noop.NewTracerProvider().Tracer(ScopeName)
	}
	return t.provider.Tracer(ScopeName)
}

// Propagator is the W3C trace-context propagator (nil when off).
func (t *Tracing) Propagator() propagation.TextMapPropagator {
	if t == nil {
		return nil
	}
	return t.propagator
}

// Handler wraps next with a server span per request, continuing the
// caller's W3C trace context. The span is named "METHOD route": the
// route template a handler declares with SetRoute (the API's operation
// path, with {namespace} and {name} in place of the values), else a
// coarse class of the path, so that no Cluster or pod name ever reaches a
// span name; the full path stays in url.path. Requests skip says (the
// live streams, which last a session) get no span. Off: next unchanged.
func (t *Tracing) Handler(next http.Handler, skip func(*http.Request) bool) http.Handler {
	if t == nil {
		return next
	}
	opts := []otelhttp.Option{
		otelhttp.WithTracerProvider(t.provider),
		otelhttp.WithPropagators(t.propagator),
		otelhttp.WithMeterProvider(metricnoop.NewMeterProvider()),
		otelhttp.WithSpanNameFormatter(serverSpanName),
	}
	if skip != nil {
		opts = append(opts, otelhttp.WithFilter(func(r *http.Request) bool { return !skip(r) }))
	}
	h := otelhttp.NewHandler(next, "", opts...)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), routeKey{}, new(atomic.Pointer[string]))))
	})
}

type routeKey struct{}

// SetRoute names the request's route template (an API operation path);
// it renames the server span and sets http.route. A no-op outside
// Handler.
func SetRoute(ctx context.Context, method, route string) {
	holder, ok := ctx.Value(routeKey{}).(*atomic.Pointer[string])
	if !ok || route == "" {
		return
	}
	holder.Store(&route)
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	span.SetName(method + " " + route)
	span.SetAttributes(AttrHTTPRoute.String(route))
}

func serverSpanName(_ string, r *http.Request) string {
	if holder, ok := r.Context().Value(routeKey{}).(*atomic.Pointer[string]); ok {
		if route := holder.Load(); route != nil {
			return r.Method + " " + *route
		}
	}
	return r.Method + " " + CoarseRoute(r.URL.Path)
}

// coarsePrefixes are the console's route families, by prefix.
var coarsePrefixes = []string{"/api/", "/assets/", "/auth/", "/oauth/", "/.well-known/"}

// CoarseRoute is the low-cardinality class of a path: /mcp, a family's
// prefix followed by *, or /* for the UI's own routes (and anything
// unknown, so a scanner's paths name no span of their own).
func CoarseRoute(path string) string {
	if path == "/mcp" {
		return path
	}
	for _, p := range coarsePrefixes {
		if strings.HasPrefix(path, p) {
			return p + "*"
		}
	}
	return "/*"
}

// KubeTransport wraps a client-go transport with a client span per
// Kubernetes API call made on behalf of a traced request; calls without a
// span in their context (the informers' lists, every watch) are not
// traced, so a trace shows what a person's request cost and nothing else.
// Spans are named "kube METHOD template" with the namespace and object
// name replaced. Nil when tracing is off.
func (t *Tracing) KubeTransport() transport.WrapperFunc {
	if t == nil {
		return nil
	}
	return func(rt http.RoundTripper) http.RoundTripper {
		// The inner round tripper runs under the client span otelhttp
		// started: the namespace goes on that span there.
		inner := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			if ns := kubeNamespace(r.URL.Path); ns != "" {
				trace.SpanFromContext(r.Context()).SetAttributes(AttrK8sNamespace.String(ns))
			}
			return rt.RoundTrip(r)
		})
		traced := otelhttp.NewTransport(inner,
			otelhttp.WithTracerProvider(t.provider),
			otelhttp.WithPropagators(t.propagator),
			otelhttp.WithMeterProvider(metricnoop.NewMeterProvider()),
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				return "kube " + r.Method + " " + KubePathTemplate(r.URL.Path)
			}),
		)
		return roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			if !trace.SpanContextFromContext(r.Context()).IsValid() || r.URL.Query().Get("watch") == "true" {
				return rt.RoundTrip(r)
			}
			return traced.RoundTrip(r)
		})
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// KubePathTemplate is an API server path with its variable segments
// replaced: /api/v1/namespaces/{namespace}/pods/{name}/log. A path of
// another shape is /{other}.
func KubePathTemplate(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	var prefix int
	switch {
	case len(parts) >= 2 && parts[0] == "api":
		prefix = 2
	case len(parts) >= 3 && parts[0] == "apis":
		prefix = 3
	default:
		return "/{other}"
	}
	out := append([]string{}, parts[:prefix]...)
	rest := parts[prefix:]
	if len(rest) >= 2 && rest[0] == "namespaces" {
		if len(rest) == 2 {
			return "/" + strings.Join(append(out, "namespaces", "{name}"), "/")
		}
		out = append(out, "namespaces", "{namespace}")
		rest = rest[2:]
	}
	if len(rest) >= 1 {
		out = append(out, rest[0])
	}
	if len(rest) >= 2 {
		out = append(out, "{name}")
	}
	if len(rest) >= 3 {
		// The subresource (log, status, scale); whatever follows (a
		// proxy's own path) is cut.
		out = append(out, rest[2])
	}
	return "/" + strings.Join(out, "/")
}

// kubeNamespace is the namespace a namespaced API path names.
func kubeNamespace(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := 2; i+2 < len(parts) && i <= 3; i++ {
		if parts[i] == "namespaces" {
			return parts[i+1]
		}
	}
	return ""
}
