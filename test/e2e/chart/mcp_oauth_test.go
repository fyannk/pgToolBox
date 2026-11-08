//go:build e2echart

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

package chart

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// oauthAccessTTL is the console's MCP_OAUTH_ACCESS_TTL in this scenario:
// short, so the client's refresh runs within the test.
const oauthAccessTTL = 20 * time.Second

// oauthRedirect is the client's loopback redirect URI. Nothing listens
// there: the fetcher reads the code from the console's redirect instead.
const oauthRedirect = "http://127.0.0.1:43177/callback"

var consentRequest = regexp.MustCompile(`name="request" value="([^"]+)"`)

// oauthConnection is an MCP client signed in through the console's own
// OAuth flow, as Claude Code or Codex would be, by the person's browser.
type oauthConnection struct {
	session *mcp.ClientSession
	handler *auth.AuthorizationCodeHandler
	browser *browser
}

// ssoTo runs dex's sign-in from the console's login start with rd, and
// returns the page the browser ends on. A 429 or 403 is waited out, as in
// signInSSO.
func (b *browser) ssoTo(base string, u dexUser, rd string) reply {
	b.t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		r := b.ssoToOnce(base, u, rd)
		if (r.Status != http.StatusTooManyRequests && r.Status != http.StatusForbidden) || time.Now().After(deadline) {
			return r
		}
		time.Sleep(8 * time.Second)
	}
}

func (b *browser) ssoToOnce(base string, u dexUser, rd string) reply {
	b.t.Helper()
	page := b.get(base + "/auth/oidc/login?rd=" + url.QueryEscape(rd))
	m := formAction.FindSubmatch(page.Body)
	if page.Status != http.StatusOK || m == nil {
		b.t.Fatalf("no dex login form at %s (%d): %s", page.URL, page.Status, tailBytes(page.Body))
	}
	action, err := page.URL.Parse(html.UnescapeString(string(m[1])))
	if err != nil {
		b.t.Fatal(err)
	}
	return b.do(http.MethodPost, action.String(), url.Values{"login": {u.Email}, "password": {u.Password}}, nil)
}

var metaRefresh = regexp.MustCompile(`http-equiv="refresh" content="0;url=([^"]+)"`)

// errRetry is a sign-in the console or dex throttled: the whole flow is run
// again a little later.
var errRetry = errors.New("throttled; retry")

// fetcher is the person at the browser: the authorization URL, the
// console's sign-in when there is no session (and back), the consent page,
// Approve (read-only), the grant's own sign-in at dex; the code is read
// from the redirect to the loopback URI.
func (f *dexFixture) fetcher(br *browser, base, who string) auth.AuthorizationCodeFetcher {
	return func(_ context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		deadline := time.Now().Add(2 * time.Minute)
		for {
			res, err := f.fetchOnce(br, base, who, args.URL) //nolint:contextcheck // the browser bounds each request by its own 30 s retry deadline
			if !errors.Is(err, errRetry) || time.Now().After(deadline) {
				return res, err
			}
			time.Sleep(8 * time.Second)
		}
	}
}

