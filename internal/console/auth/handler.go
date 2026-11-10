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
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"

	"github.com/fyannk/pgToolBox/internal/console/authz"
)

// Timing of the session lifecycle.
const (
	// refreshLead: a token this close to expiry is refreshed on the next
	// request (OIDC only).
	refreshLead = 2 * time.Minute
	// ReauthLead: the event stream announces "reauth" this long before
	// the session ends, so the UI can refresh or warn. It is
	// shorter than refreshLead, so the request the UI makes on "reauth"
	// is the one that refreshes.
	ReauthLead = 90 * time.Second
	// slideEvery: the idle timer is pushed forward (and the cookie
	// rewritten) at most this often.
	slideEvery = time.Minute
	// maxFormBytes bounds the sign-in and sign-out forms.
	maxFormBytes = 32 << 10
	// SessionPath is the API call that renews a refreshable session (the
	// UI makes it on the stream's reauth event). Token refresh happens on
	// that call only: refreshing on any request would redeem a rotating
	// refresh token from several concurrent requests.
	SessionPath = "/api/v1/session"
	// Sign-in budget per client address: every attempt costs the API
	// server a SelfSubjectReview. It is charged only when the client
	// address is a client's (ProxyTrust.SharedClientKey is false).
	signInPerMinute = 10
	signInBurst     = 10
	maxLimiterKeys  = 10000
	maxIdPErrorLen  = 200
	// The per-replica bounds below exist to protect the API server and
	// the identity provider, not to ration people: an unauthenticated
	// sender can spend any bucket it reaches (behind a shared proxy
	// address no per-address budget tells it apart), so a bucket that
	// genuine sign-ins share with junk must be far larger than any flood
	// the console is meant to ride out, and the buckets that genuine
	// sign-ins need once they have proved something (an IdP-issued token,
	// a bearer accepted before) are separate, so junk never reaches them.
	//
	// tokenReviews: SelfSubjectReviews of tokens the console has never
	// accepted (pasted tokens, raw MCP bearers it does not know). Each is
	// one request the API server authenticates and refuses, which costs it
	// a signature check; 100 a second (burst 200) is negligible for it and
	// far above any team pasting tokens by hand. Above that rate a sender
	// still refuses new token sign-ins on this replica: the residual cost
	// documented in security.md, visible as
	// pgtoolbox_console_throttled_requests_total{bucket="token_reviews"}.
	tokenReviewsPerSecond = 100
	tokenReviewsBurst     = 200
	tokenReviewsKey       = "token-form"
	// knownReviews: re-reviews of raw bearers this replica accepted
	// before (an MCP client's token once its 30 s identity cache entry
	// ends). Only a token the API server accepted gets here, so no junk
	// spends it; 20 a second refills a burst of 40 in two seconds.
	knownReviewsPerSecond = 20
	knownReviewsBurst     = 40
	knownReviewsKey       = "known-bearer"
	// knownSubjectReviews: one person's share of knownReviews, charged
	// before it: maxKnownBearersPerSubject tokens each re-reviewed once an
	// identity-cache lifetime (30 s) is 32 a minute; past that a person
	// waits, and emptying knownReviews takes some forty identities the
	// API server accepts.
	knownSubjectReviewsPerMinute = 2 * maxKnownBearersPerSubject
	knownSubjectReviewsBurst     = maxKnownBearersPerSubject
	// maxKnownBearers bounds the bearers remembered as accepted (a
	// digest and an expiry each); knownBearerTTL is how long one is
	// remembered when its own expiry is unknown (an opaque token).
	// maxKnownBearersPerSubject bounds one person's share of it, so a
	// person minting tokens evicts only their own.
	maxKnownBearers           = 4096
	maxKnownBearersPerSubject = 16
	knownBearerTTL            = time.Hour
	// The token form's per-address budget, charged before the shared
	// bucket so that one address cannot drain everyone's break-glass
	// sign-ins: at 20 a minute a single address takes a fraction of the
	// bucket. It is twice the per-token budget, so that
	// retrying one token until its own budget runs out still leaves the
	// address room for another.
	tokenAddressPerMinute = 20
	tokenAddressBurst     = 20
	// codeExchanges: the OAuth callbacks' code exchanges with the IdP.
	// Anyone can start a flow and call its callback with a junk code, so
	// this bucket is shared with junk and is sized to protect the IdP
	// and this replica, not to ration sign-ins: 100 token requests a
	// second (burst 200), each bounded by IDP_TIMEOUT. The IdP's own rate
	// limit is the real bound on what it accepts; a junk code costs one
	// refused exchange and nothing else (no SelfSubjectReview).
	codeExchangesPerSecond = 100
	codeExchangesBurst     = 200
	codeExchangesKey       = "code-exchange"
	// exchangeReviews: the SelfSubjectReview of a token the IdP has just
	// issued, charged only after a successful exchange: junk codes never
	// reach it. 20 a second (burst 40) lets a team sign in at once.
	exchangeReviewsPerSecond = 20
	exchangeReviewsBurst     = 40
	exchangeReviewsKey       = "exchange-review"
	// exchangeSubjectReviews: one person's share of exchangeReviews, keyed
	// by the IdP-verified issuer and subject of the ID token (OIDC only:
	// an OpenShift token names nobody before the review), so one account
	// signing in silently in a loop cannot keep a team from signing in.
	exchangeSubjectReviewsPerMinute = 12
	exchangeSubjectReviewsBurst     = 6
	// maxLoggedOut bounds the sessions this replica refuses as signed
	// out (loggedOut), maxLoggedOutPerSubject one person's share of it
	// (their own earliest-ending go first), maxLoggedOutUnnamed that of
	// the sessions that name nobody together. Some 100 bytes an entry.
	maxLoggedOut           = 1 << 16
	maxLoggedOutPerSubject = 64
	maxLoggedOutUnnamed    = 1024
)

