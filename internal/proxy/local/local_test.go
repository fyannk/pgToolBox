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

package local

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/fyannk/pgtoolbox/internal/proxy/config"
	"github.com/fyannk/pgtoolbox/internal/proxy/server"
)

const testPassword = "correct-horse-battery-staple"

func testEnv(t *testing.T) *server.Env {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword: %v", err)
	}
	cfg := &config.Config{
		Session: config.SessionConfig{
			CookieName:    "pgtoolbox_session",
			CookieSecrets: []string{"secret-one-abcdefghij"},
			MaxAge:        config.Duration(time.Hour),
		},
		Provider: config.ProviderConfig{Modes: []string{config.ModeLocal}},
		Users: []config.User{
			{Subject: "jane@corp.example", Level: config.LevelDBA, LocalPasswordBcrypt: string(hash)},
		},
		Routes: []config.RouteConfig{
			{PathPrefix: "/", Upstream: "http://127.0.0.1:8082", MinLevel: config.LevelView},
		},
	}
	rt, err := server.BuildRuntime(cfg)
	if err != nil {
		t.Fatalf("BuildRuntime: %v", err)
	}
	return server.NewEnv(rt, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func testMux(t *testing.T, env *server.Env) *http.ServeMux {
	t.Helper()
	p, err := New(env)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := server.New(env)
	p.Register(mux)
	return mux
}

func post(t *testing.T, mux http.Handler, form url.Values, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/auth/local/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestLoginFormRenders(t *testing.T) {
	mux := testMux(t, testEnv(t))
	r := httptest.NewRequest(http.MethodGet, "/auth/local/login?rd=/pgadmin", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `name="password"`) || !strings.Contains(body, `value="/pgadmin"`) {
		t.Fatalf("form missing fields: %s", body)
	}
	if !strings.Contains(body, "<style>") {
		t.Fatal("embedded CSS missing")
	}
}

func TestCorrectPasswordCreatesSession(t *testing.T) {
	env := testEnv(t)
	mux := testMux(t, env)
	w := post(t, mux, url.Values{
		"username": {"Jane@Corp.Example"}, // case-insensitive
		"password": {testPassword},
		"rd":       {"/pgadmin"},
	}, "10.0.0.1:1234")
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	if loc := w.Header().Get("Location"); loc != "/pgadmin" {
		t.Fatalf("Location = %q", loc)
	}
	var sessionCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "pgtoolbox_session" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("no session cookie set")
	}
	if !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("bad cookie flags: %+v", sessionCookie)
	}
	var d struct {
		Subject string `json:"sub"`
		Level   string `json:"lvl"`
	}
	if err := env.Runtime().Codec.Open(sessionCookie.Value, &d); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if d.Subject != "jane@corp.example" || d.Level != "dba" {
		t.Fatalf("session = %+v", d)
	}
}

func TestWrongPasswordRejected(t *testing.T) {
	mux := testMux(t, testEnv(t))
	w := post(t, mux, url.Values{
		"username": {"jane@corp.example"},
		"password": {"wrong"},
	}, "10.0.0.1:1234")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Invalid identity or password") {
		t.Fatal("generic error message missing")
	}
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("session cookie set on failed login")
	}
}

// TestUnknownUserRejected exercises the dummy-hash path: the request must
// fail with the same generic message as a wrong password.
func TestUnknownUserRejected(t *testing.T) {
	mux := testMux(t, testEnv(t))
	w := post(t, mux, url.Values{
		"username": {"mallory@corp.example"},
		"password": {"whatever"},
	}, "10.0.0.2:1234")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Invalid identity or password") {
		t.Fatal("generic error message missing")
	}
}

func TestLockoutAfterFailures(t *testing.T) {
	env := testEnv(t)
	p, err := New(env)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := server.New(env)
	p.Register(mux)
	ip := "10.0.0.3:1234"

	for i := 0; i < maxFailures; i++ {
		w := post(t, mux, url.Values{"username": {"jane@corp.example"}, "password": {"wrong"}}, ip)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d", i, w.Code)
		}
	}
	// Next attempt is rejected before any password check, even with the
	// correct password.
	w := post(t, mux, url.Values{"username": {"jane@corp.example"}, "password": {testPassword}}, ip)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("locked out: status = %d", w.Code)
	}

	// After the window passes, login works again.
	p.limiter.now = func() time.Time { return time.Now().Add(2 * window) }
	w = post(t, mux, url.Values{"username": {"jane@corp.example"}, "password": {testPassword}, "rd": {"/"}}, ip)
	if w.Code != http.StatusFound {
		t.Fatalf("after window: status = %d", w.Code)
	}
}

// TestRedirectPoisoningRejected: the rd parameter is validated exactly
// like the OIDC flow's.
func TestRedirectPoisoningRejected(t *testing.T) {
	mux := testMux(t, testEnv(t))
	for _, rd := range []string{"https://evil.example", "//evil.example", "evil.example"} {
		w := post(t, mux, url.Values{
			"username": {"jane@corp.example"},
			"password": {testPassword},
			"rd":       {rd},
		}, "10.0.0.4:1234")
		if w.Code != http.StatusFound {
			t.Fatalf("rd=%q: status = %d", rd, w.Code)
		}
		if loc := w.Header().Get("Location"); loc != "/" {
			t.Fatalf("rd=%q: Location = %q, want /", rd, loc)
		}
	}
}

// With an identity provider enabled alongside, the login page is still the
// local form — the provider is a button on it, not a redirect past it.
func TestLoginFormOffersExternalProviders(t *testing.T) {
	env := testEnv(t)
	rt := env.Runtime()
	rt.Config.Provider.Modes = []string{config.ModeLocal, config.ModeOIDC}

	mux := testMux(t, env)
	r := httptest.NewRequest(http.MethodGet, "/auth/local/login", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	body := w.Body.String()
	if !strings.Contains(body, `name="password"`) {
		t.Fatalf("local form missing: %s", body)
	}
	if !strings.Contains(body, `href="/auth/oidc/login`) {
		t.Fatalf("SSO button missing: %s", body)
	}
	if server.LoginPath(env) != "/auth/local/login" {
		t.Fatalf("LoginPath = %q, want the local form", server.LoginPath(env))
	}
}

// A provider that failed to start is not offered. The button would lead to
// a handler nobody registered, and the whole point of keeping local
// sign-in alive through an identity provider outage is that the page still
// works.
func TestFailedProviderIsNotOffered(t *testing.T) {
	env := testEnv(t)
	env.Runtime().Config.Provider.Modes = []string{config.ModeLocal, config.ModeOIDC}
	env.Available = []string{config.ModeLocal}

	mux := testMux(t, env)
	r := httptest.NewRequest(http.MethodGet, "/auth/local/login", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if body := w.Body.String(); strings.Contains(body, "/auth/oidc/login") {
		t.Fatalf("a provider that did not start is offered: %s", body)
	}
}
