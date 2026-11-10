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
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestOIDCCodeFlow(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderOIDC, ProviderToken)
	b := env.browser(t)
	w := b.get("/auth/oidc/login?rd=/clusters")
	login := w.Result().Cookies()[0]
	if login.Name != "__Host-pgtoolbox-console-login" || login.SameSite != http.SameSiteLaxMode || !login.HttpOnly || !login.Secure {
		t.Fatalf("login-flow cookie = %+v (must be Lax: the IdP's redirect back is cross-site)", login)
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	if loc.Query().Get("redirect_uri") != "https://c.example/auth/oidc/callback" || loc.Query().Get("nonce") == "" {
		t.Fatalf("authorization URL = %s", loc)
	}
	state := env.idp.authorize(t, loc.String())
	w = b.get("/auth/oidc/callback?code=c1&state=" + url.QueryEscape(state))
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/clusters" {
		t.Fatalf("callback = %d %s: %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	if _, ok := b.jar["__Host-pgtoolbox-console-login"]; ok {
		t.Fatal("the login-flow cookie survived the callback")
	}
	if got := b.get("/api/v1/whoami").Body.String(); got != "oidc:jane@corp.example|oidc" {
		t.Fatalf("whoami = %q", got)
	}
	logs := env.logs.String()
	if !strings.Contains(logs, `"audit":"login.success"`) || !strings.Contains(logs, `"user":"oidc:jane@corp.example"`) {
		t.Fatalf("no login.success audit line:\n%s", logs)
	}
	if strings.Contains(logs, "eyJ") || strings.Contains(logs, "refresh-1") || strings.Contains(logs, testClientSecret) {
		t.Fatalf("a credential reached the log:\n%s", logs)
	}
}

func TestOIDCCallbackRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		setup  func(*fakeIdP)
		mangle func(state string) string
		status int
		text   string
	}{
		{"state mismatch", nil, func(string) string { return "forged" }, http.StatusBadRequest, "state mismatch"},
		{"nonce mismatch", func(f *fakeIdP) { f.badNonce = true }, nil, http.StatusBadRequest, "nonce mismatch"},
		{"email not verified", func(f *fakeIdP) { f.emailVerified = false }, nil, http.StatusForbidden, "not verified"},
		{"email_verified absent", func(f *fakeIdP) { f.emailVerified = nil }, nil, http.StatusForbidden, "not verified"},
		{"email_verified as string", func(f *fakeIdP) { f.emailVerified = "true" }, nil, http.StatusFound, ""},
		{"apiserver does not trust the IdP", func(f *fakeIdP) { f.email = "x" }, nil, http.StatusForbidden, "does not accept tokens from this identity provider"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newTestEnv(t, ProviderOIDC)
			if tc.name == "apiserver does not trust the IdP" {
				env.rev.users = nil
			}
			if tc.setup != nil {
				tc.setup(env.idp)
			}
			b := env.browser(t)
			w := b.get("/auth/oidc/login?rd=/")
			state := env.idp.authorize(t, w.Header().Get("Location"))
			if tc.mangle != nil {
				state = tc.mangle(state)
			}
			w = b.get("/auth/oidc/callback?code=c1&state=" + url.QueryEscape(state))
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.text) {
				t.Fatalf("callback = %d, want %d containing %q: %s", w.Code, tc.status, tc.text, w.Body)
			}
			if tc.status != http.StatusFound {
				if _, ok := b.jar["__Host-pgtoolbox-console-0"]; ok {
					t.Fatal("a session was issued on a refused sign-in")
				}
				if !strings.Contains(env.logs.String(), `"audit":"login.failure"`) {
					t.Fatal("no login.failure audit line")
				}
			}
		})
	}
}

func TestCallbackWithoutFlowCookie(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderOIDC)
	w := env.browser(t).get("/auth/oidc/callback?code=c&state=s")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "expired") {
		t.Fatalf("callback without flow = %d %s", w.Code, w.Body)
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "script-src") {
		t.Fatalf("error page CSP = %q", csp)
	}
}