// Principal is the signed-in user of one request.
type Principal struct {
	// User is the identity the API server reported for the token.
	User authz.User
	// Provider and SessionID describe the session.
	Provider  string
	SessionID string
	// KubeContext is the kubeconfig context in local mode; empty otherwise.
	KubeContext string
	// Token is the user's bearer token, for the client factory only. It
	// is never serialised, logged or returned.
	Token string `json:"-"`
	// TokenExpiry is when Token expires (zero: unknown); Expiry when the
	// session ends unless refreshed or used.
	TokenExpiry time.Time
	Expiry      time.Time
	// AbsoluteExpiry is the MaxAge bound no activity extends.
	AbsoluteExpiry time.Time
	// Refreshable reports whether the session can renew its token.
	Refreshable bool
	// IssuedAt is the sign-in time.
	IssuedAt time.Time
	// Scopes are what a console grant (an MCP client's access token)
	// was consented for: "mcp:read", and "mcp:operate"
	// when the write tools were on. Nil for a session or a raw token,
	// whose holder acts with their own rights in full.
	Scopes []string
	// Assistant is the declared name of the MCP client a console grant
	// was issued to (unverified, as the consent page says); empty
	// otherwise. The audit trail names it.
	Assistant string
	// AssistantRedirect is the callback host that grant's consent sent
	// its code to; empty otherwise (or for a grant issued before it was
	// recorded).
	AssistantRedirect string
}

// ReauthAt is when the UI should be told to re-authenticate.
func (p *Principal) ReauthAt() time.Time { return p.Expiry.Add(-ReauthLead) }

// Context returns ctx carrying the user's token for the user client.
func (p *Principal) Context(ctx context.Context) context.Context {
	return WithUserToken(ctx, p.Token)
}

type principalKey struct{}

// WithPrincipal stores p in ctx.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the request's principal, set by Require.
func PrincipalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(*Principal)
	return p, ok && p != nil
}

// Handler serves the sign-in endpoints and authenticates requests.
type Handler struct {
	cfg         Config
	codec       *Codec
	identity    IdentityReviewer
	revocations *Revocations
	audit       *Auditor
	log         *slog.Logger
	oauth       []oauthProvider
	// grantOIDC is the OIDC grant client (OIDC_GRANT_CLIENT_ID) of grant
	// sign-ins; nil: they use the console's client.
	grantOIDC *oidcProvider
	token     bool
	refreshes singleflight.Group
	now       func() time.Time
	// Each key space has a table of its own, so that a spray of one kind
	// of key, which resets its table once full, never resets another's.
	// attempts is the per-address budget of the OAuth callbacks (and of
	// the MCP endpoint's unrecognised bearers); tokenAttempts is per
	// pasted-token digest and tokenAddresses per address of the token form.
	attempts       *RateLimiter
	tokenAttempts  *RateLimiter
	tokenAddresses *RateLimiter
	// The per-replica bounds (one key each): tokenReviews of tokens never
	// accepted (the token form, unknown raw bearers), knownReviews of raw
	// bearers accepted before, codeExchanges of the callbacks' exchanges
	// with the IdP, exchangeReviews of the tokens those exchanges return.
	tokenReviews    *RateLimiter
	knownReviews    *RateLimiter
	codeExchanges   *RateLimiter
	exchangeReviews *RateLimiter
	// knownSubjectReviews and exchangeSubjectReviews are the per-person
	// shares of knownReviews and exchangeReviews, charged before them.
	knownSubjectReviews    *RateLimiter
	exchangeSubjectReviews *RateLimiter
	// knownBearers are the raw bearers this replica accepted, by digest,
	// until their expiry: their re-reviews spend knownReviews.
	knownBearers *knownTokens
	// flows are the sign-in flows already used on this replica.
	flows *spentFlows

	// loggedOut holds sessions signed out on this replica until their
	// absolute expiry: a copy of the cookie stops working here at once.
	// It is bounded (maxLoggedOut, per person maxLoggedOutPerSubject).
	// signOuts shares them with the other replicas and across restarts
	// (the console's state ConfigMap); without it they learn of a
	// sign-out only through the revocation list.
	mu        sync.Mutex
	loggedOut map[string]loggedOutEntry
	// loggedOutMax is maxLoggedOut (a field so a test can lower it).
	loggedOutMax int
	signOuts     SignOutStore
	// grantDone receives grant sign-ins (grantsignin.go); signOutHook
	// disconnects a person's assistants on request at sign-out.
	grantDone   GrantCompletion
	signOutHook SignOutHook
}

