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
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/mcpauth"
	"github.com/fyannk/pgToolBox/internal/console/sharedstate"
)

const (
	secretA = "a-session-secret-of-at-least-32-bytes-A"
	secretB = "a-session-secret-of-at-least-32-bytes-B"
)

func setup(t *testing.T) (*clock, *fakeSessions, *replica) {
	t.Helper()
	c := &clock{now: t0}
	f := newSessions(t, c, secretA)
	return c, f, newReplica(t, f, memoryStore(c), c, nil)
}

func TestThePublicURLIsTheIssuer(t *testing.T) {
	c := &clock{now: t0}
	f := newSessions(t, c, secretA)
	for _, bad := range []string{
		"", "console.example.com", "ftp://console.example.com", "https://", "http://console.example.com",
		"https://console.example.com?x=1", "https://console.example.com?", "https://console.example.com#f",
		"https://user@console.example.com", "http://10.0.0.1:8080", "https:opaque",
	} {
		if _, err := New(Options{Sessions: f, Store: memoryStore(c), PublicURL: bad}); !errors.Is(err, ErrIssuer) {
			t.Errorf("public URL %q: %v", bad, err)
		}
	}
	// Plain http only on the person's own computer (and the e2e tests).
	for _, good := range []string{"https://console.example.com/pg", "http://localhost:8080", "http://127.0.0.1", "http://[::1]:9000"} {
		if _, err := New(Options{Sessions: f, Store: memoryStore(c), PublicURL: good}); err != nil {
			t.Errorf("public URL %q: %v", good, err)
		}
	}
	// A client's Host or forwarded headers never change the issuer.
	r := newReplica(t, f, memoryStore(c), c, func(o *Options) { o.PublicURL = console + "/" })
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://evil.example"+PathMetadata, nil)
	req.Header.Set("X-Forwarded-Proto", "http")
	req.Header.Set("X-Forwarded-Host", "evil.example")
	if w := r.do(req); !strings.Contains(w.Body.String(), `"issuer":"`+console+`"`) || strings.Contains(w.Body.String(), "evil") {
		t.Errorf("metadata: %s", w.Body)
	}
}

