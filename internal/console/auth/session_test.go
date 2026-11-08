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
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var (
	secretA = []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	secretB = []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	limits  = Limits{MaxAge: 12 * time.Hour, IdleTimeout: time.Hour}
)

func testCodec(t *testing.T, secure bool, secrets ...[]byte) *Codec {
	t.Helper()
	if len(secrets) == 0 {
		secrets = [][]byte{secretA}
	}
	c, err := NewCodec(secrets, secure, limits)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func freshSession(now time.Time) Session {
	return Session{ID: newSessionID(), Provider: ProviderToken, Token: "tok", IssuedAt: now.Unix(), LastSeen: now.Unix()}
}

// carry copies the Set-Cookie headers of a response onto a new request,
// the way a browser would (expired cookies dropped).
func carry(t *testing.T, w *httptest.ResponseRecorder, prev *http.Request) *http.Request {
	t.Helper()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "https://c.example/", nil)
	jar := map[string]string{}
	if prev != nil {
		for _, c := range prev.Cookies() {
			jar[c.Name] = c.Value
		}
	}
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(jar, c.Name)
			continue
		}
		jar[c.Name] = c.Value
	}
	for k, v := range jar {
		r.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	return r
}

func TestSessionRoundTripAndCookieAttributes(t *testing.T) {
	t.Parallel()
	c := testCodec(t, true)
	s := freshSession(time.Now())
	w := httptest.NewRecorder()
	if _, err := c.WriteSession(w, nil, s); err != nil {
		t.Fatal(err)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "__Host-pgtoolbox-console-0" {
		t.Fatalf("cookies = %+v", cookies)
	}
	ck := cookies[0]
	if !ck.Secure || !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode || ck.Path != "/" || ck.Domain != "" {
		t.Fatalf("session cookie attributes = %+v", ck)
	}
	if strings.Contains(ck.Value, "tok") {
		t.Fatal("the token is readable in the cookie")
	}
	got, err := c.ReadSession(carry(t, w, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got != s {
		t.Fatalf("round trip = %+v, want %+v", got, s)
	}
}

func TestSessionKeyRotation(t *testing.T) {
	t.Parallel()
	old := testCodec(t, true, secretA)
	rotated := testCodec(t, true, secretB, secretA)
	retired := testCodec(t, true, secretB)
	w := httptest.NewRecorder()
	if _, err := old.WriteSession(w, nil, freshSession(time.Now())); err != nil {
		t.Fatal(err)
	}
	r := carry(t, w, nil)
	if _, err := rotated.ReadSession(r); err != nil {
		t.Fatalf("a rotated codec must open cookies of the previous key: %v", err)
	}
	if _, err := retired.ReadSession(r); !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("a retired key must not open: %v", err)
	}
}

func TestSessionTamperAndCrossCookie(t *testing.T) {
	t.Parallel()
	c := testCodec(t, true)
	w := httptest.NewRecorder()
	if err := c.writeLogin(w, loginFlow{Provider: "oidc", State: "s", Verifier: "v", Expiry: time.Now().Add(time.Minute).Unix()}); err != nil {
		t.Fatal(err)
	}
	login := w.Result().Cookies()[0]
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: "__Host-pgtoolbox-console-0", Value: login.Value})
	if _, err := c.ReadSession(r); err == nil {
		t.Fatal("a login-flow cookie opened as a session")
	}

	w = httptest.NewRecorder()
	if _, err := c.WriteSession(w, nil, freshSession(time.Now())); err != nil {
		t.Fatal(err)
	}
	v := w.Result().Cookies()[0].Value
	flipped := []byte(v)
	flipped[len(flipped)/2] ^= 0x01
	r = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: "__Host-pgtoolbox-console-0", Value: string(flipped)})
	if _, err := c.ReadSession(r); err == nil {
		t.Fatal("a tampered cookie opened")
	}
}

