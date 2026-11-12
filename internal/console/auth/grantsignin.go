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

	"golang.org/x/oauth2"

	"github.com/fyannk/pgToolBox/internal/console/authz"
)

// A console grant (an MCP client's, internal/console/mcpoauth) must not
// share the browser session's IdP refresh token: identity providers that
// rotate refresh tokens refuse, or treat as theft, the second use of one,
// and the session and the grant would end each other. A grant of a
// provider that renews tokens therefore gets its own IdP sign-in at
// consent: a second authorization-code round trip whose tokens go to the
// grant alone (a lineage of its own), not to a session cookie. With an
// identity provider that keeps a browser session this round trip is
// silent; one that does not (dex) asks for the password again.

// GrantCompletion receives a grant sign-in's tokens: pending is the value
// BeginGrantSignIn was given, user who the API server says the tokens'
// owner is. It answers the browser.
type GrantCompletion func(w http.ResponseWriter, r *http.Request, pending string, tokens UpstreamTokens, user authz.User)

// SetGrantCompletion installs the receiver of grant sign-ins; without one
// a grant sign-in's callback fails.
func (h *Handler) SetGrantCompletion(f GrantCompletion) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.grantDone = f
}

// Renews reports whether a session of provider renews its IdP token (an
// OIDC sign-in): a grant then gets its own sign-in (BeginGrantSignIn).
// Otherwise a grant carries the session's token, which no one renews, and
// ends with it.
func (h *Handler) Renews(provider string) bool {
	for _, p := range h.oauth {
		if p.name() == provider {
			_, ok := p.(refresher)
			return ok
		}
	}
	return false
}

// BeginGrantSignIn sends the browser to provider's authorization endpoint
// for a grant's own tokens; the callback hands them, with pending, to the
// GrantCompletion. The page it answers moves on by itself (a document's
// navigation, not a redirect of the consent form's submission, which the
// consent page's form-action would have to allow up to the identity
// provider's own redirects).
func (h *Handler) BeginGrantSignIn(w http.ResponseWriter, r *http.Request, provider, pending string) error {
	var p oauthProvider
	for _, x := range h.oauth {
		if x.name() == provider {
			p = x
		}
	}
	if p == nil || pending == "" {
		return errors.New("this sign-in cannot renew its token")
	}
	f := loginFlow{
		Provider: p.name(),
		State:    randomToken(),
		Nonce:    randomToken(),
		Verifier: oauth2.GenerateVerifier(),
		Grant:    pending,
		Expiry:   h.now().Add(loginFlowMaxAge).Unix(),
	}
	// With an OIDC grant client, the grant signs in as that client: an
	// identity provider that keeps one refresh token per person and
	// client (dex) then keeps the browser session's. Same callback.
	client := p
	if p.name() == ProviderOIDC && h.grantOIDC != nil {
		client, f.GrantClient = h.grantOIDC, h.grantOIDC.cfg.ClientID
	}
	if err := h.codec.writeLogin(w, f); err != nil {
		return err
	}
	// The target is the IdP's authorization endpoint from configuration
	// and discovery, not from the request.
	renderPage(w, http.StatusOK, "continue", "Continuing", struct{ Target string }{client.authCodeURL(h.redirectURI(r, p), f)})
	return nil
}

// completeGrant is the callback of a grant sign-in: the API server says
// whose the tokens are, and the GrantCompletion takes over. No session
// cookie is written.
func (h *Handler) completeGrant(w http.ResponseWriter, r *http.Request, provider string, ts tokenSet, pending string) {
	h.mu.Lock()
	done := h.grantDone
	h.mu.Unlock()
	if done == nil {
		h.fail(w, r, provider, &loginError{http.StatusNotFound, "This console does not connect assistants.", nil})
		return
	}
	u, err := h.identity.Review(r.Context(), ts.token)
	if errors.Is(err, ErrTokenRejected) {
		h.fail(w, r, provider, &loginError{http.StatusForbidden, "The Kubernetes API server does not accept this sign-in.", err})
		return
	}
	if err != nil {
		h.fail(w, r, provider, &loginError{http.StatusServiceUnavailable, "The Kubernetes API server could not be reached to check this sign-in; please try again.", err})
		return
	}
	logins.WithLabelValues(provider, "grant").Inc()
	done(w, r, pending, UpstreamTokens{Token: ts.token, RefreshToken: ts.refresh, Expiry: ts.expiry, Provider: provider}, u)
}

// SignOutHook runs on a sign-out that asked to disconnect the person's
// assistants too: nil once they are disconnected on every replica, an
// error when that is not confirmed (the signed-out page then says they
// may still be connected).
type SignOutHook func(ctx context.Context, c Client, user string) error

// SetSignOutHook installs the "and disconnect my assistants" part of
// sign-out; without one the sign-out page does not offer it.
func (h *Handler) SetSignOutHook(f SignOutHook) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.signOutHook = f
}

func (h *Handler) signOutHookFn() SignOutHook {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.signOutHook
}
