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
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// tokenSet is what a provider hands back: the apiserver token, an
// optional refresh token, and when the token expires (zero: unknown).
type tokenSet struct {
	token   string
	refresh string
	expiry  time.Time
}

// loginError is a failed sign-in: a status and a message for the person,
// and the cause for the log (never shown).
type loginError struct {
	status  int
	message string
	cause   error
}

func (e *loginError) Error() string { return e.message }

// oauthProvider is one authorization-code flow.
type oauthProvider interface {
	name() string
	label() string
	// authCodeURL builds the redirect to the authorization endpoint.
	authCodeURL(redirectURI string, f loginFlow) string
	// exchange redeems the code.
	exchange(ctx context.Context, redirectURI, code string, f loginFlow) (tokenSet, *loginError)
}

// refresher is implemented by providers that can renew a token.
type refresher interface {
	refresh(ctx context.Context, refreshToken string) (tokenSet, error)
}

// newIdPClient builds the HTTP client for one identity provider: bounded
// by timeout (go-oidc and oauth2 default to http.DefaultClient, which has
// none: a blackholed IdP would hang a callback forever), with caFile added
// to the system pool when set.
func newIdPClient(caFile string, timeout time.Duration) (*http.Client, error) {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("unexpected default transport")
	}
	transport = transport.Clone()
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		pem, err := readBoundedFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("reading CA file: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA file %s holds no usable certificate", caFile)
		}
		tlsCfg.RootCAs = pool
	}
	transport.TLSClientConfig = tlsCfg
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Errorf("crypto/rand unavailable: %w", err))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// exchangeFailure maps a failed code exchange to a status and a message a
// person can act on.
func exchangeFailure(err error) *loginError {
	var retrieve *oauth2.RetrieveError
	if !errors.As(err, &retrieve) {
		return &loginError{http.StatusBadGateway, "The identity provider could not be reached.", err}
	}
	switch retrieve.ErrorCode {
	case "invalid_client", "unauthorized_client":
		return &loginError{http.StatusInternalServerError, "This console was rejected by the identity provider (" +
			retrieve.ErrorCode + "): its client ID or client secret is wrong; an administrator has to correct the console's auth settings.", err}
	case "invalid_grant":
		return &loginError{http.StatusBadRequest, "The sign-in could not be completed; please try again.", err}
	case "invalid_request", "unsupported_grant_type":
		return &loginError{http.StatusInternalServerError, "The identity provider rejected this console's request (" +
			retrieve.ErrorCode + "). The registered redirect URI most likely does not match the address this console is reached on.", err}
	}
	if retrieve.Response != nil && retrieve.Response.StatusCode >= 500 {
		return &loginError{http.StatusBadGateway, "The identity provider failed to complete the sign-in.", err}
	}
	return &loginError{http.StatusInternalServerError, "The identity provider refused to complete the sign-in.", err}
}

// ---------------------------------------------------------------- OIDC

// oidcProvider is the OIDC code flow with PKCE (S256), state and nonce
// The ID token it obtains is the token the
// console presents to the API server, which must trust the same issuer
// and client.
type oidcProvider struct {
	cfg        OIDCConfig
	endpoint   oauth2.Endpoint
	keys       gooidc.KeySet
	verifier   *gooidc.IDTokenVerifier
	httpClient *http.Client
	now        func() time.Time
}

func newOIDCProvider(ctx context.Context, cfg OIDCConfig, timeout time.Duration) (*oidcProvider, error) {
	client, err := newIdPClient(cfg.CAFile, timeout)
	if err != nil {
		return nil, err
	}
	discoverCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// go-oidc keeps the client from this context for later JWKS fetches.
	p, err := gooidc.NewProvider(gooidc.ClientContext(discoverCtx, client), cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("discovering OIDC issuer %s: %w", cfg.IssuerURL, err)
	}
	keyCtx := gooidc.ClientContext(context.WithoutCancel(ctx), client)
	keySet := gooidc.NewRemoteKeySet(keyCtx, jwksURL(p))
	return &oidcProvider{
		cfg:        cfg,
		endpoint:   p.Endpoint(),
		keys:       keySet,
		verifier:   gooidc.NewVerifier(cfg.IssuerURL, keySet, &gooidc.Config{ClientID: cfg.ClientID}),
		httpClient: client,
		now:        time.Now,
	}, nil
}

