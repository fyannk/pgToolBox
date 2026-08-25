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

// Package openshift implements the openshift authentication provider: an
// OAuth2 authorization-code flow against OpenShift's integrated OAuth
// server, using the workload service account as the OAuth client. The
// service-account token doubles as the client secret and is read at
// redemption time so projected-token rotation works.
package openshift

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/fyannk/pgtoolbox/internal/proxy/config"
	"github.com/fyannk/pgtoolbox/internal/proxy/pages"
	"github.com/fyannk/pgtoolbox/internal/proxy/server"
)

const (
	// transientMaxAge bounds the login flow; the transient cookie carrying
	// state and the PKCE verifier dies with it.
	transientMaxAge = 10 * time.Minute

	discoveryTimeout = 10 * time.Second
	maxDiscoveryBody = 1 << 20

	defaultDiscoveryURL = "https://openshift.default.svc/.well-known/oauth-authorization-server"
	defaultAPIURL       = "https://kubernetes.default.svc"
	defaultUserInfoPath = "/apis/user.openshift.io/v1/users/~"
)

// transient is the sealed login-flow state.
type transient struct {
	State      string `json:"state"`
	Verifier   string `json:"verifier"`
	RedirectTo string `json:"rd"`
	Expiry     int64  `json:"exp"`
}

// discoveryDocument is the subset of the OAuth authorization-server
// discovery document this provider needs.
type discoveryDocument struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
}

// userInfo is the subset of the OpenShift User object this provider needs.
type userInfo struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
}

// Provider is the OpenShift authentication provider.
type Provider struct {
	env              *server.Env
	clientSecretFile string
	oauth2           oauth2.Config
	userInfoURL      string
	httpClient       *http.Client
}

// New builds the provider. It performs OAuth endpoint discovery during
// initialization and fails startup when required endpoints are missing.
func New(ctx context.Context, env *server.Env, cfg *config.OpenShiftConfig) (*Provider, error) {
	httpClient, err := buildHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	p := &Provider{
		env:              env,
		clientSecretFile: cfg.ClientSecretFile,
		httpClient:       httpClient,
	}

	discoveryURL := cfg.DiscoveryURL
	if discoveryURL == "" {
		discoveryURL = defaultDiscoveryURL
	}
	doc, err := p.discover(ctx, discoveryURL)
	if err != nil {
		return nil, err
	}

	apiURL := cfg.APIURL
	if apiURL == "" {
		apiURL = defaultAPIURL
	}
	userInfoPath := cfg.UserInfoPath
	if userInfoPath == "" {
		userInfoPath = defaultUserInfoPath
	}
	p.userInfoURL = strings.TrimSuffix(apiURL, "/") + userInfoPath

	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{"user:info"}
	}
	p.oauth2 = oauth2.Config{
		ClientID:    cfg.ClientID,
		Endpoint:    oauth2.Endpoint{AuthURL: doc.AuthorizationEndpoint, TokenURL: doc.TokenEndpoint},
		RedirectURL: cfg.RedirectURL,
		Scopes:      scopes,
	}
	return p, nil
}

// buildHTTPClient builds the client used for discovery, token exchange, and
// the current-user lookup. When CAFile is set it is added to the root pool;
// empty uses the system pool.
func buildHTTPClient(cfg *config.OpenShiftConfig) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.CAFile != "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		ca, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read OpenShift CA file: %w", err)
		}
		if !pool.AppendCertsFromPEM(ca) {
			return nil, fmt.Errorf("OpenShift CA file %s has no usable certificates", cfg.CAFile)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: transport, Timeout: discoveryTimeout}, nil
}

// Mode implements server.Provider.
func (p *Provider) Mode() string { return config.ModeOpenShift }

// Register implements server.Provider.
func (p *Provider) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/openshift/login", p.handleLogin)
	mux.HandleFunc("GET /auth/openshift/callback", p.handleCallback)
}