func TestOIDCRefreshOnNearExpiry(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderOIDC)
	env.idp.idTokenTTL = time.Minute // inside refreshLead from the start
	b := env.browser(t)
	if w := b.oidcLogin("/"); w.Code != http.StatusFound {
		t.Fatalf("login = %d %s", w.Code, w.Body)
	}
	before := b.jar["__Host-pgtoolbox-console-0"].Value
	// Ordinary calls never refresh: concurrent requests would redeem a
	// rotating refresh token more than once.
	if w := b.get("/api/v1/whoami"); w.Code != http.StatusOK || env.idp.refreshes != 0 {
		t.Fatalf("whoami = %d, refreshes = %d", w.Code, env.idp.refreshes)
	}
	if w := b.get(SessionPath); w.Code != http.StatusOK {
		t.Fatalf("session = %d", w.Code)
	}
	if env.idp.refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", env.idp.refreshes)
	}
	if b.jar["__Host-pgtoolbox-console-0"].Value == before {
		t.Fatal("the refreshed token was not written back")
	}

	// A failed refresh keeps the session until the token itself expires.
	env.idp.refreshFails = true
	if w := b.get(SessionPath); w.Code != http.StatusOK {
		t.Fatalf("whoami after failed refresh = %d", w.Code)
	}
}

func TestOpenShiftCodeFlow(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderOpenShift)
	b := env.browser(t)
	// A single redirect provider and no token form: the sign-in page is
	// skipped.
	w := b.get("/auth/login?rd=/clusters")
	if w.Code != http.StatusFound || !strings.HasPrefix(w.Header().Get("Location"), "/auth/openshift/login") {
		t.Fatalf("login page = %d %s", w.Code, w.Header().Get("Location"))
	}
	w = b.get(w.Header().Get("Location"))
	state := env.idp.authorize(t, w.Header().Get("Location"))
	w = b.get("/auth/openshift/callback?code=c&state=" + url.QueryEscape(state))
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/clusters" {
		t.Fatalf("callback = %d %s", w.Code, w.Body)
	}
	if got := b.get("/clusters").Body.String(); got != "jane|openshift" {
		t.Fatalf("whoami = %q", got)
	}
}

func TestOpenShiftWrongClientSecret(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderOpenShift)
	env.h.oauth[0].(*openshiftProvider).cfg.ClientSecretFile = writeFile(t, "wrong")
	b := env.browser(t)
	w := b.get("/auth/openshift/login")
	state := env.idp.authorize(t, w.Header().Get("Location"))
	w = b.get("/auth/openshift/callback?code=c&state=" + url.QueryEscape(state))
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "client secret is wrong") {
		t.Fatalf("callback = %d %s", w.Code, w.Body)
	}
}

func TestTokenProvider(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		token  string
		hdr    map[string]string
		status int
		where  string
	}{
		{"accepted", "sa-token", sameOriginForm, http.StatusFound, "/clusters"},
		{"bearer prefix", "Bearer sa-token", sameOriginForm, http.StatusFound, "/clusters"},
		{"rejected by the API server", "nope", sameOriginForm, http.StatusForbidden, ""},
		{"not a token", "a b", sameOriginForm, http.StatusBadRequest, ""},
		{"cross-site post", "sa-token", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden, ""},
		{"no origin", "sa-token", nil, http.StatusForbidden, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newTestEnv(t, ProviderToken)
			b := env.browser(t)
			w := b.do(http.MethodPost, "/auth/token", url.Values{"token": {tc.token}, "rd": {"/clusters"}}, tc.hdr)
			if w.Code != tc.status || (tc.where != "" && w.Header().Get("Location") != tc.where) {
				t.Fatalf("POST /auth/token = %d %s: %s", w.Code, w.Header().Get("Location"), w.Body)
			}
			_, has := b.jar["__Host-pgtoolbox-console-0"]
			if has != (tc.status == http.StatusFound) {
				t.Fatalf("session cookie present = %v", has)
			}
			if strings.Contains(env.logs.String(), "sa-token") {
				t.Fatal("the pasted token reached the log")
			}
		})
	}
}

