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

package mcpoauth

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/sharedstate"
)

const maxState = 512

// pkceChallenge is an S256 challenge: the base64url SHA-256 of the
// verifier, 43 characters (RFC 7636 §4.2).
var pkceChallenge = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// authorizeRequest is a validated authorization request.
type authorizeRequest struct {
	client      client
	redirectURI string
	state       string
	challenge   string
	resource    string
	// scope is the most the consent may grant.
	scope string
}

// redirectable is an error answered to the client's redirect URI (RFC
// 6749 §4.1.2.1): the client and the redirect URI were valid.
type redirectable struct{ code, desc string }

func (e *redirectable) Error() string { return e.code + ": " + e.desc }

// parseAuthorize validates the query. An error that is not *redirectable
// is shown to the person: the redirect URI cannot be trusted with it.
// It runs only for a signed-in person: an anonymous caller makes the
// console fetch nothing (a client ID metadata document) and learns
// nothing about a client ID.
func (s *Server) parseAuthorize(r *http.Request) (authorizeRequest, error) {
	q := r.URL.Query()
	for k, v := range q {
		if len(v) > 1 {
			return authorizeRequest{}, fmt.Errorf("the parameter %s is repeated", k)
		}
	}
	c, err := s.resolveClient(r.Context(), q.Get("client_id"))
	if err != nil {
		return authorizeRequest{}, err
	}
	ru := q.Get("redirect_uri")
	if ru == "" {
		if len(c.RedirectURIs) != 1 {
			return authorizeRequest{}, errors.New("redirect_uri is required: this client registered several")
		}
		ru = c.RedirectURIs[0]
	}
	matched, ok := matchRedirect(c.RedirectURIs, ru)
	if !ok {
		return authorizeRequest{}, errors.New("redirect_uri is not one this client registered")
	}
	req := authorizeRequest{client: c, redirectURI: matched, state: q.Get("state")}
	switch {
	case q.Get("response_type") != "code":
		return req, &redirectable{"unsupported_response_type", "response_type must be code"}
	case req.state == "" || len(req.state) > maxState:
		return req, &redirectable{"invalid_request", "a state of at most 512 characters is required"}
	case q.Get("code_challenge_method") != "S256" || !pkceChallenge.MatchString(q.Get("code_challenge")):
		return req, &redirectable{"invalid_request", "PKCE with code_challenge_method S256 is required"}
	case q.Get("request") != "" || q.Get("request_uri") != "":
		return req, &redirectable{"request_not_supported", "request objects are not supported"}
	}
	req.challenge = q.Get("code_challenge")
	want := s.resourceFor(r)
	req.resource = want
	if res := q.Get("resource"); res != "" && !sameResource(res, want) {
		return req, &redirectable{"invalid_target", "the only resource this console grants is " + want}
	}
	scope, err := s.grantScope(q.Get("scope"))
	if err != nil {
		return req, &redirectable{"invalid_scope", err.Error()}
	}
	req.scope = scope
	return req, nil
}

// identityScopes are OpenID Connect scopes some clients add to every
// request; this server issues no ID token and ignores them.
var identityScopes = []string{"openid", "profile", "email", "offline_access"}

// grantScope is the most a consent may grant for the requested scopes:
// read (also when nothing is requested), plus operate only when the
// client asks for it and the write tools are on (the server may grant
// less than asked, RFC 6749 §3.3; the token response says what it
// granted). An unknown scope is refused.
func (s *Server) grantScope(requested string) (string, error) {
	asked := strings.Fields(requested)
	for _, sc := range asked {
		if sc != ScopeRead && sc != ScopeOperate && !slices.Contains(identityScopes, sc) {
			return "", fmt.Errorf("the scope %q is not one this console grants (%s)", truncate(sc, 64), strings.Join(s.Scopes(), ", "))
		}
	}
	if s.o.WriteTools && slices.Contains(asked, ScopeOperate) {
		return ScopeRead + " " + ScopeOperate, nil
	}
	return ScopeRead, nil
}

func truncate(v string, n int) string {
	if len(v) <= n {
		return v
	}
	return v[:n] + "..."
}