func (f *dexFixture) fetchOnce(br *browser, base, who, authorizeURL string) (*auth.AuthorizationResult, error) {
	page := br.noRedirect().get(authorizeURL)
	if page.Status == http.StatusFound {
		loc, err := url.Parse(page.Header.Get("Location"))
		if err != nil || loc.Path != "/auth/login" || !strings.HasPrefix(loc.Query().Get("rd"), "/oauth/authorize?") {
			return nil, fmt.Errorf("without a session /oauth/authorize sent the browser to %q", page.Header.Get("Location"))
		}
		// Signed in, the browser comes back to the authorization request.
		page = br.ssoTo(base, f.Users[who], loc.Query().Get("rd"))
	}
	m := consentRequest.FindSubmatch(page.Body)
	if page.Status != http.StatusOK || m == nil || !strings.Contains(string(page.Body), "(unverified)") ||
		!strings.Contains(string(page.Body), "127.0.0.1:43177/callback") {
		return nil, fmt.Errorf("no consent page (%d at %s): %s", page.Status, page.URL, tailBytes(page.Body))
	}
	if csp := page.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") ||
		!strings.Contains(csp, "form-action 'self' http://127.0.0.1:43177") {
		return nil, fmt.Errorf("consent page CSP %q", csp)
	}
	answer := br.noRedirect().do(http.MethodPost, base+"/oauth/authorize",
		url.Values{"request": {html.UnescapeString(string(m[1]))}, "decision": {"approve"}, "access": {"read"}},
		map[string]string{"Origin": origin(base)})
	// An OIDC sign-in renews its token: the grant gets its own sign-in at
	// dex, which keeps no browser session and asks for the password again.
	r := answer
	if c := metaRefresh.FindSubmatch(answer.Body); answer.Status == http.StatusOK && c != nil {
		stop := br.stopAt(oauthRedirect)
		login := stop.get(html.UnescapeString(string(c[1])))
		fa := formAction.FindSubmatch(login.Body)
		if login.Status != http.StatusOK || fa == nil {
			return nil, fmt.Errorf("the grant's sign-in found no dex login form (%d at %s): %s", login.Status, login.URL, tailBytes(login.Body))
		}
		action, err := login.URL.Parse(html.UnescapeString(string(fa[1])))
		if err != nil {
			return nil, err
		}
		u := f.Users[who]
		r = stop.do(http.MethodPost, action.String(), url.Values{"login": {u.Email}, "password": {u.Password}}, nil)
		if r.Status == http.StatusTooManyRequests || r.Status == http.StatusForbidden {
			return nil, errRetry
		}
	}
	loc, err := url.Parse(r.Header.Get("Location"))
	if r.Status != http.StatusFound || err != nil || !strings.HasPrefix(loc.String(), oauthRedirect+"?") {
		return nil, fmt.Errorf("consent answered %d, Location %q: %s", r.Status, r.Header.Get("Location"), tailBytes(r.Body))
	}
	q := loc.Query()
	return &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}, nil
}