// grantClient is the provider's twin for the IdP sign-ins of console
// grants (OIDC_GRANT_CLIENT_ID): the same issuer, endpoints, keys and
// redirect URI, another client, whose ID tokens it verifies. nil without
// one.
func (p *oidcProvider) grantClient() *oidcProvider {
	if p.cfg.GrantClientID == "" {
		return nil
	}
	g := *p
	g.cfg.ClientID, g.cfg.ClientSecretFile = p.cfg.GrantClientID, p.cfg.GrantClientSecretFile
	g.cfg.GrantClientID, g.cfg.GrantClientSecretFile = "", ""
	g.verifier = gooidc.NewVerifier(p.cfg.IssuerURL, p.keys, &gooidc.Config{ClientID: g.cfg.ClientID})
	return &g
}

// jwksURL reads jwks_uri from the discovered metadata.
func jwksURL(p *gooidc.Provider) string {
	var m struct {
		JWKSURI string `json:"jwks_uri"`
	}
	_ = p.Claims(&m)
	return m.JWKSURI
}

func (p *oidcProvider) name() string  { return ProviderOIDC }
func (p *oidcProvider) label() string { return "Sign in with single sign-on" }

func (p *oidcProvider) oauth(redirectURI, secret string) oauth2.Config {
	return oauth2.Config{
		ClientID:     p.cfg.ClientID,
		ClientSecret: secret,
		Endpoint:     p.endpoint,
		RedirectURL:  redirectURI,
		Scopes:       p.cfg.Scopes,
	}
}

func (p *oidcProvider) authCodeURL(redirectURI string, f loginFlow) string {
	cfg := p.oauth(redirectURI, "")
	return cfg.AuthCodeURL(f.State, gooidc.Nonce(f.Nonce), oauth2.S256ChallengeOption(f.Verifier))
}

func (p *oidcProvider) exchange(ctx context.Context, redirectURI, code string, f loginFlow) (tokenSet, *loginError) {
	// The client secret is read per exchange, so a rotated Secret takes
	// effect without a restart.
	secret, err := readSecretFile(p.cfg.ClientSecretFile)
	if err != nil {
		return tokenSet{}, &loginError{http.StatusInternalServerError, "This console's OIDC client secret could not be read; an administrator has to check its mount.", err}
	}
	cfg := p.oauth(redirectURI, secret)
	tok, err := cfg.Exchange(context.WithValue(ctx, oauth2.HTTPClient, p.httpClient), code, oauth2.VerifierOption(f.Verifier))
	if err != nil {
		return tokenSet{}, exchangeFailure(err)
	}
	ts, lerr := p.verify(ctx, tok, f.Nonce)
	if lerr != nil {
		return tokenSet{}, lerr
	}
	return ts, nil
}

// verify checks the ID token of tok: signature, issuer, audience, expiry,
// nonce (when one was sent) and email_verified.
func (p *oidcProvider) verify(ctx context.Context, tok *oauth2.Token, nonce string) (tokenSet, *loginError) {
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return tokenSet{}, &loginError{http.StatusBadGateway, "The identity provider did not return an ID token.", nil}
	}
	idt, err := p.verifier.Verify(gooidc.ClientContext(ctx, p.httpClient), raw)
	if err != nil {
		return tokenSet{}, &loginError{http.StatusBadRequest, "The identity token could not be verified.", err}
	}
	if nonce != "" && subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(nonce)) != 1 {
		return tokenSet{}, &loginError{http.StatusBadRequest, "Sign-in nonce mismatch; please try again.", nil}
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		return tokenSet{}, &loginError{http.StatusBadGateway, "The identity token claims could not be read.", err}
	}
	if !emailVouched(claims, p.cfg.RequireEmailVerified) {
		return tokenSet{}, &loginError{http.StatusForbidden, "The identity provider has not verified this e-mail address.", nil}
	}
	return tokenSet{token: raw, refresh: tok.RefreshToken, expiry: idt.Expiry}, nil
}

// emailVouched enforces email_verified: a token that carries
// an email claim must also carry email_verified=true (or "true", for IdPs
// that serialise every claim as a string) unless the check is disabled.
// IdPs with self-service profiles let anyone type an administrator's
// address, and API servers commonly map the username from email.
func emailVouched(claims map[string]any, required bool) bool {
	if !required {
		return true
	}
	if _, has := claims["email"]; !has {
		return true
	}
	switch v := claims["email_verified"].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true")
	}
	return false
}

