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
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/sharedstate"
)

// pkceVerifier is RFC 7636 §4.1: 43 to 128 unreserved characters.
var pkceVerifier = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

// tokenResponse is RFC 6749 §5.1.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// tokenFailure is a refused token request: its status, body, and the
// audit outcome.
type tokenFailure struct {
	status  int
	code    string
	desc    string
	outcome string
}

func (f *tokenFailure) Error() string { return f.code + ": " + f.desc }

func invalidGrant(desc string) *tokenFailure {
	return &tokenFailure{http.StatusBadRequest, "invalid_grant", desc, "refused"}
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	client := auth.ClientOf(r, s.o.TrustedProxy)
	// Per client address when it is a client's; behind a proxy the
	// console does not trust every client has that address, and only the
	// per-replica buckets apply (a client_id is public, so a budget per
	// client would let anyone spend another's). Those are charged once
	// the code or refresh token has been opened (redeemCode,
	// redeemRefresh): a token this console sealed that passes the checks
	// that cost nothing spends its grant's bucket and tokenRequests;
	// anything else (junk, or a sealed token expired, revoked or replayed)
	// unknownTokenRequests, so neither junk nor a token once held refuses
	// a known client's refresh.
	if key, perAddress := auth.BudgetKey(r, s.o.TrustedProxy); perAddress && !s.requests.Allow(key) {
		requests.WithLabelValues("token", "throttled").Inc()
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusTooManyRequests, "slow_down", "too many token requests from this address")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "the body is not a form")
		return
	}
	f := r.PostForm
	for k, v := range f {
		if len(v) > 1 {
			writeError(w, http.StatusBadRequest, "invalid_request", "the parameter "+k+" is repeated")
			return
		}
	}
	// Public clients only: a client that authenticates is not one this
	// console registered (RFC 6749 §2.3 and §3.2.1).
	if _, _, basic := r.BasicAuth(); basic || f.Get("client_secret") != "" || f.Get("client_assertion") != "" {
		requests.WithLabelValues("token", "refused").Inc()
		writeError(w, http.StatusUnauthorized, "invalid_client", "this console admits public clients only (token_endpoint_auth_method none)")
		return
	}
	grantType := f.Get("grant_type")
	var (
		resp    tokenResponse
		failure *tokenFailure
		base    grantBase
	)
	switch grantType {
	case "authorization_code":
		resp, base, failure = s.redeemCode(r)
	case "refresh_token":
		resp, base, failure = s.redeemRefresh(r)
	default:
		writeError(w, http.StatusBadRequest, "unsupported_grant_type", "authorization_code and refresh_token only")
		return
	}
	attrs := []any{"grant_type", grantType}
	if base.ID != "" {
		attrs = append(attrs, "grant", base.ID, "user", base.Subject, "client", base.Name)
	}
	if failure != nil && failure.outcome == "throttled" {
		// Not audited: a flood would otherwise write a line per request.
		requests.WithLabelValues("token", failure.outcome).Inc()
		w.Header().Set("Retry-After", "5")
		writeError(w, failure.status, failure.code, failure.desc)
		return
	}
	if failure != nil {
		requests.WithLabelValues("token", failure.outcome).Inc()
		s.audit.Record(r.Context(), client, auth.AuditMCPToken, append(attrs, "outcome", failure.outcome, "reason", failure.desc)...)
		writeError(w, failure.status, failure.code, failure.desc)
		return
	}
	requests.WithLabelValues("token", "ok").Inc()
	s.audit.Record(r.Context(), client, auth.AuditMCPToken, append(attrs, "outcome", "issued", "scope", resp.Scope, "expires_in", resp.ExpiresIn)...)
	writeJSON(w, http.StatusOK, resp)
}

