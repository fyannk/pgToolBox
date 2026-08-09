/*
Copyright © contributors to the pgtoolbox project.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

// Package oidc implements the oidc authentication provider: an
// authorization-code flow with PKCE (S256), state, and nonce, with the
// flow state carried in a short-lived sealed transient cookie.
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/fyannk/pgtoolbox/internal/proxy/config"
	"github.com/fyannk/pgtoolbox/internal/proxy/pages"
	"github.com/fyannk/pgtoolbox/internal/proxy/server"
)

// transientMaxAge bounds the login flow; the transient cookie carrying
// state, nonce, and PKCE verifier dies with it.
const transientMaxAge = 10 * time.Minute

// transient is the sealed login-flow state.
type transient struct {
	State      string `json:"state"`
	Nonce      string `json:"nonce"`
	Verifier   string `json:"verifier"`
	RedirectTo string `json:"rd"`
	Expiry     int64  `json:"exp"`
}

// Provider is the OIDC authentication provider.
type Provider struct {
	env          *server.Env
	oauth2       oauth2.Config
	verifier     *gooidc.IDTokenVerifier
	subjectClaim string
}

// New discovers the issuer and builds the provider. The clientSecret is
// read by the caller from the configured file; it is never logged.
func New(ctx context.Context, env *server.Env, cfg *config.OIDCConfig, clientSecret string) (*Provider, error) {
	discovered, err := gooidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("discovering OIDC issuer: %w", err)
	}
	return &Provider{
		env: env,
		oauth2: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: clientSecret,
			Endpoint:     discovered.Endpoint(),
			RedirectURL:  cfg.RedirectURL,
			Scopes:       cfg.Scopes,
		},
		verifier:     discovered.Verifier(&gooidc.Config{ClientID: cfg.ClientID}),
		subjectClaim: cfg.SubjectClaim,
	}, nil
}

// callbackPath is where the provider returns the browser.
const callbackPath = "/auth/oidc/callback"

// Mode implements server.Provider.
func (p *Provider) Mode() string { return config.ModeOIDC }

// Register implements server.Provider.
func (p *Provider) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/oidc/login", p.handleLogin)
	mux.HandleFunc("GET "+callbackPath, p.handleCallback)
}

// handleLogin starts the flow: it seals state, nonce, PKCE verifier, and
// the validated redirect target into a transient cookie, then redirects
// to the provider.
func (p *Provider) handleLogin(w http.ResponseWriter, r *http.Request) {
	rt := p.env.Runtime()
	rd := server.SafeRedirect(r.URL.Query().Get("rd"))
	t := transient{
		State:      randomToken(),
		Nonce:      randomToken(),
		Verifier:   oauth2.GenerateVerifier(),
		RedirectTo: rd,
		Expiry:     time.Now().Add(transientMaxAge).Unix(),
	}
	v, err := rt.Codec.Seal(t)
	if err != nil {
		p.env.Logger.Error("sealing transient cookie failed", "error", err)
		pages.Error(w, http.StatusInternalServerError, "The login flow could not be started.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     transientCookieName(rt),
		Value:    v,
		Path:     "/",
		MaxAge:   int(transientMaxAge.Seconds()),
		HttpOnly: true,
		Secure:   rt.Config.Session.CookieSecure(),
		SameSite: http.SameSiteLaxMode,
	})
	config := p.configFor(r)
	url := config.AuthCodeURL(t.State,
		gooidc.Nonce(t.Nonce),
		oauth2.S256ChallengeOption(t.Verifier),
	)
	http.Redirect(w, r, url, http.StatusFound)
}

// handleCallback completes the flow. It rejects on state mismatch, nonce
// mismatch, an expired or tampered transient cookie, and any ID-token
// verification failure.
func (p *Provider) handleCallback(w http.ResponseWriter, r *http.Request) {
	rt := p.env.Runtime()
	clear := func() {
		http.SetCookie(w, &http.Cookie{
			Name:   transientCookieName(rt),
			Value:  "",
			Path:   "/",
			MaxAge: -1,
		})
	}
	fail := func(status int, msg string) {
		clear()
		pages.Error(w, status, msg)
	}

	if errParam := r.URL.Query().Get("error"); errParam != "" {
		fail(http.StatusBadRequest, "The identity provider returned an error.")
		return
	}
	t, err := p.readTransient(r, rt)
	if err != nil {
		fail(http.StatusBadRequest, "The login flow expired; please try again.")
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("state")), []byte(t.State)) != 1 {
		fail(http.StatusBadRequest, "Login state mismatch; please try again.")
		return
	}
	exchange := p.configFor(r)
	token, err := exchange.Exchange(r.Context(), r.URL.Query().Get("code"),
		oauth2.VerifierOption(t.Verifier))
	if err != nil {
		p.env.Logger.Warn("OIDC code exchange failed", "error", err)
		fail(http.StatusBadGateway, "The identity provider could not complete the login.")
		return
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		fail(http.StatusBadGateway, "The identity provider did not return an ID token.")
		return
	}
	idToken, err := p.verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		p.env.Logger.Warn("OIDC ID token verification failed", "error", err)
		fail(http.StatusBadRequest, "The identity token could not be verified.")
		return
	}
	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(t.Nonce)) != 1 {
		fail(http.StatusBadRequest, "Login nonce mismatch; please try again.")
		return
	}
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		fail(http.StatusBadGateway, "The identity token claims could not be read.")
		return
	}
	subject, _ := claims[p.subjectClaim].(string)
	if subject == "" {
		fail(http.StatusForbidden, fmt.Sprintf("The identity token has no %q claim.", p.subjectClaim))
		return
	}
	subject = strings.ToLower(subject)

	level := config.LevelNone
	if u, ok := p.env.LookupUser(subject); ok {
		level = u.Level
	}
	if err := p.env.IssueSession(w, subject, level, config.ModeOIDC); err != nil {
		p.env.Logger.Error("issuing session failed", "error", err)
		fail(http.StatusInternalServerError, "The session could not be created.")
		return
	}
	clear()
	// RedirectTo was validated at login time; validate again defensively.
	http.Redirect(w, r, server.SafeRedirect(t.RedirectTo), http.StatusFound)
}

// readTransient opens and validates the transient cookie.
func (p *Provider) readTransient(r *http.Request, rt *server.Runtime) (transient, error) {
	cookie, err := r.Cookie(transientCookieName(rt))
	if err != nil {
		return transient{}, fmt.Errorf("no transient cookie")
	}
	var t transient
	if err := rt.Codec.Open(cookie.Value, &t); err != nil {
		return transient{}, err
	}
	if time.Now().Unix() >= t.Expiry || t.State == "" || t.Nonce == "" || t.Verifier == "" {
		return transient{}, fmt.Errorf("transient cookie is expired or incomplete")
	}
	return t, nil
}

func transientCookieName(rt *server.Runtime) string {
	return rt.Config.Session.CookieName + "_oauth"
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Errorf("crypto/rand unavailable: %w", err))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// configFor returns the OAuth2 configuration to use for one request,
// resolving the redirect URI against the origin the browser actually used
// when the deployment has not pinned one.
//
// The identity provider sends the browser to that URI, so it has to name
// the address the browser reached the proxy on — not the port the proxy
// listens on inside its Pod, which is what a configuration built without
// an external hostname could only guess at. A console reached through
// kubectl port-forward has no hostname to build one from, and every guess
// is wrong.
//
// Deriving it from the request is safe for the reason it is standard: the
// provider only redirects to a URI registered against the client, so a
// forged Host yields one it refuses rather than one it honours. Where the
// deployment does state an external URL, that wins and nothing is derived.
func (p *Provider) configFor(r *http.Request) oauth2.Config {
	if p.oauth2.RedirectURL != "" {
		return p.oauth2
	}
	config := p.oauth2
	config.RedirectURL = requestOrigin(r) + callbackPath
	return config
}

// requestOrigin reconstructs the scheme and host the client used. The
// forwarded headers are honoured because an ingress in front of the proxy
// is the only thing that can know TLS terminated there.
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		scheme = forwarded
	}
	host := r.Host
	if forwarded := r.Header.Get("X-Forwarded-Host"); forwarded != "" {
		host = forwarded
	}
	return scheme + "://" + host
}
