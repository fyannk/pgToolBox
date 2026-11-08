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
	"html"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/internal/console/authz"
)

// grantSignIn runs a grant sign-in through env's browser b and returns
// the completion's tokens.
func grantSignIn(t *testing.T, env *testEnv, b *browser) UpstreamTokens {
	t.Helper()
	got := make(chan UpstreamTokens, 1)
	env.h.SetGrantCompletion(func(w http.ResponseWriter, _ *http.Request, _ string, tokens UpstreamTokens, _ authz.User) {
		got <- tokens
		w.WriteHeader(http.StatusNoContent)
	})
	env.mux.HandleFunc("POST /test/begin", func(w http.ResponseWriter, r *http.Request) {
		if err := env.h.BeginGrantSignIn(w, r, ProviderOIDC, "pending-1"); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	w := b.do(http.MethodPost, "/test/begin", url.Values{}, sameOriginForm)
	m := metaRefresh.FindStringSubmatch(w.Body.String())
	if w.Code != http.StatusOK || m == nil {
		t.Fatalf("continue page: %d %s", w.Code, w.Body)
	}
	state := env.idp.authorize(t, html.UnescapeString(m[1]))
	if w := b.get("/auth/oidc/callback?code=c2&state=" + url.QueryEscape(state)); w.Code != http.StatusNoContent {
		t.Fatalf("callback: %d %s", w.Code, w.Body)
	}
	return <-got
}

// with OIDC_GRANT_CLIENT_ID, a grant signs in, redeems its code and
// renews its tokens as the grant client, and the browser session as the
// console's client: an IdP that keeps one refresh token per person and
// client (dex) keeps both.
func TestGrantClientKeepsTheSessionsRefreshToken(t *testing.T) {
	t.Parallel()
	env := newTestEnvWith(t, func(c *Config) {
		c.OIDC.GrantClientID = testGrantClientID
		c.OIDC.GrantClientSecretFile = writeFile(t, testGrantClientSecret)
	}, ProviderOIDC)
	b := env.browser(t)
	if w := b.oidcLogin("/clusters"); w.Code != http.StatusFound {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	tokens := grantSignIn(t, env, b)
	if tokens.Provider != ProviderOIDCGrant || tokens.RefreshToken == "" || !strings.HasPrefix(tokens.Token, "eyJ") {
		t.Fatalf("grant tokens: provider %q, refresh %v", tokens.Provider, tokens.RefreshToken != "")
	}
	if _, err := env.h.RefreshUpstream(context.Background(), tokens.Provider, "grant-x", tokens.RefreshToken); err != nil {
		t.Fatalf("renewing the grant's tokens: %v", err)
	}
	if _, err := env.h.RefreshUpstream(context.Background(), ProviderOIDC, "grant-y", "refresh-1"); err != nil {
		t.Fatalf("renewing with the console's client: %v", err)
	}
	env.idp.mu.Lock()
	authorized, clients := slices.Clone(env.idp.authorizedAs), slices.Clone(env.idp.clients)
	env.idp.mu.Unlock()
	if want := []string{testClientID, testGrantClientID}; !slices.Equal(authorized, want) {
		t.Errorf("authorization requests by %v, want %v", authorized, want)
	}
	// Session code, grant code, grant refresh, console refresh.
	if want := []string{testClientID, testGrantClientID, testGrantClientID, testClientID}; !slices.Equal(clients, want) {
		t.Errorf("token requests by %v, want %v", clients, want)
	}
}

// Without a grant client, a grant signs in as the console's client.
func TestGrantSignInWithoutAGrantClient(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderOIDC)
	b := env.browser(t)
	b.oidcLogin("/clusters")
	if tokens := grantSignIn(t, env, b); tokens.Provider != ProviderOIDC {
		t.Errorf("grant tokens of provider %q", tokens.Provider)
	}
	env.idp.mu.Lock()
	defer env.idp.mu.Unlock()
	if want := []string{testClientID, testClientID}; !slices.Equal(env.idp.authorizedAs, want) {
		t.Errorf("authorization requests by %v, want %v", env.idp.authorizedAs, want)
	}
	if _, err := env.h.RefreshUpstream(context.Background(), ProviderOIDCGrant, "grant-z", "refresh-1"); err == nil {
		t.Error("a grant client's refresh ran without one")
	}
}

// A flow begun with a grant client this replica does not have (another
// configuration during a rollout) is refused, never redeemed with the
// console's client.
func TestGrantClientMismatchIsRefused(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderOIDC)
	b := env.browser(t)
	b.oidcLogin("/clusters")
	w := httptestRecorder()
	if err := env.codec.writeLogin(w, loginFlow{
		Provider: ProviderOIDC, State: "s1", Nonce: "n1", Verifier: "v1", Grant: "pending-1",
		GrantClient: testGrantClientID, Expiry: env.h.now().Add(loginFlowMaxAge).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range w.Result().Cookies() {
		b.jar[c.Name] = c
	}
	if w := b.get("/auth/oidc/callback?code=c2&state=s1"); w.Code != http.StatusBadRequest {
		t.Errorf("callback of a flow begun with another client: %d", w.Code)
	}
	env.idp.mu.Lock()
	defer env.idp.mu.Unlock()
	if len(env.idp.clients) != 1 {
		t.Errorf("the code was redeemed: token requests by %v", env.idp.clients)
	}
}
