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

package mcpleader

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/fyannk/pgToolBox/internal/console/auth"
)

func TestPathsMatchByElement(t *testing.T) {
	for _, tc := range []struct {
		path  string
		gated bool
	}{
		{"/mcp", true},
		{"/mcp/confirm", true},
		{"/mcpx", false},
		{"/oauth/token", true},
		{"/oauth/authorize", true},
		{"/oauth/register", true},
		{"/.well-known/oauth-authorization-server", false},
		{"/api/v1/session/grants", true},
		{"/api/v1/session/grants/grant-1", true},
		{"/api/v1/session", false},
		{"/api/v1/assistant-setup", false},
		{"/auth/oidc/callback", false},
		{"/logout", false}, // routed to the leader, not gated
		{"/", false},
	} {
		if got := gated(tc.path); got != tc.gated {
			t.Errorf("gated(%q) = %v, want %v", tc.path, got, tc.gated)
		}
	}
	routed := 0
	for _, p := range Paths {
		if p.Match("/logout") {
			routed++
		}
	}
	if routed != 1 {
		t.Fatalf("/logout matched by %d paths, want 1", routed)
	}
}

func TestGate(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	for _, tc := range []struct {
		name, method, path string
		leading            bool
		status             int
		contentType        string
	}{
		{"leader serves mcp", http.MethodPost, "/mcp", true, http.StatusTeapot, ""},
		{"leader serves token", http.MethodPost, "/oauth/token", true, http.StatusTeapot, ""},
		{"follower refuses mcp", http.MethodPost, "/mcp", false, http.StatusServiceUnavailable, "text/plain; charset=utf-8"},
		{"follower refuses token as OAuth", http.MethodPost, "/oauth/token", false, http.StatusServiceUnavailable, "application/json"},
		{"follower refuses grants as a problem", http.MethodDelete, "/api/v1/session/grants/grant-1", false, http.StatusServiceUnavailable, "application/problem+json"},
		{"follower refuses consent", http.MethodGet, "/oauth/authorize", false, http.StatusServiceUnavailable, "text/plain; charset=utf-8"},
		{"follower passes logout", http.MethodPost, "/logout", false, http.StatusTeapot, ""},
		{"follower passes the rest", http.MethodGet, "/api/v1/clusters", false, http.StatusTeapot, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &Leader{}
			l.serving.Store(tc.leading)
			rec := httptest.NewRecorder()
			l.Gate(ok).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, nil))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			if tc.status != http.StatusServiceUnavailable {
				return
			}
			if rec.Header().Get("Retry-After") != RetryAfter || rec.Header().Get("Content-Type") != tc.contentType {
				t.Fatalf("headers = %v", rec.Header())
			}
			switch tc.contentType {
			case "application/json":
				var body map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] != "temporarily_unavailable" {
					t.Fatalf("body = %s", rec.Body)
				}
			case "application/problem+json":
				var p auth.Problem
				if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || p.Status != http.StatusServiceUnavailable {
					t.Fatalf("body = %s", rec.Body)
				}
			}
		})
	}
}

func TestHookRefusesOffTheLeader(t *testing.T) {
	called := 0
	hook := func(context.Context, auth.Client, string) error { called++; return nil }
	l := &Leader{}
	if err := l.Hook(hook)(t.Context(), auth.Client{}, "alice"); !errors.Is(err, ErrNotLeader) || called != 0 {
		t.Fatalf("off the leader: err = %v, called = %d", err, called)
	}
	l.serving.Store(true)
	if err := l.Hook(hook)(t.Context(), auth.Client{}, "alice"); err != nil || called != 1 {
		t.Fatalf("on the leader: err = %v, called = %d", err, called)
	}
}

// countingGrants is a grant index that counts its loads.
type countingGrants struct{ loads atomic.Int32 }

func (g *countingGrants) LoadGrants(context.Context) error { g.loads.Add(1); return nil }
func (g *countingGrants) DropGrants()                      {}

// MCP_CANDIDATE=false: on a Lease nobody holds, a candidate leads at
// once; a replica that is no candidate never leads, never loads the
// index and never writes the Lease.
func TestNotCandidateNeverLeads(t *testing.T) {
	for _, tc := range []struct {
		name         string
		notCandidate bool
	}{{"candidate", false}, {"not a candidate", true}} {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewClientset(&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "console-mcp", Namespace: "team-a"}})
			grants := &countingGrants{}
			l := New(Options{
				Leases: cs.CoordinationV1().Leases("team-a"), Grants: grants, Namespace: "team-a", Lease: "console-mcp",
				Identity: "console-0_abc", NotCandidate: tc.notCandidate, RetryPeriod: 10 * time.Millisecond,
			})
			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()
			done := make(chan struct{})
			go func() { _ = l.Run(ctx); close(done) }()
			deadline := time.Now().Add(250 * time.Millisecond)
			for !l.Leading() && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			leading := l.Leading()
			cancel()
			<-done
			if leading == tc.notCandidate {
				t.Fatalf("leading = %v", leading)
			}
			if !tc.notCandidate {
				return
			}
			if grants.loads.Load() != 0 {
				t.Fatal("a replica that is no candidate loaded the grant index")
			}
			for _, a := range cs.Actions() {
				if a.GetVerb() != "get" && a.GetVerb() != "list" && a.GetVerb() != "watch" {
					t.Fatalf("a replica that is no candidate wrote: %v", a)
				}
			}
		})
	}
}
