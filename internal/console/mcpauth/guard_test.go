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

package mcpauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
)

const publicURL = "https://console.example"

// checker is a BearerChecker standing in for the sign-in handler.
type checker struct {
	on    bool
	down  bool
	calls int
}

func (c *checker) TokenLoginEnabled() bool { return c.on }

func (c *checker) BearerPrincipal(_ context.Context, token string) (*auth.Principal, error) {
	c.calls++
	switch {
	case !c.on:
		return nil, auth.ErrTokenLoginDisabled
	case c.down:
		return nil, errors.New("dial tcp: connection refused")
	case token == "revoked":
		return nil, auth.ErrBearerRevoked
	case token == "busy":
		return nil, auth.ErrReviewsBusy
	case strings.HasPrefix(token, "good-"):
		return &auth.Principal{User: authz.User{Name: strings.TrimPrefix(token, "good-")}, SessionID: auth.BearerReference(token)}, nil
	}
	return nil, auth.ErrTokenRejected
}

type guardEnv struct {
	h    http.Handler
	c    *checker
	logs *bytes.Buffer
}

func newGuardEnv(t *testing.T, mutate func(*Options)) guardEnv {
	t.Helper()
	c := &checker{on: true}
	logs := &bytes.Buffer{}
	o := Options{
		Authenticator: Chain{RawToken{Checker: c}},
		PublicURL:     publicURL,
		Audit:         &auth.Auditor{Logger: slog.New(slog.NewJSONHandler(logs, nil))},
	}
	if mutate != nil {
		mutate(&o)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.PrincipalFrom(r.Context())
		_, ok := ClientFrom(r.Context())
		_, _ = w.Write([]byte(p.User.Name + "|" + map[bool]string{true: "client", false: "none"}[ok]))
	})
	return guardEnv{h: NewGuard(o).Wrap(next), c: c, logs: logs}
}

func (e guardEnv) do(t *testing.T, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, publicURL+PathMCP, strings.NewReader(`{}`))
	r.RemoteAddr = "10.0.0.7:5555"
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func bearerHdr(tok string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + tok}
}

const wantMetadata = `resource_metadata="https://console.example/.well-known/oauth-protected-resource/mcp"`

func TestGuard(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		mutate    func(*Options)
		down, off bool
		hdr       map[string]string
		code      int
		challenge string // substring of WWW-Authenticate; "" = no header
		body      string
		denied    string // the mcp.denied reason, "" = no audit line
	}{
		{name: "no token is the discovery challenge", hdr: nil, code: 401, challenge: wantMetadata, body: "Authorization: Bearer"},
		{name: "a session cookie grants nothing", hdr: map[string]string{"Cookie": "pgtoolbox_console_session=abc"}, code: 401, challenge: wantMetadata},
		{name: "basic auth is not a bearer", hdr: map[string]string{"Authorization": "Basic YTpi"}, code: 401, challenge: wantMetadata},
		{name: "empty bearer", hdr: map[string]string{"Authorization": "Bearer  "}, code: 401, challenge: wantMetadata},
		{name: "a rejected token", hdr: bearerHdr("forged"), code: 401, challenge: `error="invalid_token"`, denied: "rejected by the API server"},
		{name: "a revoked token", hdr: bearerHdr("revoked"), code: 401, challenge: `error_description="the token is revoked"`, denied: "revoked"},
		{name: "token sign-in off", off: true, hdr: bearerHdr("good-dba"), code: 401, challenge: `error="invalid_token"`, denied: "not a token this console accepts"},
		{name: "API server down is not a refusal", down: true, hdr: bearerHdr("good-dba"), code: 503},
		{name: "the shared review bucket is spent", hdr: bearerHdr("busy"), code: 429, body: "too many tokens", denied: "review budget"},
		{name: "accepted", hdr: bearerHdr("good-dba"), code: 200, body: "dba|client"},
		{name: "scheme is case-insensitive", hdr: map[string]string{"Authorization": "bearer good-dba"}, code: 200, body: "dba|client"},
		{name: "same origin is accepted", hdr: map[string]string{"Authorization": "Bearer good-dba", "Origin": publicURL}, code: 200},
		{name: "cross origin is refused before the token is read", hdr: map[string]string{"Authorization": "Bearer good-dba", "Origin": "https://evil.example"}, code: 403, denied: "cross-origin request"},
		{
			name: "the metadata URL follows the public URL", mutate: func(o *Options) { o.PublicURL = "https://pg.example/" }, code: 401,
			challenge: `resource_metadata="https://pg.example/.well-known/oauth-protected-resource/mcp"`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			env := newGuardEnv(t, c.mutate)
			env.c.down, env.c.on = c.down, !c.off
			w := env.do(t, c.hdr)
			if w.Code != c.code {
				t.Fatalf("code = %d, want %d: %s", w.Code, c.code, w.Body)
			}
			got := w.Header().Get("WWW-Authenticate")
			if c.challenge == "" && got != "" || c.challenge != "" && (!strings.HasPrefix(got, "Bearer ") || !strings.Contains(got, c.challenge)) {
				t.Errorf("WWW-Authenticate = %q, want %q", got, c.challenge)
			}
			if !strings.Contains(w.Body.String(), c.body) {
				t.Errorf("body = %q, want %q", w.Body, c.body)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Error("no Cache-Control: no-store")
			}
			logs := env.logs.String()
			if c.denied == "" && strings.Contains(logs, auth.AuditMCPDenied) {
				t.Errorf("unexpected audit line: %s", logs)
			}
			if c.denied != "" && !strings.Contains(logs, `"reason":"`+c.denied+`"`) {
				t.Errorf("audit = %s, want reason %q", logs, c.denied)
			}
			for _, tok := range []string{"forged", "good-dba"} {
				if strings.Contains(logs, tok) {
					t.Errorf("the token %q reached the log", tok)
				}
			}
		})
	}
}

