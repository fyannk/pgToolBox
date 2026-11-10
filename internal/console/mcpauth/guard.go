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

package mcpauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/auth"
)

// Paths of the endpoint and of its protected resource metadata (RFC 9728
// §3.1: the well-known prefix inserted before the resource's path; the
// bare well-known path is served too, for clients that look at the root).
const (
	PathMCP              = "/mcp"
	PathResourceMetadata = "/.well-known/oauth-protected-resource"
)

// Budgets. Every request to /mcp spends one call of its user's budget
// (tools/list as much as tools/call), and carries one JSON-RPC message:
// batches are refused, so a request is one call. A user has at most
// DefaultMaxConcurrent requests in flight. A bearer the console has not
// accepted recently spends one attempt of its client address's budget
// before the API server is asked about it; the console shares that
// budget with its sign-in form.
const (
	DefaultCallsPerMinute    = 120
	DefaultCallsBurst        = 60
	DefaultMaxConcurrent     = 4
	DefaultMaxBodyBytes      = 64 << 10
	DefaultAttemptsPerMinute = 30
	DefaultAttemptsBurst     = 30
	maxBudgetKeys            = 10000
	// knownTTL is how long an accepted bearer skips the address budget;
	// it matches the identity cache, so a known token costs the API
	// server nothing until both expire.
	knownTTL      = auth.DefaultIdentityTTL
	maxKnownSlots = 4096
)

// Options configure a Guard.
type Options struct {
	Authenticator Authenticator
	// PublicURL is the console's external origin (AUTH_PUBLIC_URL); empty
	// derives it from each request, like the CSRF check.
	PublicURL string
	// TrustedProxy is which X-Forwarded-For entries are believed.
	TrustedProxy auth.ProxyTrust
	// Audit receives mcp.denied lines.
	Audit *auth.Auditor
	// Calls is the per-user budget; nil is DefaultCallsPerMinute/Burst.
	Calls *auth.RateLimiter
	// Attempts is the per-address budget for bearers not accepted
	// recently; nil is DefaultAttemptsPerMinute/Burst. The console passes
	// its sign-in form's limiter: one guessing budget per address.
	Attempts *auth.RateLimiter
	// MaxConcurrent bounds one user's requests in flight; 0 is
	// DefaultMaxConcurrent.
	MaxConcurrent int
	// MaxBodyBytes bounds a request body; 0 is DefaultMaxBodyBytes.
	MaxBodyBytes int
	Log          *slog.Logger
	Now          func() time.Time
}

// Guard authenticates every request to the MCP endpoint.
type Guard struct {
	o        Options
	mu       sync.Mutex
	known    map[[32]byte]time.Time
	inflight map[string]int
}

// NewGuard fills the defaults.
func NewGuard(o Options) *Guard {
	if o.Calls == nil {
		o.Calls = auth.NewRateLimiter(DefaultCallsPerMinute, DefaultCallsBurst, maxBudgetKeys)
	}
	if o.Attempts == nil {
		o.Attempts = auth.NewRateLimiter(DefaultAttemptsPerMinute, DefaultAttemptsBurst, maxBudgetKeys)
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MaxConcurrent <= 0 {
		o.MaxConcurrent = DefaultMaxConcurrent
	}
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = DefaultMaxBodyBytes
	}
	return &Guard{o: o, known: map[[32]byte]time.Time{}, inflight: map[string]int{}}
}

type clientKey struct{}

// ClientFrom returns the request's client as the guard recorded it, for
// the audit lines written once the request has been decoded.
func ClientFrom(ctx context.Context) (auth.Client, bool) {
	c, ok := ctx.Value(clientKey{}).(auth.Client)
	return c, ok
}

type resourceKey struct{}

// WithResource stores the resource the request is for (this endpoint's
// RFC 8707 identifier), so an authenticator can refuse a token issued
// for another audience.
func WithResource(ctx context.Context, resource string) context.Context {
	return context.WithValue(ctx, resourceKey{}, resource)
}

// ResourceFrom returns the resource WithResource stored.
func ResourceFrom(ctx context.Context) (string, bool) {
	r, ok := ctx.Value(resourceKey{}).(string)
	return r, ok && r != ""
}

