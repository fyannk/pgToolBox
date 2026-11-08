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
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/fyannk/pgToolBox/internal/console/authz"
)

func TestKeyedHashes(t *testing.T) {
	t.Parallel()
	other := []byte("another-session-secret-of-32-bytes!!")
	one, two := testCodec(t, true), testCodec(t, true, secretA, other)
	h := two.KeyedHashes("mcp-grant-user", "alice")
	if len(h) != 2 || len(h[0]) != 32 || h[0] == h[1] {
		t.Fatalf("hashes = %v", h)
	}
	if one.KeyedHashes("mcp-grant-user", "alice")[0] != h[0] {
		t.Error("the current secret's hash is not first, or not deterministic")
	}
	for name, x := range map[string]string{
		"another person":  two.KeyedHashes("mcp-grant-user", "bob")[0],
		"another purpose": two.KeyedHashes("other", "alice")[0],
		"another secret":  testCodec(t, true, other).KeyedHashes("mcp-grant-user", "alice")[0],
	} {
		if x == h[0] {
			t.Errorf("%s gave the same hash", name)
		}
	}
}

func TestRefreshFailureKeepsOnlyTheErrorCode(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"error code":  {&oauth2.RetrieveError{ErrorCode: "invalid_grant", Body: []byte(`{"error":"invalid_grant","refresh_token":"rt-secret"}`)}, "invalid_grant"},
		"status only": {&oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusBadGateway}, Body: []byte("rt-secret")}, "HTTP 502"},
		"nothing":     {&oauth2.RetrieveError{Body: []byte("rt-secret")}, "no error code"},
		"control":     {&oauth2.RetrieveError{ErrorCode: "bad\ncode rt-secret"}, "unreadable error code"},
	} {
		err := refreshFailure(c.err)
		if !errors.Is(err, ErrNotRefreshable) || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), "rt-secret") {
			t.Errorf("%s: %v", name, err)
		}
	}
	plain := errors.New("dial tcp: timeout")
	if err := refreshFailure(plain); !errors.Is(err, plain) || errors.Is(err, ErrNotRefreshable) {
		t.Error("a transport error was rewritten")
	}
}

var metaRefresh = regexp.MustCompile(`content="0;url=([^"]+)"`)

// A grant's own IdP sign-in: the continue page goes to the IdP, the
// callback hands the tokens to the completion, and no session cookie is
// written.
func TestGrantSignIn(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderOIDC, ProviderToken)
	if !env.h.Renews(ProviderOIDC) || env.h.Renews(ProviderToken) || env.h.Renews("nope") {
		t.Fatal("Renews")
	}
	type done struct {
		pending string
		tokens  UpstreamTokens
		user    authz.User
	}
	got := make(chan done, 1)
	env.h.SetGrantCompletion(func(w http.ResponseWriter, _ *http.Request, pending string, tokens UpstreamTokens, user authz.User) {
		got <- done{pending, tokens, user}
		w.WriteHeader(http.StatusNoContent)
	})
	env.mux.HandleFunc("POST /test/begin", func(w http.ResponseWriter, r *http.Request) {
		if err := env.h.BeginGrantSignIn(w, r, ProviderOIDC, "pending-1"); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	b := env.browser(t)
	b.oidcLogin("/clusters")
	session := b.jar["__Host-pgtoolbox-console-0"].Value

	w := b.do(http.MethodPost, "/test/begin", url.Values{}, sameOriginForm)
	m := metaRefresh.FindStringSubmatch(w.Body.String())
	if w.Code != http.StatusOK || m == nil {
		t.Fatalf("continue page: %d %s", w.Code, w.Body)
	}
	target := html.UnescapeString(m[1])
	if !strings.HasPrefix(target, env.idp.srv.URL) {
		t.Fatalf("the continue page goes to %s", target)
	}
	state := env.idp.authorize(t, target)
	if w := b.get("/auth/oidc/callback?code=c2&state=" + url.QueryEscape(state)); w.Code != http.StatusNoContent {
		t.Fatalf("callback: %d %s", w.Code, w.Body)
	}
	d := <-got
	if d.pending != "pending-1" || d.user.Name != "oidc:jane@corp.example" || d.tokens.RefreshToken == "" || d.tokens.Token == "" {
		t.Errorf("completion: %+v", d)
	}
	if b.jar["__Host-pgtoolbox-console-0"].Value != session {
		t.Error("a grant sign-in replaced the browser session")
	}
	if strings.Contains(env.logs.String(), d.tokens.RefreshToken) || strings.Contains(env.logs.String(), d.tokens.Token) {
		t.Error("a grant's token reached the log")
	}
	if err := env.h.BeginGrantSignIn(httptestRecorder(), nil, ProviderToken, "p"); err == nil {
		t.Error("a grant sign-in started for a provider that renews nothing")
	}
}

// "Sign out and disconnect my assistants" calls the hook with the person
// the API server named; a plain sign-out does not.
func TestSignOutDisconnectsAssistants(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderToken)
	var calls []string
	b := env.browser(t)
	signIn(t, b)
	if strings.Contains(b.get("/logout").Body.String(), `value="assistants"`) {
		t.Error("the sign-out page offers to disconnect assistants without a hook")
	}
	env.h.SetSignOutHook(func(_ context.Context, c Client, user string) int {
		calls = append(calls, user)
		return 1
	})
	if !strings.Contains(b.get("/logout").Body.String(), `value="assistants"`) {
		t.Error("the sign-out page does not offer to disconnect assistants")
	}
	if w := b.do(http.MethodPost, "/logout", url.Values{}, sameOriginForm); w.Code != http.StatusOK || len(calls) != 0 {
		t.Fatalf("plain sign-out: %d, hook calls %v", w.Code, calls)
	}
	signIn(t, b)
	if w := b.do(http.MethodPost, "/logout", url.Values{"disconnect": {"assistants"}}, sameOriginForm); w.Code != http.StatusOK {
		t.Fatalf("sign-out and disconnect: %d", w.Code)
	}
	if len(calls) != 1 || calls[0] != "system:serviceaccount:team-a:pgtoolbox-console-breakglass" {
		t.Errorf("hook calls %v", calls)
	}
	// Without a session, nothing is disconnected.
	if w := b.do(http.MethodPost, "/logout", url.Values{"disconnect": {"assistants"}}, sameOriginForm); w.Code != http.StatusOK || len(calls) != 1 {
		t.Errorf("sign-out without a session: %d, hook calls %v", w.Code, calls)
	}
}