// redirectTo answers the client at its redirect URI with params, and iss
// (RFC 9207).
func (s *Server) redirectTo(w http.ResponseWriter, r *http.Request, redirectURI string, params url.Values) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		auth.ErrorPage(w, http.StatusBadRequest, "The application's redirect address is not valid.")
		return
	}
	q := u.Query()
	for k, v := range params {
		q[k] = v
	}
	q.Set("iss", s.Issuer(r))
	u.RawQuery = q.Encode()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// The target is a loopback URI the client registered and the request
	// matched (parseLoopback, matchRedirect): it cannot leave the
	// person's computer.
	http.Redirect(w, r, u.String(), http.StatusFound) // #nosec G710
}

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	// Per client address, or, when the address is shared by every client
	// behind a proxy the console does not trust, per signed-in person once
	// the session is known (an anonymous request only goes to the sign-in).
	key, perAddress := auth.BudgetKey(r, s.o.TrustedProxy)
	if perAddress && !s.requests.Allow(key) {
		requests.WithLabelValues("authorize", "throttled").Inc()
		auth.ErrorPage(w, http.StatusTooManyRequests, "Too many authorization requests from this address; wait a minute and try again.")
		return
	}
	// The person first: nothing about the request is looked at (no client
	// ID resolved, no document fetched, no error sent to a redirect URI)
	// before the console knows who asks.
	sess, _, err := s.o.Sessions.CurrentSession(w, r)
	if err != nil {
		auth.ErrorPage(w, http.StatusServiceUnavailable, "The Kubernetes API server could not be reached to check your session; please retry.")
		return
	}
	if sess == nil {
		// Back from a cross-site sign-in round trip, the SameSite=Strict
		// session cookie is withheld from a redirected navigation: a page
		// of this origin continues, and its navigation carries the cookie.
		if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" || site == "same-site" {
			auth.ContinuePage(w, r.URL.RequestURI())
			return
		}
		// LoginURL keeps rd a same-origin path (auth.SafeRedirect).
		http.Redirect(w, r, auth.LoginURL(r.URL.RequestURI()), http.StatusFound) // #nosec G710 -- same-origin target
		return
	}
	if !perAddress && !s.requests.Allow("user:"+s.userKey(sess.Principal.User.Name)) {
		requests.WithLabelValues("authorize", "throttled").Inc()
		auth.ErrorPage(w, http.StatusTooManyRequests, "Too many authorization requests; wait a minute and try again.")
		return
	}
	// A request in error is answered with a page, never a redirect to the
	// client, although its redirect URI is known by now (RFC 6749 §4.1.2.1
	// would allow one): without the person's consent, a redirect would tell
	// any site that drew them here that they are signed in to this console.
	req, err := s.parseAuthorize(r)
	var re *redirectable
	switch {
	case errors.As(err, &re):
		requests.WithLabelValues("authorize", "refused").Inc()
		auth.ErrorPage(w, http.StatusBadRequest, "This connection request cannot be accepted ("+re.code+"): "+re.desc+".")
		return
	case err != nil:
		requests.WithLabelValues("authorize", "refused").Inc()
		auth.ErrorPage(w, http.StatusBadRequest, "This connection request cannot be accepted: "+err.Error()+".")
		return
	}
	user := sess.Principal.User.Name
	if len(s.Grants(r.Context(), user)) >= sharedstate.MaxGrantsPerUser {
		auth.ErrorPage(w, http.StatusConflict, fmt.Sprintf("You already have %d connected assistants on this console: disconnect one from the Assistants page first.", sharedstate.MaxGrantsPerUser))
		return
	}
	now := s.o.Now().UTC()
	sealed, err := s.seal("", purposeConsent, consentRequest{
		Session: sess.Principal.SessionID, Subject: user, Client: req.client.ID, Name: req.client.Name, ClientURL: req.client.URL,
		RedirectURI: req.redirectURI, Challenge: req.challenge, State: req.state, Resource: req.resource,
		Scope: req.scope, Nonce: randomID(), Expiry: now.Add(consentTTL).Unix(),
	})
	if err != nil {
		auth.ErrorPage(w, http.StatusInternalServerError, "The consent page could not be prepared.")
		return
	}
	ru, _ := url.Parse(req.redirectURI)
	operate := strings.Contains(req.scope, ScopeOperate)
	access := "read-only"
	if operate {
		access = "read-only, or read and operate: your choice below"
	}
	requests.WithLabelValues("authorize", "consent").Inc()
	auth.ConsentPage(w, auth.ConsentView{
		Subject: user, ClientName: req.client.Name, ClientURL: req.client.URL,
		RedirectHost: ru.Host + ru.EscapedPath(), Console: s.Issuer(r), Access: access, Operate: operate,
		Lifetime: s.lifetime(now, s.o.Sessions.Renews(sess.Provider)), Action: PathAuthorize, Request: sealed,
		RedirectOrigin: ru.Scheme + "://" + ru.Host,
	})
}

