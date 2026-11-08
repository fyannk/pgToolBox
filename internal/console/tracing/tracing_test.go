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

package tracing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const (
	callerTraceID     = "4bf92f3577b34da6a3ce929d0e0e4736"
	callerTraceparent = "00-" + callerTraceID + "-00f067aa0ba902b7-01"
)

func recording() (*Tracing, *tracetest.SpanRecorder) {
	rec := tracetest.NewSpanRecorder()
	return New(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)), false), rec
}

func TestOffIsNothing(t *testing.T) {
	t.Parallel()
	var tr *Tracing
	if New(nil, true) != nil {
		t.Error("New(nil) is not off")
	}
	if tr.Enabled() || tr.IncludePayloads() || tr.KubeTransport() != nil || tr.Propagator() != nil {
		t.Error("off reports something on")
	}
	next := http.NewServeMux()
	if h, ok := tr.Handler(next, nil).(*http.ServeMux); !ok || h != next {
		t.Error("off wraps the handler")
	}
	_, span := tr.Tracer().Start(context.Background(), "x")
	if span.IsRecording() || span.SpanContext().IsValid() {
		t.Error("off records spans")
	}
	SetRoute(context.Background(), "GET", "/x") // no holder: no panic, nothing
}

func TestServerSpans(t *testing.T) {
	t.Parallel()
	tr, rec := recording()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/routed") {
			SetRoute(r.Context(), r.Method, "/api/v1/namespaces/{namespace}/clusters/{name}/routed")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := tr.Handler(mux, func(r *http.Request) bool { return r.URL.Path == "/api/v1/stream" })

	cases := []struct {
		path, traceparent, want string
	}{
		{"/api/v1/namespaces/team-a/clusters/orders/routed", "", "GET /api/v1/namespaces/{namespace}/clusters/{name}/routed"},
		{"/api/v1/namespaces/team-a/clusters/orders", callerTraceparent, "GET /api/*"},
		{"/clusters/team-a/orders", "", "GET /*"},
		{"/wp-admin/install.php", "", "GET /*"},
		{"/api/v1/stream", "", ""},
	}
	for _, c := range cases {
		rec.Reset()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, c.path, nil)
		if c.traceparent != "" {
			req.Header.Set("Traceparent", c.traceparent)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		spans := rec.Ended()
		if c.want == "" {
			if len(spans) != 0 {
				t.Errorf("%s: %d spans, want none", c.path, len(spans))
			}
			continue
		}
		if len(spans) != 1 {
			t.Fatalf("%s: %d spans", c.path, len(spans))
		}
		s := spans[0]
		if s.Name() != c.want {
			t.Errorf("%s: span %q, want %q", c.path, s.Name(), c.want)
		}
		if c.traceparent != "" && s.SpanContext().TraceID().String() != callerTraceID {
			t.Errorf("%s: trace %s, want the caller's", c.path, s.SpanContext().TraceID())
		}
		for _, kv := range s.Attributes() {
			if kv.Key == AttrHTTPRoute && strings.Contains(kv.Value.AsString(), "orders") {
				t.Errorf("%s: http.route %q names the Cluster", c.path, kv.Value.AsString())
			}
		}
	}
}

func TestCoarseRoute(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]string{
		"/mcp": "/mcp", "/mcp/x": "/*", "/api/v1/clusters": "/api/*", "/assets/app.js": "/assets/*",
		"/auth/callback": "/auth/*", "/oauth/token": "/oauth/*", "/.well-known/oauth-protected-resource/mcp": "/.well-known/*",
		"/": "/*", "/clusters/team-a/orders": "/*",
	} {
		if got := CoarseRoute(path); got != want {
			t.Errorf("CoarseRoute(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestKubePathTemplate(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]string{
		"/api/v1/namespaces/team-a/pods/orders-1/log":                          "/api/v1/namespaces/{namespace}/pods/{name}/log",
		"/api/v1/namespaces/team-a/pods":                                       "/api/v1/namespaces/{namespace}/pods",
		"/api/v1/namespaces/team-a":                                            "/api/v1/namespaces/{name}",
		"/api/v1/nodes/n1":                                                     "/api/v1/nodes/{name}",
		"/apis/authorization.k8s.io/v1/selfsubjectaccessreviews":               "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews",
		"/apis/postgresql.cnpg.io/v1/namespaces/team-a/clusters/orders/status": "/apis/postgresql.cnpg.io/v1/namespaces/{namespace}/clusters/{name}/status",
		"/api/v1/namespaces/team-a/pods/orders-1/proxy/metrics/secret":         "/api/v1/namespaces/{namespace}/pods/{name}/proxy",
		"/version": "/{other}",
		"/apis":    "/{other}",
	} {
		if got := KubePathTemplate(path); got != want {
			t.Errorf("KubePathTemplate(%q) = %q, want %q", path, got, want)
		}
	}
}

// API calls made while serving a traced request get a client span that
// carries the trace to the API server; the informers' calls and the
// watches get none.
func TestKubeTransport(t *testing.T) {
	t.Parallel()
	tr, rec := recording()
	var seen []string
	apiserver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Traceparent"))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(apiserver.Close)
	client := &http.Client{Transport: tr.KubeTransport()(http.DefaultTransport)}
	do := func(ctx context.Context, path string) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, apiserver.URL+path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}

	do(context.Background(), "/api/v1/namespaces/team-a/pods")
	ctx, parent := tr.Tracer().Start(context.Background(), "request")
	do(ctx, "/api/v1/namespaces/team-a/pods?watch=true")
	do(ctx, "/api/v1/namespaces/team-a/pods/orders-1/log?container=postgres")
	parent.End()

	var client1 sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if strings.HasPrefix(s.Name(), "kube ") {
			if client1 != nil {
				t.Fatalf("more than one client span: %s", s.Name())
			}
			client1 = s
		}
	}
	if client1 == nil {
		t.Fatal("no client span for the traced call")
	}
	if client1.Name() != "kube GET /api/v1/namespaces/{namespace}/pods/{name}/log" {
		t.Errorf("name %q", client1.Name())
	}
	if client1.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Error("the client span is not the request's child")
	}
	ns := ""
	for _, kv := range client1.Attributes() {
		if kv.Key == AttrK8sNamespace {
			ns = kv.Value.AsString()
		}
	}
	if ns != "team-a" {
		t.Errorf("k8s.namespace.name %q", ns)
	}
	if len(seen) != 3 || seen[0] != "" || seen[1] != "" || !strings.Contains(seen[2], client1.SpanContext().TraceID().String()) {
		t.Errorf("traceparent sent %q: want only on the traced call", seen)
	}
}
