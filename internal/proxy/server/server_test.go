/*
Copyright © contributors to the pgtoolbox project.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fyannk/pgtoolbox/internal/proxy/config"
	"github.com/fyannk/pgtoolbox/internal/proxy/session"
)

// upstreamRecorder is a fake upstream capturing the identity headers it
// receives.
type upstreamRecorder struct {
	srv *httptest.Server
	mu  sync.Mutex
	hdr http.Header
}

func newUpstreamRecorder(t *testing.T) *upstreamRecorder {
	t.Helper()
	u := &upstreamRecorder{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.hdr = r.Header.Clone()
		u.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream-ok"))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstreamRecorder) lastHeaders() http.Header {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hdr.Clone()
}

func testRuntime(t *testing.T, upstreamURL string, users []config.User) *Runtime {
	t.Helper()
	cfg := &config.Config{
		Session: config.SessionConfig{
			CookieName:    "pgtoolbox_session",
			CookieSecrets: []string{"secret-one-abcdefghij"},
			MaxAge:        config.Duration(time.Hour),
		},
		Provider: config.ProviderConfig{Modes: []string{config.ModeLocal}},
		Users:    users,
		Routes: []config.RouteConfig{
			{PathPrefix: "/pgadmin", Upstream: upstreamURL, MinLevel: config.LevelDBA},
			{PathPrefix: "/ops", Upstream: upstreamURL, MinLevel: config.LevelPowerUser},
			{PathPrefix: "/", Upstream: upstreamURL, MinLevel: config.LevelView},
		},
		AccessRequest: config.AccessRequestConfig{
			Enabled:     true,
			ConsoleName: "my-console",
			Namespace:   "my-ns",
		},
	}
	rt, err := BuildRuntime(cfg)
	if err != nil {
		t.Fatalf("BuildRuntime: %v", err)
	}
	return rt
}

func testEnv(t *testing.T, rt *Runtime) *Env {
	t.Helper()
	return NewEnv(rt, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// sessionCookie mints a session cookie directly with the runtime codec.
func sessionCookie(t *testing.T, rt *Runtime, subject, level string) *http.Cookie {
	t.Helper()
	d := rt.Codec.NewData(subject, level, config.ModeLocal, time.Hour)
	v, err := rt.Codec.Seal(d)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return &http.Cookie{Name: "pgtoolbox_session", Value: v}
}

func TestAuthzMatrix(t *testing.T) {
	up := newUpstreamRecorder(t)
	rt := testRuntime(t, up.srv.URL, nil)
	env := testEnv(t, rt)
	env.AccessRequests = &fakeCreator{}
	mux := New(env)

	levels := []string{"view", "poweruser", "dba", ""} // "" = unknown user
	routes := []struct {
		path     string
		minLevel string
	}{
		{"/pgadmin/servers", "dba"},
		{"/ops/restart", "poweruser"},
		{"/dashboard", "view"},
	}
	want := map[string]map[string]bool{
		"view":      {"/dashboard": true},
		"poweruser": {"/ops/restart": true, "/dashboard": true},
		"dba":       {"/pgadmin/servers": true, "/ops/restart": true, "/dashboard": true},
		"":          {},
	}
	for _, level := range levels {
		for _, route := range routes {
			name := level + "→" + route.path
			if level == "" {
				name = "unknown→" + route.path
			}
			t.Run(name, func(t *testing.T) {
				r := httptest.NewRequest(http.MethodGet, route.path, nil)
				r.AddCookie(sessionCookie(t, rt, "user@corp.example", level))
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, r)
				if want[level][route.path] {
					if w.Code != http.StatusOK || w.Body.String() != "upstream-ok" {
						t.Fatalf("status = %d body = %q, want proxied 200", w.Code, w.Body)
					}
				} else {
					if w.Code != http.StatusForbidden {
						t.Fatalf("status = %d, want 403", w.Code)
					}
					body := w.Body.String()
					if level == "" {
						if !strings.Contains(body, "Request access") {
							t.Fatal("unknown user should see the request-access form")
						}
					} else {
						if !strings.Contains(body, "Insufficient privileges") {
							t.Fatal("known user should see the insufficient-privileges page")
						}
						if strings.Contains(body, "Request access") {
							t.Fatal("known user must not see the request-access form")
						}
					}
				}
			})
		}
	}
}

func TestUnauthenticatedRedirectAndUnauthorized(t *testing.T) {
	up := newUpstreamRecorder(t)
	rt := testRuntime(t, up.srv.URL, nil)
	mux := New(testEnv(t, rt))

	// GET without a session redirects to the provider login page with
	// the original target.
	r := httptest.NewRequest(http.MethodGet, "/pgadmin/x?y=1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("GET: status = %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/auth/local/login?rd=") || !strings.Contains(loc, url.QueryEscape("/pgadmin/x?y=1")) {
		t.Fatalf("GET: Location = %q", loc)
	}

	// Non-GET without a session is a plain 401, never a redirect.
	r = httptest.NewRequest(http.MethodPost, "/pgadmin/x", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("POST: status = %d, want 401", w.Code)
	}

	// A forged (garbage) session cookie is treated as unauthenticated.
	r = httptest.NewRequest(http.MethodGet, "/pgadmin/x", nil)
	r.AddCookie(&http.Cookie{Name: "pgtoolbox_session", Value: "garbage"})
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("forged cookie: status = %d, want 302", w.Code)
	}
}

func TestHeaderHygiene(t *testing.T) {
	up := newUpstreamRecorder(t)
	rt := testRuntime(t, up.srv.URL, nil)
	mux := New(testEnv(t, rt))

	r := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	r.AddCookie(sessionCookie(t, rt, "jane@corp.example", "view"))
	// Client tries to forge identity headers.
	r.Header.Set(HeaderUser, "admin@corp.example")
	r.Header.Set(HeaderLevel, "dba")
	r.Header.Set(HeaderGroups, "system:masters")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	got := up.lastHeaders()
	if v := got.Get(HeaderUser); v != "jane@corp.example" {
		t.Fatalf("%s = %q, want session subject", HeaderUser, v)
	}
	if v := got.Get(HeaderLevel); v != "view" {
		t.Fatalf("%s = %q, want session level", HeaderLevel, v)
	}
	if v := got.Get(HeaderGroups); v != "" {
		t.Fatalf("%s = %q, want stripped", HeaderGroups, v)
	}
}

func TestExpiredSessionTreatedAsUnauthenticated(t *testing.T) {
	up := newUpstreamRecorder(t)
	rt := testRuntime(t, up.srv.URL, nil)
	mux := New(testEnv(t, rt))

	d := session.Data{
		Subject: "jane@corp.example",
		Level:   "dba",
		Expiry:  time.Now().Add(-time.Minute).Unix(),
	}
	v, err := rt.Codec.Seal(d)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	r := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	r.AddCookie(&http.Cookie{Name: "pgtoolbox_session", Value: v})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want redirect to login", w.Code)
	}
}

func TestSessionsSurviveReloadWithUnchangedKeys(t *testing.T) {
	up := newUpstreamRecorder(t)
	rt := testRuntime(t, up.srv.URL, nil)
	env := testEnv(t, rt)
	mux := New(env)
	cookie := sessionCookie(t, rt, "jane@corp.example", "dba")

	// Reload: same config → new runtime instance, same keys.
	rt2 := testRuntime(t, up.srv.URL, nil)
	env.Swap(rt2)

	r := httptest.NewRequest(http.MethodGet, "/pgadmin/servers", nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want session to survive reload", w.Code)
	}
}

func TestLogout(t *testing.T) {
	up := newUpstreamRecorder(t)
	rt := testRuntime(t, up.srv.URL, nil)
	mux := New(testEnv(t, rt))

	r := httptest.NewRequest(http.MethodGet, "/logout", nil)
	r.AddCookie(sessionCookie(t, rt, "jane@corp.example", "dba"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d", w.Code)
	}
	var cleared bool
	for _, c := range w.Result().Cookies() {
		if c.Name == "pgtoolbox_session" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("session cookie was not cleared")
	}
	if loc := w.Header().Get("Location"); loc != "/auth/local/login" {
		t.Fatalf("Location = %q", loc)
	}
}

func TestHealthzUnauthenticated(t *testing.T) {
	up := newUpstreamRecorder(t)
	rt := testRuntime(t, up.srv.URL, nil)
	mux := New(testEnv(t, rt))
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "ok" {
		t.Fatalf("status = %d body = %q", w.Code, w.Body)
	}
}

// fakeCreator records access-request creations.
type fakeCreator struct {
	mu    sync.Mutex
	calls []createCall
	err   error
}

type createCall struct {
	console, namespace, subject, message string
}

func (f *fakeCreator) CreateAccessRequest(_ context.Context, consoleName, namespace, subject, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, createCall{consoleName, namespace, subject, message})
	return f.err
}

func (f *fakeCreator) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func TestAccessRequestFlow(t *testing.T) {
	up := newUpstreamRecorder(t)
	rt := testRuntime(t, up.srv.URL, nil)
	env := testEnv(t, rt)
	fc := &fakeCreator{}
	env.AccessRequests = fc
	mux := New(env)

	// Unknown user session (level none).
	d := rt.Codec.NewData("stranger@corp.example", "", config.ModeOIDC, time.Hour)
	v, err := rt.Codec.Seal(d)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	cookie := &http.Cookie{Name: "pgtoolbox_session", Value: v}

	// The denied page embeds the CSRF token bound to this session.
	r := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("denied page: status = %d", w.Code)
	}
	token := rt.Codec.CSRFToken(d)
	if !strings.Contains(w.Body.String(), token) {
		t.Fatal("denied page does not embed the CSRF token")
	}

	// POST with the valid token creates the request.
	form := url.Values{"csrf": {token}, "message": {"need dba for incident"}}
	r = httptest.NewRequest(http.MethodPost, "/auth/access-request", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("POST: status = %d body = %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "Request sent") {
		t.Fatal("confirmation page missing")
	}
	if fc.callCount() != 1 {
		t.Fatalf("creator calls = %d", fc.callCount())
	}
	call := fc.calls[0]
	if call.console != "my-console" || call.namespace != "my-ns" ||
		call.subject != "stranger@corp.example" || call.message != "need dba for incident" {
		t.Fatalf("created request = %+v", call)
	}
}

func TestAccessRequestCSRFRejected(t *testing.T) {
	up := newUpstreamRecorder(t)
	rt := testRuntime(t, up.srv.URL, nil)
	env := testEnv(t, rt)
	fc := &fakeCreator{}
	env.AccessRequests = fc
	mux := New(env)

	cookie := sessionCookie(t, rt, "stranger@corp.example", "")
	for _, form := range []url.Values{
		{"csrf": {"forged-token"}, "message": {"hi"}},
		{"message": {"no token at all"}},
	} {
		r := httptest.NewRequest(http.MethodPost, "/auth/access-request", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("form %v: status = %d, want 403", form, w.Code)
		}
	}
	if fc.callCount() != 0 {
		t.Fatal("creator called despite CSRF rejection")
	}
}

func TestAccessRequestUnauthenticatedRejected(t *testing.T) {
	up := newUpstreamRecorder(t)
	rt := testRuntime(t, up.srv.URL, nil)
	env := testEnv(t, rt)
	env.AccessRequests = &fakeCreator{}
	mux := New(env)

	form := url.Values{"csrf": {"x"}, "message": {"hi"}}
	r := httptest.NewRequest(http.MethodPost, "/auth/access-request", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestAccessRequestDisabledShowsContactAdmin(t *testing.T) {
	up := newUpstreamRecorder(t)
	rt := testRuntime(t, up.srv.URL, nil)
	// No creator: in-cluster config unavailable.
	env := testEnv(t, rt)
	mux := New(env)

	r := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	r.AddCookie(sessionCookie(t, rt, "stranger@corp.example", ""))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	body := w.Body.String()
	if strings.Contains(body, "Request access") && strings.Contains(body, "<form") {
		t.Fatal("form rendered without a creator")
	}
	if !strings.Contains(body, "administrator") {
		t.Fatal("contact-administrator message missing")
	}
}

func TestAccessRequestMessageTruncated(t *testing.T) {
	up := newUpstreamRecorder(t)
	rt := testRuntime(t, up.srv.URL, nil)
	env := testEnv(t, rt)
	fc := &fakeCreator{}
	env.AccessRequests = fc
	mux := New(env)

	d := rt.Codec.NewData("stranger@corp.example", "", config.ModeOIDC, time.Hour)
	v, _ := rt.Codec.Seal(d)
	cookie := &http.Cookie{Name: "pgtoolbox_session", Value: v}
	long := strings.Repeat("é", 2000) // multi-byte runes
	form := url.Values{"csrf": {rt.Codec.CSRFToken(d)}, "message": {long}}
	r := httptest.NewRequest(http.MethodPost, "/auth/access-request", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if got := fc.calls[0].message; len([]rune(got)) != maxMessageRunes {
		t.Fatalf("message runes = %d, want %d", len([]rune(got)), maxMessageRunes)
	}
}

func TestSafeRedirect(t *testing.T) {
	cases := map[string]string{
		"/":                    "/",
		"/pgadmin":             "/pgadmin",
		"/a/b?c=d":             "/a/b?c=d",
		"":                     "/",
		"https://evil.example": "/",
		"//evil.example":       "/",
		"/\\evil.example":      "/",
		"evil.example":         "/",
		"/path\r\nX-Evil: 1":   "/",
	}
	for in, want := range cases {
		if got := SafeRedirect(in); got != want {
			t.Errorf("SafeRedirect(%q) = %q, want %q", in, got, want)
		}
	}
}