func TestPastedTokenExpiryCapsSession(t *testing.T) {
	t.Parallel()
	exp := time.Now().Add(30 * time.Minute).Unix()
	jwt := b64url([]byte(`{"alg":"none"}`)) + "." + b64url([]byte(`{"exp":`+itoa(exp)+`}`)) + ".sig"
	if got := pastedTokenExpiry(jwt).Unix(); got != exp {
		t.Fatalf("pastedTokenExpiry = %d, want %d", got, exp)
	}
	for _, opaque := range []string{"sha256~abc", "a.b", "x.!!.y", b64url([]byte(`{}`)) + "." + b64url([]byte(`{"exp":"soon"}`)) + ".s"} {
		if !pastedTokenExpiry(opaque).IsZero() {
			t.Errorf("pastedTokenExpiry(%q) is not zero", opaque)
		}
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestRequireUnauthenticated(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderToken)
	b := env.browser(t)
	w := b.get("/clusters?ns=a")
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/auth/login?rd=%2Fclusters%3Fns%3Da" {
		t.Fatalf("page without session = %d %s", w.Code, w.Header().Get("Location"))
	}
	w = b.get("/api/v1/whoami")
	var p Problem
	if w.Code != http.StatusUnauthorized || json.Unmarshal(w.Body.Bytes(), &p) != nil || p.LoginURL != "/auth/login" {
		t.Fatalf("API without session = %d %s", w.Code, w.Body)
	}
	if w.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("content type = %q", w.Header().Get("Content-Type"))
	}
	// A garbage cookie is cleared, not trusted.
	b.jar["__Host-pgtoolbox-console-0"] = &http.Cookie{Name: "__Host-pgtoolbox-console-0", Value: "garbage"}
	if w := b.get("/api/v1/whoami"); w.Code != http.StatusUnauthorized {
		t.Fatalf("garbage cookie = %d", w.Code)
	}
	if _, ok := b.jar["__Host-pgtoolbox-console-0"]; ok {
		t.Fatal("the garbage cookie was not cleared")
	}
}

func signIn(t *testing.T, b *browser) {
	t.Helper()
	if w := b.do(http.MethodPost, "/auth/token", url.Values{"token": {"sa-token"}}, sameOriginForm); w.Code != http.StatusFound {
		t.Fatalf("sign-in = %d %s", w.Code, w.Body)
	}
}

func TestRequireTokenRejectedLater(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderToken)
	b := env.browser(t)
	signIn(t, b)
	delete(env.rev.users, "sa-token") // the binding or the ServiceAccount is gone
	w := b.get("/api/v1/whoami")
	var p Problem
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if w.Code != http.StatusUnauthorized || p.LoginURL != "/auth/login?reason=rejected" {
		t.Fatalf("whoami with a rejected token = %d %s", w.Code, w.Body)
	}
}

func TestRequireAPIServerDown(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderToken)
	b := env.browser(t)
	signIn(t, b)
	env.rev.down = true
	if w := b.get("/api/v1/whoami"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("whoami with the API server down = %d", w.Code)
	}
	if _, ok := b.jar["__Host-pgtoolbox-console-0"]; !ok {
		t.Fatal("an API server outage signed the user out")
	}
}

func TestRequireRevoked(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderToken)
	b := env.browser(t)
	signIn(t, b)
	s, err := env.codec.ReadSession(carryJar(t, b))
	if err != nil {
		t.Fatal(err)
	}
	rev := NewRevocations(nil, "x", nil)
	rev.log = env.h.log
	rev.apply(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "x"}, Data: map[string]string{"sessions": s.ID + "\n"}})
	env.h.revocations = rev
	w := b.get("/api/v1/whoami")
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "reason=revoked") {
		t.Fatalf("revoked session = %d %s", w.Code, w.Body)
	}
	if !strings.Contains(env.logs.String(), `"audit":"session.revoked"`) {
		t.Fatal("no session.revoked audit line")
	}
}

func carryJar(t *testing.T, b *browser) *http.Request {
	t.Helper()
	r, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://c.example/", nil)
	for _, c := range b.jar {
		r.AddCookie(c)
	}
	return r
}

func TestIdleTimerSlides(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderToken)
	b := env.browser(t)
	signIn(t, b)
	now := time.Now()
	env.h.now = func() time.Time { return now.Add(30 * time.Minute) }
	env.codec.now = env.h.now
	before := b.jar["__Host-pgtoolbox-console-0"].Value
	if w := b.get("/api/v1/whoami"); w.Code != http.StatusOK {
		t.Fatalf("whoami = %d", w.Code)
	}
	if b.jar["__Host-pgtoolbox-console-0"].Value == before {
		t.Fatal("the idle timer did not slide")
	}
	// Past the idle timeout from the last activity, the session ends.
	env.h.now = func() time.Time { return now.Add(30*time.Minute + time.Hour + time.Second) }
	env.codec.now = env.h.now
	if w := b.get("/api/v1/whoami"); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "reason=expired") {
		t.Fatalf("idle session = %d %s", w.Code, w.Body)
	}
}

