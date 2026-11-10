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

// Package mcpoauth makes the console the OAuth 2.1 authorization server
// of its own MCP endpoint, so that an MCP client (Claude
// Code, Codex, OpenCode, an IDE) goes from "a console URL" to "my
// assistant sees my clusters" with one browser sign-in and one consent
// click, and nothing changes in the identity provider or the API server.
//
// The flow, as the MCP authorization specification lays it out:
//
//  1. The client calls /mcp without a token: 401 naming the protected
//     resource metadata (RFC 9728), which names this console as the
//     authorization server; its metadata (RFC 8414) is at
//     /.well-known/oauth-authorization-server.
//  2. The client registers, statelessly: dynamic registration (RFC 7591,
//     POST /oauth/register) answers a client ID that is the sealed
//     registration itself (name and redirect URIs), so every replica
//     reads it and nothing is stored; or, when enabled, the client ID is
//     the HTTPS URL of a client ID metadata document, fetched with SSRF
//     guards and cached. Only public clients (no secret) with loopback
//     redirect URIs (RFC 8252 §7.3) are admitted.
//  3. GET /oauth/authorize checks the client, the redirect URI (exact,
//     the loopback port aside), PKCE S256, state and the resource (RFC
//     8707: this console's /mcp), sends a person without a console session
//     through the console's own sign-in, and shows one consent page. The
//     POST of that page (same-origin, CSRF-checked like every form)
//     answers the client with a code and iss (RFC 9207).
//  4. POST /oauth/token redeems the code (PKCE verified, single use) or a
//     refresh token (rotated on every use) for a console grant.
//
// A grant of an OIDC sign-in gets its own IdP tokens: Approve sends the
// browser through a second authorization-code round trip with the
// identity provider (silent where it keeps a browser session), whose
// tokens go to the grant alone. Sharing the browser session's refresh
// token would make the two end each other at an identity provider that
// rotates refresh tokens. A sign-in that renews nothing (a pasted token,
// OpenShift) gives the grant its token, and the grant ends with it.
//
// Formats. Everything the console hands out is sealed with a key derived
// from the session secrets for its purpose alone (nothing new to keep
// secret), so no value opens as another kind: client
// IDs ("pgc_cl_"), consent requests, codes ("pgc_ac_", 60 s), access
// tokens ("pgc_at_", one hour by default, never past the IdP token they
// carry) and refresh tokens ("pgc_rt_", at most the session's maximum
// age from the consent). A grant carries the person's IdP token as the
// session does; the API server says who it is on every use
// (SelfSubjectReview) and the SubjectAccessReview filtering applies
// unchanged.
//
// Shared state. The grant index, revocations, refresh generations and
// redeemed codes live in the console's state ConfigMap
// (internal/console/sharedstate, grants.json), which every replica reads.
// The index is authoritative: a token whose grant it does not hold is
// refused once replicationGrace has passed since the token was issued
// (the time its creation takes to reach the other replicas), and every
// grant is refused while the index cannot be trusted (the ConfigMap not
// read, or unwritable for more than a moment). A revocation reaches
// every replica within about a second; a code is refused on the replica
// that redeemed it at once and on the others once they have read that
// write (about a second): the residual window needs the code and its PKCE
// verifier both, so a stolen code alone is useless. Without the ConfigMap
// the index is this replica's memory: a restart disconnects every
// assistant, and a second replica refuses the first one's grants.
package mcpoauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/mcpauth"
	"github.com/fyannk/pgToolBox/internal/console/sharedstate"
	"github.com/fyannk/pgToolBox/internal/console/tracing"
)

// Paths of the authorization server.
const (
	PathMetadata  = "/.well-known/oauth-authorization-server"
	PathAuthorize = "/oauth/authorize"
	PathToken     = "/oauth/token" // #nosec G101 -- a path, not a credential
	PathRegister  = "/oauth/register"
)

// Scopes. A grant is read-only unless the write tools are on and the
// person consented to them; mcpauth.HasScope checks them per tool.
const (
	ScopeRead    = mcpauth.ScopeRead
	ScopeOperate = mcpauth.ScopeOperate
)

// tokenPrefix starts every value this server hands out: a bearer with it
// is this server's to judge, never another authenticator's.
const tokenPrefix = "pgc_"

