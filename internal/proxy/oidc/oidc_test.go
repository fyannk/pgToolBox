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

package oidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/fyannk/pgtoolbox/internal/proxy/config"
	"github.com/fyannk/pgtoolbox/internal/proxy/server"
)

const (
	testClientID     = "pgtoolbox"
	testClientSecret = "test-client-secret"
	testSubject      = "jane@corp.example"
)

// fakeIdP is a minimal OIDC provider for tests: discovery document, JWKS
// with a test RSA key, and a token endpoint that mints hand-signed RS256
// ID tokens. Test tokens are signed with stdlib crypto only.
type fakeIdP struct {
	t     *testing.T
	srv   *httptest.Server
	key   *rsa.PrivateKey
	kid   string
	email string
	// badNonce makes the token endpoint sign a wrong nonce.
	badNonce bool
	// expectedNonce is set by the test before the callback: the token
	// endpoint cannot see the nonce (it travels in the auth redirect),
	// so the test hands it over out of band.
	expectedNonce string
	// sawVerifier records the PKCE verifier received at the token
	// endpoint.
	sawVerifier string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	f := &fakeIdP{t: t, key: key, kid: "test-key", email: testSubject}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", f.discovery)
	mux.HandleFunc("GET /jwks", f.jwks)
	mux.HandleFunc("POST /token", f.token)
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		// The proxy only redirects here; a real user agent would carry
		// on. Tests inspect the redirect instead of calling this.
		w.WriteHeader(http.StatusOK)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIdP) discovery(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuer":                 f.srv.URL,
		"authorization_endpoint": f.srv.URL + "/authorize",
		"token_endpoint":         f.srv.URL + "/token",
		"jwks_uri":               f.srv.URL + "/jwks",
	})
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (f *fakeIdP) jwks(w http.ResponseWriter, _ *http.Request) {
	e := big.NewInt(int64(f.key.E))
	_ = json.NewEncoder(w).Encode(map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA",
			"kid": f.kid,
			"use": "sig",
			"alg": "RS256",
			"n":   b64url(f.key.N.Bytes()),
			"e":   b64url(e.Bytes()),
		}},
	})
}

// signJWT mints an RS256 token with the given claims.
func (f *fakeIdP) signJWT(claims map[string]any) string {
	header := b64url([]byte(fmt.Sprintf(`{"alg":"RS256","kid":"%s","typ":"JWT"}`, f.kid)))
	payload, err := json.Marshal(claims)
	if err != nil {
		f.t.Fatalf("marshal claims: %v", err)
	}
	input := header + "." + b64url(payload)
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, digest[:])
	if err != nil {
		f.t.Fatalf("sign: %v", err)
	}
	return input + "." + b64url(sig)
}

func (f *fakeIdP) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if got := r.PostFormValue("grant_type"); got != "authorization_code" {
		http.Error(w, "bad grant_type "+got, http.StatusBadRequest)
		return
	}
	// PKCE: the proxy must send a verifier; record it so tests can
	// match it against the challenge sent to /authorize.
	f.sawVerifier = r.PostFormValue("code_verifier")
	if f.sawVerifier == "" {
		http.Error(w, "missing code_verifier", http.StatusBadRequest)
		return
	}
	// Client authentication via basic auth.
	if id, secret, ok := r.BasicAuth(); !ok || id != testClientID || secret != testClientSecret {
		http.Error(w, "bad client auth", http.StatusUnauthorized)
		return
	}
	now := time.Now()
	signedNonce := f.expectedNonce
	if f.badNonce {
		signedNonce = "wrong-nonce"
	}
	idToken := f.signJWT(map[string]any{
		"iss":   f.srv.URL,
		"sub":   "12345",
		"aud":   testClientID,
		"exp":   now.Add(10 * time.Minute).Unix(),
		"iat":   now.Unix(),
		"nonce": signedNonce,
		"email": f.email,
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "test-access-token",
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     idToken,
	})
}

