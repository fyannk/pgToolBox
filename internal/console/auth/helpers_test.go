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

package auth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/authz"
)

// fakeIdP is a minimal OIDC provider and OpenShift OAuth server for tests
// (with refresh and OpenShift discovery): discovery, JWKS with a test RSA key, a token
// endpoint that checks PKCE and mints RS256 ID tokens.
type fakeIdP struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey
	// caFile holds the server's certificate, for the provider's CA option.
	caFile string

	mu            sync.Mutex
	challenge     string // code_challenge seen at /authorize
	nonce         string // nonce seen at /authorize
	badNonce      bool
	email         string
	emailVerified any
	idTokenTTL    time.Duration
	refreshes     int
	refreshFails  bool
	tokens        int
	// subject is the sub claim of every ID token; empty: a new one per
	// code exchange (each sign-in a different person, as a team's are),
	// so the per-person budgets stay out of tests about other bounds.
	subject string
	// authorizedAs and clients record the client of each authorization
	// request and each token request (code or refresh), in order.
	authorizedAs []string
	clients      []string
}

const (
	testClientID     = "pgtoolbox-console"
	testClientSecret = "test-client-secret"
	// The OIDC grant client (OIDC_GRANT_CLIENT_ID) the fake IdP knows too.
	testGrantClientID     = "pgtoolbox-console-grants"
	testGrantClientSecret = "test-grant-client-secret"
)

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{t: t, key: key, email: "jane@corp.example", emailVerified: true, idTokenTTL: 10 * time.Minute}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", f.discovery)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", f.discovery)
	mux.HandleFunc("GET /jwks", f.jwks)
	mux.HandleFunc("POST /token", f.token)
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	f.caFile = filepath.Join(t.TempDir(), "ca.crt")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	if err := os.WriteFile(f.caFile, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
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
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
		"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
		"n": b64url(f.key.N.Bytes()), "e": b64url(big.NewInt(int64(f.key.E)).Bytes()),
	}}})
}