func TestLogout(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderToken)
	b := env.browser(t)
	signIn(t, b)
	if w := b.get("/logout"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "system:serviceaccount:team-a:pgtoolbox-console-breakglass") {
		t.Fatalf("GET /logout must only confirm: %d %s", w.Code, w.Body)
	}
	if _, ok := b.jar["__Host-pgtoolbox-console-0"]; !ok {
		t.Fatal("GET /logout signed out")
	}
	if w := b.do(http.MethodPost, "/logout", url.Values{}, map[string]string{"Origin": "https://evil.example"}); w.Code != http.StatusForbidden {
		t.Fatalf("cross-site logout = %d", w.Code)
	}
	if w := b.do(http.MethodPost, "/logout", url.Values{}, sameOriginForm); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Signed out") {
		t.Fatalf("POST /logout = %d", w.Code)
	}
	if _, ok := b.jar["__Host-pgtoolbox-console-0"]; ok {
		t.Fatal("the session survived sign-out")
	}
	if !strings.Contains(env.logs.String(), `"audit":"logout"`) {
		t.Fatal("no logout audit line")
	}
}

func TestLoginPage(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderOIDC, ProviderOpenShift, ProviderToken)
	w := env.browser(t).get("/auth/login?rd=/c/x&reason=revoked")
	body := w.Body.String()
	for _, want := range []string{
		"Sign in with single sign-on", "Sign in with OpenShift", `action="/auth/token"`,
		"revoked by an administrator", `href="/auth/oidc/login?rd=%2fc%2fx"`, "kubectl create token",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("login page lacks %q", want)
		}
	}
	if strings.Contains(body, "<script") || strings.Contains(body, `name="password"`) {
		t.Fatal("the login page has a script or a password field")
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("headers = %v", w.Header())
	}
}

func TestPagesRenderAndStyleHashMatches(t *testing.T) {
	t.Parallel()
	for _, page := range []func(http.ResponseWriter){
		func(w http.ResponseWriter) { NoScriptPage(w) },
		func(w http.ResponseWriter) { ErrorPage(w, http.StatusBadGateway, "x <b>") },
		func(w http.ResponseWriter) {
			renderPage(w, http.StatusForbidden, "denied", "Access denied", deniedPage{Subject: "s", Message: "m"})
		},
		func(w http.ResponseWriter) { renderPage(w, http.StatusOK, "signed-out", "Signed out", signedOutPage{}) },
	} {
		w := httptestRecorder()
		page(w)
		body := w.Body.String()
		if !strings.Contains(body, "<style>"+pageCSS+"</style>") {
			t.Fatal("the inline style is not the hashed stylesheet")
		}
		if strings.Contains(body, "<b>") {
			t.Fatal("a message was not escaped")
		}
	}
}

func httptestRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

func TestSignInRateLimit(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderToken)
	b := env.browser(t)
	for i := range signInBurst {
		if w := b.do(http.MethodPost, "/auth/token", url.Values{"token": {"nope"}}, sameOriginForm); w.Code != http.StatusForbidden {
			t.Fatalf("attempt %d = %d", i, w.Code)
		}
	}
	calls := env.rev.calls
	if w := b.do(http.MethodPost, "/auth/token", url.Values{"token": {"nope"}}, sameOriginForm); w.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt over budget = %d", w.Code)
	}
	if env.rev.calls != calls {
		t.Fatal("a refused attempt still reached the API server")
	}
	if strings.Contains(env.logs.String(), "nope") {
		t.Fatal("the pasted token reached the log")
	}
}