// WithClient stores c in ctx (tests and the guard).
func WithClient(ctx context.Context, c auth.Client) context.Context {
	return context.WithValue(ctx, clientKey{}, c)
}

// Wrap authenticates every request before next sees it. The session
// cookie is never read: a browser's ambient credentials grant nothing
// here, which is also why the CSRF check does not apply. A request that
// carries an Origin must come from the console's own origin (MCP's
// DNS-rebinding rule); MCP clients send none.
func (g *Guard) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		client := auth.ClientOf(r, g.o.TrustedProxy)
		if o := r.Header.Get("Origin"); o != "" && !auth.SameOrigin(o, auth.ExpectedOrigin(r, g.o.PublicURL)) {
			g.deny(r, client, "", "cross-origin request")
			http.Error(w, "cross-origin requests to the MCP endpoint are refused", http.StatusForbidden)
			return
		}
		token, ok := bearer(r)
		if !ok {
			// The first call of an OAuth-capable client: no audit line,
			// the challenge is the discovery step.
			g.challenge(w, r, "", "")
			return
		}
		if !g.oneMessage(w, r, client) {
			return
		}
		digest := sha256.Sum256([]byte(token))
		if !g.isKnown(digest) && !g.o.Attempts.Allow(auth.ClientKey(r, g.o.TrustedProxy)) {
			g.deny(r, client, "", "address budget")
			tooMany(w, "too many unrecognised tokens from this address; wait a minute")
			return
		}
		p, err := g.o.Authenticator.Authenticate(WithResource(r.Context(), ResourceURL(r, g.o.PublicURL)), token)
		switch {
		case errors.Is(err, auth.ErrReviewsBusy):
			g.deny(r, client, "", "review budget")
			tooMany(w, "the console is reviewing too many tokens; wait a few seconds")
			return
		case err != nil && refused(err):
			g.forget(digest)
			g.deny(r, client, "", reason(err))
			g.challenge(w, r, "invalid_token", "the token is "+reason(err))
			return
		case err != nil:
			g.o.Log.Warn("the MCP bearer could not be checked", "error", err)
			w.Header().Set("Retry-After", "5")
			http.Error(w, "the Kubernetes API server could not be reached to check the token; retry", http.StatusServiceUnavailable)
			return
		case p == nil:
			g.deny(r, client, "", "no principal")
			g.challenge(w, r, "invalid_token", "the token is not accepted")
			return
		}
		g.remember(digest)
		if !g.o.Calls.Allow(p.User.Name) {
			g.deny(r, client, p.User.Name, "user budget")
			tooMany(w, "too many MCP requests for this user; wait a few seconds")
			return
		}
		if !g.acquire(p.User.Name) {
			g.deny(r, client, p.User.Name, "concurrent calls")
			tooMany(w, "too many MCP requests in flight for this user; wait for one to finish")
			return
		}
		defer g.release(p.User.Name)
		ctx := WithClient(auth.WithPrincipal(r.Context(), p), client)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// bearer extracts the token of `Authorization: Bearer <token>`, parsed
// exactly as the SDK parses it (two fields, the first "bearer" in any
// case), so a header the guard accepts is one the SDK accepts too.
func bearer(r *http.Request) (string, bool) {
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "bearer") {
		return "", false
	}
	return fields[1], true
}