// redeemCode is the authorization_code grant: the code opens, is unexpired
// and unused, was issued to this client for this redirect URI and
// resource, and the verifier matches its S256 challenge.
func (s *Server) redeemCode(r *http.Request) (tokenResponse, grantBase, *tokenFailure) {
	f := r.PostForm
	var c authorizationCode
	if !s.open(codePrefix, purposeCode, f.Get("code"), &c) {
		return tokenResponse{}, grantBase{}, s.unknownToken(invalidGrant("the code is not one this console issued"))
	}
	// The checks that cost nothing come first, and a code they refuse
	// spends the bucket of unknown tokens: a code once held (a revoked
	// person's, one copied from a log) replayed in a loop never spends the
	// bucket connected clients refresh from.
	now := s.o.Now().UTC()
	var refusal *tokenFailure
	switch {
	case now.Unix() >= c.Expiry:
		refusal = invalidGrant("the code expired")
	case f.Get("client_id") != c.Client:
		refusal = invalidGrant("the code was issued to another client")
	case f.Get("redirect_uri") != c.RedirectURI:
		refusal = invalidGrant("redirect_uri differs from the authorization request's")
	case f.Get("resource") != "" && !sameResource(f.Get("resource"), c.Resource):
		refusal = &tokenFailure{http.StatusBadRequest, "invalid_target", "the code was issued for " + c.Resource, "refused"}
	case !verifyPKCE(f.Get("code_verifier"), c.Challenge):
		refusal = invalidGrant("the PKCE code_verifier does not match the challenge")
	case s.o.Sessions.Revoked(c.Subject, time.Unix(c.IssuedAt, 0), c.ID, c.Session):
		refusal = &tokenFailure{http.StatusBadRequest, "invalid_grant", "the sign-in this code came from is revoked", "revoked"}
	case s.o.Sessions.SignedOut(c.Session):
		// The approval went with the session, as at consent (completeGrant):
		// a code issued just before its session signed out buys no grant
		// (round 22, security).
		refusal = &tokenFailure{http.StatusBadRequest, "invalid_grant", "the console session this code was approved in signed out", "revoked"}
	case s.o.Store.GrantRevoked(c.ID):
		refusal = &tokenFailure{http.StatusBadRequest, "invalid_grant", "the code was already used; the grant it bought is revoked", "replayed"}
	}
	if refusal != nil {
		return tokenResponse{}, c.grantBase, s.unknownToken(refusal)
	}
	if failure := s.chargeToken(c.ID, c.Subject); failure != nil {
		return tokenResponse{}, c.grantBase, failure
	}
	if !s.o.Store.GrantsUsable() {
		return tokenResponse{}, c.grantBase, unavailable()
	}
	key := s.userKey(c.Subject)
	switch err := s.o.Store.UseCode(c.Nonce, key, time.Unix(c.Expiry, 0).Add(clockSkew)); {
	case errors.Is(err, sharedstate.ErrSpent):
		// A code redeemed twice: whatever it bought is revoked too (RFC
		// 6749 §4.1.2), on every replica: one that has not recorded the
		// grant yet (a concurrent first redemption elsewhere) gets a
		// revoked placeholder its record merges into, which the fair
		// share does not refuse (sharedstate.Store.RevokeGrant).
		s.o.Store.RevokeGrant(c.ID, key, now, now.Add(sharedstate.RevokedKeep))
		return tokenResponse{}, c.grantBase, &tokenFailure{http.StatusBadRequest, "invalid_grant", "the code was already used; the grant it bought is revoked", "replayed"}
	case err != nil:
		// The table of spent codes is full: nothing was spent, nothing is
		// revoked; the client may retry the same code within its minute.
		return tokenResponse{}, c.grantBase, &tokenFailure{http.StatusServiceUnavailable, "temporarily_unavailable", "too many sign-ins in progress; retry shortly", "busy"}
	}
	up, failure := s.freshUpstream(r.Context(), c.grantBase, c.Upstream, now)
	if failure != nil {
		return tokenResponse{}, c.grantBase, failure
	}
	expiry := now.Add(s.o.Sessions.Limits().MaxAge)
	err := s.o.Store.AddGrant(sharedstate.Grant{
		ID: c.ID, UserHash: key, Client: c.Name, Scope: c.Scope,
		Created: time.Unix(c.IssuedAt, 0), LastUsed: now, Expiry: expiry,
	})
	if errors.Is(err, sharedstate.ErrTooManyGrants) {
		return tokenResponse{}, c.grantBase, invalidGrant(err.Error())
	}
	if err != nil {
		return tokenResponse{}, c.grantBase, &tokenFailure{http.StatusInternalServerError, "server_error", "the grant could not be recorded", "error"}
	}
	// Written through before the first token (round 22, Go P2-1,
	// security): every grant an assistant holds is then in the shared
	// state, so a disconnection on any replica, which reads it, revokes
	// it, even when this replica is cut off from the API server since.
	// One that cannot be written is revoked here and buys no token; the
	// client signs in again.
	if err := s.o.Store.WriteGrant(r.Context(), c.ID); err != nil {
		s.o.Store.RevokeGrant(c.ID, key, now, expiry)
		return tokenResponse{}, c.grantBase, &tokenFailure{
			http.StatusServiceUnavailable, "temporarily_unavailable",
			"the grant could not be recorded for every console replica; start the connection again", "unrecorded",
		}
	}
	if s.o.Store.GrantRevoked(c.ID) {
		// A replay elsewhere revoked it while this redemption ran.
		return tokenResponse{}, c.grantBase, &tokenFailure{http.StatusBadRequest, "invalid_grant", "the code was already used; the grant it bought is revoked", "replayed"}
	}
	resp, failure := s.issue(c.grantBase, up, expiry.Unix(), 0, now)
	return resp, c.grantBase, failure
}