// oauthConnect registers a client with the console, runs the code flow with
// PKCE as who, and opens an MCP session with the access token.
func (f *dexFixture) oauthConnect(t *testing.T, hosts map[string]string, base, who string) *oauthConnection {
	t.Helper()
	br := newBrowser(t, hosts, f.CA)
	hc := newBrowser(t, hosts, nil).build(false)
	hc.Jar = nil
	h, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			RedirectURIs: []string{oauthRedirect}, ClientName: "pgtoolbox e2e (" + who + ")",
			GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"},
			TokenEndpointAuthMethod: "none",
		}},
		RedirectURL:              oauthRedirect,
		AuthorizationCodeFetcher: f.fetcher(br, base, who),
		Client:                   hc,
	})
	if err != nil {
		t.Fatal(err)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "pgtoolbox-e2e-oauth", Version: "v0"}, nil)
	cs, err := c.Connect(ctx(), &mcp.StreamableClientTransport{Endpoint: base + "/mcp", HTTPClient: hc, MaxRetries: -1, OAuthHandler: h}, nil)
	if err != nil {
		t.Fatalf("MCP connect to %s as %s through the console's OAuth: %v", base, who, err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return &oauthConnection{session: cs, handler: h, browser: br}
}

func (o *oauthConnection) accessToken(t *testing.T) (string, time.Time) {
	t.Helper()
	ts, err := o.handler.TokenSource(ctx())
	if err != nil || ts == nil {
		t.Fatalf("token source: %v", err)
	}
	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	return tok.AccessToken, tok.Expiry
}

// clusters is list_clusters as namespace/name, sorted.
func (o *oauthConnection) clusters(t *testing.T) []string {
	t.Helper()
	res, err := o.session.CallTool(ctx(), &mcp.CallToolParams{Name: "list_clusters", Arguments: map[string]any{}})
	// The token endpoint answers temporarily_unavailable while the API
	// server does not answer its identity review (a loaded host's etcd
	// hiccup): a client retries, and so does this one, for a while.
	for deadline := time.Now().Add(45 * time.Second); err != nil && strings.Contains(err.Error(), "temporarily_unavailable") && time.Now().Before(deadline); {
		t.Logf("list_clusters: %v; retrying", err)
		time.Sleep(3 * time.Second)
		res, err = o.session.CallTool(ctx(), &mcp.CallToolParams{Name: "list_clusters", Arguments: map[string]any{}})
	}
	if err != nil || res.IsError {
		t.Fatalf("list_clusters: %v %+v", err, res)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Data struct {
			Items []struct {
				Ref struct{ Namespace, Name string } `json:"ref"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("list_clusters: %v: %s", err, tailBytes(raw))
	}
	out := make([]string, 0, len(env.Data.Items))
	for _, it := range env.Data.Items {
		out = append(out, it.Ref.Namespace+"/"+it.Ref.Name)
	}
	return sortedStrings(out)
}

// grants is the browser's GET /api/v1/session/grants.
func (o *oauthConnection) grants(t *testing.T, base string) []struct {
	ID     string   `json:"id"`
	Client string   `json:"client"`
	Scopes []string `json:"scopes"`
} {
	t.Helper()
	r := o.browser.get(base + "/api/v1/session/grants")
	var list struct {
		Items []struct {
			ID     string   `json:"id"`
			Client string   `json:"client"`
			Scopes []string `json:"scopes"`
		} `json:"items"`
	}
	if r.Status != http.StatusOK || json.Unmarshal(r.Body, &list) != nil {
		t.Fatalf("GET /api/v1/session/grants = %d: %s", r.Status, tailBytes(r.Body))
	}
	return list.Items
}

// bearerStatus is the status of an MCP request with token, on a new
// connection (so that the Service picks a replica at random).
func bearerStatus(t *testing.T, hosts map[string]string, base, token string) int {
	t.Helper()
	hc := newBrowser(t, hosts, nil).build(true)
	hc.Jar = nil
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"0"}}}`)
	req, err := http.NewRequestWithContext(ctx(), http.MethodPost, base+"/mcp", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// refusedEverywhere asserts that, while token is still unexpired, six
// requests in a row on new connections (the Service picks a replica each
// time: both are all but sure to be asked) are refused with 401. A 200
// starts the count again. A 429 (the per-address budget for tokens a
// replica does not know as accepted) says nothing either way: it is waited
// out. The token expiring first proves nothing and fails.
func refusedEverywhere(t *testing.T, hosts map[string]string, base, token string, expiry time.Time, what string) {
	t.Helper()
	var seen []int
	refused := 0
	for refused < 6 {
		if !time.Now().Before(expiry.Add(-time.Second)) {
			t.Errorf("%s: the access token expired before every replica was seen refusing it (statuses %v)", what, seen)
			return
		}
		st := bearerStatus(t, hosts, base, token)
		seen = append(seen, st)
		switch st {
		case http.StatusUnauthorized:
			refused++
			time.Sleep(200 * time.Millisecond)
		case http.StatusTooManyRequests:
			time.Sleep(2 * time.Second)
		default:
			refused = 0
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// consoleWarnings is what the console's pods logged at WARN or ERROR
// level, for a failure's message (no token reaches these lines).
func consoleWarnings(c console) string {
	out, _ := exec.CommandContext(context.Background(), "kubectl", "-n", c.Namespace, "logs", "-l",
		"app.kubernetes.io/name=pgtoolbox-console,app.kubernetes.io/instance="+c.Release,
		"--all-containers", "--prefix", "--tail=400").CombinedOutput()
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		if strings.Contains(l, `"level":"WARN"`) || strings.Contains(l, `"level":"ERROR"`) {
			lines = append(lines, l)
		}
	}
	return tail(strings.Join(lines, "\n"), 6000)
}

// liveToken is an access token of o with at least 15 seconds left: the
// client reuses its token until it is about to expire, so a token near
// its end is waited out and the next one taken.
func (o *oauthConnection) liveToken(t *testing.T) (string, time.Time) {
	t.Helper()
	tok, exp := o.accessToken(t)
	if time.Until(exp) < 15*time.Second {
		time.Sleep(time.Until(exp) + time.Second)
		tok, exp = o.accessToken(t)
	}
	return tok, exp
}

// mcpOAuthScenario: each person connects an MCP client through the
// console's OAuth flow; it sees exactly the REST table; its access token
// is refreshed after expiry; a disconnected grant is refused by every
// replica.
func mcpOAuthScenario(t *testing.T, c console, dex *dexFixture, hosts map[string]string, base string, want map[string][]string) {
	t.Helper()
	for _, p := range consolePods(t, c) {
		if got := envOf(&p.Spec.Containers[0])["MCP_OAUTH_ACCESS_TTL"]; got != oauthAccessTTL.String() {
			t.Fatalf("pod %s: MCP_OAUTH_ACCESS_TTL=%q, want %s", p.Name, got, oauthAccessTTL)
		}
	}
	conns := map[string]*oauthConnection{}
	signedIn := time.Now() // dba's sign-ins at dex: the browser's, then the grant's
	// connected: when each person's browser signed in at dex (no earlier).
	connected := map[string]time.Time{}
	for _, who := range []string{"dba", "ops", "stranger"} {
		connected[who] = time.Now()
		o := dex.oauthConnect(t, hosts, base, who)
		conns[who] = o
		got := o.clusters(t)
		rest, st := o.browser.clusterNames(base)
		if st != http.StatusOK || strings.Join(got, ",") != strings.Join(rest, ",") || strings.Join(got, ",") != strings.Join(want[who], ",") {
			t.Errorf("OAuth MCP: %s sees %v; the REST table %v (HTTP %d); want %v", who, got, rest, st, want[who])
		}
		// The grant index reaches the other replica within about a second.
		eventually(t, 10*time.Second, who+"'s grant is listed", func() error {
			if gs := o.grants(t, base); len(gs) != 1 || !strings.HasPrefix(gs[0].Client, "pgtoolbox e2e") ||
				strings.Join(gs[0].Scopes, " ") != "mcp:read" {
				return fmt.Errorf("%s's grants = %+v", who, gs)
			}
			return nil
		})
	}

	dba := conns["dba"]
	first, firstExpiry := dba.accessToken(t)
	if left := time.Until(firstExpiry); left <= 0 || left > oauthAccessTTL+5*time.Second {
		t.Fatalf("the access token expires in %s, want at most %s", left, oauthAccessTTL)
	}
	if st := bearerStatus(t, hosts, base, first); st == http.StatusUnauthorized {
		t.Fatalf("a fresh access token is refused")
	}
	// Past the access token's lifetime the client refreshes by itself; past
	// the console's two-minute lead before the end of dex's ID token, that
	// refresh renews the grant's IdP token at dex, with the grant's own
	// refresh token.
	renewal := signedIn.Add(dexIDTokenTTL - 2*time.Minute + 10*time.Second)
	time.Sleep(max(time.Until(renewal), oauthAccessTTL+5*time.Second))
	if st := bearerStatus(t, hosts, base, first); st != http.StatusUnauthorized {
		t.Errorf("an expired access token answered %d", st)
	}
	if got := dba.clusters(t); strings.Join(got, ",") != strings.Join(want["dba"], ",") {
		t.Errorf("after the refresh dba sees %v", got)
	}
	second, _ := dba.accessToken(t)
	if second == first {
		t.Fatal("the access token was not refreshed")
	}
	// The browser sessions renew too: the grants signed in at dex
	// with the grant client, so dex's one refresh token per person and
	// client left the sessions' own. The session page renews a session
	// within two minutes of its ID token's end; it is asked 45 seconds
	// before, after the grant's sign-in.
	for _, who := range []string{"dba", "ops"} {
		time.Sleep(time.Until(connected[who].Add(dexIDTokenTTL - 45*time.Second)))
		v, st := conns[who].browser.session(base)
		switch {
		case st != http.StatusOK:
			t.Errorf("%s's session page before the ID token's end: HTTP %d", who, st)
		case !v.Refreshable || v.TokenExpiresAt == nil || !v.TokenExpiresAt.After(connected[who].Add(dexIDTokenTTL+time.Minute)):
			t.Errorf("%s's session did not renew its ID token (refreshable %v, token until %v, first token until about %s); console warnings:\n%s",
				who, v.Refreshable, v.TokenExpiresAt, connected[who].Add(dexIDTokenTTL).UTC().Format(time.RFC3339), consoleWarnings(c))
		}
	}
	// Past the end of the first ID token the grant still works: it renewed
	// at dex by itself, again. So does the browser session.
	time.Sleep(time.Until(signedIn.Add(dexIDTokenTTL + 10*time.Second)))
	if got := dba.clusters(t); strings.Join(got, ",") != strings.Join(want["dba"], ",") {
		t.Errorf("dba's grant past the first ID token's end sees %v", got)
	}
	if got, st := dba.browser.clusterNames(base); st != http.StatusOK || strings.Join(got, ",") != strings.Join(want["dba"], ",") {
		t.Errorf("dba's browser past the first ID token's end, after a grant's sign-in: %v (HTTP %d); console warnings:\n%s", got, st, consoleWarnings(c))
	}

	// Disconnect the grant from the session page: every replica refuses it.
	gs := dba.grants(t, base)
	if len(gs) != 1 {
		t.Fatalf("dba's grants = %+v", gs)
	}
	live, liveExpiry := dba.liveToken(t)
	if st := bearerStatus(t, hosts, base, live); st == http.StatusUnauthorized {
		t.Fatal("dba's live access token is refused before the revocation")
	}
	r := dba.browser.do(http.MethodDelete, base+"/api/v1/session/grants/"+gs[0].ID, nil,
		map[string]string{"Origin": origin(base), "X-PgToolBox-Request": "1"})
	if r.Status != http.StatusNoContent {
		t.Fatalf("DELETE the grant = %d: %s", r.Status, tailBytes(r.Body))
	}
	refusedEverywhere(t, hosts, base, live, liveExpiry, "dba's revoked grant")
	if gs := dba.grants(t, base); len(gs) != 0 {
		t.Errorf("a revoked grant is still listed: %+v", gs)
	}
	// The others are untouched (ops's grant renews at dex here, its first
	// ID token being over).
	ops := conns["ops"]
	opsToken, _ := ops.accessToken(t)
	if st := bearerStatus(t, hosts, base, opsToken); st == http.StatusUnauthorized {
		t.Errorf("ops's grant was refused after dba's revocation")
	}

	// ops's browser session renewed as well, without a new sign-in.
	time.Sleep(time.Until(connected["ops"].Add(dexIDTokenTTL + 10*time.Second)))
	if got, st := ops.browser.clusterNames(base); st != http.StatusOK || strings.Join(got, ",") != strings.Join(want["ops"], ",") {
		t.Errorf("ops's browser past the first ID token's end, after a grant's sign-in: %v (HTTP %d); console warnings:\n%s", got, st, consoleWarnings(c))
	}

	// "Disconnect every assistant" ends all of a person's grants. The
	// grant's IdP token was just renewed: its next console refreshes need
	// no dex refresh, so a live token is still at hand.
	opsLive, opsLiveExpiry := ops.liveToken(t)
	if st := bearerStatus(t, hosts, base, opsLive); st == http.StatusUnauthorized {
		t.Fatal("ops's live access token is refused before the disconnect")
	}
	r = ops.browser.do(http.MethodDelete, base+"/api/v1/session/grants", nil,
		map[string]string{"Origin": origin(base), "X-PgToolBox-Request": "1"})
	if r.Status != http.StatusNoContent {
		t.Fatalf("DELETE all grants = %d: %s", r.Status, tailBytes(r.Body))
	}
	refusedEverywhere(t, hosts, base, opsLive, opsLiveExpiry, "ops's disconnected grant")
	if gs := ops.grants(t, base); len(gs) != 0 {
		t.Errorf("ops's grants after disconnecting all: %+v", gs)
	}
}