// SignOutStore shares signed-out sessions between replicas
// (internal/console/sharedstate). It keeps a hash of the ID, never the ID.
type SignOutStore interface {
	// RecordSignOut shares sid, of the person tagged tag
	// (Session.SubjectTag; empty when the session names nobody), until it
	// would have ended; false when a bound of the store refused it.
	RecordSignOut(sid, tag string, until time.Time) bool
	SignedOut(sid string) bool
}

// Options are the dependencies of a Handler.
type Options struct {
	Config      Config
	Codec       *Codec
	Identity    IdentityReviewer
	Revocations *Revocations
	// SignOuts, when set, shares sign-outs with the other replicas.
	SignOuts SignOutStore
	Logger   *slog.Logger
}

// New builds the handler and its providers. Provider discovery happens
// here, bounded by the IdP timeout: a console that cannot reach its
// identity provider at start fails to start and says why.
func New(ctx context.Context, o Options) (*Handler, error) {
	var providers []oauthProvider
	for _, name := range o.Config.Providers {
		switch name {
		case ProviderOIDC:
			p, err := newOIDCProvider(ctx, o.Config.OIDC, o.Config.IdPTimeout)
			if err != nil {
				return nil, err
			}
			providers = append(providers, p)
		case ProviderOpenShift:
			p, err := newOpenShiftProvider(ctx, o.Config.OpenShift, o.Config.IdPTimeout)
			if err != nil {
				return nil, err
			}
			providers = append(providers, p)
		}
	}
	return newHandler(o, providers), nil
}

func newHandler(o Options, providers []oauthProvider) *Handler {
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	for _, warning := range o.Config.Warnings() {
		log.Warn(warning)
	}
	var grantOIDC *oidcProvider
	for _, p := range providers {
		if op, ok := p.(*oidcProvider); ok {
			grantOIDC = op.grantClient()
		}
	}
	return &Handler{
		grantOIDC:       grantOIDC,
		cfg:             o.Config,
		codec:           o.Codec,
		identity:        o.Identity,
		revocations:     o.Revocations,
		audit:           &Auditor{Logger: log, TrustedProxy: o.Config.TrustedProxy},
		log:             log,
		oauth:           providers,
		token:           slices.Contains(o.Config.Providers, ProviderToken),
		now:             time.Now,
		attempts:        NewRateLimiter(signInPerMinute, signInBurst, maxLimiterKeys).Named(BucketSignInAddress),
		tokenAttempts:   NewRateLimiter(signInPerMinute, signInBurst, maxLimiterKeys).Named(BucketTokenFormToken),
		tokenAddresses:  NewRateLimiter(tokenAddressPerMinute, tokenAddressBurst, maxLimiterKeys).Named(BucketTokenFormAddress),
		tokenReviews:    NewRateLimiter(tokenReviewsPerSecond*60, tokenReviewsBurst, 1).Named(BucketTokenReviews),
		knownReviews:    NewRateLimiter(knownReviewsPerSecond*60, knownReviewsBurst, 1).Named(BucketKnownReviews),
		codeExchanges:   NewRateLimiter(codeExchangesPerSecond*60, codeExchangesBurst, 1).Named(BucketCodeExchanges),
		exchangeReviews: NewRateLimiter(exchangeReviewsPerSecond*60, exchangeReviewsBurst, 1).Named(BucketExchangeReviews),
		knownSubjectReviews: NewRateLimiter(knownSubjectReviewsPerMinute, knownSubjectReviewsBurst, maxLimiterKeys).
			Named(BucketKnownReviewsSubject),
		exchangeSubjectReviews: NewRateLimiter(exchangeSubjectReviewsPerMinute, exchangeSubjectReviewsBurst, maxLimiterKeys).
			Named(BucketExchangeReviewsSubject),
		knownBearers: newKnownTokens(maxKnownBearers, maxKnownBearersPerSubject),
		flows:        newSpentFlows(maxSpentFlows),
		loggedOut:    map[string]loggedOutEntry{},
		loggedOutMax: maxLoggedOut,
		signOuts:     o.SignOuts,
	}
}

// Register mounts the sign-in endpoints on mux. All of them are public:
// they run before a session exists.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", h.handleLoginPage)
	for _, p := range h.oauth {
		mux.HandleFunc("GET /auth/"+p.name()+"/login", func(w http.ResponseWriter, r *http.Request) { h.handleStart(w, r, p) })
		mux.HandleFunc("GET /auth/"+p.name()+"/callback", func(w http.ResponseWriter, r *http.Request) { h.handleCallback(w, r, p) })
	}
	if h.token {
		mux.HandleFunc("POST /auth/token", h.handleToken)
	}
	mux.HandleFunc("GET /auth/noscript", func(w http.ResponseWriter, _ *http.Request) { NoScriptPage(w) })
	mux.HandleFunc("GET /logout", h.handleLogoutPage)
	mux.HandleFunc("POST /logout", h.handleLogout)
}