// handleLogin starts the flow: it seals state, the PKCE verifier, and the
// validated redirect target into a transient cookie, then redirects to the
// OpenShift authorization endpoint.
func (p *Provider) handleLogin(w http.ResponseWriter, r *http.Request) {
	rt := p.env.Runtime()
	rd := server.SafeRedirect(r.URL.Query().Get("rd"))
	t := transient{
		State:      randomToken(),
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
	// Secure follows the deployment's TLS setting rather than a literal,
	// which is the only attribute G124 cannot see; HttpOnly and SameSite
	// are set below.
	http.SetCookie(w, &http.Cookie{ // #nosec G124
		Name:     transientCookieName(rt),
		Value:    v,
		Path:     "/",
		MaxAge:   int(transientMaxAge.Seconds()),
		HttpOnly: true,
		Secure:   rt.Config.Session.CookieSecure(),
		SameSite: http.SameSiteLaxMode,
	})
	url := p.oauth2.AuthCodeURL(t.State, oauth2.S256ChallengeOption(t.Verifier))
	http.Redirect(w, r, url, http.StatusFound)
}

// handleCallback completes the flow. It rejects on state mismatch, an
// expired or tampered transient cookie, and any token-exchange or user
// lookup failure. The access token is never logged.
func (p *Provider) handleCallback(w http.ResponseWriter, r *http.Request) {
	rt := p.env.Runtime()
	clear := func() {
		// Same attributes the cookie was set with, and the same ones
		// session.ClearCookie uses: an expiry that does not match the
		// original's flags is a cookie a browser may decline to replace,
		// and it advertises a laxer policy than the value ever had.
		// Secure follows the deployment's TLS setting rather than a literal,
		// which is the only attribute G124 cannot see; HttpOnly and SameSite
		// are set below.
		http.SetCookie(w, &http.Cookie{ // #nosec G124
			Name:     transientCookieName(rt),
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: true,
			Secure:   rt.Config.Session.CookieSecure(),
			SameSite: http.SameSiteLaxMode,
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
	code := r.URL.Query().Get("code")
	if code == "" {
		fail(http.StatusBadRequest, "The authorization code is missing.")
		return
	}

	secret, err := readSecretFile(p.clientSecretFile)
	if err != nil {
		p.env.Logger.Error("reading OpenShift OAuth client secret failed", "error", err)
		fail(http.StatusInternalServerError, "This console's OAuth client secret could not be read; "+
			"an administrator has to check the ServiceAccount token mount.")
		return
	}

	exchange := p.oauth2
	exchange.ClientSecret = secret
	ctx := context.WithValue(r.Context(), oauth2.HTTPClient, p.httpClient)
	token, err := exchange.Exchange(ctx, code, oauth2.VerifierOption(t.Verifier))
	if err != nil {
		status, message := server.ExchangeFailure(err)
		p.env.Logger.Error("OpenShift code exchange failed", "error", err, "status", status)
		fail(status, message)
		return
	}
	if token.AccessToken == "" {
		fail(http.StatusBadGateway, "The identity provider did not return an access token.")
		return
	}

	subject, err := p.fetchUser(r.Context(), token.AccessToken)
	if err != nil {
		p.env.Logger.Warn("OpenShift user lookup failed", "error", err)
		fail(http.StatusBadGateway, "The current user could not be resolved.")
		return
	}

	level := config.LevelNone
	if u, ok := p.env.LookupUser(subject); ok {
		level = u.Level
	}
	if err := p.env.IssueSession(w, subject, level, config.ModeOpenShift); err != nil {
		p.env.Logger.Error("issuing session failed", "error", err)
		fail(http.StatusInternalServerError, "The session could not be created.")
		return
	}
	clear()
	http.Redirect(w, r, server.SafeRedirect(t.RedirectTo), http.StatusFound)
}

// discover fetches and validates the OAuth authorization-server discovery
// document. Response bodies are bounded and errors never expose credentials.
func (p *Provider) discover(ctx context.Context, discoveryURL string) (*discoveryDocument, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build discovery request: %w", err)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discover OAuth endpoints: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OAuth discovery returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryBody))
	if err != nil {
		return nil, fmt.Errorf("read discovery document: %w", err)
	}
	var doc discoveryDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse discovery document: %w", err)
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" {
		return nil, fmt.Errorf("discovery document is missing authorization or token endpoints")
	}
	return &doc, nil
}

// fetchUser calls the OpenShift current-user endpoint with the human user's
// access token and returns the canonical username.
func (p *Provider) fetchUser(ctx context.Context, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.userInfoURL, nil)
	if err != nil {
		return "", fmt.Errorf("build user lookup request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch user info: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("user lookup returned %d", resp.StatusCode)
	}
	var u userInfo
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return "", fmt.Errorf("parse user info: %w", err)
	}
	if u.Metadata.Name == "" {
		return "", fmt.Errorf("user lookup returned no username")
	}
	return u.Metadata.Name, nil
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
	if time.Now().Unix() >= t.Expiry || t.State == "" || t.Verifier == "" {
		return transient{}, fmt.Errorf("transient cookie is expired or incomplete")
	}
	return t, nil
}

func transientCookieName(rt *server.Runtime) string {
	return rt.Config.Session.CookieName + "_oauth"
}

// readSecretFile reads the service-account token; the contents are returned
// but never logged. The path comes from the operator-rendered configuration.
func readSecretFile(path string) (string, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- path from operator-rendered config
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "", fmt.Errorf("secret file is empty")
	}
	return s, nil
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Errorf("crypto/rand unavailable: %w", err))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