func (f *fakeIdP) signJWT(claims map[string]any) string {
	header := b64url([]byte(`{"alg":"RS256","kid":"k1","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		f.t.Fatal(err)
	}
	input := header + "." + b64url(payload)
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, digest[:])
	if err != nil {
		f.t.Fatal(err)
	}
	return input + "." + b64url(sig)
}

// authorize records what the console sent to the authorization endpoint,
// as a browser following the redirect would deliver it.
func (f *fakeIdP) authorize(t *testing.T, location string) (state string) {
	t.Helper()
	u, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Fatalf("authorization request without PKCE S256: %s", location)
	}
	f.mu.Lock()
	f.challenge, f.nonce = q.Get("code_challenge"), q.Get("nonce")
	f.authorizedAs = append(f.authorizedAs, q.Get("client_id"))
	f.mu.Unlock()
	return q.Get("state")
}

func (f *fakeIdP) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok || (id != testClientID || secret != testClientSecret) && (id != testGrantClientID || secret != testGrantClientSecret) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens++
	f.clients = append(f.clients, id)
	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
		if b64url(sum[:]) != f.challenge {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		// An authorization is redeemed once, as a real IdP's code is.
		f.challenge = ""
		nonce := f.nonce
		if f.badNonce {
			nonce = "wrong"
		}
		f.write(w, id, nonce, "refresh-1", "")
	case "refresh_token":
		if f.refreshFails || !strings.HasPrefix(r.PostFormValue("refresh_token"), "refresh-") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		f.refreshes++
		f.write(w, id, "", fmt.Sprintf("refresh-%d", f.refreshes+1), "person-refreshed")
	default:
		http.Error(w, "grant", http.StatusBadRequest)
	}
}

func (f *fakeIdP) write(w http.ResponseWriter, aud, nonce, refresh, sub string) {
	now := time.Now()
	switch {
	case f.subject != "":
		sub = f.subject
	case sub == "":
		sub = "person-" + strconv.Itoa(f.tokens)
	}
	claims := map[string]any{
		"iss": f.srv.URL, "sub": sub, "aud": aud,
		"exp": now.Add(f.idTokenTTL).Unix(), "iat": now.Unix(), "email": f.email,
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	if f.emailVerified != nil {
		claims["email_verified"] = f.emailVerified
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "opaque-access-" + strconv.Itoa(f.tokens), "token_type": "Bearer", "expires_in": 3600,
		"id_token": f.signJWT(claims), "refresh_token": refresh,
	})
}

// fakeReviewer stands in for the API server's SelfSubjectReview.
type fakeReviewer struct {
	mu    sync.Mutex
	users map[string]authz.User // token -> user; prefix match with "*"
	down  bool
	calls int
}

func (f *fakeReviewer) Review(_ context.Context, token string) (authz.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.down {
		return authz.User{}, errors.New("apiserver unavailable")
	}
	if u, ok := f.users[token]; ok {
		return u, nil
	}
	for prefix, u := range f.users {
		if p, ok := strings.CutSuffix(prefix, "*"); ok && strings.HasPrefix(token, p) {
			return u, nil
		}
	}
	return authz.User{}, ErrTokenRejected
}

type testEnv struct {
	h       *Handler
	mux     *http.ServeMux
	codec   *Codec
	idp     *fakeIdP
	rev     *fakeReviewer
	logs    *bytes.Buffer
	secrets string
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// newTestEnv builds a handler with the given providers against a fake IdP
// and a fake API server. The protected route /api/v1/whoami and the page
// /clusters echo the principal.
func newTestEnv(t *testing.T, providers ...string) *testEnv {
	t.Helper()
	return newTestEnvWith(t, nil, providers...)
}

// newTestEnvWith is newTestEnv with a change to the configuration.
func newTestEnvWith(t *testing.T, tweak func(*Config), providers ...string) *testEnv {
	t.Helper()
	idp := newFakeIdP(t)
	secretFile := writeFile(t, testClientSecret)
	cfg := Config{
		Providers:    providers,
		CookieSecure: true,
		MaxAge:       limits.MaxAge,
		IdleTimeout:  limits.IdleTimeout,
		PublicURL:    "https://c.example",
		IdPTimeout:   5 * time.Second,
		OIDC: OIDCConfig{
			IssuerURL: idp.srv.URL, ClientID: testClientID, ClientSecretFile: secretFile,
			Scopes: []string{"openid", "email"}, CAFile: idp.caFile, RequireEmailVerified: true,
		},
		OpenShift: OpenShiftConfig{
			ClientID: testClientID, ClientSecretFile: secretFile, Scopes: []string{"user:full"},
			DiscoveryURL: idp.srv.URL + "/.well-known/oauth-authorization-server", CAFile: idp.caFile,
		},
	}
	if tweak != nil {
		tweak(&cfg)
	}
	logs := &bytes.Buffer{}
	rev := &fakeReviewer{users: map[string]authz.User{
		"eyJ*":            {Name: "oidc:jane@corp.example", Groups: []string{"dba", "system:authenticated"}},
		"opaque-access-*": {Name: "jane", Groups: []string{"system:authenticated:oauth"}},
		"sa-token":        {Name: "system:serviceaccount:team-a:pgtoolbox-console-breakglass"},
	}}
	codec := testCodec(t, true)
	h, err := New(t.Context(), Options{
		Config: cfg, Codec: codec, Identity: rev,
		Revocations: NewRevocations(nil, "", nil),
		Logger:      slog.New(slog.NewJSONHandler(logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.Register(mux)
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := PrincipalFrom(r.Context())
		_, _ = fmt.Fprintf(w, "%s|%s", p.User.Name, p.Provider)
	})
	mux.Handle("/api/v1/whoami", h.Require(echo))
	mux.Handle("/clusters", h.Require(echo))
	mux.Handle(SessionPath, h.Require(echo))
	return &testEnv{h: h, mux: mux, codec: codec, idp: idp, rev: rev, logs: logs, secrets: secretFile}
}

// browser carries cookies between requests the way a browser on
// https://c.example would.
type browser struct {
	t   *testing.T
	env *testEnv
	jar map[string]*http.Cookie
	// addr is the client's address (host:port); "" keeps httptest's.
	addr string
}

func (e *testEnv) browser(t *testing.T) *browser {
	return &browser{t: t, env: e, jar: map[string]*http.Cookie{}}
}

func (b *browser) do(method, target string, body url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	b.t.Helper()
	var r *http.Request
	if body != nil {
		r = httptest.NewRequestWithContext(context.Background(), method, "https://c.example"+target, strings.NewReader(body.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequestWithContext(context.Background(), method, "https://c.example"+target, nil)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	if b.addr != "" {
		r.RemoteAddr = b.addr
	}
	for _, c := range b.jar {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	b.env.mux.ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.jar, c.Name)
		} else {
			b.jar[c.Name] = c
		}
	}
	return w
}

func (b *browser) get(target string) *httptest.ResponseRecorder {
	return b.do(http.MethodGet, target, nil, nil)
}

var sameOriginForm = map[string]string{"Origin": "https://c.example", "Sec-Fetch-Site": "same-origin"}

// oidcLogin runs the full code flow and returns the callback response.
func (b *browser) oidcLogin(rd string) *httptest.ResponseRecorder {
	b.t.Helper()
	w := b.get("/auth/oidc/login?rd=" + url.QueryEscape(rd))
	if w.Code != http.StatusFound {
		b.t.Fatalf("login start = %d", w.Code)
	}
	state := b.env.idp.authorize(b.t, w.Header().Get("Location"))
	return b.get("/auth/oidc/callback?code=c1&state=" + url.QueryEscape(state))
}