func TestMetadata(t *testing.T) {
	_, _, r := setup(t)
	w := r.do(httptest.NewRequestWithContext(context.Background(), http.MethodGet, console+PathMetadata, nil))
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	want := map[string]any{
		"issuer": console, "authorization_endpoint": console + PathAuthorize, "token_endpoint": console + PathToken,
		"registration_endpoint": console + PathRegister, "authorization_response_iss_parameter_supported": true,
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
	for k, v := range map[string]string{"code_challenge_methods_supported": "S256", "token_endpoint_auth_methods_supported": "none"} {
		if l, _ := m[k].([]any); len(l) != 1 || l[0] != v {
			t.Errorf("%s = %v", k, m[k])
		}
	}
	if _, ok := m["client_id_metadata_document_supported"]; ok {
		t.Error("CIMD advertised while off")
	}
}

func TestRegistration(t *testing.T) {
	_, _, r := setup(t)
	for _, c := range []struct {
		name, body, code string
	}{
		{"confidential client", `{"redirect_uris":["http://127.0.0.1/cb"],"token_endpoint_auth_method":"client_secret_basic"}`, "invalid_client_metadata"},
		{"client credentials", `{"redirect_uris":["http://127.0.0.1/cb"],"grant_types":["client_credentials"]}`, "invalid_client_metadata"},
		{"implicit", `{"redirect_uris":["http://127.0.0.1/cb"],"response_types":["token"]}`, "invalid_client_metadata"},
		{"web application", `{"redirect_uris":["http://127.0.0.1/cb"],"application_type":"web"}`, "invalid_client_metadata"},
		{"no redirect", `{"redirect_uris":[]}`, "invalid_redirect_uri"},
		{"https redirect", `{"redirect_uris":["https://app.example.com/cb"]}`, "invalid_redirect_uri"},
		{"http non-loopback", `{"redirect_uris":["http://192.168.1.10:8080/cb"]}`, "invalid_redirect_uri"},
		{"loopback look-alike", `{"redirect_uris":["http://127.0.0.1.evil.example/cb"]}`, "invalid_redirect_uri"},
		{"custom scheme", `{"redirect_uris":["cursor://anysphere.cursor-retrieval/oauth"]}`, "invalid_redirect_uri"},
		{"user information", `{"redirect_uris":["http://user@127.0.0.1/cb"]}`, "invalid_redirect_uri"},
		{"fragment", `{"redirect_uris":["http://127.0.0.1/cb#x"]}`, "invalid_redirect_uri"},
		{"not JSON", `redirect_uris=x`, "invalid_client_metadata"},
		{"two objects", `{"redirect_uris":["http://127.0.0.1/cb"]}{}`, "invalid_client_metadata"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w, out := r.register(t, c.body)
			if w.Code != http.StatusBadRequest || out["error"] != c.code {
				t.Errorf("%d %v", w.Code, out)
			}
		})
	}

	// VS Code registers an https redirect beside its loopback one: the
	// loopback one is kept, and the answer says so.
	w, out := r.register(t, `{"client_name":"Visual Studio Code","redirect_uris":["https://vscode.dev/redirect","http://127.0.0.1:33418/"]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if uris, _ := out["redirect_uris"].([]any); len(uris) != 1 || uris[0] != "http://127.0.0.1:33418/" {
		t.Errorf("redirect_uris = %v", out["redirect_uris"])
	}
	if _, ok := out["client_secret"]; ok || out["token_endpoint_auth_method"] != "none" {
		t.Errorf("registration answer = %v", out)
	}
	if id, _ := out["client_id"].(string); !strings.HasPrefix(id, clientIDPrefix) {
		t.Errorf("client_id = %q", id)
	}
}

func TestAuthorizeRefusals(t *testing.T) {
	_, f, r := setup(t)
	id := r.clientID(t)

	// Errors the redirect URI cannot be trusted with are shown, not sent.
	for name, mod := range map[string]func(url.Values){
		"unknown client":        func(q url.Values) { q.Set("client_id", "pgc_cl_forged") },
		"unregistered redirect": func(q url.Values) { q.Set("redirect_uri", "http://127.0.0.1:1/elsewhere") },
		"non-loopback redirect": func(q url.Values) { q.Set("redirect_uri", "https://evil.example/callback") },
		"repeated parameter":    func(q url.Values) { q.Add("state", "again") },
		"metadata document off": func(q url.Values) { q.Set("client_id", "https://client.example/metadata.json") },
	} {
		q := authorizeQuery(id, loopback)
		mod(q)
		if w := r.authorize(q); w.Code != http.StatusBadRequest || w.Header().Get("Location") != "" {
			t.Errorf("%s: %d %s", name, w.Code, w.Header().Get("Location"))
		}
	}

	// Errors of a valid client's request: a page, not a redirect (P4-D: no
	// consent was given, and a redirect would tell the client the person
	// is signed in).
	for name, c := range map[string]struct {
		mod  func(url.Values)
		code string
	}{
		"no PKCE":       {func(q url.Values) { q.Del("code_challenge"); q.Del("code_challenge_method") }, "invalid_request"},
		"plain PKCE":    {func(q url.Values) { q.Set("code_challenge_method", "plain") }, "invalid_request"},
		"no state":      {func(q url.Values) { q.Del("state") }, "invalid_request"},
		"token flow":    {func(q url.Values) { q.Set("response_type", "token") }, "unsupported_response_type"},
		"other target":  {func(q url.Values) { q.Set("resource", "https://other.example.com/mcp") }, "invalid_target"},
		"request param": {func(q url.Values) { q.Set("request", "eyJ") }, "request_not_supported"},
	} {
		q := authorizeQuery(id, loopback)
		c.mod(q)
		w := r.authorize(q)
		if w.Code != http.StatusBadRequest || w.Header().Get("Location") != "" || !strings.Contains(w.Body.String(), "("+c.code+")") {
			t.Errorf("%s: %d %s", name, w.Code, w.Header().Get("Location"))
		}
	}

	// The loopback port is the client's choice at each run.
	q := authorizeQuery(id, "http://127.0.0.1:51000/callback")
	if w := r.authorize(q); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "(unverified)") {
		t.Errorf("another port: %d", w.Code)
	} else if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' http://127.0.0.1:51000;") ||
		!strings.Contains(csp, "frame-ancestors 'none'") || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("consent page headers: %q", csp)
	}

	// No session: sign in, then come back here.
	f.mu.Lock()
	f.session = nil
	f.mu.Unlock()
	w := r.authorize(authorizeQuery(id, loopback))
	if loc := w.Header().Get("Location"); w.Code != http.StatusFound || !strings.HasPrefix(loc, "/auth/login?rd=%2Foauth%2Fauthorize%3F") {
		t.Errorf("no session: %d %s", w.Code, loc)
	}
	// Back from the IdP (a cross-site redirect), a page of this origin
	// continues so the SameSite=Strict cookie is sent.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, console+PathAuthorize+"?"+authorizeQuery(id, loopback).Encode(), nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if w := r.do(req); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `http-equiv="refresh"`) {
		t.Errorf("continue page: %d %s", w.Code, w.Body)
	}
}

func TestDenyAndForeignConsent(t *testing.T) {
	_, f, r := setup(t)
	id := r.clientID(t)
	q := r.consent(t, authorizeQuery(id, loopback), "deny")
	if q.Get("error") != "access_denied" || q.Get("state") != "st-1" || q.Get("code") != "" {
		t.Errorf("deny: %v", q)
	}

	// A consent page answered from another sign-in is refused.
	w := r.authorize(authorizeQuery(id, loopback))
	m := consentRequestField.FindStringSubmatch(w.Body.String())
	f.mu.Lock()
	f.session.Principal.SessionID = "ffffffffffffffffffffffffffffffff"
	f.mu.Unlock()
	form := url.Values{"request": {m[1]}, "decision": {"approve"}}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, console+PathAuthorize, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if w := r.do(req); w.Code != http.StatusForbidden {
		t.Errorf("foreign consent: %d", w.Code)
	}
}

func TestCodeFlowAndAccessToken(t *testing.T) {
	c, f, r := setup(t)
	id := r.clientID(t)
	q := r.consent(t, authorizeQuery(id, loopback), "approve")
	if q.Get("iss") != console || q.Get("state") != "st-1" || !strings.HasPrefix(q.Get("code"), codePrefix) {
		t.Fatalf("approve: %v", q)
	}
	code := q.Get("code")

	for name, mod := range map[string]func(url.Values){
		"wrong verifier":     func(v url.Values) { v.Set("code_verifier", strings.Repeat("x", 43)) },
		"missing verifier":   func(v url.Values) { v.Del("code_verifier") },
		"other client":       func(v url.Values) { v.Set("client_id", r.clientID(t)) },
		"other redirect":     func(v url.Values) { v.Set("redirect_uri", "http://127.0.0.1:9/callback") },
		"other resource":     func(v url.Values) { v.Set("resource", "https://other.example.com/mcp") },
		"forged code":        func(v url.Values) { v.Set("code", codePrefix+"AAAA") },
		"refresh as a code":  func(v url.Values) { v.Set("grant_type", "refresh_token") },
		"client credentials": func(v url.Values) { v.Set("grant_type", "client_credentials") },
	} {
		form := codeForm(id, code)
		mod(form)
		if w, _, e := r.token(t, form); w.Code != http.StatusBadRequest || e == "" {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	// A client that authenticates is not one of this console's.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, console+PathToken, strings.NewReader(codeForm(id, code).Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(id, "secret")
	if w := r.do(req); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid_client") {
		t.Errorf("basic auth: %d %s", w.Code, w.Body)
	}

	w, tok, _ := r.token(t, codeForm(id, code))
	if w.Code != http.StatusOK || !strings.HasPrefix(tok.Access, accessPrefix) || !strings.HasPrefix(tok.Refresh, refreshPrefix) ||
		tok.Scope != ScopeRead || tok.ExpiresIn != int64(time.Hour/time.Second) {
		t.Fatalf("exchange: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "idp-alice") {
		t.Fatal("the IdP token is readable in the token response")
	}
	p, err := r.authenticate(tok.Access)
	if err != nil {
		t.Fatal(err)
	}
	// The grant carries its own IdP sign-in, never the browser session's.
	if p.User.Name != "alice" || p.Provider != auth.GrantProvider || !auth.IsGrantReference(p.SessionID) || p.Token != "idp-alice-g1" ||
		len(p.Scopes) != 1 || p.Scopes[0] != ScopeRead || p.AssistantRedirect != "127.0.0.1:33418" {
		t.Errorf("principal = %+v", p)
	}
	if gs := r.s.Grants(context.Background(), "alice"); len(gs) != 1 || gs[0].ID != p.SessionID || gs[0].Client != "Claude Code" {
		t.Errorf("grants = %+v", gs)
	}

	// Not mine, other audience, expiry.
	if _, err := r.authenticate("eyJhbGciOi.raw.token"); !errors.Is(err, mcpauth.ErrNotMine) {
		t.Errorf("raw token: %v", err)
	}
	if _, err := r.s.Authenticate(mcpauth.WithResource(context.Background(), "https://other.example.com/mcp"), tok.Access); !errors.Is(err, mcpauth.ErrUnauthenticated) {
		t.Errorf("other resource: %v", err)
	}
	// Whatever carries this console's prefix is judged here, never passed
	// on to the raw-token authenticator.
	for _, mine := range []string{tok.Refresh, "pgc_", "pgc_at_forged", "pgc_cl_x"} {
		if _, err := r.authenticate(mine); !errors.Is(err, mcpauth.ErrUnauthenticated) {
			t.Errorf("%.12s... as a bearer: %v", mine, err)
		}
	}
	c.Add(time.Hour)
	if _, err := r.authenticate(tok.Access); !errors.Is(err, auth.ErrBearerExpired) {
		t.Errorf("expired: %v", err)
	}
	_ = f
}

func TestCodeReplayRevokesTheGrant(t *testing.T) {
	_, _, r := setup(t)
	id := r.clientID(t)
	code := r.consent(t, authorizeQuery(id, loopback), "approve").Get("code")
	_, tok, _ := r.token(t, codeForm(id, code))
	if _, err := r.authenticate(tok.Access); err != nil {
		t.Fatal(err)
	}
	if w, _, e := r.token(t, codeForm(id, code)); w.Code != http.StatusBadRequest || e != "invalid_grant" {
		t.Fatalf("replay: %d %s", w.Code, w.Body)
	}
	if _, err := r.authenticate(tok.Access); !errors.Is(err, auth.ErrBearerRevoked) {
		t.Errorf("the replayed code's grant still works: %v", err)
	}
	if w, _, _ := r.token(t, refreshForm(id, tok.Refresh)); w.Code != http.StatusBadRequest {
		t.Errorf("refresh of a revoked grant: %d", w.Code)
	}
}

func TestCodeExpiresWithinAMinute(t *testing.T) {
	c, _, r := setup(t)
	id := r.clientID(t)
	code := r.consent(t, authorizeQuery(id, loopback), "approve").Get("code")
	c.Add(codeTTL)
	if w, _, e := r.token(t, codeForm(id, code)); w.Code != http.StatusBadRequest || e != "invalid_grant" {
		t.Errorf("expired code: %d %s", w.Code, w.Body)
	}
}

func TestGrantFromAnotherKeyIsRefused(t *testing.T) {
	c := &clock{now: t0}
	a := newReplica(t, newSessions(t, c, secretA), memoryStore(c), c, nil)
	b := newReplica(t, newSessions(t, c, secretB), memoryStore(c), c, nil)
	id, tok := a.connect(t)
	if _, err := b.authenticate(tok.Access); !errors.Is(err, mcpauth.ErrUnauthenticated) {
		t.Errorf("access token under another key: %v", err)
	}
	if w, _, _ := b.token(t, refreshForm(id, tok.Refresh)); w.Code != http.StatusBadRequest {
		t.Errorf("refresh token under another key: %d", w.Code)
	}
	if w := b.authorize(authorizeQuery(id, loopback)); w.Code != http.StatusBadRequest {
		t.Errorf("client ID under another key: %d", w.Code)
	}
}

func TestRefreshRotation(t *testing.T) {
	c, f, r := setup(t)
	id, first := r.connect(t)
	c.Add(10 * time.Minute)
	w, second, _ := r.token(t, refreshForm(id, first.Refresh))
	if w.Code != http.StatusOK || second.Refresh == first.Refresh || second.Access == first.Access {
		t.Fatalf("refresh: %d %s", w.Code, w.Body)
	}
	if _, err := r.authenticate(second.Access); err != nil {
		t.Fatal(err)
	}
	// Another client cannot use it.
	if w, _, _ := r.token(t, refreshForm(r.clientID(t), second.Refresh)); w.Code != http.StatusBadRequest {
		t.Errorf("refresh by another client: %d", w.Code)
	}
	// The first refresh token again: one of the holders is not the
	// client, and the grant ends for both.
	if w, _, e := r.token(t, refreshForm(id, first.Refresh)); w.Code != http.StatusBadRequest || e != "invalid_grant" {
		t.Fatalf("reuse: %d %s", w.Code, w.Body)
	}
	if _, err := r.authenticate(second.Access); !errors.Is(err, auth.ErrBearerRevoked) {
		t.Errorf("after reuse: %v", err)
	}
	if w, _, _ := r.token(t, refreshForm(id, second.Refresh)); w.Code != http.StatusBadRequest {
		t.Errorf("the newest refresh token still works after reuse: %d", w.Code)
	}
	_ = f
}

// The IdP token is renewed at the token endpoint when it nears expiry,
// and an access token never outlives the IdP token it carries.
func TestRefreshRenewsTheIdPToken(t *testing.T) {
	c, f, r := setup(t)
	id, first := r.connect(t)
	if first.ExpiresIn != int64(time.Hour/time.Second) {
		t.Fatalf("expires_in = %d", first.ExpiresIn)
	}
	c.Add(59 * time.Minute)
	_, second, _ := r.token(t, refreshForm(id, first.Refresh))
	p, err := r.authenticate(second.Access)
	if err != nil {
		t.Fatal(err)
	}
	if f.refreshes != 1 || p.Token != "idp-alice-1" {
		t.Errorf("IdP refreshes = %d, token %q", f.refreshes, p.Token)
	}
	// The grant ends at the session's maximum age whatever the refreshes.
	for range 12 {
		c.Add(59 * time.Minute)
		var w *httptest.ResponseRecorder
		w, second, _ = r.token(t, refreshForm(id, second.Refresh))
		if w.Code != http.StatusOK {
			if !strings.Contains(w.Body.String(), "expired") {
				t.Fatalf("refresh: %d %s", w.Code, w.Body)
			}
			return
		}
	}
	t.Error("the grant outlived the session's maximum age")
}

func TestRevocationListEndsGrants(t *testing.T) {
	for name, ref := range map[string]func(p string) string{
		"grant ID":       func(p string) string { return p },
		"its session":    func(string) string { return sessionID },
		"the whole user": func(string) string { return "user:alice" },
	} {
		t.Run(name, func(t *testing.T) {
			_, f, r := setup(t)
			id, tok := r.connect(t)
			p, err := r.authenticate(tok.Access)
			if err != nil {
				t.Fatal(err)
			}
			f.revoke(ref(p.SessionID))
			if _, err := r.authenticate(tok.Access); !errors.Is(err, auth.ErrBearerRevoked) {
				t.Errorf("access: %v", err)
			}
			if w, _, _ := r.token(t, refreshForm(id, tok.Refresh)); w.Code != http.StatusBadRequest {
				t.Errorf("refresh: %d", w.Code)
			}
		})
	}
}

func TestTheAPIServerStillDecides(t *testing.T) {
	_, f, r := setup(t)
	id, tok := r.connect(t)
	f.mu.Lock()
	delete(f.tokens, "idp-alice-g1")
	f.mu.Unlock()
	if _, err := r.authenticate(tok.Access); err == nil {
		t.Error("a token the API server rejects authenticated")
	}
	if w, _, _ := r.token(t, refreshForm(id, tok.Refresh)); w.Code != http.StatusBadRequest {
		t.Errorf("refresh with a rejected IdP token: %d", w.Code)
	}
	// The same token now naming someone else is refused too.
	f.mu.Lock()
	f.tokens["idp-alice-g1"] = "mallory"
	f.mu.Unlock()
	if _, err := r.authenticate(tok.Access); !errors.Is(err, mcpauth.ErrUnauthenticated) {
		t.Errorf("token naming another person: %v", err)
	}
}

func TestRevokeGrantFromTheSessionPage(t *testing.T) {
	_, f, r := setup(t)
	_, tok := r.connect(t)
	p, _ := r.authenticate(tok.Access)
	if err := r.s.RevokeGrant(context.Background(), auth.Client{}, "bob", p.SessionID); !errors.Is(err, ErrNoSuchGrant) {
		t.Errorf("bob revoking alice's grant: %v", err)
	}
	if err := r.s.RevokeGrant(context.Background(), auth.Client{}, "alice", p.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := r.s.RevokeGrant(context.Background(), auth.Client{}, "alice", p.SessionID); !errors.Is(err, ErrNoSuchGrant) {
		t.Errorf("second revocation: %v", err)
	}
	if _, err := r.authenticate(tok.Access); !errors.Is(err, auth.ErrBearerRevoked) {
		t.Errorf("after revocation: %v", err)
	}
	if gs := r.s.Grants(context.Background(), "alice"); len(gs) != 0 {
		t.Errorf("revoked grant listed: %+v", gs)
	}
	_ = f
}

func TestPerUserGrantBound(t *testing.T) {
	c, _, r := setup(t)
	r.unthrottled()
	for range sharedstate.MaxGrantsPerUser {
		r.connect(t)
		// Spent codes and consent pages expire; grants stand.
		c.Add(consentTTL + 2*time.Minute)
	}
	if w := r.authorize(authorizeQuery(r.clientID(t), loopback)); w.Code != http.StatusConflict {
		t.Errorf("past the bound: %d", w.Code)
	}
}

func TestMatchRedirect(t *testing.T) {
	reg := []string{"http://127.0.0.1:33418/callback", "http://localhost/cb?x=1"}
	for given, want := range map[string]bool{
		"http://127.0.0.1:1/callback":     true,
		"http://127.0.0.1/callback":       true,
		"http://127.0.0.1:1/callback/":    false,
		"http://127.0.0.1:1/Callback":     false,
		"http://localhost:5/cb?x=1":       true,
		"http://localhost:5/cb?x=2":       false,
		"http://localhost:5/cb":           false,
		"http://[::1]:1/callback":         false,
		"https://127.0.0.1:1/callback":    false,
		"http://127.0.0.1:1/callback#f":   false,
		"http://127.0.0.1:99999/callback": false,
	} {
		if _, got := matchRedirect(reg, given); got != want {
			t.Errorf("matchRedirect(%q) = %v, want %v", given, got, want)
		}
	}
}

func TestBudgets(t *testing.T) {
	_, _, r := setup(t)
	body := `{"redirect_uris":["http://127.0.0.1/cb"]}`
	codes := make([]int, 0, registersBurst+1)
	for range registersBurst + 1 {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, console+PathRegister, strings.NewReader(body))
		req.RemoteAddr = "198.51.100.7:4000"
		codes = append(codes, r.do(req).Code)
	}
	if codes[0] != http.StatusCreated || codes[registersBurst] != http.StatusTooManyRequests {
		t.Errorf("registration budget: %v", codes)
	}
	big := `{"client_name":"` + strings.Repeat("x", maxRegisterBytes) + `","redirect_uris":["http://127.0.0.1/cb"]}`
	if w, _ := r.register(t, big); w.Code != http.StatusBadRequest {
		t.Errorf("oversized registration: %d", w.Code)
	}
}

func TestVerifyPKCE(t *testing.T) {
	ch := challengeOf(verifier)
	for v, want := range map[string]bool{verifier: true, verifier + "x": false, "short": false, "": false, strings.Repeat("a", 129): false} {
		if got := verifyPKCE(v, ch); got != want {
			t.Errorf("verifyPKCE(%q) = %v", v, got)
		}
	}
}