// Login reasons shown on the sign-in page.
var loginNotices = map[string]string{
	"expired":  "Your session has ended; please sign in again.",
	"revoked":  "Your session was revoked by an administrator; please sign in again.",
	"rejected": "The Kubernetes API server no longer accepts your sign-in; please sign in again.",
}

func (h *Handler) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	rd := SafeRedirect(r.URL.Query().Get("rd"))
	// One redirect provider and nothing to type: the provider is the
	// whole sign-in, so go there directly, unless a notice must be read
	// first.
	notice := loginNotices[r.URL.Query().Get("reason")]
	if len(h.oauth) == 1 && !h.token && notice == "" {
		http.Redirect(w, r, "/auth/"+h.oauth[0].name()+"/login?rd="+url.QueryEscape(rd), http.StatusFound)
		return
	}
	page := loginPage{Notice: notice, RedirectTo: rd, Token: h.token}
	for _, p := range h.oauth {
		page.Buttons = append(page.Buttons, loginButton{Label: p.label(), Path: "/auth/" + p.name() + "/login"})
	}
	renderPage(w, http.StatusOK, "login", "Sign in", page)
}

func (h *Handler) redirectURI(r *http.Request, p oauthProvider) string {
	if p.name() == ProviderOIDC && h.cfg.OIDC.RedirectURL != "" {
		return h.cfg.OIDC.RedirectURL
	}
	return ExpectedOrigin(r, h.cfg.PublicURL) + "/auth/" + p.name() + "/callback"
}

func (h *Handler) handleStart(w http.ResponseWriter, r *http.Request, p oauthProvider) {
	f := loginFlow{
		Provider:   p.name(),
		State:      randomToken(),
		Nonce:      randomToken(),
		Verifier:   oauth2.GenerateVerifier(),
		RedirectTo: SafeRedirect(r.URL.Query().Get("rd")),
		Expiry:     h.now().Add(loginFlowMaxAge).Unix(),
	}
	if err := h.codec.writeLogin(w, f); err != nil {
		h.log.Error("sealing the login-flow cookie failed", "error", err)
		ErrorPage(w, http.StatusInternalServerError, "The sign-in could not be started.")
		return
	}
	// The target is the IdP's authorization endpoint from configuration
	// and discovery, not from the request.
	http.Redirect(w, r, p.authCodeURL(h.redirectURI(r, p), f), http.StatusFound) // #nosec G710
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, provider string, e *loginError) {
	logins.WithLabelValues(provider, "failure").Inc()
	attrs := []any{"provider", provider, "reason", e.message}
	if e.cause != nil {
		attrs = append(attrs, "error", e.cause.Error())
	}
	h.audit.Log(r.Context(), r, AuditLoginFailure, attrs...)
	ErrorPage(w, e.status, e.message)
}

func (h *Handler) handleCallback(w http.ResponseWriter, r *http.Request, p oauthProvider) {
	// The flow cookie is single-use whatever the outcome; it is expired
	// before anything is written, since a header set after the status
	// line is lost.
	h.codec.clearLogin(w)
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		if len(e) > maxIdPErrorLen {
			e = e[:maxIdPErrorLen]
		}
		h.fail(w, r, p.name(), &loginError{http.StatusBadRequest, "The identity provider returned an error.", errors.New(e)})
		return
	}
	f, err := h.codec.readLogin(r, p.name())
	if err != nil {
		h.fail(w, r, p.name(), &loginError{http.StatusBadRequest, "The sign-in expired; please try again.", err})
		return
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(f.State)) != 1 {
		h.fail(w, r, p.name(), &loginError{http.StatusBadRequest, "Sign-in state mismatch; please try again.", nil})
		return
	}
	// A flow is used once, whatever the browser does with the cookie: a
	// replayed cookie and state are refused here, before the budget.
	if h.flows.spent(p.name(), f.State, h.now()) {
		h.fail(w, r, p.name(), &loginError{http.StatusBadRequest, "This sign-in was already used; please start again.", nil})
		return
	}
	// The budgets are charged only once the flow cookie and its state
	// match: a callback without them costs nothing (no code exchange, no
	// SelfSubjectReview). That alone does not protect a shared address:
	// anyone can start a flow (GET /auth/<provider>/login) and call its
	// callback with a junk code, and each such flow passes the checks
	// above once. So the per-address budget is charged only when the
	// address is a client's; behind proxies the console does not trust
	// (SharedClientKey) every user has the proxy's address, and the
	// callbacks are bounded instead by their single use (spend below) and
	// the per-replica exchange bucket, which is sized for the IdP, not for
	// people: a junk code costs one refused exchange and no review.
	if key, ok := h.addressBudget(r); ok && !h.allowSignIn(w, r, p.name(), h.attempts, key) {
		return
	}
	if !h.codeExchanges.Allow(codeExchangesKey) {
		h.fail(w, r, p.name(), &loginError{http.StatusTooManyRequests, "The console is receiving too many sign-ins; wait a few seconds and try again.", nil})
		return
	}
	if !h.flows.spend(p.name(), f.State, time.Unix(f.Expiry, 0), h.now()) {
		h.fail(w, r, p.name(), &loginError{http.StatusBadRequest, "This sign-in was already used; please start again.", nil})
		return
	}
	code := q.Get("code")
	if code == "" {
		h.fail(w, r, p.name(), &loginError{http.StatusBadRequest, "The authorization code is missing.", nil})
		return
	}
	exchanger, issuer := p, p.name()
	if f.GrantClient != "" {
		// Begun with the grant client: redeemed with it (the code is bound
		// to the client), or refused when this replica has another.
		if f.Grant == "" || h.grantOIDC == nil || h.grantOIDC.cfg.ClientID != f.GrantClient || p.name() != ProviderOIDC {
			h.fail(w, r, p.name(), &loginError{http.StatusBadRequest, "This sign-in was started with another client configuration; please start the connection again.", nil})
			return
		}
		exchanger, issuer = h.grantOIDC, ProviderOIDCGrant
	}
	ts, lerr := exchanger.exchange(r.Context(), h.redirectURI(r, p), code, f)
	if lerr != nil {
		h.fail(w, r, p.name(), lerr)
		return
	}
	// The IdP issued a token: only now is a SelfSubjectReview spent, from
	// a bucket that no junk code reaches, after the person's own share of
	// it when the IdP named them.
	if ts.identity != "" && !h.exchangeSubjectReviews.Allow(ts.identity) {
		h.fail(w, r, p.name(), &loginError{http.StatusTooManyRequests, "You are signing in too often; wait a minute and try again.", nil})
		return
	}
	if !h.exchangeReviews.Allow(exchangeReviewsKey) {
		h.fail(w, r, p.name(), &loginError{http.StatusTooManyRequests, "The console is receiving too many sign-ins; wait a few seconds and try again.", nil})
		return
	}
	if f.Grant != "" {
		h.completeGrant(w, r, issuer, ts, f.Grant)
		return
	}
	h.complete(w, r, p.name(), ts, f.RedirectTo)
}