// Lifetimes and budgets.
const (
	DefaultAccessTTL = time.Hour
	// replicationGrace is how long after a token was issued a replica
	// accepts it while the index does not hold its grant yet: the urgent
	// write takes about a second, a conflict or two a few more.
	replicationGrace = 30 * time.Second
	// pendingTTL bounds a grant's IdP round trip from Approve.
	pendingTTL = 10 * time.Minute
	// codeTTL bounds an authorization code (RFC 6749 §4.1.2 recommends at
	// most ten minutes; the round trip from consent to the client's token
	// request takes seconds).
	codeTTL = 60 * time.Second
	// consentTTL bounds the consent page's sealed request: its answer's
	// nonce is held in the shared table of spent values until then (and
	// clockSkew), so a short one leaves that table to the codes.
	consentTTL = 3 * time.Minute
	// clockSkew is how long past its expiry a spent value is remembered,
	// for replicas whose clocks differ a little.
	clockSkew = 30 * time.Second
	// upstreamLead: an IdP token this close to expiry is renewed before a
	// grant is issued or refreshed.
	upstreamLead = 2 * time.Minute
	// touchEvery debounces the grant index's last-used time per replica.
	touchEvery         = 5 * time.Minute
	maxFormBytes       = 64 << 10
	maxRegisterBytes   = 16 << 10
	requestsPerMinute  = 60
	requestsBurst      = 30
	registersPerMinute = 10
	registersBurst     = 10
	// decisionsPerMinute bounds a person's consent answers: each spends a
	// single-use value of the shared index.
	decisionsPerMinute = 10
	decisionsBurst     = 5
	maxLimiterKeys     = 10000
	// userKeyPurpose keys the grant index's user hashes.
	userKeyPurpose = "mcp-grant-user"
)

// SessionSource is the slice of the sign-in layer (auth.Handler) the
// authorization server uses.
type SessionSource interface {
	CurrentSession(w http.ResponseWriter, r *http.Request) (*auth.SessionGrant, string, error)
	Codec() *auth.Codec
	Limits() auth.Limits
	Review(ctx context.Context, token string) (authz.User, error)
	Revoked(user string, issued time.Time, refs ...string) bool
	// SignedOut reports whether a browser session was signed out (on any
	// replica). A grant outlives the session it was approved in; a grant
	// still being approved does not.
	SignedOut(session string) bool
	RefreshUpstream(ctx context.Context, provider, key, refreshToken string) (auth.UpstreamTokens, error)
	// Renews, BeginGrantSignIn and SetGrantCompletion give a grant its own
	// IdP sign-in; SetSignOutHook offers "disconnect my assistants" at
	// sign-out.
	Renews(provider string) bool
	BeginGrantSignIn(w http.ResponseWriter, r *http.Request, provider, pending string) error
	SetGrantCompletion(f auth.GrantCompletion)
	SetSignOutHook(f auth.SignOutHook)
}

// GrantStore is the shared grant index (sharedstate.Store).
type GrantStore interface {
	AddGrant(g sharedstate.Grant) error
	Grant(id string) (sharedstate.Grant, bool)
	GrantsOf(userHash string) []sharedstate.Grant
	RevokeGrant(id, userHash string, at, until time.Time) bool
	RevokeGrantsOf(userHash string, at time.Time) []string
	GrantRevoked(id string) bool
	TouchGrant(id string, at time.Time)
	AdvanceGrant(id string, from int, at time.Time) sharedstate.Advance
	UseCode(code, userHash string, until time.Time) error
	GrantsUsable() bool
	GrantsShared() bool
}

// Options configure a Server.
type Options struct {
	Sessions SessionSource
	Store    GrantStore
	// PublicURL is the console's external origin (AUTH_PUBLIC_URL),
	// required: the issuer and the resource are never derived from a
	// request's Host or forwarded headers, which a client chooses.
	PublicURL    string
	TrustedProxy auth.ProxyTrust
	// WriteTools is MCP_WRITE_TOOLS: consent then offers "read and
	// operate".
	WriteTools bool
	// AccessTTL is an access token's lifetime (MCP_OAUTH_ACCESS_TTL);
	// DefaultAccessTTL when zero.
	AccessTTL time.Duration
	// ClientMetadataDocuments admits HTTPS client IDs (client ID metadata
	// documents, MCP_OAUTH_CLIENT_METADATA_DOCUMENTS). Off by default: the
	// console then fetches nothing from outside.
	ClientMetadataDocuments bool
	// ClientMetadataHosts, when set, are the only hosts whose documents
	// are fetched (MCP_OAUTH_CLIENT_METADATA_HOSTS).
	ClientMetadataHosts []string
	// Fetcher fetches client ID metadata documents; nil builds the
	// default (SSRF-guarded) one when they are enabled.
	Fetcher *Fetcher
	Log     *slog.Logger
	Now     func() time.Time
}