func TestGuardBudgets(t *testing.T) {
	t.Parallel()
	t.Run("per user", func(t *testing.T) {
		t.Parallel()
		env := newGuardEnv(t, func(o *Options) { o.Calls = auth.NewRateLimiter(1, 3, 100) })
		for i := range 3 {
			if w := env.do(t, bearerHdr("good-dba")); w.Code != 200 {
				t.Fatalf("call %d = %d", i, w.Code)
			}
		}
		w := env.do(t, bearerHdr("good-dba"))
		if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
			t.Fatalf("over budget = %d", w.Code)
		}
		if !strings.Contains(env.logs.String(), `"reason":"user budget"`) || !strings.Contains(env.logs.String(), `"user":"dba"`) {
			t.Errorf("audit = %s", env.logs)
		}
		// Another user has a budget of their own.
		if w := env.do(t, bearerHdr("good-ops")); w.Code != 200 {
			t.Errorf("another user = %d", w.Code)
		}
	})
	t.Run("per address, for tokens not seen accepted", func(t *testing.T) {
		t.Parallel()
		env := newGuardEnv(t, func(o *Options) { o.Attempts = auth.NewRateLimiter(1, 2, 100) })
		// A token accepted once is known: it no longer spends the
		// address budget, so a busy agent is not throttled by it.
		for range 5 {
			if w := env.do(t, bearerHdr("good-dba")); w.Code != 200 {
				t.Fatalf("known token = %d", w.Code)
			}
		}
		calls := env.c.calls
		if w := env.do(t, bearerHdr("forged")); w.Code != 401 {
			t.Fatalf("first guess = %d", w.Code)
		}
		w := env.do(t, bearerHdr("forged-2"))
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("guessing past the budget = %d", w.Code)
		}
		if env.c.calls != calls+1 {
			t.Errorf("the API server was asked %d times for 2 guesses, the second over budget", env.c.calls-calls)
		}
		if w := env.do(t, bearerHdr("good-dba")); w.Code != 200 {
			t.Errorf("the known token after the guesses = %d", w.Code)
		}
	})
	t.Run("per peer, whatever X-Forwarded-For an untrusted peer claims", func(t *testing.T) {
		t.Parallel()
		env := newGuardEnv(t, func(o *Options) {
			o.Attempts = auth.NewRateLimiter(1, 2, 100)
			o.TrustedProxy = auth.ProxyTrust{Hops: 1, CIDRs: []netip.Prefix{netip.MustParsePrefix("10.128.0.0/14")}}
		})
		codes := make([]int, 0, 3)
		for i := range 3 {
			hdr := bearerHdr(fmt.Sprintf("forged-%d", i))
			hdr["X-Forwarded-For"] = fmt.Sprintf("198.18.0.%d", i)
			codes = append(codes, env.do(t, hdr).Code)
		}
		if codes[2] != http.StatusTooManyRequests {
			t.Fatalf("guesses with a fresh forged address each = %v, want the third over the peer's budget", codes)
		}
		if strings.Contains(env.logs.String(), "198.18.0.") {
			t.Errorf("a forged address reached the audit trail: %s", env.logs)
		}
	})
	t.Run("a refused token is forgotten", func(t *testing.T) {
		t.Parallel()
		env := newGuardEnv(t, func(o *Options) { o.Attempts = auth.NewRateLimiter(1, 1, 100) })
		if w := env.do(t, bearerHdr("good-dba")); w.Code != 200 {
			t.Fatal(w.Code)
		}
		env.c.on = false // token sign-in turned off: the known token is refused, then forgotten
		if w := env.do(t, bearerHdr("good-dba")); w.Code != 401 {
			t.Fatalf("after token sign-in off = %d", w.Code)
		}
		if w := env.do(t, bearerHdr("good-dba")); w.Code != http.StatusTooManyRequests {
			t.Fatalf("the forgotten token kept its free pass: %d", w.Code)
		}
	})
}