// testEnv builds a proxy env wired to the fake IdP.
func testEnv(t *testing.T, idp *fakeIdP, users []config.User) *server.Env {
	t.Helper()
	cfg := &config.Config{
		Session: config.SessionConfig{
			CookieName:    "pgtoolbox_session",
			CookieSecrets: []string{"secret-one-abcdefghij"},
			MaxAge:        config.Duration(time.Hour),
		},
		Provider: config.ProviderConfig{
			Modes: []string{config.ModeOIDC},
			OIDC: &config.OIDCConfig{
				IssuerURL:    idp.srv.URL,
				ClientID:     testClientID,
				RedirectURL:  "http://proxy.example/auth/oidc/callback",
				Scopes:       []string{"openid", "email"},
				SubjectClaim: "email",
			},
		},
		Users: users,
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

func testProvider(t *testing.T, env *server.Env) (*Provider, *http.ServeMux) {
	t.Helper()
	cfg := env.Runtime().Config.Provider.OIDC
	p, err := New(context.Background(), env, cfg, testClientSecret)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := server.New(env)
	p.Register(mux)
	return p, mux
}

// loginResult captures what handleLogin produced.
type loginResult struct {
	authURL   *url.URL
	state     string
	nonce     string
	challenge string
	cookies   []*http.Cookie
}

func startLogin(t *testing.T, mux http.Handler, rd string) loginResult {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/auth/oidc/login?rd="+url.QueryEscape(rd), nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("login: status = %d, body = %s", w.Code, w.Body)
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("login: bad Location: %v", err)
	}
	q := loc.Query()
	if q.Get("client_id") != testClientID {
		t.Fatalf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("response_type") != "code" {
		t.Fatalf("response_type = %q", q.Get("response_type"))
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Fatalf("code_challenge_method = %q", q.Get("code_challenge_method"))
	}
	if q.Get("nonce") == "" || q.Get("state") == "" {
		t.Fatal("state and nonce must be present in the auth URL")
	}
	return loginResult{
		authURL:   loc,
		state:     q.Get("state"),
		nonce:     q.Get("nonce"),
		challenge: q.Get("code_challenge"),
		cookies:   w.Result().Cookies(),
	}
}

func callback(t *testing.T, mux http.Handler, state, code string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?state="+url.QueryEscape(state)+"&code="+url.QueryEscape(code), nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestFullFlow(t *testing.T) {
	idp := newFakeIdP(t)
	env := testEnv(t, idp, []config.User{{Subject: testSubject, Level: config.LevelPowerUser}})
	_, mux := testProvider(t, env)

	login := startLogin(t, mux, "/pgadmin/settings")
	idp.expectedNonce = login.nonce

	w := callback(t, mux, login.state, "the-code", login.cookies)
	if w.Code != http.StatusFound {
		t.Fatalf("callback: status = %d, body = %s", w.Code, w.Body)
	}
	if loc := w.Header().Get("Location"); loc != "/pgadmin/settings" {
		t.Fatalf("callback: Location = %q", loc)
	}

	// PKCE: verifier at the token endpoint matches the challenge.
	sum := sha256.Sum256([]byte(idp.sawVerifier))
	if got := b64url(sum[:]); got != login.challenge {
		t.Fatalf("PKCE mismatch: challenge %q, verifier hashes to %q", login.challenge, got)
	}

	// Session cookie carries subject and level.
	var sessionValue string
	for _, c := range w.Result().Cookies() {
		if c.Name == "pgtoolbox_session" {
			sessionValue = c.Value
		}
		if c.Name == "pgtoolbox_session_oauth" && c.MaxAge >= 0 {
			t.Fatal("transient cookie was not cleared")
		}
	}
	if sessionValue == "" {
		t.Fatal("no session cookie")
	}
	var d struct {
		Subject string `json:"sub"`
		Level   string `json:"lvl"`
	}
	if err := env.Runtime().Codec.Open(sessionValue, &d); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if d.Subject != testSubject || d.Level != "poweruser" {
		t.Fatalf("session = %+v", d)
	}
}

func TestUnknownUserGetsNoneLevelSession(t *testing.T) {
	idp := newFakeIdP(t)
	idp.email = "stranger@corp.example"
	env := testEnv(t, idp, []config.User{{Subject: testSubject, Level: config.LevelDBA}})
	_, mux := testProvider(t, env)

	login := startLogin(t, mux, "/")
	idp.expectedNonce = login.nonce
	w := callback(t, mux, login.state, "the-code", login.cookies)
	if w.Code != http.StatusFound {
		t.Fatalf("callback: status = %d", w.Code)
	}
	var sessionValue string
	for _, c := range w.Result().Cookies() {
		if c.Name == "pgtoolbox_session" {
			sessionValue = c.Value
		}
	}
	var d struct {
		Subject string `json:"sub"`
		Level   string `json:"lvl"`
	}
	if err := env.Runtime().Codec.Open(sessionValue, &d); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if d.Subject != "stranger@corp.example" || d.Level != "" {
		t.Fatalf("session = %+v, want empty level", d)
	}
}

func TestStateMismatchRejected(t *testing.T) {
	idp := newFakeIdP(t)
	env := testEnv(t, idp, nil)
	_, mux := testProvider(t, env)

	login := startLogin(t, mux, "/")
	w := callback(t, mux, "not-the-state", "the-code", login.cookies)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "pgtoolbox_session" && c.MaxAge >= 0 {
			t.Fatal("session cookie set despite state mismatch")
		}
	}
}

func TestNonceMismatchRejected(t *testing.T) {
	idp := newFakeIdP(t)
	idp.badNonce = true
	env := testEnv(t, idp, nil)
	_, mux := testProvider(t, env)

	login := startLogin(t, mux, "/")
	idp.expectedNonce = login.nonce
	w := callback(t, mux, login.state, "the-code", login.cookies)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestMissingTransientCookieRejected(t *testing.T) {
	idp := newFakeIdP(t)
	env := testEnv(t, idp, nil)
	_, mux := testProvider(t, env)
	w := callback(t, mux, "whatever", "the-code", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestAbsoluteRedirectPoisoningRejected(t *testing.T) {
	for _, rd := range []string{"https://evil.example/steal", "//evil.example", `/\evil.example`} {
		t.Run(rd, func(t *testing.T) {
			idp := newFakeIdP(t)
			env := testEnv(t, idp, []config.User{{Subject: testSubject, Level: config.LevelView}})
			_, mux := testProvider(t, env)

			login := startLogin(t, mux, rd)
			idp.expectedNonce = login.nonce
			w := callback(t, mux, login.state, "the-code", login.cookies)
			if w.Code != http.StatusFound {
				t.Fatalf("status = %d", w.Code)
			}
			if loc := w.Header().Get("Location"); loc != "/" {
				t.Fatalf("Location = %q, want /", loc)
			}
		})
	}
}