// Server is the authorization server.
type Server struct {
	o         Options
	codec     *auth.Codec
	audit     *auth.Auditor
	requests  *auth.RateLimiter
	registry  *auth.RateLimiter
	decisions *auth.RateLimiter

	mu      sync.Mutex
	touched map[string]time.Time
}

// ErrIssuer refuses a public URL that cannot be an issuer: the
// authorization server is then off (raw API-server tokens still work).
var ErrIssuer = errors.New("mcpoauth: the console's public URL (AUTH_PUBLIC_URL) must be https, without user information, query or fragment " +
	"(http only on a loopback host: localhost, 127.0.0.1, [::1]): it is the OAuth issuer")

// checkIssuer admits an https URL with a host and no user information,
// query or fragment (RFC 8414 §2), or plain http on a loopback host (a
// console on the person's own computer, the e2e tests).
func checkIssuer(raw string) error {
	u, err := url.Parse(raw)
	if raw == "" || err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		strings.ContainsAny(raw, "?#") || u.Opaque != "" {
		return ErrIssuer
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		switch strings.ToLower(u.Hostname()) {
		case "localhost", "127.0.0.1", "::1":
			return nil
		}
	}
	return ErrIssuer
}

// New builds the server and installs its hooks in the sign-in layer (the
// grant sign-in's completion, "disconnect my assistants" at sign-out).
func New(o Options) (*Server, error) {
	if o.Sessions == nil || o.Store == nil {
		return nil, errors.New("mcpoauth: a session source and a grant store are required")
	}
	o.PublicURL = strings.TrimRight(o.PublicURL, "/")
	if err := checkIssuer(o.PublicURL); err != nil {
		return nil, err
	}
	if o.AccessTTL <= 0 {
		o.AccessTTL = DefaultAccessTTL
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.ClientMetadataDocuments && o.Fetcher == nil {
		o.Fetcher = NewFetcher(FetcherOptions{Now: o.Now, AllowedHosts: o.ClientMetadataHosts})
	}
	s := &Server{
		o: o, codec: o.Sessions.Codec(),
		audit:     &auth.Auditor{Logger: o.Log, TrustedProxy: o.TrustedProxy},
		requests:  auth.NewRateLimiter(requestsPerMinute, requestsBurst, maxLimiterKeys),
		registry:  auth.NewRateLimiter(registersPerMinute, registersBurst, maxLimiterKeys),
		decisions: auth.NewRateLimiter(decisionsPerMinute, decisionsBurst, maxLimiterKeys),
		touched:   map[string]time.Time{},
	}
	if len(s.userKeys("probe")) == 0 {
		return nil, errors.New("mcpoauth: the session codec derives no keys")
	}
	o.Sessions.SetGrantCompletion(s.completeGrant)
	o.Sessions.SetSignOutHook(s.disconnectAll)
	return s, nil
}

// userKeys are the grant index's names of user, under each session
// secret, the current one first: a keyed hash, so that the ConfigMap does
// not say whose a grant is to someone without the secret.
func (s *Server) userKeys(user string) []string { return s.codec.KeyedHashes(userKeyPurpose, user) }

// userKey is user's name in the index under the current secret.
func (s *Server) userKey(user string) string {
	if keys := s.userKeys(user); len(keys) > 0 {
		return keys[0]
	}
	return ""
}

// withinGrace reports whether a token issued at issued may still be
// waiting for its grant to reach this replica's index.
func (s *Server) withinGrace(issued int64, now time.Time) bool {
	return s.o.Store.GrantsShared() && issued > 0 && now.Unix() < issued+int64(replicationGrace/time.Second) &&
		issued <= now.Add(replicationGrace).Unix()
}

// Register mounts the routes MCP clients call without a cookie (metadata,
// registration, token) on the outermost mux, outside the CSRF check:
// they authenticate by what they carry.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+PathMetadata, routed(PathMetadata, s.handleMetadata))
	mux.HandleFunc("POST "+PathRegister, routed(PathRegister, s.handleRegister))
	mux.HandleFunc("POST "+PathToken, routed(PathToken, s.handleToken))
	for _, p := range []string{PathRegister, PathToken, PathMetadata} {
		mux.HandleFunc("OPTIONS "+p, routed(p, preflight))
	}
}