func (h *Handler) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := CheckRequest(r, h.cfg.PublicURL, false); err != nil {
		ErrorPage(w, http.StatusForbidden, "This form can only be submitted from the console's own sign-in page.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		ErrorPage(w, http.StatusBadRequest, "The form could not be read.")
		return
	}
	tok, ok := normalizePastedToken(r.PostForm.Get("token"))
	if !ok {
		h.fail(w, r, ProviderToken, &loginError{http.StatusBadRequest, "That is not a token: paste the output of kubectl create token.", nil})
		return
	}
	// The token's own budget first, keyed on its digest: retrying one
	// token is bounded without spending its sender's address budget.
	if !h.allowSignIn(w, r, ProviderToken, h.tokenAttempts, tokenAttemptKey(tok)) {
		return
	}
	// Then the sender's address, when it is a client's: distinct junk
	// tokens from one address stop there, and cannot drain the shared
	// bucket below. A shared address (SharedClientKey) is not charged:
	// one sender would spend it for everyone behind the proxy.
	if key, ok := h.addressBudget(r); ok && !h.allowSignIn(w, r, ProviderToken, h.tokenAddresses, key) {
		return
	}
	// Then the process-wide bound, before anything reaches the API
	// server: a token refused by either budget spends none of it.
	if !h.tokenReviews.Allow(tokenReviewsKey) {
		h.fail(w, r, ProviderToken, &loginError{http.StatusTooManyRequests, "The console is receiving too many token sign-ins; wait a few seconds and try again.", nil})
		return
	}
	h.complete(w, r, ProviderToken, tokenSet{token: tok, expiry: pastedTokenExpiry(tok)}, r.PostForm.Get("rd"))
}

// addressBudget returns the per-address budget key of r's client, and
// false when the client address is shared by every client behind an
// untrusted proxy: such a key is not charged by the sign-in paths.
func (h *Handler) addressBudget(r *http.Request) (string, bool) {
	return BudgetKey(r, h.cfg.TrustedProxy)
}

// allowSignIn spends one sign-in attempt of key's budget in limiter.
func (h *Handler) allowSignIn(w http.ResponseWriter, r *http.Request, provider string, limiter *RateLimiter, key string) bool {
	if limiter.Allow(key) {
		return true
	}
	h.fail(w, r, provider, &loginError{http.StatusTooManyRequests, "Too many sign-in attempts; wait a minute and try again.", nil})
	return false
}

// tokenAttemptKey is the sign-in budget key of a pasted token: a
// digest, so the limiter never holds the token itself, in a key space
// ("token:") that no address key can collide with.
func tokenAttemptKey(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return "token:" + hex.EncodeToString(sum[:16])
}

// normalizePastedToken trims a pasted token and an optional "Bearer "
// prefix, and refuses anything that cannot be a bearer token.
func normalizePastedToken(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) > 7 && strings.EqualFold(s[:7], "bearer ") {
		s = strings.TrimSpace(s[7:])
	}
	if s == "" || len(s) > maxPastedToken {
		return "", false
	}
	for _, c := range s {
		if c <= 0x20 || c >= 0x7f {
			return "", false
		}
	}
	return s, true
}