// TestJunkSignInsDoNotLockOutOthers is the negative of the shared
// bucket: every request here comes from one address (one Ingress hop
// not counted in trustedProxyHops), and an attacker's junk callbacks
// and pasted tokens must not spend the budget a legitimate user's
// sign-in is charged against.
func TestJunkSignInsDoNotLockOutOthers(t *testing.T) {
	t.Parallel()
	junk := map[string]func(b *browser) *httptest.ResponseRecorder{
		"callbacks without a flow cookie": func(b *browser) *httptest.ResponseRecorder {
			return b.get("/auth/oidc/callback?code=x&state=y")
		},
		"callbacks with a forged state": func(b *browser) *httptest.ResponseRecorder {
			if w := b.get("/auth/oidc/login?rd=/clusters"); w.Code != http.StatusFound {
				b.t.Fatalf("login start = %d", w.Code)
			}
			return b.get("/auth/oidc/callback?code=x&state=forged")
		},
		"pasted junk tokens": func(b *browser) *httptest.ResponseRecorder {
			return b.do(http.MethodPost, "/auth/token", url.Values{"token": {"nope"}}, sameOriginForm)
		},
	}
	legit := map[string]func(b *browser) *httptest.ResponseRecorder{
		"oidc": func(b *browser) *httptest.ResponseRecorder { return b.oidcLogin("/clusters") },
		"token": func(b *browser) *httptest.ResponseRecorder {
			return b.do(http.MethodPost, "/auth/token", url.Values{"token": {"sa-token"}}, sameOriginForm)
		},
	}
	for jn, attack := range junk {
		for ln, signIn := range legit {
			t.Run(jn+" then "+ln, func(t *testing.T) {
				t.Parallel()
				env := newTestEnv(t, ProviderOIDC, ProviderToken)
				attacker := env.browser(t)
				for i := range 3 * signInBurst {
					if w := attack(attacker); w.Code == http.StatusFound {
						t.Fatalf("junk attempt %d signed in", i)
					}
				}
				if w := signIn(env.browser(t)); w.Code != http.StatusFound {
					t.Fatalf("legitimate sign-in after junk = %d: %s", w.Code, w.Body)
				}
			})
		}
	}
}

// TestDistinctJunkTokensAreBoundedProcessWide is the negative of the
// per-token key: a flood of distinct junk tokens, each with a budget of
// its own, still makes at most the shared bucket's worth of
// SelfSubjectReviews, and a legitimate token signs in again once the
// bucket refills.
func TestDistinctJunkTokensAreBoundedProcessWide(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderToken)
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	env.h.tokenReviews.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	b := env.browser(t)
	calls := env.rev.calls
	refused := 0
	const flood = 3 * tokenReviewsBurst
	for i := range flood {
		// Each from an address of its own, so that only the shared bucket
		// bounds them (one address is stopped sooner by its own budget).
		b.addr = fmt.Sprintf("10.1.%d.%d:4711", i/256, i%256)
		w := b.do(http.MethodPost, "/auth/token", url.Values{"token": {fmt.Sprintf("junk-%d", i)}}, sameOriginForm)
		switch w.Code {
		case http.StatusTooManyRequests:
			refused++
		case http.StatusForbidden:
		default:
			t.Fatalf("junk token %d = %d", i, w.Code)
		}
	}
	if reviews := env.rev.calls - calls; reviews > tokenReviewsBurst {
		t.Fatalf("%d distinct junk tokens made %d reviews, want at most %d", flood, reviews, tokenReviewsBurst)
	}
	if refused != flood-tokenReviewsBurst {
		t.Fatalf("%d refused, want %d", refused, flood-tokenReviewsBurst)
	}
	// Exhausted: a legitimate token is refused without a review.
	calls = env.rev.calls
	if w := b.do(http.MethodPost, "/auth/token", url.Values{"token": {"sa-token"}}, sameOriginForm); w.Code != http.StatusTooManyRequests {
		t.Fatalf("legitimate token on an empty bucket = %d", w.Code)
	}
	if env.rev.calls != calls {
		t.Fatal("a refused token still reached the API server")
	}
	// One refill interval later it signs in.
	mu.Lock()
	now = now.Add(time.Second / tokenReviewsPerSecond)
	mu.Unlock()
	if w := env.browser(t).do(http.MethodPost, "/auth/token", url.Values{"token": {"sa-token"}}, sameOriginForm); w.Code != http.StatusFound {
		t.Fatalf("legitimate token after the refill = %d: %s", w.Code, w.Body)
	}
}

func TestCallbackBudgetIsChargedAfterTheFlowValidates(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderOIDC)
	b := env.browser(t)
	for i := range signInBurst {
		if w := b.oidcLogin("/clusters"); w.Code != http.StatusFound {
			t.Fatalf("sign-in %d = %d", i, w.Code)
		}
	}
	if w := b.oidcLogin("/clusters"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("validated callback over budget = %d", w.Code)
	}
}

func TestLogoutStopsCopiesOfTheCookie(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderToken)
	b := env.browser(t)
	signIn(t, b)
	stolen := map[string]*http.Cookie{}
	for k, v := range b.jar {
		stolen[k] = v
	}
	if w := b.do(http.MethodPost, "/logout", url.Values{}, sameOriginForm); w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	b.jar = stolen
	if w := b.get("/api/v1/whoami"); w.Code != http.StatusUnauthorized {
		t.Fatalf("a copy of a signed-out cookie = %d", w.Code)
	}
}

