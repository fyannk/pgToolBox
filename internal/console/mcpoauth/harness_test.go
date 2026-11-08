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
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/mcpauth"
	"github.com/fyannk/pgToolBox/internal/console/sharedstate"
)

const (
	console   = "https://console.example.com"
	resource  = console + "/mcp"
	loopback  = "http://127.0.0.1:33418/callback"
	verifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r-wW1gFWFOEjXk-and-some-more-entropy"
	sessionID = "0123456789abcdef0123456789abcdef"
)

var t0 = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func challengeOf(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeSessions is the sign-in layer: one signed-in person, IdP tokens the
// "API server" maps to usernames, the revocation list, and an IdP that
// rotates refresh tokens and signs a grant in on its own (renews).
type fakeSessions struct {
	codec *auth.Codec
	clock *clock

	mu        sync.Mutex
	session   *auth.SessionGrant
	tokens    map[string]string
	revoked   map[string]bool
	refreshes int
	reviewErr error
	// renews: the provider renews its tokens, so an approval goes through
	// the grant's own IdP sign-in (BeginGrantSignIn, then the completion).
	renews bool
	// pending is the sealed pending grant of a started grant sign-in.
	pending string
	// grantUser is whom the IdP signs in at a grant sign-in ("": the
	// session's person); grantLogins counts them.
	grantUser   string
	grantLogins int
	completion  auth.GrantCompletion
	signOut     auth.SignOutHook
}

func newSessions(t *testing.T, c *clock, secret string) *fakeSessions {
	t.Helper()
	codec, err := auth.NewCodec([][]byte{[]byte(secret)}, true, auth.Limits{MaxAge: 12 * time.Hour, IdleTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSessions{codec: codec, clock: c, tokens: map[string]string{}, revoked: map[string]bool{}, renews: true}
	f.signIn("alice", "idp-alice")
	return f
}

func (f *fakeSessions) signIn(user, token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[token] = user
	f.session = &auth.SessionGrant{
		Principal: &auth.Principal{User: authz.User{Name: user}, Provider: "oidc", SessionID: sessionID},
		Provider:  "oidc", Token: token, RefreshToken: "rt-" + token, TokenExpiry: f.clock.Now().Add(time.Hour),
	}
}

func (f *fakeSessions) CurrentSession(http.ResponseWriter, *http.Request) (*auth.SessionGrant, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.session, "", nil
}

func (f *fakeSessions) Codec() *auth.Codec  { return f.codec }
func (f *fakeSessions) Limits() auth.Limits { return f.codec.Limits() }

func (f *fakeSessions) Review(_ context.Context, token string) (authz.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reviewErr != nil {
		return authz.User{}, f.reviewErr
	}
	u, ok := f.tokens[token]
	if !ok {
		return authz.User{}, auth.ErrTokenRejected
	}
	return authz.User{Name: u}, nil
}

func (f *fakeSessions) Revoked(user string, _ time.Time, refs ...string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.revoked["user:"+user] {
		return true
	}
	for _, r := range refs {
		if f.revoked[r] {
			return true
		}
	}
	return false
}

// SignedOut: a session is signed out when its ID is revoked as "out:<id>".
func (f *fakeSessions) SignedOut(session string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revoked["out:"+session]
}

func (f *fakeSessions) RefreshUpstream(_ context.Context, _, _, refreshToken string) (auth.UpstreamTokens, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	user, ok := f.tokens[strings.TrimPrefix(refreshToken, "rt-")]
	if !ok {
		return auth.UpstreamTokens{}, auth.ErrNotRefreshable
	}
	f.refreshes++
	tok := fmt.Sprintf("idp-%s-%d", user, f.refreshes)
	f.tokens[tok] = user
	return auth.UpstreamTokens{Token: tok, RefreshToken: "rt-" + tok, Expiry: f.clock.Now().Add(time.Hour)}, nil
}

func (f *fakeSessions) Renews(string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.renews
}

func (f *fakeSessions) BeginGrantSignIn(w http.ResponseWriter, _ *http.Request, _, pending string) error {
	f.mu.Lock()
	f.pending = pending
	f.mu.Unlock()
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("continue to the identity provider"))
	return nil
}

func (f *fakeSessions) SetGrantCompletion(c auth.GrantCompletion) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completion = c
}

func (f *fakeSessions) SetSignOutHook(h auth.SignOutHook) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signOut = h
}

// takePending returns and forgets the started grant sign-in.
func (f *fakeSessions) takePending() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pending
	f.pending = ""
	return p
}

// grantSignIn is the IdP's answer to a grant's own sign-in: a new token
// lineage, never the session's.
func (f *fakeSessions) grantSignIn() (auth.UpstreamTokens, authz.User) {
	f.mu.Lock()
	defer f.mu.Unlock()
	user := f.grantUser
	if user == "" && f.session != nil {
		user = f.session.Principal.User.Name
	}
	f.grantLogins++
	tok := fmt.Sprintf("idp-%s-g%d", user, f.grantLogins)
	f.tokens[tok] = user
	return auth.UpstreamTokens{Token: tok, RefreshToken: "rt-" + tok, Expiry: f.clock.Now().Add(time.Hour)}, authz.User{Name: user}
}

func (f *fakeSessions) revoke(ref string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked[ref] = true
}

// replica is one console replica: its authorization server mounted the
// way the console mounts it.
type replica struct {
	s   *Server
	mux *http.ServeMux
	// f and complete are the sign-in layer and the completion this
	// replica's server installed in it.
	f        *fakeSessions
	complete auth.GrantCompletion
}