// oneMessage refuses a body over the limit (413) and a JSON-RPC batch
// (400), and hands the body on unread. The SDK still accepts batches from
// clients of protocol 2025-03-26; one request would then carry hundreds
// of calls for one unit of the budget.
func (g *Guard) oneMessage(w http.ResponseWriter, r *http.Request, c auth.Client) bool {
	if r.Method != http.MethodPost || r.Body == nil {
		return true
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(g.o.MaxBodyBytes)+1))
	_ = r.Body.Close()
	switch {
	case err != nil:
		http.Error(w, "the request body could not be read", http.StatusBadRequest)
		return false
	case len(body) > g.o.MaxBodyBytes:
		g.deny(r, c, "", "body too large")
		http.Error(w, "the request body is too large", http.StatusRequestEntityTooLarge)
		return false
	}
	if trimmed := bytes.TrimLeft(body, " \t\r\n"); len(trimmed) > 0 && trimmed[0] == '[' {
		g.deny(r, c, "", "batch")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"JSON-RPC batches are not accepted: send one message per request"}}`)
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return true
}

func (g *Guard) acquire(user string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inflight[user] >= g.o.MaxConcurrent {
		return false
	}
	g.inflight[user]++
	return true
}

func (g *Guard) release(user string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inflight[user] <= 1 {
		delete(g.inflight, user)
		return
	}
	g.inflight[user]--
}

// ResourceURL is the endpoint's resource identifier for r.
func ResourceURL(r *http.Request, publicURL string) string {
	return strings.TrimRight(auth.ExpectedOrigin(r, publicURL), "/") + PathMCP
}

// MetadataURL is where the endpoint's protected resource metadata is
// served for r (RFC 9728 §3.1).
func MetadataURL(r *http.Request, publicURL string) string {
	return strings.TrimRight(auth.ExpectedOrigin(r, publicURL), "/") + PathResourceMetadata + PathMCP
}

// challenge answers 401 with the RFC 6750 / RFC 9728 challenge.
func (g *Guard) challenge(w http.ResponseWriter, r *http.Request, code, desc string) {
	params := []string{"resource_metadata=" + strconv.Quote(MetadataURL(r, g.o.PublicURL))}
	if code != "" {
		params = append(params, "error="+strconv.Quote(code))
	}
	if desc != "" {
		params = append(params, "error_description="+strconv.Quote(desc))
	}
	w.Header().Set("WWW-Authenticate", "Bearer "+strings.Join(params, ", "))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	msg := "send Authorization: Bearer <token>"
	if desc != "" {
		msg = desc
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized", "detail": msg})
}

func tooMany(w http.ResponseWriter, msg string) {
	w.Header().Set("Retry-After", "10")
	http.Error(w, msg, http.StatusTooManyRequests)
}

func (g *Guard) deny(r *http.Request, c auth.Client, user, why string) {
	attrs := []any{"reason", why}
	if user != "" {
		attrs = append(attrs, "user", user)
	}
	g.o.Audit.Record(r.Context(), c, auth.AuditMCPDenied, attrs...)
}

func (g *Guard) isKnown(d [32]byte) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	exp, ok := g.known[d]
	return ok && g.o.Now().Before(exp)
}

func (g *Guard) remember(d [32]byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.o.Now()
	if len(g.known) >= maxKnownSlots {
		for k, exp := range g.known {
			if !now.Before(exp) {
				delete(g.known, k)
			}
		}
		if len(g.known) >= maxKnownSlots {
			clear(g.known)
		}
	}
	g.known[d] = now.Add(knownTTL)
}

func (g *Guard) forget(d [32]byte) {
	g.mu.Lock()
	delete(g.known, d)
	g.mu.Unlock()
}

// Metadata is the RFC 9728 protected resource metadata document. It
// names the console itself as the authorization server when the
// console's OAuth flow is mounted (internal/console/mcpoauth);
// without it, none (authorization_servers is optional, RFC 9728
// §2) and bearers are raw API-server tokens.
type Metadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers,omitempty"`
	ScopesSupported        []string `json:"scopes_supported,omitempty"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
	ResourceName           string   `json:"resource_name"`
	ResourceDocumentation  string   `json:"resource_documentation,omitempty"`
}

// MetadataHandler serves the document; authServers is nil when the
// console is not the authorization server, scopes lists the scopes it
// grants.
func MetadataHandler(publicURL string, authServers func(r *http.Request) []string, scopes ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Public discovery data: any origin may read it.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cache-Control", "no-store")
		m := Metadata{
			Resource:               ResourceURL(r, publicURL),
			BearerMethodsSupported: []string{"header"},
			ResourceName:           "pgToolBox console",
		}
		if authServers != nil {
			m.AuthorizationServers = authServers(r)
			m.ScopesSupported = scopes
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m)
	})
}
