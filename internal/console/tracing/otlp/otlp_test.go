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

package otlp

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/fyannk/pgToolBox/internal/console/config"
)

// Off builds nothing: no tracing, no exporter goroutine.
func TestOffBuildsNothing(t *testing.T) {
	before := runtime.NumGoroutine()
	tr, shutdown, err := New(t.Context(), config.Tracing{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if tr != nil || tr.Enabled() {
		t.Error("tracing is on")
	}
	if err := shutdown(t.Context()); err != nil {
		t.Error(err)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("%d goroutines after, %d before", after, before)
	}
}

func lookupOf(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "headers")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The exporter posts OTLP/HTTP to the configured endpoint with the
// environment's headers overridden by the mounted file's.
func TestExportsWithTheFileHeaders(t *testing.T) {
	var mu sync.Mutex
	var got http.Header
	var path string
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got, path = r.Header.Clone(), r.URL.Path
		mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(collector.Close)
	t.Setenv(config.EnvOTLPEndpoint, collector.URL)
	t.Setenv(config.EnvOTLPHeaders, "x-api-key=from-env,a=1")
	file := writeFile(t, "x-api-key=from%20file\nLangsmith-Project=pg\n")
	env := map[string]string{config.EnvOTLPHeaders: "x-api-key=from-env,a=1"}
	var logs bytes.Buffer
	tr, shutdown, err := New(t.Context(), config.Tracing{Enabled: true, Endpoint: collector.URL, HeadersFile: file},
		Options{Version: "1.3.0", Lookup: lookupOf(env), Log: slog.New(slog.NewJSONHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	_, span := tr.Tracer().Start(t.Context(), "probe")
	span.End()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if path != "/v1/traces" {
		t.Fatalf("posted to %q, want /v1/traces", path)
	}
	for k, want := range map[string]string{"X-Api-Key": "from file", "Langsmith-Project": "pg", "A": "1"} {
		if v := got.Get(k); v != want {
			t.Errorf("header %s = %q, want %q", k, v, want)
		}
	}
	if strings.Contains(logs.String(), "from file") || strings.Contains(logs.String(), "from-env") {
		t.Errorf("a header value reached the log: %s", logs.String())
	}
}

func TestResource(t *testing.T) {
	exp := keep{tracetest.NewInMemoryExporter()}
	t.Setenv(config.EnvOTelServiceName, "")
	t.Setenv(config.EnvOTelResourceAttributes, "k8s.namespace.name=team-a")
	tr, shutdown, err := New(t.Context(), config.Tracing{Enabled: true}, Options{Version: "1.3.0", Exporter: exp})
	if err != nil {
		t.Fatal(err)
	}
	_, span := tr.Tracer().Start(t.Context(), "probe")
	span.End()
	if err := shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("%d spans", len(spans))
	}
	attrs := map[string]string{}
	for _, kv := range spans[0].Resource.Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	for k, want := range map[string]string{"service.name": "pgtoolbox-console", "service.version": "1.3.0", "k8s.namespace.name": "team-a"} {
		if attrs[k] != want {
			t.Errorf("resource %s = %q, want %q", k, attrs[k], want)
		}
	}
}

func TestServiceNameFromTheEnvironment(t *testing.T) {
	exp := keep{tracetest.NewInMemoryExporter()}
	t.Setenv(config.EnvOTelServiceName, "pgtoolbox-console-team-a")
	tr, shutdown, err := New(t.Context(), config.Tracing{Enabled: true}, Options{Exporter: exp})
	if err != nil {
		t.Fatal(err)
	}
	_, span := tr.Tracer().Start(t.Context(), "probe")
	span.End()
	_ = shutdown(t.Context())
	for _, kv := range exp.GetSpans()[0].Resource.Attributes() {
		if kv.Key == "service.name" && kv.Value.AsString() != "pgtoolbox-console-team-a" {
			t.Errorf("service.name %q", kv.Value.AsString())
		}
	}
}

func TestHeadersFileErrors(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"missing":       filepath.Join(t.TempDir(), "absent"),
		"empty":         writeFile(t, "\n"),
		"not key=value": writeFile(t, "s3cr3t-value"),
		"too large":     writeFile(t, "k="+strings.Repeat("v", maxHeadersFile)),
	}
	for name, file := range cases {
		_, err := mergedHeaders(file, lookupOf(nil))
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		if strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("%s: the error quotes the file: %v", name, err)
		}
	}
}

func TestErrorLogIsRateLimited(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	e := &errorLog{log: slog.New(slog.NewJSONHandler(&logs, nil)), every: time.Hour}
	for range 5 {
		e.Handle(os.ErrDeadlineExceeded)
	}
	if n := strings.Count(logs.String(), "tracing export failed"); n != 1 {
		t.Errorf("%d lines, want 1", n)
	}
}

// keep is the in-memory exporter without its Shutdown, which forgets.
type keep struct{ *tracetest.InMemoryExporter }

func (keep) Shutdown(context.Context) error { return nil }