func TestSessionChunkingAndBudget(t *testing.T) {
	t.Parallel()
	c := testCodec(t, true)
	now := time.Now()
	big := freshSession(now)
	big.Token = strings.Repeat("t", 3000)
	big.RefreshToken = strings.Repeat("r", 1200)
	w := httptest.NewRecorder()
	got, err := c.WriteSession(w, nil, big)
	if err != nil {
		t.Fatal(err)
	}
	if got.RefreshToken == "" || len(w.Result().Cookies()) != 2 {
		t.Fatalf("a 4 KiB session should take two chunks and keep its refresh token: %d cookies", len(w.Result().Cookies()))
	}
	r := carry(t, w, nil)
	if back, err := c.ReadSession(r); err != nil || back.Token != big.Token {
		t.Fatalf("chunked round trip failed: %v", err)
	}

	// A smaller session written over it expires the stale second chunk.
	w = httptest.NewRecorder()
	if _, err := c.WriteSession(w, r, freshSession(now)); err != nil {
		t.Fatal(err)
	}
	var expired bool
	for _, ck := range w.Result().Cookies() {
		if ck.Name == "__Host-pgtoolbox-console-1" && ck.MaxAge < 0 {
			expired = true
		}
	}
	if !expired {
		t.Fatal("the stale second chunk was not expired")
	}

	huge := freshSession(now)
	huge.Token = strings.Repeat("t", 4500)
	huge.RefreshToken = strings.Repeat("r", 2000)
	got, err = c.WriteSession(httptest.NewRecorder(), nil, huge)
	if err != nil || got.RefreshToken != "" {
		t.Fatalf("an oversized session must drop its refresh token first: err=%v refresh=%d", err, len(got.RefreshToken))
	}
	huge.Token = strings.Repeat("t", 6000)
	if _, err := c.WriteSession(httptest.NewRecorder(), nil, huge); !errors.Is(err, ErrSessionTooLarge) {
		t.Fatalf("a token over the budget must fail: %v", err)
	}
}

