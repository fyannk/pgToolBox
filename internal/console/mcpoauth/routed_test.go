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

package mcpoauth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/fyannk/pgToolBox/internal/console/tracing"
)

// The authorization server's requests are named by their route, not by the
// coarse /oauth/* class.
func TestRoutedNamesTheSpan(t *testing.T) {
	t.Parallel()
	rec := tracetest.NewSpanRecorder()
	tr := tracing.New(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)), false)
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+PathToken, routed(PathToken, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	tr.Handler(mux, nil).ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodPost, PathToken, nil))
	spans := rec.Ended()
	if len(spans) != 1 || spans[0].Name() != "POST "+PathToken {
		t.Fatalf("spans = %v, want one named %q", spans, "POST "+PathToken)
	}
}
