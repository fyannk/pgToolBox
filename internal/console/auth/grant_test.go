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
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSealPurposeIsBoundToItsPurposeAndKey(t *testing.T) {
	t.Parallel()
	c := testCodec(t, true)
	type payload struct{ A string }
	s, err := c.SealPurpose("mcp-code", payload{"x"})
	if err != nil {
		t.Fatal(err)
	}
	var got payload
	if err := c.OpenPurpose("mcp-code", s, &got); err != nil || got.A != "x" {
		t.Fatalf("round trip: %v %+v", err, got)
	}
	if err := c.OpenPurpose("mcp-access", s, &got); err == nil {
		t.Error("a code opened as an access token")
	}
	other := testCodec(t, true, []byte("another-session-secret-of-32-bytes!!"))
	if err := other.OpenPurpose("mcp-code", s, &got); err == nil {
		t.Error("opened under another key")
	}
	// Flip one bit of the sealed bytes, not of the text: a textual edit can be
	// a no-op (the value may already end with it) or fail in base64 before the
	// AEAD is reached.
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{0, len(raw) / 2, len(raw) - 1} {
		tampered := bytes.Clone(raw)
		tampered[i] ^= 0x01
		if err := c.OpenPurpose("mcp-code", base64.RawURLEncoding.EncodeToString(tampered), &got); err == nil {
			t.Errorf("a value tampered at byte %d of %d opened", i, len(raw))
		}
	}
}

func TestGrantReferencesInTheRevocationList(t *testing.T) {
	t.Parallel()
	gid := GrantReferencePrefix + strings.Repeat("0a", 16)
	l, err := ParseRevocationList(map[string]string{"sessions": gid + "\n"})
	if err != nil {
		t.Fatal(err)
	}
	if !l.Revoked(gid, "alice", time.Now()) || l.Revoked(GrantReferencePrefix+strings.Repeat("0b", 16), "alice", time.Now()) {
		t.Error("a listed grant is not revoked, or another one is")
	}
	for _, bad := range []string{"grant-XYZ", "grant-" + strings.Repeat("A", 32), "grant-" + strings.Repeat("a", 31)} {
		if _, err := ParseRevocationList(map[string]string{"sessions": bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
		if IsGrantReference(bad) {
			t.Errorf("IsGrantReference(%q)", bad)
		}
	}
}

func TestCurrentSessionAndUpstreamRefresh(t *testing.T) {
	env := newTestEnv(t, "oidc", "openshift")
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "https://c.example/oauth/authorize", nil)
	if g, _, err := env.h.CurrentSession(httptest.NewRecorder(), r); g != nil || err != nil {
		t.Fatalf("no cookie: %+v %v", g, err)
	}
	b := env.browser(t)
	if w := b.oidcLogin("/"); w.Code != http.StatusFound {
		t.Fatalf("login: %d", w.Code)
	}
	for _, c := range b.jar {
		r.AddCookie(c)
	}
	g, _, err := env.h.CurrentSession(httptest.NewRecorder(), r)
	if err != nil || g == nil {
		t.Fatalf("CurrentSession: %+v %v", g, err)
	}
	if g.Principal.User.Name != "oidc:jane@corp.example" || g.Provider != "oidc" || !strings.HasPrefix(g.Token, "eyJ") ||
		g.RefreshToken != "refresh-1" || g.TokenExpiry.IsZero() {
		t.Errorf("session grant = %+v", g)
	}
	// A grant still being approved goes with its session (mcpoauth asks).
	if env.h.SignedOut(g.Principal.SessionID) || env.h.SignedOut("") {
		t.Fatal("a live session reads as signed out")
	}

	up, err := env.h.RefreshUpstream(context.Background(), "oidc", "grant-x", g.RefreshToken)
	if err != nil || !strings.HasPrefix(up.Token, "eyJ") || up.Token == g.Token || up.RefreshToken != "refresh-2" {
		t.Fatalf("RefreshUpstream: %+v %v", up, err)
	}
	for name, c := range map[string][2]string{
		"no refresh token": {"oidc", ""},
		"openshift":        {"openshift", "refresh-1"},
		"unknown provider": {"token", "refresh-1"},
	} {
		if _, err := env.h.RefreshUpstream(context.Background(), c[0], "grant-y", c[1]); !errors.Is(err, ErrNotRefreshable) {
			t.Errorf("%s: %v", name, err)
		}
	}
	env.idp.mu.Lock()
	env.idp.refreshFails = true
	env.idp.mu.Unlock()
	if _, err := env.h.RefreshUpstream(context.Background(), "oidc", "grant-z", "refresh-2"); err == nil {
		t.Error("a refused refresh succeeded")
	}
	if w := b.do(http.MethodPost, "/logout", url.Values{}, sameOriginForm); w.Code != http.StatusOK {
		t.Fatalf("sign-out: %d", w.Code)
	}
	if !env.h.SignedOut(g.Principal.SessionID) {
		t.Error("a signed-out session does not read as signed out")
	}
}

func TestConsentPageAllowsOnlyTheLoopbackRedirect(t *testing.T) {
	t.Parallel()
	for origin, want := range map[string]string{
		"http://127.0.0.1:4000":     "form-action 'self' http://127.0.0.1:4000;",
		"":                          "form-action 'self';",
		"http://x; script-src *":    "form-action 'self';",
		"http://127.0.0.1:1'unsafe": "form-action 'self';",
	} {
		w := httptest.NewRecorder()
		ConsentPage(w, ConsentView{Subject: "alice", ClientName: "<b>x</b>", Action: "/oauth/authorize", Request: "r", RedirectOrigin: origin})
		if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, want) || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%q: CSP %q", origin, csp)
		}
		if strings.Contains(w.Body.String(), "<b>x</b>") || !strings.Contains(w.Body.String(), "(unverified)") {
			t.Errorf("%q: the client's name is not escaped or not marked", origin)
		}
	}
}