func TestChain(t *testing.T) {
	t.Parallel()
	mine := func(name string) Authenticator {
		return AuthenticatorFunc(func(_ context.Context, tok string) (*auth.Principal, error) {
			if !strings.HasPrefix(tok, name+":") {
				return nil, ErrNotMine
			}
			if tok == name+":bad" {
				return nil, ErrUnauthenticated
			}
			return &auth.Principal{User: authz.User{Name: name}}, nil
		})
	}
	ch := Chain{mine("grant"), mine("raw")}
	for _, c := range []struct {
		token, user string
		err         error
	}{
		{"grant:x", "grant", nil},
		{"raw:x", "raw", nil},
		{"grant:bad", "", ErrUnauthenticated},
		{"other", "", ErrNotMine},
	} {
		p, err := ch.Authenticate(t.Context(), c.token)
		if !errors.Is(err, c.err) || (c.err == nil && p.User.Name != c.user) {
			t.Errorf("%s: %v %v", c.token, p, err)
		}
	}
	if _, err := (Chain{}).Authenticate(t.Context(), "x"); !errors.Is(err, ErrNotMine) {
		t.Errorf("empty chain: %v", err)
	}
	if _, err := (RawToken{}).Authenticate(t.Context(), "x"); !errors.Is(err, ErrNotMine) {
		t.Errorf("RawToken without a checker: %v", err)
	}
}

func TestMetadataHandler(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, public, host, want string
		servers                  func(*http.Request) []string
	}{
		{"public URL", publicURL, "ignored.example", publicURL + "/mcp", nil},
		{"from the request", "", "c.example", "http://c.example/mcp", nil},
		{"with servers", publicURL, "", publicURL + "/mcp", func(*http.Request) []string { return []string{publicURL} }},
	} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+c.host+PathResourceMetadata+PathMCP, nil)
		if c.host != "" {
			r.Host = c.host
		}
		w := httptest.NewRecorder()
		MetadataHandler(c.public, c.servers).ServeHTTP(w, r)
		var m map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		if m["resource"] != c.want || w.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("%s: %v", c.name, m)
		}
		if _, ok := m["authorization_servers"]; ok != (c.servers != nil) {
			t.Errorf("%s: authorization_servers = %v", c.name, m["authorization_servers"])
		}
		if bm, _ := m["bearer_methods_supported"].([]any); len(bm) != 1 || bm[0] != "header" {
			t.Errorf("%s: bearer methods = %v", c.name, bm)
		}
	}
}