// lifetime describes how long a grant lasts.
func (s *Server) lifetime(now time.Time, renews bool) string {
	maxAge := s.o.Sessions.Limits().MaxAge
	if !renews {
		return fmt.Sprintf("at most %s, until %s UTC, and no longer than the token you signed in with",
			humanDuration(maxAge), now.Add(maxAge).Format("2006-01-02 15:04"))
	}
	return fmt.Sprintf("at most %s, until %s UTC (access renewed every %s by the application while your identity provider renews its sign-in)",
		humanDuration(maxAge), now.Add(maxAge).Format("2006-01-02 15:04"), humanDuration(s.o.AccessTTL))
}

func humanDuration(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		if d == time.Hour {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", d/time.Hour)
	case d >= time.Minute && d%time.Minute == 0:
		if d == time.Minute {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", d/time.Minute)
	}
	return d.String()
}

// handleDecision is the consent form's POST (CSRF-checked by the session
// mux). One page is good for one answer (its nonce is spent in the
// shared index), and a person's answers are rate-limited. Deny answers
// access_denied. Approve of an OIDC sign-in starts the grant's own IdP
// sign-in (completeGrant finishes it); of a sign-in that renews nothing,
// answers a code carrying the session's token.
func (s *Server) handleDecision(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		auth.ErrorPage(w, http.StatusBadRequest, "The form could not be read.")
		return
	}
	var cr consentRequest
	now := s.o.Now().UTC()
	if s.codec.OpenPurpose(purposeConsent, r.PostForm.Get("request"), &cr) != nil || now.Unix() >= cr.Expiry || cr.Nonce == "" {
		auth.ErrorPage(w, http.StatusBadRequest, "This consent page expired; start the connection from the application again.")
		return
	}
	sess, _, err := s.o.Sessions.CurrentSession(w, r)
	if err != nil {
		auth.ErrorPage(w, http.StatusServiceUnavailable, "The Kubernetes API server could not be reached to check your session; please retry.")
		return
	}
	if sess == nil || sess.Principal.SessionID != cr.Session || sess.Principal.User.Name != cr.Subject {
		auth.ErrorPage(w, http.StatusForbidden, "This consent page belongs to another sign-in; start the connection from the application again.")
		return
	}
	key := s.userKey(cr.Subject)
	if !s.decisions.Allow(key) {
		requests.WithLabelValues("authorize", "throttled").Inc()
		auth.ErrorPage(w, http.StatusTooManyRequests, "Too many connection answers; wait a minute and start the connection from the application again.")
		return
	}
	if !s.o.Store.GrantsUsable() {
		auth.ErrorPage(w, http.StatusServiceUnavailable, "The console cannot record connections right now; retry in a minute.")
		return
	}
	switch err := s.o.Store.UseCode("consent/"+cr.Nonce, key, time.Unix(cr.Expiry, 0).Add(clockSkew)); {
	case errors.Is(err, sharedstate.ErrSpent):
		auth.ErrorPage(w, http.StatusConflict, "This consent page was already answered; start the connection from the application again.")
		return
	case err != nil:
		auth.ErrorPage(w, http.StatusServiceUnavailable, "Too many connections in progress; retry in a minute.")
		return
	}
	client := auth.ClientOf(r, s.o.TrustedProxy)
	if r.PostForm.Get("decision") != "approve" {
		requests.WithLabelValues("authorize", "denied").Inc()
		s.audit.Record(r.Context(), client, auth.AuditMCPGrant, "outcome", "denied", "user", cr.Subject, "client", cr.Name, "session", cr.Session)
		s.redirectTo(w, r, cr.RedirectURI, url.Values{"error": {"access_denied"}, "error_description": {"the person declined"}, "state": {cr.State}})
		return
	}
	scope := ScopeRead
	if r.PostForm.Get("access") == "operate" && strings.Contains(cr.Scope, ScopeOperate) {
		scope = cr.Scope
	}
	p := pendingGrant{
		grantBase: grantBase{
			ID: newGrantID(), Client: cr.Client, Name: cr.Name, Scope: scope, Resource: cr.Resource,
			Subject: cr.Subject, Session: cr.Session, IssuedAt: now.Unix(), RedirectHost: hostOf(cr.RedirectURI),
		},
		Provider:    sess.Provider,
		RedirectURI: cr.RedirectURI, Challenge: cr.Challenge, State: cr.State, Expiry: now.Add(pendingTTL).Unix(),
	}
	if s.o.Sessions.Renews(sess.Provider) {
		sealed, err := s.seal("", purposePending, p)
		if err == nil {
			err = s.o.Sessions.BeginGrantSignIn(w, r, sess.Provider, sealed)
		}
		if err != nil {
			auth.ErrorPage(w, http.StatusInternalServerError, "The connection's sign-in could not be started.")
		}
		return
	}
	up := upstream{Provider: sess.Provider, Token: sess.Token}
	if !sess.TokenExpiry.IsZero() {
		up.Expiry = sess.TokenExpiry.Unix()
	}
	s.answerCode(w, r, p, up, now)
}