// sharedSignOuts stands for the state ConfigMap both replicas read.
type sharedSignOuts struct {
	mu  sync.Mutex
	ids map[string]time.Time
}

func (s *sharedSignOuts) RecordSignOut(sid, _ string, until time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ids[sid] = until
	return true
}

func (s *sharedSignOuts) SignedOut(sid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.ids[sid]
	return ok
}

func TestLogoutOnOneReplicaStopsCopiesOnTheOther(t *testing.T) {
	t.Parallel()
	shared := &sharedSignOuts{ids: map[string]time.Time{}}
	env := newTestEnv(t, ProviderToken)
	env.h.signOuts = shared
	// Replica B: same session key, same API server, its own memory.
	replicaB := newHandler(Options{
		Config: env.h.cfg, Codec: env.codec, Identity: env.rev,
		Revocations: NewRevocations(nil, "", nil), SignOuts: shared,
	}, nil)
	muxB := http.NewServeMux()
	muxB.Handle("/api/v1/whoami", replicaB.Require(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })))

	b := env.browser(t)
	signIn(t, b)
	whoamiOnB := func() int {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://c.example/api/v1/whoami", nil)
		for _, c := range b.jar {
			req.AddCookie(c)
		}
		w := httptest.NewRecorder()
		muxB.ServeHTTP(w, req)
		return w.Code
	}
	if code := whoamiOnB(); code != http.StatusOK {
		t.Fatalf("replica B before the sign-out = %d", code)
	}
	stolen := map[string]*http.Cookie{}
	for k, v := range b.jar {
		stolen[k] = v
	}
	if w := b.do(http.MethodPost, "/logout", url.Values{}, sameOriginForm); w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	b.jar = stolen
	if code := whoamiOnB(); code != http.StatusUnauthorized {
		t.Fatalf("a copy of a cookie signed out on replica A = %d on replica B", code)
	}
	// The session, and its token as a raw bearer (the MCP endpoint).
	if len(shared.ids) != 2 {
		t.Fatalf("shared sign-outs = %v", shared.ids)
	}
}

func TestStillValid(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderToken)
	p := &Principal{User: env.rev.users["sa-token"], Token: "sa-token", SessionID: "s1", Expiry: time.Now().Add(time.Hour), IssuedAt: time.Now()}
	if !env.h.StillValid(t.Context(), p) {
		t.Fatal("a valid principal was refused")
	}
	env.rev.down = true
	if !env.h.StillValid(t.Context(), p) {
		t.Fatal("an API server outage ended the stream")
	}
	env.rev.down = false
	env.h.markLoggedOut("s1", "", time.Now().Add(time.Hour), true)
	if env.h.StillValid(t.Context(), p) {
		t.Fatal("a signed-out session stayed valid")
	}
	p.SessionID = "s2"
	delete(env.rev.users, "sa-token")
	if env.h.StillValid(t.Context(), p) {
		t.Fatal("a rejected token stayed valid")
	}
}

func TestCrossSiteRequestsDoNotSlideIdleTimer(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderToken)
	b := env.browser(t)
	signIn(t, b)
	now := time.Now()
	env.h.now = func() time.Time { return now.Add(30 * time.Minute) }
	env.codec.now = env.h.now
	before := b.jar["__Host-pgtoolbox-console-0"].Value
	b.do(http.MethodGet, "/api/v1/whoami", nil, map[string]string{"Sec-Fetch-Site": "same-site"})
	if b.jar["__Host-pgtoolbox-console-0"].Value != before {
		t.Fatal("a same-site (neighbour application) request slid the idle timer")
	}
}