func newReplica(t *testing.T, sessions SessionSource, store GrantStore, c *clock, mod func(*Options)) *replica {
	t.Helper()
	o := Options{Sessions: sessions, Store: store, PublicURL: console, AccessTTL: time.Hour, Now: c.Now}
	if mod != nil {
		mod(&o)
	}
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.Register(mux)
	s.RegisterSession(mux)
	r := &replica{s: s, mux: mux}
	if f, ok := sessions.(*fakeSessions); ok {
		f.mu.Lock()
		r.f, r.complete = f, f.completion
		f.mu.Unlock()
	}
	return r
}

// addresses gives each request its own client address, so the per-address
// budgets do not interfere with the flows under test (TestBudgets tests
// them).
var addresses atomic.Uint32

func (r *replica) do(req *http.Request) *httptest.ResponseRecorder {
	if req.RemoteAddr == "192.0.2.1:1234" { // httptest's default
		n := addresses.Add(1)
		req.RemoteAddr = fmt.Sprintf("10.%d.%d.%d:1234", byte(n>>16), byte(n>>8), byte(n))
	}
	w := httptest.NewRecorder()
	r.mux.ServeHTTP(w, req)
	return w
}

func (r *replica) register(t *testing.T, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, console+PathRegister, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := r.do(req)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func (r *replica) clientID(t *testing.T) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{
		"client_name": "Claude Code", "redirect_uris": []string{loopback}, "token_endpoint_auth_method": "none",
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
	})
	w, out := r.register(t, string(b))
	if w.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}
	return out["client_id"].(string)
}

func authorizeQuery(clientID, redirect string) url.Values {
	return url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect}, "state": {"st-1"},
		"code_challenge": {challengeOf(verifier)}, "code_challenge_method": {"S256"}, "resource": {resource},
		"scope": {"mcp:read"},
	}
}

func (r *replica) authorize(q url.Values) *httptest.ResponseRecorder {
	return r.do(httptest.NewRequestWithContext(context.Background(), http.MethodGet, console+PathAuthorize+"?"+q.Encode(), nil))
}

var consentRequestField = regexp.MustCompile(`name="request" value="([^"]+)"`)

// consent runs the authorization request and answers the consent page;
// it returns the redirect's query.
func (r *replica) consent(t *testing.T, q url.Values, decision string) url.Values {
	t.Helper()
	return r.consentWith(t, q, url.Values{"decision": {decision}})
}

// consentPage runs the authorization request and returns the consent
// page's sealed request.
func (r *replica) consentPage(t *testing.T, q url.Values) string {
	t.Helper()
	w := r.authorize(q)
	m := consentRequestField.FindStringSubmatch(w.Body.String())
	if w.Code != http.StatusOK || m == nil {
		t.Fatalf("consent page: %d %s", w.Code, w.Body)
	}
	return m[1]
}

// answer posts the consent form; an approval that starts the grant's own
// IdP sign-in is completed as the sign-in layer's callback would.
func (r *replica) answer(t *testing.T, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, console+PathAuthorize, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := r.do(req)
	if r.f == nil {
		return w
	}
	pending := r.f.takePending()
	if pending == "" {
		return w
	}
	if w.Code != http.StatusOK {
		t.Fatalf("grant sign-in started with %d", w.Code)
	}
	return r.finishGrantSignIn(pending)
}

// finishGrantSignIn is the sign-in layer's callback of a grant sign-in.
func (r *replica) finishGrantSignIn(pending string) *httptest.ResponseRecorder {
	tokens, user := r.f.grantSignIn()
	w := httptest.NewRecorder()
	r.complete(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, console+"/auth/callback", nil), pending, tokens, user)
	return w
}

// consentWith answers the consent page with form (decision, access) and
// returns the redirect's query.
func (r *replica) consentWith(t *testing.T, q url.Values, form url.Values) url.Values {
	t.Helper()
	form.Set("request", r.consentPage(t, q))
	return redirectQuery(t, r.answer(t, form))
}

func redirectQuery(t *testing.T, w *httptest.ResponseRecorder) url.Values {
	t.Helper()
	if w.Code != http.StatusFound {
		t.Fatalf("decision: %d %s", w.Code, w.Body)
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return loc.Query()
}

type tokens struct {
	Access, Refresh string
	ExpiresIn       int64
	Scope           string
}

func (r *replica) token(t *testing.T, form url.Values) (*httptest.ResponseRecorder, tokens, string) {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, console+PathToken, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := r.do(req)
	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
		Error        string `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w, tokens{body.AccessToken, body.RefreshToken, body.ExpiresIn, body.Scope}, body.Error
}

func codeForm(clientID, code string) url.Values {
	return url.Values{
		"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code},
		"redirect_uri": {loopback}, "code_verifier": {verifier}, "resource": {resource},
	}
}

func refreshForm(clientID, refresh string) url.Values {
	return url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {refresh}}
}

// connect runs the whole flow: registration, consent, code exchange.
func (r *replica) connect(t *testing.T) (string, tokens) {
	t.Helper()
	id := r.clientID(t)
	q := r.consent(t, authorizeQuery(id, loopback), "approve")
	w, tok, _ := r.token(t, codeForm(id, q.Get("code")))
	if w.Code != http.StatusOK {
		t.Fatalf("code exchange: %d %s", w.Code, w.Body)
	}
	return id, tok
}

func (r *replica) authenticate(token string) (*auth.Principal, error) {
	return r.s.Authenticate(mcpauth.WithResource(context.Background(), resource), token)
}

func memoryStore(c *clock) *sharedstate.Store {
	return sharedstate.New(sharedstate.Options{Now: c.Now})
}

// unthrottled lifts the per-person budget of consent answers, for tests
// that connect many times.
func (r *replica) unthrottled() *replica {
	r.s.decisions = auth.NewRateLimiter(1<<20, 1<<20, maxLimiterKeys)
	return r
}