// RegisterSession mounts the authorization endpoint on the session mux:
// it reads the console's session cookie, and its POST (the consent form)
// passes the console's CSRF check like every form.
func (s *Server) RegisterSession(mux *http.ServeMux) {
	mux.HandleFunc("GET "+PathAuthorize, routed(PathAuthorize, s.handleAuthorize))
	mux.HandleFunc("POST "+PathAuthorize, routed(PathAuthorize, s.handleDecision))
}

// routed names the request's span after its route: the paths carry no
// value of their own, so the exact path is the template.
func routed(path string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tracing.SetRoute(r.Context(), r.Method, path)
		h(w, r)
	}
}

// Issuer is the authorization server's identifier: the console's public
// URL (RFC 8414 §2: no query or fragment), whatever the request's Host.
func (s *Server) Issuer(*http.Request) string { return s.o.PublicURL }

// AuthorizationServers is the protected resource metadata's list.
func (s *Server) AuthorizationServers(r *http.Request) []string { return []string{s.Issuer(r)} }

// Scopes are the scopes this console grants.
func (s *Server) Scopes() []string {
	if s.o.WriteTools {
		return []string{ScopeRead, ScopeOperate}
	}
	return []string{ScopeRead}
}

// metadata is the RFC 8414 document.
type metadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint"`
	ScopesSupported                   []string `json:"scopes_supported"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	ResponseModesSupported            []string `json:"response_modes_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	AuthorizationResponseIss          bool     `json:"authorization_response_iss_parameter_supported"`
	ClientIDMetadataDocumentSupported bool     `json:"client_id_metadata_document_supported,omitempty"`
	ServiceDocumentation              string   `json:"service_documentation,omitempty"`
}

func (s *Server) handleMetadata(w http.ResponseWriter, r *http.Request) {
	iss := s.Issuer(r)
	// Public discovery data: any origin may read it.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	writeJSON(w, http.StatusOK, metadata{
		Issuer:                            iss,
		AuthorizationEndpoint:             iss + PathAuthorize,
		TokenEndpoint:                     iss + PathToken,
		RegistrationEndpoint:              iss + PathRegister,
		ScopesSupported:                   s.Scopes(),
		ResponseTypesSupported:            []string{"code"},
		ResponseModesSupported:            []string{"query"},
		GrantTypesSupported:               []string{"authorization_code", "refresh_token"},
		TokenEndpointAuthMethodsSupported: []string{"none"},
		CodeChallengeMethodsSupported:     []string{"S256"},
		AuthorizationResponseIss:          true,
		ClientIDMetadataDocumentSupported: s.o.ClientMetadataDocuments,
	})
}

// preflight answers CORS preflights of the client-facing routes with no
// grant: the MCP clients this server is for are native programs.
func preflight(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// oauthError is an RFC 6749 §5.2 error body.
type oauthError struct {
	Code        string `json:"error"`
	Description string `json:"error_description,omitempty"`
}

func (e *oauthError) Error() string { return e.Code + ": " + e.Description }

func writeError(w http.ResponseWriter, status int, code, desc string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="pgtoolbox-console"`)
	}
	writeJSON(w, status, oauthError{Code: code, Description: desc})
}

// newGrantID is a grant reference: "grant-" and 128 random bits.
func newGrantID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return auth.GrantReferencePrefix + hex.EncodeToString(b)
}

// randomID is 128 random bits for a code's or token's single-use name.
func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// resourceFor is this console's MCP resource identifier for r.
func (s *Server) resourceFor(*http.Request) string { return s.o.PublicURL + mcpauth.PathMCP }

// sameResource compares an RFC 8707 resource with this console's /mcp,
// tolerating a trailing slash.
func sameResource(got, want string) bool {
	return strings.TrimRight(got, "/") == strings.TrimRight(want, "/")
}

var _ mcpauth.Authenticator = (*Server)(nil)