// TestReplayedCallbackIsRefusedWithoutCharging: the flow cookie is
// cleared by the callback, but a client that ignores that and replays
// the cookie and state with junk codes is refused by the server, before
// the sign-in budget and without a token exchange.
func TestReplayedCallbackIsRefusedWithoutCharging(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"signed in", "refused by the IdP"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			env := newTestEnv(t, ProviderOIDC)
			b := env.browser(t)
			w := b.get("/auth/oidc/login?rd=/clusters")
			state := env.idp.authorize(t, w.Header().Get("Location"))
			flow := maps.Clone(b.jar)
			code := "c1"
			if outcome != "signed in" {
				env.idp.badNonce = true
			}
			first := b.get("/auth/oidc/callback?code=" + code + "&state=" + url.QueryEscape(state))
			if (outcome == "signed in") != (first.Code == http.StatusFound) {
				t.Fatalf("first callback = %d: %s", first.Code, first.Body)
			}
			env.idp.badNonce = false
			exchanges := env.idp.tokens
			replayer := env.browser(t)
			for i := range 3 * signInBurst {
				replayer.jar = maps.Clone(flow)
				w := replayer.get("/auth/oidc/callback?code=junk-" + strconv.Itoa(i) + "&state=" + url.QueryEscape(state))
				if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "already used") {
					t.Fatalf("replay %d = %d: %s", i, w.Code, w.Body)
				}
			}
			if env.idp.tokens != exchanges {
				t.Fatalf("replays made %d token exchanges", env.idp.tokens-exchanges)
			}
			// The first callback spent one attempt and the replays none:
			// the rest of the burst still signs in from this address.
			for i := range signInBurst - 1 {
				if w := env.browser(t).oidcLogin("/clusters"); w.Code != http.StatusFound {
					t.Fatalf("sign-in %d after the replays = %d: %s", i, w.Code, w.Body)
				}
			}
		})
	}
}

// TestTokenSprayFromOneAddressDoesNotStarveAnother: distinct junk tokens
// from one address stop at that address's budget and leave the shared
// bucket to everyone else's break-glass sign-ins.
func TestTokenSprayFromOneAddressDoesNotStarveAnother(t *testing.T) {
	t.Parallel()
	// Both clients come through the same trusted proxy (httptest's
	// peer, 192.0.2.1), which names each one in X-Forwarded-For.
	env := newTestEnvWith(t, func(c *Config) {
		c.TrustedProxy = ProxyTrust{Hops: 1, CIDRs: []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32")}}
	}, ProviderToken)
	attacker := env.browser(t)
	calls := env.rev.calls
	for i := range 3 * tokenReviewsBurst {
		hdr := maps.Clone(sameOriginForm)
		hdr["X-Forwarded-For"] = "203.0.113.7"
		w := attacker.do(http.MethodPost, "/auth/token", url.Values{"token": {fmt.Sprintf("junk-%d", i)}}, hdr)
		if w.Code != http.StatusForbidden && w.Code != http.StatusTooManyRequests {
			t.Fatalf("junk token %d = %d", i, w.Code)
		}
	}
	if reviews := env.rev.calls - calls; reviews > tokenAddressBurst {
		t.Fatalf("one address made %d reviews, want at most %d", reviews, tokenAddressBurst)
	}
	hdr := maps.Clone(sameOriginForm)
	hdr["X-Forwarded-For"] = "198.51.100.9"
	if w := env.browser(t).do(http.MethodPost, "/auth/token", url.Values{"token": {"sa-token"}}, hdr); w.Code != http.StatusFound {
		t.Fatalf("another address's token after the spray = %d: %s", w.Code, w.Body)
	}
}

// TestTokenSprayDoesNotResetAddressBudgets: enough distinct tokens to
// fill a limiter table reset the token table, never the address budgets
// of the callbacks (which the MCP endpoint shares).
func TestTokenSprayDoesNotResetAddressBudgets(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderOIDC, ProviderToken)
	// The limiters' clock stands still: the spray takes seconds under
	// -race, and a refill during it would hide a reset.
	frozen := time.Now()
	for _, l := range []*RateLimiter{env.h.attempts, env.h.tokenAttempts, env.h.tokenAddresses, env.h.tokenReviews} {
		l.now = func() time.Time { return frozen }
	}
	b := env.browser(t)
	for range signInBurst {
		if w := b.oidcLogin("/clusters"); w.Code != http.StatusFound {
			t.Fatalf("sign-in = %d", w.Code)
		}
	}
	if w := b.oidcLogin("/clusters"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("callback over budget = %d", w.Code)
	}
	sprayer := env.browser(t)
	for i := range maxLimiterKeys + 1 {
		sprayer.do(http.MethodPost, "/auth/token", url.Values{"token": {"junk-" + strconv.Itoa(i)}}, sameOriginForm)
	}
	if w := b.oidcLogin("/clusters"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("callback budget after a token spray = %d, want it still spent", w.Code)
	}
}
