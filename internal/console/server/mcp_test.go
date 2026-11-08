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

	"github.com/fyannk/pgToolBox/internal/console/sse"
)

// mounter is an MCP endpoint stand-in.
type mounter struct{}

func (mounter) Register(mux *http.ServeMux) {
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "mcp") })
}

func TestMCPRoutesBypassCSRFOnly(t *testing.T) {
	t.Parallel()
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) })
	h := New(Options{Auth: testAuth(t), API: api, UI: api, Ready: &Readiness{}, Broker: sse.NewBroker(), MCP: mounter{}}).Handler()
	// An MCP client sends no Origin: the CSRF check would refuse it.
	w := get(h, http.MethodPost, "/mcp", nil)
	if w.Code != http.StatusOK || w.Body.String() != "mcp" {
		t.Fatalf("POST /mcp = %d %q", w.Code, w.Body)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Content-Security-Policy") == "" {
		t.Errorf("/mcp lost the security headers: %v", w.Header())
	}
	// Everything else is still behind the CSRF check.
	if w := get(h, http.MethodPost, "/api/v1/x", nil); w.Code != http.StatusForbidden {
		t.Errorf("POST /api/v1/x without Origin = %d", w.Code)
	}
	if w := get(h, http.MethodPost, "/mcp/x", nil); w.Code != http.StatusForbidden {
		t.Errorf("POST /mcp/x is not the endpoint and must stay behind CSRF: %d", w.Code)
	}
	// Without an endpoint, /mcp is an ordinary path behind the CSRF check.
	if w := get(testServer(t, &Readiness{}).Handler(), http.MethodPost, "/mcp", nil); w.Code != http.StatusForbidden {
		t.Errorf("POST /mcp with MCP off = %d", w.Code)
	}
}