func (p *oidcProvider) refresh(ctx context.Context, refreshToken string) (tokenSet, error) {
	secret, err := readSecretFile(p.cfg.ClientSecretFile)
	if err != nil {
		return tokenSet{}, err
	}
	cfg := p.oauth("", secret)
	src := cfg.TokenSource(context.WithValue(ctx, oauth2.HTTPClient, p.httpClient), &oauth2.Token{RefreshToken: refreshToken, Expiry: p.now().Add(-time.Minute)})
	tok, err := src.Token()
	if err != nil {
		return tokenSet{}, fmt.Errorf("refreshing: %w", err)
	}
	ts, lerr := p.verify(ctx, tok, "")
	if lerr != nil {
		return tokenSet{}, lerr
	}
	if ts.refresh == "" {
		ts.refresh = refreshToken
	}
	return ts, nil
}

// ------------------------------------------------------------ OpenShift

const defaultOpenShiftDiscoveryURL = "https://openshift.default.svc/.well-known/oauth-authorization-server"

// openshiftProvider is the OAuth code flow against OpenShift's built-in
// OAuth server with the console's ServiceAccount as the client.
// The access token it obtains is an apiserver token.
type openshiftProvider struct {
	cfg        OpenShiftConfig
	endpoint   oauth2.Endpoint
	httpClient *http.Client
}

func newOpenShiftProvider(ctx context.Context, cfg OpenShiftConfig, timeout time.Duration) (*openshiftProvider, error) {
	ca := cfg.CAFile
	if ca == "" {
		ca = DefaultServiceCAFile
	}
	client, err := newIdPClient(ca, timeout)
	if err != nil {
		return nil, err
	}
	u := cfg.DiscoveryURL
	if u == "" {
		u = defaultOpenShiftDiscoveryURL
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discovering OpenShift OAuth endpoints: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OpenShift OAuth discovery returned %d", resp.StatusCode)
	}
	var doc struct {
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("parsing the OpenShift OAuth discovery document: %w", err)
	}
	if !strings.HasPrefix(doc.AuthorizationEndpoint, "https://") || !strings.HasPrefix(doc.TokenEndpoint, "https://") {
		return nil, errors.New("the OpenShift OAuth discovery document lacks https authorization or token endpoints")
	}
	return &openshiftProvider{
		cfg:        cfg,
		endpoint:   oauth2.Endpoint{AuthURL: doc.AuthorizationEndpoint, TokenURL: doc.TokenEndpoint},
		httpClient: client,
	}, nil
}

func (p *openshiftProvider) name() string  { return ProviderOpenShift }
func (p *openshiftProvider) label() string { return "Sign in with OpenShift" }

func (p *openshiftProvider) oauth(redirectURI, secret string) oauth2.Config {
	return oauth2.Config{ClientID: p.cfg.ClientID, ClientSecret: secret, Endpoint: p.endpoint, RedirectURL: redirectURI, Scopes: p.cfg.Scopes}
}

func (p *openshiftProvider) authCodeURL(redirectURI string, f loginFlow) string {
	cfg := p.oauth(redirectURI, "")
	return cfg.AuthCodeURL(f.State, oauth2.S256ChallengeOption(f.Verifier))
}

func (p *openshiftProvider) exchange(ctx context.Context, redirectURI, code string, f loginFlow) (tokenSet, *loginError) {
	// The ServiceAccount token is the client secret; it is read at
	// redemption so the kubelet's rotation of the projected token works.
	secret, err := readSecretFile(p.cfg.ClientSecretFile)
	if err != nil {
		return tokenSet{}, &loginError{http.StatusInternalServerError, "This console's OAuth client secret (its ServiceAccount token) could not be read.", err}
	}
	cfg := p.oauth(redirectURI, secret)
	tok, err := cfg.Exchange(context.WithValue(ctx, oauth2.HTTPClient, p.httpClient), code, oauth2.VerifierOption(f.Verifier))
	if err != nil {
		return tokenSet{}, exchangeFailure(err)
	}
	if tok.AccessToken == "" {
		return tokenSet{}, &loginError{http.StatusBadGateway, "The identity provider did not return an access token.", nil}
	}
	return tokenSet{token: tok.AccessToken, expiry: tok.Expiry}, nil
}

// ------------------------------------------------------------ token

// maxPastedToken bounds the pasted break-glass token.
const maxPastedToken = 16 << 10

// pastedTokenExpiry reads the exp claim of a JWT without verifying it,
// only to end the session no later than the token: the API server is the
// one that verifies the token, on every request. Opaque tokens yield
// zero (unknown).
func pastedTokenExpiry(tok string) time.Time {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(raw) > maxPastedToken {
		return time.Time{}
	}
	var c struct {
		Exp json.Number `json:"exp"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return time.Time{}
	}
	n, err := c.Exp.Int64()
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}