// chargeToken spends one request of grant's own bucket, then one of its
// person's, then one of the bucket of tokens this console sealed; nil
// when there were all three. The grant ID and the subject are sealed in
// the code or refresh token, so no sender spends another grant's or
// another person's bucket; the per-grant bucket keeps one live grant (a
// holder replaying its own fresh tokens), and the per-person bucket one
// person's grants, from emptying the shared one.
func (s *Server) chargeToken(grant, subject string) *tokenFailure {
	if !s.grantTokenRequests.Allow(grant) {
		return throttled("this grant is making too many token requests; retry in a few seconds")
	}
	if !s.subjectTokenRequests.Allow(subject) {
		return throttled("your grants are making too many token requests; retry in a few seconds")
	}
	if s.tokenRequests.Allow(processKey) {
		return nil
	}
	return throttled("the console is receiving too many token requests; retry in a few seconds")
}

// unknownToken spends one request of the bucket of codes and refresh
// tokens that do not open, and returns refusal, or a throttled answer
// (not audited) when that bucket is empty.
func (s *Server) unknownToken(refusal *tokenFailure) *tokenFailure {
	if s.unknownTokenRequests.Allow(processKey) {
		return refusal
	}
	return throttled("the console is receiving too many invalid token requests; retry in a few seconds")
}

func throttled(desc string) *tokenFailure {
	return &tokenFailure{http.StatusTooManyRequests, "slow_down", desc, "throttled"}
}

// unavailable: the grant index cannot be trusted right now.
func unavailable() *tokenFailure {
	return &tokenFailure{http.StatusServiceUnavailable, "temporarily_unavailable", errIndexUnavailable.Error(), "error"}
}