// complete turns a provider's token into a session: the API server says
// whose token it is (SelfSubjectReview), and only then is a cookie set.
func (h *Handler) complete(w http.ResponseWriter, r *http.Request, provider string, ts tokenSet, rd string) {
	u, err := h.identity.Review(r.Context(), ts.token)
	if errors.Is(err, ErrTokenRejected) {
		msg := "The Kubernetes API server does not accept this token."
		if provider == ProviderOIDC {
			msg = "The Kubernetes API server does not accept tokens from this identity provider. It has to trust the issuer " +
				"(the --oidc-* flags or an AuthenticationConfiguration); until it does, sign in with an access token."
		}
		h.fail(w, r, provider, &loginError{http.StatusForbidden, msg, err})
		return
	}
	if err != nil {
		h.fail(w, r, provider, &loginError{http.StatusServiceUnavailable, "The Kubernetes API server could not be reached to check this sign-in; please try again.", err})
		return
	}
	now := h.now()
	s := Session{
		ID:           newSessionID(),
		Provider:     provider,
		Token:        ts.token,
		RefreshToken: ts.refresh,
		IssuedAt:     now.Unix(),
		LastSeen:     now.Unix(),
	}
	if tags := h.codec.KeyedHashes(SignOutSubjectPurpose, u.Name); len(tags) > 0 {
		s.SubjectTag = tags[0]
	}
	if !ts.expiry.IsZero() {
		s.TokenExpiry = ts.expiry.Unix()
	}
	s, err = h.codec.WriteSession(w, r, s)
	if err != nil {
		h.fail(w, r, provider, &loginError{http.StatusInternalServerError, "The session could not be created: " + err.Error(), err})
		return
	}
	logins.WithLabelValues(provider, "success").Inc()
	h.audit.Log(r.Context(), r, AuditLoginSuccess, "user", u.Name, "provider", provider, "session", s.ID,
		"expires", s.Expiry(h.codec.Limits()).Format(time.RFC3339))
	// SafeRedirect admits only a same-origin relative path.
	http.Redirect(w, r, SafeRedirect(rd), http.StatusFound) // #nosec G710
}

func (h *Handler) handleLogoutPage(w http.ResponseWriter, r *http.Request) {
	s, err := h.codec.ReadSession(r)
	if err != nil {
		renderPage(w, http.StatusOK, "signed-out", "Signed out", signedOutPage{})
		return
	}
	subject := ""
	if u, err := h.identity.Review(r.Context(), s.Token); err == nil {
		subject = u.Name
	}
	renderPage(w, http.StatusOK, "signed-out", "Sign out", signedOutPage{Confirm: true, Subject: subject, Disconnect: h.signOutHookFn() != nil})
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := CheckRequest(r, h.cfg.PublicURL, false); err != nil {
		ErrorPage(w, http.StatusForbidden, "Sign-out can only be requested from this console's own pages.")
		return
	}
	if s, err := h.codec.ReadSession(r); err == nil {
		user := ""
		u, err := h.identity.Review(r.Context(), s.Token)
		if err == nil {
			user = u.Name
		}
		// A token the API server refuses, in a session that cannot renew
		// it, is refused by every replica already (their identity caches
		// last DefaultIdentityTTL): sharing its sign-out would only spend
		// the state ConfigMap's bounds.
		share := !errors.Is(err, ErrTokenRejected) || s.RefreshToken != ""
		if c, ok := h.identity.(*CachedReviewer); ok {
			c.Forget(s.Token)
		}
		h.audit.Log(r.Context(), r, AuditLogout, "user", user, "provider", s.Provider, "session", s.ID)
		// "Sign out and disconnect my assistants": the person's grants end
		// too. The API server named the person: a session whose token it
		// no longer accepts disconnects nothing (the grants then fail on
		// their own, carrying the same identity).
		if hook := h.signOutHookFn(); hook != nil && user != "" && r.PostFormValue("disconnect") == "assistants" {
			hook(r.Context(), ClientOf(r, h.cfg.TrustedProxy), user)
		}
		// The sign-out counts against the person sealed at sign-in, not
		// against what the API server says now (nobody, once the token is
		// revoked).
		h.markLoggedOut(s.ID, s.SubjectTag, s.AbsoluteExpiry(h.codec.Limits()), share)
		// The session's token is refused as a raw bearer too (the MCP
		// endpoint), until the session would have ended.
		h.markLoggedOut(BearerReference(s.Token), s.SubjectTag, s.AbsoluteExpiry(h.codec.Limits()), share)
	}
	h.codec.ClearSession(w)
	h.codec.clearLogin(w)
	renderPage(w, http.StatusOK, "signed-out", "Signed out", signedOutPage{})
}