func TestSessionExpiry(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		s    Session
		at   time.Time
		ok   bool
	}{
		{"fresh", Session{IssuedAt: t0.Unix(), LastSeen: t0.Unix()}, t0.Add(time.Minute), true},
		{"idle", Session{IssuedAt: t0.Unix(), LastSeen: t0.Unix()}, t0.Add(time.Hour), false},
		{"active but past max age", Session{IssuedAt: t0.Unix(), LastSeen: t0.Add(12 * time.Hour).Unix()}, t0.Add(12 * time.Hour), false},
		{"token expired, no refresh", Session{IssuedAt: t0.Unix(), LastSeen: t0.Unix(), TokenExpiry: t0.Add(10 * time.Minute).Unix()}, t0.Add(10 * time.Minute), false},
		{"token expired, refreshable", Session{IssuedAt: t0.Unix(), LastSeen: t0.Unix(), TokenExpiry: t0.Add(10 * time.Minute).Unix(), RefreshToken: "r"}, t0.Add(10 * time.Minute), true},
		{"issued in the future", Session{IssuedAt: t0.Add(time.Hour).Unix(), LastSeen: t0.Add(time.Hour).Unix()}, t0, false},
	}
	for _, tc := range cases {
		tc.s.ID, tc.s.Provider, tc.s.Token = "id", ProviderToken, "tok"
		if err := tc.s.check(tc.at, limits); (err == nil) != tc.ok {
			t.Errorf("%s: check = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
	if err := (Session{IssuedAt: t0.Unix()}).check(t0, limits); err == nil {
		t.Error("an incomplete session passed")
	}
}

func TestInsecureCookieNames(t *testing.T) {
	t.Parallel()
	c := testCodec(t, false)
	w := httptest.NewRecorder()
	if _, err := c.WriteSession(w, nil, freshSession(time.Now())); err != nil {
		t.Fatal(err)
	}
	ck := w.Result().Cookies()[0]
	if ck.Name != "pgtoolbox-console-0" || ck.Secure {
		t.Fatalf("insecure cookie = %+v", ck)
	}
}

func TestLoadConfig(t *testing.T) {
	t.Parallel()
	base := map[string]string{
		"AUTH_PROVIDER":    "token",
		"SESSION_KEY_FILE": "/s",
	}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"minimal token", with(), ""},
		{"no provider", with("AUTH_PROVIDER", ""), "AUTH_PROVIDER is empty"},
		{"unknown provider", with("AUTH_PROVIDER", "local"), "unknown provider \"local\""},
		{"no secret", with("SESSION_KEY_FILE", ""), "SESSION_KEY_FILE is required"},
		{"oidc needs issuer and client", with("AUTH_PROVIDER", "oidc"), "OIDC_ISSUER_URL"},
		{"oidc http issuer", with("AUTH_PROVIDER", "oidc", "OIDC_ISSUER_URL", "http://idp", "OIDC_CLIENT_ID", "c", "OIDC_CLIENT_SECRET_FILE", "/f"), "must be an https URL"},
		{"oidc complete", with("AUTH_PROVIDER", "oidc", "OIDC_ISSUER_URL", "https://idp", "OIDC_CLIENT_ID", "c", "OIDC_CLIENT_SECRET_FILE", "/f"), ""},
		{"oidc grant client", with("AUTH_PROVIDER", "oidc", "OIDC_ISSUER_URL", "https://idp", "OIDC_CLIENT_ID", "c", "OIDC_CLIENT_SECRET_FILE", "/f",
			"OIDC_GRANT_CLIENT_ID", "g", "OIDC_GRANT_CLIENT_SECRET_FILE", "/g"), ""},
		{"oidc grant client without secret", with("AUTH_PROVIDER", "oidc", "OIDC_ISSUER_URL", "https://idp", "OIDC_CLIENT_ID", "c", "OIDC_CLIENT_SECRET_FILE", "/f",
			"OIDC_GRANT_CLIENT_ID", "g"), "go together"},
		{"oidc grant client is the console's", with("AUTH_PROVIDER", "oidc", "OIDC_ISSUER_URL", "https://idp", "OIDC_CLIENT_ID", "c", "OIDC_CLIENT_SECRET_FILE", "/f",
			"OIDC_GRANT_CLIENT_ID", "c", "OIDC_GRANT_CLIENT_SECRET_FILE", "/g"), "must differ"},
		{"grant client without oidc", with("OIDC_GRANT_CLIENT_ID", "g", "OIDC_GRANT_CLIENT_SECRET_FILE", "/g"), "needs the oidc provider"},
		{"openshift client id", with("AUTH_PROVIDER", "openshift", "OPENSHIFT_CLIENT_ID", "pgtoolbox-console"), "system:serviceaccount"},
		{"idle over max", with("SESSION_IDLE_TIMEOUT", "13h"), "exceeds"},
		{"bad duration", with("SESSION_MAX_AGE", "forever"), "not a positive duration"},
		{"http public url with secure cookies", with("PUBLIC_URL", "http://c.example"), "drop every cookie"},
		{"public url with a path", with("PUBLIC_URL", "https://c.example/console"), "must be an origin"},
		{"revocation needs namespace", with("REVOCATION_CONFIGMAP", "revoked"), "POD_NAMESPACE"},
		{"chart's 8h idle timeout", with("SESSION_IDLE_TIMEOUT", "8h"), ""},
		{"redirect URL sets the public origin", with("AUTH_PROVIDER", "oidc", "OIDC_ISSUER_URL", "https://idp", "OIDC_CLIENT_ID", "c", "OIDC_CLIENT_SECRET_FILE", "/f", "OIDC_REDIRECT_URL", "http://c.example/auth/oidc/callback"), "drop every cookie"},
		{"proxy hops", with("TRUSTED_PROXY_HOPS", "9"), "between 0 and 8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadConfig(func(k string) (string, bool) { v, ok := tc.env[k]; return v, ok })
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadConfigBreakGlassAndServiceAccountClient(t *testing.T) {
	t.Parallel()
	payload := b64url([]byte(`{"sub":"system:serviceaccount:team-a:pgtoolbox-console"}`))
	tokenFile := writeFile(t, "h."+payload+".s")
	env := map[string]string{"AUTH_PROVIDER": "openshift", "SESSION_KEY_FILE": "/s", "OPENSHIFT_CLIENT_SECRET_FILE": tokenFile}
	cfg, err := LoadConfig(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Providers, []string{ProviderOpenShift, ProviderToken}) {
		t.Fatalf("providers = %v: the token form is the break-glass path and stays offered", cfg.Providers)
	}
	if cfg.OpenShift.ClientID != "system:serviceaccount:team-a:pgtoolbox-console" {
		t.Fatalf("client ID = %q", cfg.OpenShift.ClientID)
	}
	env["AUTH_TOKEN_LOGIN"] = "false"
	cfg, _ = LoadConfig(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if !slices.Equal(cfg.Providers, []string{ProviderOpenShift}) {
		t.Fatalf("providers with AUTH_TOKEN_LOGIN=false = %v", cfg.Providers)
	}
}

func TestLoadSecrets(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	got, err := LoadSecrets(write("ok", "\n"+string(secretB)+"\n\n"+string(secretA)+"\n"))
	if err != nil || len(got) != 2 || string(got[0]) != string(secretB) {
		t.Fatalf("LoadSecrets = %q, %v", got, err)
	}
	if _, err := LoadSecrets(write("short", "short\n")); err == nil {
		t.Fatal("a short secret was accepted")
	}
	if _, err := LoadSecrets(write("empty", "\n\n")); err == nil {
		t.Fatal("an empty file was accepted")
	}
	if _, err := LoadSecrets(write("big", strings.Repeat("x", maxSecretFileBytes+1))); err == nil {
		t.Fatal("an oversized file was accepted")
	}
}
