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

package api

import (
	"net/http"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/fyannk/pgToolBox/internal/console/tracing"
)

// An API request's span is named by its operation's path template: the
// Cluster's name is in url.path, never in the span name or http.route.
func TestSpansAreNamedByTheOperationPath(t *testing.T) {
	t.Parallel()
	rec := tracetest.NewSpanRecorder()
	tr := tracing.New(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)), false)
	h, _ := New(Deps{Source: FixtureSource{}, Authz: FixtureAuthorizer{}})
	h = tr.Handler(h, nil)
	if w := call(t, h, FixturePrincipal(), "/api/v1/namespaces/team-a/clusters/orders/instances"); w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("%d spans", len(spans))
	}
	const want = "/api/v1/namespaces/{namespace}/clusters/{name}/instances"
	if got := spans[0].Name(); got != "GET "+want {
		t.Errorf("span %q, want GET %s", got, want)
	}
	attrs := map[string]string{}
	for _, kv := range spans[0].Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if attrs["http.route"] != want {
		t.Errorf("http.route %q", attrs["http.route"])
	}
	if attrs["url.path"] != "/api/v1/namespaces/team-a/clusters/orders/instances" {
		t.Errorf("url.path %q", attrs["url.path"])
	}
}