// redeemRefresh is the refresh_token grant: the token opens, is
// unexpired, of this client, of a grant the index holds (or one rotated
// within the replication grace) and has not revoked, not behind the
// grant's generation (a second use revokes the grant), and its IdP token
// is renewed when it is about to expire.
func (s *Server) redeemRefresh(r *http.Request) (tokenResponse, grantBase, *tokenFailure) {
	f := r.PostForm
	var rt refreshToken
	if !s.open(refreshPrefix, purposeRefresh, f.Get("refresh_token"), &rt) || rt.ID == "" {
		return tokenResponse{}, grantBase{}, s.unknownToken(invalidGrant("the refresh token is not one this console issued"))
	}
	// As for a code: a refresh token the cheap checks refuse (expired,
	// another client's, of a revoked or unknown grant) spends the bucket
	// of unknown tokens, so a revoked person replaying an old refresh
	// token never refuses a connected client's refresh.
	now := s.o.Now().UTC()
	issued := time.Unix(rt.IssuedAt, 0).UTC()
	key := s.userKey(rt.Subject)
	var refusal *tokenFailure
	switch {
	case now.Unix() >= rt.Expiry:
		refusal = invalidGrant("the grant expired; connect the application again")
	case f.Get("client_id") != rt.Client:
		refusal = invalidGrant("the refresh token was issued to another client")
	case !s.o.Store.GrantsUsable():
		// The index cannot be trusted (an outage of the state ConfigMap):
		// nothing can tell a live grant from a revoked one, so the answer
		// spends the bucket of unknown tokens like any refusal, and an
		// outage replayed in a loop writes a bounded number of audit lines.
		refusal = unavailable()
	case s.o.Store.GrantRevoked(rt.ID) || s.o.Sessions.Revoked(rt.Subject, issued, rt.ID, rt.Session):
		refusal = &tokenFailure{http.StatusBadRequest, "invalid_grant", "the grant is revoked", "revoked"}
	default:
		g, known := s.o.Store.Grant(rt.ID)
		switch {
		case !known && !s.withinGrace(rt.TokenIssued, now):
			refusal = &tokenFailure{http.StatusBadRequest, "invalid_grant", "the grant is not known to this console (revoked, or lost); connect the application again", "revoked"}
		case known && g.Generation > rt.Generation:
			// A refresh token behind its grant's generation was rotated
			// already: a second use (OAuth 2.1 §4.3.1). Refused here, before
			// the grant's buckets, the IdP and the API server are spent; the
			// grant ends for both holders, as AdvanceGrant would end it.
			s.o.Store.RevokeGrant(rt.ID, key, now, time.Unix(rt.Expiry, 0))
			refusal = &tokenFailure{http.StatusBadRequest, "invalid_grant", "the refresh token was already used; the grant is revoked", "replayed"}
		}
	}
	if refusal != nil {
		return tokenResponse{}, rt.grantBase, s.unknownToken(refusal)
	}
	if failure := s.chargeToken(rt.ID, rt.Subject); failure != nil {
		return tokenResponse{}, rt.grantBase, failure
	}
	up, failure := s.freshUpstream(r.Context(), rt.grantBase, rt.Upstream, now)
	if failure != nil {
		return tokenResponse{}, rt.grantBase, failure
	}
	switch s.o.Store.AdvanceGrant(rt.ID, rt.Generation, now) {
	case sharedstate.Advanced:
	case sharedstate.Replayed:
		// A refresh token used twice: one of the two holders is not the
		// client (OAuth 2.1 §4.3.1). The grant ends for both.
		s.o.Store.RevokeGrant(rt.ID, key, now, time.Unix(rt.Expiry, 0))
		return tokenResponse{}, rt.grantBase, &tokenFailure{http.StatusBadRequest, "invalid_grant", "the refresh token was already used; the grant is revoked", "replayed"}
	case sharedstate.Revoked:
		return tokenResponse{}, rt.grantBase, &tokenFailure{http.StatusBadRequest, "invalid_grant", "the grant is revoked", "revoked"}
	case sharedstate.Unknown:
		// Rotated on another replica moments ago: record it here, at the
		// generation this refresh moves it to. Not new work (the replica
		// that issued the token admitted it): the fair share does not
		// apply, so a legitimate holder is not disconnected by it.
		err := s.o.Store.AddGrant(sharedstate.Grant{
			ID: rt.ID, UserHash: key, Client: rt.Name, Scope: rt.Scope, Created: time.Unix(rt.IssuedAt, 0),
			LastUsed: now, Expiry: time.Unix(rt.Expiry, 0), Generation: rt.Generation + 1, Rotated: true,
		})
		if err != nil {
			return tokenResponse{}, rt.grantBase, invalidGrant("the grant could not be recorded: " + err.Error())
		}
	}
	resp, failure := s.issue(rt.grantBase, up, rt.Expiry, rt.Generation+1, now)
	return resp, rt.grantBase, failure
}

