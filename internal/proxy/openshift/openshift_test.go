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

package openshift

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgtoolbox/internal/proxy/config"
	"github.com/fyannk/pgtoolbox/internal/proxy/server"
)

func testEnv(t *testing.T, users []config.User) *server.Env {
	t.Helper()
	cfg := &config.Config{
		Session: config.SessionConfig{
			CookieName:    "pgtoolbox_session",
			CookieSecrets: []string{"secret-one-abcdefghij"},
			MaxAge:        config.Duration(time.Hour),
		},
		Provider: config.ProviderConfig{Mode: config.ModeOpenShift},
		Users:    users,
	}
	rt, err := server.BuildRuntime(cfg)
	if err != nil {
		t.Fatalf("BuildRuntime: %v", err)
	}
	return server.NewEnv(rt, discardLogger())
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func secretFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	return path
}

func TestNewRejectsDiscoveryWithoutEndpoints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{})
	}))
	defer server.Close()

	env := testEnv(t, nil)
	cfg := &config.OpenShiftConfig{
		ClientID:         "test-client",
		ClientSecretFile: secretFile(t, "token"),
		RedirectURL:      "http://proxy/auth/openshift/callback",
		DiscoveryURL:     server.URL + "/.well-known/oauth-authorization-server",
		APIURL:           server.URL,
	}
	if _, err := New(context.Background(), env, cfg); err == nil {
		t.Fatalf("discovery without endpoints must fail")
	}
}

func TestLoginAndCallback(t *testing.T) {
	var tokenCalls int
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 server.URL,
			"authorization_endpoint": server.URL + "/oauth/authorize",
			"token_endpoint":         server.URL + "/oauth/token",
		})
	})
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		tokenCalls++
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse token form: %v", err)
		}
		if r.FormValue("code") != "the-code" {
			t.Errorf("code = %q", r.FormValue("code"))
		}
		if r.FormValue("code_verifier") == "" {
			t.Errorf("code_verifier missing")
		}
		secret := r.FormValue("client_secret")
		if secret == "" {
			if user, pass, ok := r.BasicAuth(); ok && user == "test-client" {
				secret = pass
			}
		}
		if secret != "the-secret" {
			t.Errorf("client_secret = %q", secret)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "user-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	})
	mux.HandleFunc("GET /apis/user.openshift.io/v1/users/~", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer user-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"apiVersion": "user.openshift.io/v1",
			"kind":       "User",
			"metadata":   map[string]any{"name": "alice"},
		})
	})

	users := []config.User{{Subject: "alice", Level: config.LevelDBA}}
	env := testEnv(t, users)
	cfg := &config.OpenShiftConfig{
		ClientID:         "test-client",
		ClientSecretFile: secretFile(t, "the-secret"),
		RedirectURL:      "http://proxy/auth/openshift/callback",
		DiscoveryURL:     server.URL + "/.well-known/oauth-authorization-server",
		APIURL:           server.URL,
	}
	provider, err := New(context.Background(), env, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	proxyMux := http.NewServeMux()
	provider.Register(proxyMux)

	// Login redirects to the authorization endpoint and seals the transient cookie.
	loginReq := httptest.NewRequest(http.MethodGet, "/auth/openshift/login?rd=/", nil)
	loginW := httptest.NewRecorder()
	proxyMux.ServeHTTP(loginW, loginReq)
	if loginW.Code != http.StatusFound {
		t.Fatalf("login status = %d, body = %s", loginW.Code, loginW.Body.String())
	}
	authLocation := loginW.Header().Get("Location")
	if !strings.HasPrefix(authLocation, server.URL+"/oauth/authorize") {
		t.Fatalf("login redirect = %q", authLocation)
	}
	authURL, err := url.Parse(authLocation)
	if err != nil {
		t.Fatalf("parse auth URL: %v", err)
	}
	if authURL.Query().Get("state") == "" {
		t.Fatalf("auth URL has no state")
	}
	if authURL.Query().Get("code_challenge") == "" {
		t.Fatalf("auth URL has no PKCE challenge")
	}
	transientCookie := findCookie(loginW.Result().Cookies(), "pgtoolbox_session_oauth")
	if transientCookie == nil {
		t.Fatalf("transient cookie missing")
	}

	// Callback exchanges the code, resolves the user, and mints the session.
	callbackPath := "/auth/openshift/callback?code=the-code&state=" + authURL.Query().Get("state")
	callbackReq := httptest.NewRequest(http.MethodGet, callbackPath, nil)
	callbackReq.AddCookie(transientCookie)
	callbackW := httptest.NewRecorder()
	proxyMux.ServeHTTP(callbackW, callbackReq)
	if callbackW.Code != http.StatusFound {
		t.Fatalf("callback status = %d, body = %s", callbackW.Code, callbackW.Body.String())
	}
	if callbackW.Header().Get("Location") != "/" {
		t.Fatalf("callback redirect = %q", callbackW.Header().Get("Location"))
	}
	if tokenCalls != 1 {
		t.Fatalf("token calls = %d", tokenCalls)
	}

	sessionCookie := findCookie(callbackW.Result().Cookies(), "pgtoolbox_session")
	if sessionCookie == nil {
		t.Fatalf("session cookie missing")
	}
	readReq := httptest.NewRequest(http.MethodGet, "/", nil)
	readReq.AddCookie(sessionCookie)
	sess, err := env.Runtime().Codec.ReadCookie(readReq, "pgtoolbox_session")
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	if sess.Subject != "alice" {
		t.Fatalf("session subject = %q", sess.Subject)
	}
	if sess.Level != string(config.LevelDBA) {
		t.Fatalf("session level = %q", sess.Level)
	}
}

func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}