// Require authenticates every request it wraps: a sealed, unexpired,
// unrevoked session whose token the API server still accepts. It renews
// an OIDC token close to expiry and slides the idle timer. Unauthenticated
// API calls get 401 JSON; page loads are sent to the sign-in page.
func (h *Handler) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, reason, err := h.authenticate(w, r)
		if err != nil {
			h.log.Warn("authentication backend unavailable", "error", err)
			if isAPI(r) {
				writeProblem(w, http.StatusServiceUnavailable, "The Kubernetes API server could not be reached to check your session.", "")
				return
			}
			ErrorPage(w, http.StatusServiceUnavailable, "The Kubernetes API server could not be reached to check your session; please retry.")
			return
		}
		if p == nil {
			h.unauthenticated(w, r, reason)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

func isAPI(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/api/") }

func (h *Handler) unauthenticated(w http.ResponseWriter, r *http.Request, reason string) {
	login := "/auth/login"
	if reason != "" {
		login += "?reason=" + url.QueryEscape(reason)
	}
	if isAPI(r) || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		writeProblem(w, http.StatusUnauthorized, "Sign in to use the console.", login)
		return
	}
	sep := "?"
	if reason != "" {
		sep = "&"
	}
	http.Redirect(w, r, login+sep+"rd="+url.QueryEscape(SafeRedirect(r.URL.RequestURI())), http.StatusFound)
}

// authenticate returns the principal, or nil with the reason the session
// was refused ("" when there was none), or an error when the API server
// could not be asked.
func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request) (*Principal, string, error) {
	p, _, reason, err := h.authenticateSession(w, r)
	return p, reason, err
}

// authenticateSession is authenticate, with the session it read (its
// tokens, after any refresh) for the MCP authorization server.
func (h *Handler) authenticateSession(w http.ResponseWriter, r *http.Request) (*Principal, Session, string, error) {
	s, err := h.codec.ReadSession(r)
	switch {
	case errors.Is(err, errNoSession):
		return nil, Session{}, "", nil
	case errors.Is(err, errSessionExpired):
		sessionRejections.WithLabelValues("expired").Inc()
		h.codec.ClearSession(w)
		return nil, Session{}, "expired", nil
	case err != nil:
		sessionRejections.WithLabelValues("malformed").Inc()
		h.codec.ClearSession(w)
		return nil, Session{}, "", nil //nolint:nilerr // a malformed session cookie is cleared and treated as no session, so the reader signs in again
	}
	now := h.now()
	dirty := false
	if h.isLoggedOut(s.ID) {
		h.codec.ClearSession(w)
		return nil, Session{}, "", nil
	}
	if r.URL.Path == SessionPath && s.RefreshToken != "" && s.TokenExpiry > 0 && now.After(time.Unix(s.TokenExpiry, 0).Add(-refreshLead)) {
		if ns, ok := h.refresh(r.Context(), s); ok {
			s, dirty = ns, true
		} else if !now.Before(time.Unix(s.TokenExpiry, 0)) {
			sessionRejections.WithLabelValues("expired").Inc()
			h.codec.ClearSession(w)
			return nil, Session{}, "expired", nil
		}
	}
	u, err := h.identity.Review(r.Context(), s.Token)
	if errors.Is(err, ErrTokenRejected) {
		sessionRejections.WithLabelValues("token_rejected").Inc()
		h.codec.ClearSession(w)
		return nil, Session{}, "rejected", nil
	}
	if err != nil {
		return nil, Session{}, "", err
	}
	if h.revocations.Revoked(s.ID, u.Name, time.Unix(s.IssuedAt, 0)) {
		sessionRejections.WithLabelValues("revoked").Inc()
		h.audit.Log(r.Context(), r, AuditRevoked, "user", u.Name, "provider", s.Provider, "session", s.ID)
		h.codec.ClearSession(w)
		return nil, Session{}, "revoked", nil
	}
	// Only the console's own pages slide the idle timer: on OpenShift a
	// neighbouring *.apps application is same-site, and its <img> pointing
	// here would otherwise keep an abandoned session alive.
	site := r.Header.Get("Sec-Fetch-Site")
	if (site == "" || site == "same-origin" || site == "none") && now.Sub(time.Unix(s.LastSeen, 0)) >= slideEvery {
		s.LastSeen = now.Unix()
		dirty = true
	}
	if dirty {
		if s, err = h.codec.WriteSession(w, r, s); err != nil {
			h.log.Error("rewriting the session cookie failed", "session", LogSafe(s.ID), "error", err)
		}
	}
	limits := h.codec.Limits()
	p := &Principal{
		User:           u,
		Provider:       s.Provider,
		SessionID:      s.ID,
		Token:          s.Token,
		Expiry:         s.Expiry(limits),
		AbsoluteExpiry: s.AbsoluteExpiry(limits),
		Refreshable:    s.RefreshToken != "",
		IssuedAt:       time.Unix(s.IssuedAt, 0).UTC(),
	}
	if s.TokenExpiry > 0 {
		p.TokenExpiry = time.Unix(s.TokenExpiry, 0).UTC()
		if s.RefreshToken != "" && p.TokenExpiry.Before(p.Expiry) {
			// A refreshable session still has to come back before the
			// token runs out, so the stream announces reauth then.
			p.Expiry = p.TokenExpiry
		}
	}
	return p, s, "", nil
}