// freshUpstream checks the IdP token with the API server, renewing it
// first when it expires within upstreamLead, and refuses a grant whose
// person the API server no longer accepts or names differently.
func (s *Server) freshUpstream(ctx context.Context, g grantBase, up upstream, now time.Time) (upstream, *tokenFailure) {
	if up.Expiry > 0 && now.Add(upstreamLead).Unix() >= up.Expiry {
		renewed, err := s.o.Sessions.RefreshUpstream(ctx, up.Provider, g.ID, up.Refresh)
		switch {
		case err == nil:
			up.Token, up.Refresh, up.Expiry = renewed.Token, renewed.RefreshToken, 0
			if !renewed.Expiry.IsZero() {
				up.Expiry = renewed.Expiry.Unix()
			}
		case now.Unix() >= up.Expiry:
			return up, invalidGrant("your identity provider's sign-in ended and could not be renewed; connect the application again")
		default:
			s.o.Log.Warn("renewing a grant's IdP token failed; it is used until it expires", "grant", auth.LogSafe(g.ID), "error", err)
		}
	}
	u, err := s.o.Sessions.Review(ctx, up.Token)
	switch {
	case errors.Is(err, auth.ErrTokenRejected):
		return up, invalidGrant("the Kubernetes API server no longer accepts your sign-in; connect the application again")
	case err != nil:
		// The review's error names the API server's failure, never the token.
		s.o.Log.Warn("the API server did not answer a grant's identity review", "grant", auth.LogSafe(g.ID), "error", err)
		return up, &tokenFailure{http.StatusServiceUnavailable, "temporarily_unavailable", "the Kubernetes API server could not be reached; retry", "error"}
	case u.Name != g.Subject:
		return up, invalidGrant("the identity provider's token names another person")
	}
	return up, nil
}

// issue seals a new access token and refresh token of a grant.
func (s *Server) issue(g grantBase, up upstream, grantExpiry int64, generation int, now time.Time) (tokenResponse, *tokenFailure) {
	exp := now.Add(s.o.AccessTTL).Unix()
	if up.Expiry > 0 && up.Expiry < exp {
		// An access token never outlives the IdP token it carries: the
		// client refreshes, and the refresh renews the IdP token.
		exp = up.Expiry
	}
	exp = min(exp, grantExpiry)
	access, err := s.seal(accessPrefix, purposeAccess, accessToken{
		grantBase: g, Upstream: upstream{Provider: up.Provider, Token: up.Token, Expiry: up.Expiry}, Expiry: exp, TokenIssued: now.Unix(),
	})
	if err != nil {
		return tokenResponse{}, &tokenFailure{http.StatusInternalServerError, "server_error", "the token could not be sealed", "error"}
	}
	refresh, err := s.seal(refreshPrefix, purposeRefresh, refreshToken{
		grantBase: g, Upstream: up, Expiry: grantExpiry, Generation: generation, TokenIssued: now.Unix(),
	})
	if err != nil {
		return tokenResponse{}, &tokenFailure{http.StatusInternalServerError, "server_error", "the token could not be sealed", "error"}
	}
	return tokenResponse{
		AccessToken: access, TokenType: "Bearer", ExpiresIn: max(exp-now.Unix(), 1),
		RefreshToken: refresh, Scope: g.Scope,
	}, nil
}

// verifyPKCE checks an S256 verifier against its challenge.
func verifyPKCE(verifier, challenge string) bool {
	if !pkceVerifier.MatchString(verifier) {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
}
