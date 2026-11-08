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

// sessionMounter is an MCP endpoint with an OAuth authorization server:
// its token route on the outer mux, its authorization route on the
// session mux.
type sessionMounter struct{}

func (sessionMounter) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "token") })
}

func (sessionMounter) RegisterSession(mux *http.ServeMux) {
	mux.HandleFunc("/oauth/authorize", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "authorize") })
}

func TestOAuthConsentIsBehindCSRF(t *testing.T) {
	t.Parallel()
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) })
	h := New(Options{Auth: testAuth(t), API: api, UI: api, Ready: &Readiness{}, Broker: sse.NewBroker(), MCP: sessionMounter{}}).Handler()
	// The token endpoint is called by a native client, without Origin.
	if w := get(h, http.MethodPost, "/oauth/token", nil); w.Code != http.StatusOK || w.Body.String() != "token" {
		t.Errorf("POST /oauth/token = %d %q", w.Code, w.Body)
	}
	// The consent form is a browser form: a cross-site post is refused.
	if w := get(h, http.MethodPost, "/oauth/authorize", map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}); w.Code != http.StatusForbidden {
		t.Errorf("cross-site consent = %d", w.Code)
	}
	if w := get(h, http.MethodPost, "/oauth/authorize", map[string]string{"Origin": "https://c.example", "Sec-Fetch-Site": "same-origin"}); w.Code != http.StatusOK || w.Body.String() != "authorize" {
		t.Errorf("same-origin consent = %d %q", w.Code, w.Body)
	}
	if w := get(h, http.MethodGet, "/oauth/authorize", nil); w.Code != http.StatusOK {
		t.Errorf("GET /oauth/authorize = %d", w.Code)
	}
}