// refresh renews the session's token with its provider. Concurrent
// requests of one session share one refresh, so a rotating refresh token
// is redeemed once.
func (h *Handler) refresh(ctx context.Context, s Session) (Session, bool) {
	var ref refresher
	for _, p := range h.oauth {
		if p.name() == s.Provider {
			ref, _ = p.(refresher)
		}
	}
	if ref == nil {
		return s, false
	}
	v, err, _ := h.refreshes.Do(s.ID, func() (any, error) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.cfg.IdPTimeout)
		defer cancel()
		return ref.refresh(ctx, s.RefreshToken)
	})
	if err != nil {
		tokenRefreshes.WithLabelValues("failure").Inc()
		h.log.Warn("token refresh failed; the session ends with its token", "session", LogSafe(s.ID), "provider", LogSafe(s.Provider), "error", err)
		return s, false
	}
	ts, _ := v.(tokenSet)
	tokenRefreshes.WithLabelValues("success").Inc()
	s.Token, s.RefreshToken = ts.token, ts.refresh
	s.TokenExpiry = 0
	if !ts.expiry.IsZero() {
		s.TokenExpiry = ts.expiry.Unix()
	}
	return s, true
}

// Problem is the RFC 9457 body of the sign-in layer's API errors; the
// Huma API uses the same shape.
type Problem struct {
	Status   int    `json:"status"`
	Title    string `json:"title"`
	Detail   string `json:"detail,omitempty"`
	LoginURL string `json:"loginURL,omitempty"`
}

func writeProblem(w http.ResponseWriter, status int, detail, login string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Problem{Status: status, Title: http.StatusText(status), Detail: detail, LoginURL: login})
}

// String renders a principal for logs without its token.
func (p *Principal) String() string {
	return fmt.Sprintf("%s via %s (session %s)", p.User.Name, p.Provider, p.SessionID)
}

// loggedOutEntry is one session signed out on this replica: until when
// it is refused, and its person's tag (Session.SubjectTag).
type loggedOutEntry struct {
	until time.Time
	tag   string
}

// markLoggedOut refuses sid on this replica until it would have ended,
// and, when share is set, shares it with the others under the person's
// tag. The shared document bounds each person's sign-outs and its own
// size (sharedstate.MaxSignOuts); a sign-out it refuses is still refused
// here. This replica's own table is bounded too: a person past their
// share forgets their own earliest-ending sign-out, and a full table
// those that end first.
func (h *Handler) markLoggedOut(sid, tag string, until time.Time, share bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	held := 0
	limit := maxLoggedOutPerSubject
	if tag == "" {
		limit = maxLoggedOutUnnamed
	}
	var mineFirst string
	for k, e := range h.loggedOut {
		switch {
		case now.After(e.until):
			delete(h.loggedOut, k)
		case e.tag == tag && k != sid:
			held++
			if mineFirst == "" || endsBefore(k, e, mineFirst, h.loggedOut[mineFirst]) {
				mineFirst = k
			}
		}
	}
	if _, again := h.loggedOut[sid]; !again {
		if held >= limit {
			delete(h.loggedOut, mineFirst)
		}
		for len(h.loggedOut) >= h.loggedOutMax {
			first := ""
			for k, e := range h.loggedOut {
				if first == "" || endsBefore(k, e, first, h.loggedOut[first]) {
					first = k
				}
			}
			delete(h.loggedOut, first)
		}
	}
	if prev, ok := h.loggedOut[sid]; !ok || until.After(prev.until) {
		h.loggedOut[sid] = loggedOutEntry{until: until, tag: tag}
	}
	if share && h.signOuts != nil {
		h.signOuts.RecordSignOut(sid, tag, until)
	}
}

// endsBefore orders loggedOut entries by when they end, then by key.
func endsBefore(ak string, a loggedOutEntry, bk string, b loggedOutEntry) bool {
	if !a.until.Equal(b.until) {
		return a.until.Before(b.until)
	}
	return ak < bk
}

// SignedOut reports whether the session sid was signed out, on this
// replica or (shared state) another.
func (h *Handler) SignedOut(sid string) bool { return sid != "" && h.isLoggedOut(sid) }

func (h *Handler) isLoggedOut(sid string) bool {
	h.mu.Lock()
	_, ok := h.loggedOut[sid]
	h.mu.Unlock()
	return ok || (h.signOuts != nil && h.signOuts.SignedOut(sid))
}

// StillValid re-checks an established principal: the API server still
// accepts its token for the same user, and the session is neither revoked
// nor signed out. Long-lived requests (the event stream) call it
// periodically.
func (h *Handler) StillValid(ctx context.Context, p *Principal) bool {
	if p == nil || h.isLoggedOut(p.SessionID) || !h.now().Before(p.Expiry) {
		return false
	}
	u, err := h.identity.Review(ctx, p.Token)
	if err != nil {
		// An unreachable API server is not a revocation: keep the stream
		// and let the next check decide.
		return !errors.Is(err, ErrTokenRejected)
	}
	if u.Name != p.User.Name {
		return false
	}
	return !h.revocations.Revoked(p.SessionID, u.Name, p.IssuedAt)
}