// completeGrant receives a grant's own IdP sign-in (auth.GrantCompletion):
// the same person as at consent, the consent still fresh and unrevoked,
// then the code.
func (s *Server) completeGrant(w http.ResponseWriter, r *http.Request, pending string, tokens auth.UpstreamTokens, user authz.User) {
	var p pendingGrant
	now := s.o.Now().UTC()
	if s.codec.OpenPurpose(purposePending, pending, &p) != nil || now.Unix() >= p.Expiry {
		auth.ErrorPage(w, http.StatusBadRequest, "This connection took too long; start it from the application again.")
		return
	}
	if user.Name != p.Subject {
		s.audit.Record(r.Context(), auth.ClientOf(r, s.o.TrustedProxy), auth.AuditMCPGrant, "outcome", "refused",
			"user", p.Subject, "client", p.Name, "reason", "the identity provider signed in another person")
		auth.ErrorPage(w, http.StatusForbidden, "The identity provider signed in another person than the one who approved; start the connection from the application again.")
		return
	}
	if s.o.Sessions.Revoked(p.Subject, time.Unix(p.IssuedAt, 0), p.Session) {
		auth.ErrorPage(w, http.StatusForbidden, "The sign-in this connection was approved in is revoked.")
		return
	}
	// Signed out between Approve and the IdP's answer: the approval went
	// with the session.
	if s.o.Sessions.SignedOut(p.Session) {
		s.audit.Record(r.Context(), auth.ClientOf(r, s.o.TrustedProxy), auth.AuditMCPGrant, "outcome", "refused",
			"user", p.Subject, "client", p.Name, "reason", "signed out before the connection completed")
		auth.ErrorPage(w, http.StatusForbidden, "You signed out of the console after approving this connection; start it from the application again.")
		return
	}
	up := upstream{Provider: p.Provider, Token: tokens.Token, Refresh: tokens.RefreshToken}
	if tokens.Provider != "" {
		// The sign-in layer names the client that renews these tokens
		// (the OIDC grant client, when one is configured).
		up.Provider = tokens.Provider
	}
	if !tokens.Expiry.IsZero() {
		up.Expiry = tokens.Expiry.Unix()
	}
	s.answerCode(w, r, p, up, now)
}

// answerCode seals the code of an approved grant and sends it to the
// client.
func (s *Server) answerCode(w http.ResponseWriter, r *http.Request, p pendingGrant, up upstream, now time.Time) {
	code := authorizationCode{
		grantBase:   p.grantBase,
		Upstream:    up,
		Nonce:       randomID(),
		RedirectURI: p.RedirectURI,
		Challenge:   p.Challenge,
		Expiry:      now.Add(codeTTL).Unix(),
	}
	sealed, err := s.seal(codePrefix, purposeCode, code)
	if err != nil {
		auth.ErrorPage(w, http.StatusInternalServerError, "The authorization could not be sealed.")
		return
	}
	requests.WithLabelValues("authorize", "approved").Inc()
	s.audit.Record(r.Context(), auth.ClientOf(r, s.o.TrustedProxy), auth.AuditMCPGrant, "outcome", "approved", "user", p.Subject,
		"grant", p.ID, "client", p.Name, "scope", p.Scope, "session", p.Session, "redirect_host", hostOf(p.RedirectURI),
		"own_idp_tokens", up.Refresh != "")
	s.redirectTo(w, r, p.RedirectURI, url.Values{"code": {sealed}, "state": {p.State}})
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}
