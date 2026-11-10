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
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Errors of BearerPrincipal. None carries the token.
var (
	// ErrTokenLoginDisabled: raw API-server tokens are accepted only where
	// the break-glass token sign-in is on (AUTH_TOKEN_LOGIN).
	ErrTokenLoginDisabled = errors.New("raw API-server tokens are not accepted: token sign-in is off on this console")
	// ErrBearerRevoked: the revocation list ends this token's use.
	ErrBearerRevoked = errors.New("this token's sessions are revoked")
	// ErrMalformedBearer: the value cannot be a bearer token.
	ErrMalformedBearer = errors.New("the bearer value is not a token")
	// ErrBearerExpired is a JWT whose exp has passed: refused even while
	// the API server's answer about it is still cached.
	ErrBearerExpired = errors.New("the token has expired")
	// ErrReviewsBusy: the process-wide budget of token reviews, which the
	// token form and raw bearers share, is spent; the caller answers 429.
	ErrReviewsBusy = errors.New("the console is reviewing too many tokens; retry in a few seconds")
)

// BearerProvider is the Principal.Provider of a raw bearer token.
const BearerProvider = ProviderToken

// bearerReferencePrefix marks the session reference of a raw bearer
// token in the audit log and the revocation list's sessions key.
const bearerReferencePrefix = "bearer-"

// AttemptLimiter is the per-address budget of the sign-in form, which
// the MCP endpoint spends on bearers it has not accepted recently: one
// guessing budget per address, whichever door is knocked on.
func (h *Handler) AttemptLimiter() *RateLimiter { return h.attempts }

// TokenLoginEnabled reports whether the break-glass token sign-in is on,
// the switch that also admits raw API-server tokens as bearers.
func (h *Handler) TokenLoginEnabled() bool { return h.token }

// BearerPrincipal checks a raw API-server token presented as a bearer
// (the MCP endpoint) the way the token sign-in checks a
// pasted one: only while token sign-in is on, the API server says whose
// token it is (SelfSubjectReview, positive answers cached 30 s), and the
// revocation list applies. No session is created and no cookie is read
// or set.
//
// The principal's SessionID is a reference derived from a digest of the
// token: it names the token in the audit log and can be listed under the
// revocation ConfigMap's sessions key, and it is not a credential. Its
// IssuedAt is the token's iat claim when the token is a JWT (which the API
// server has just verified); for an opaque token it is unknown, so a
// users or notBefore revocation that names its user refuses it until the
// entry is removed: the conservative reading.
func (h *Handler) BearerPrincipal(ctx context.Context, token string) (*Principal, error) {
	if !h.token {
		return nil, ErrTokenLoginDisabled
	}
	tok, ok := normalizePastedToken(token)
	if !ok {
		return nil, ErrMalformedBearer
	}
	// A token the API server has to be asked about spends one review: of
	// knownReviews when this replica accepted it before (an MCP client's
	// token once its cached answer ends), which no junk reaches; otherwise
	// of the per-replica bucket the token form also spends (the caller has
	// already charged its sender's address or the token's own budget). A
	// flood of distinct junk bearers therefore cannot fail a known
	// client's re-review. A token whose answer is cached spends nothing.
	//
	// A known token spends its person's share of knownReviews first (the
	// name the API server gave when it accepted the token), so several
	// identities each minting maxKnownBearersPerSubject tokens cannot keep
	// knownReviews empty for everyone else.
	if !h.reviewCached(tok) {
		bucket, key := h.tokenReviews, tokenReviewsKey
		if subject, known := h.knownBearers.subjectOf(tok, h.now()); known {
			if !h.knownSubjectReviews.Allow(subject) {
				return nil, ErrReviewsBusy
			}
			bucket, key = h.knownReviews, knownReviewsKey
		}
		if !bucket.Allow(key) {
			return nil, ErrReviewsBusy
		}
	}
	u, err := h.identity.Review(ctx, tok)
	if errors.Is(err, ErrTokenRejected) {
		h.knownBearers.forget(tok)
	}
	if err != nil {
		return nil, err
	}
	ref := BearerReference(tok)
	issued, expiry := bearerTimes(tok)
	if !expiry.IsZero() && !h.now().Before(expiry) {
		return nil, ErrBearerExpired
	}
	if h.isLoggedOut(ref) || h.revocations.Revoked(ref, u.Name, issued) {
		return nil, ErrBearerRevoked
	}
	now := h.now()
	p := &Principal{
		User:        u,
		Provider:    BearerProvider,
		SessionID:   ref,
		Token:       tok,
		TokenExpiry: expiry,
		IssuedAt:    issued,
		// A bearer is checked on every request; Expiry only bounds what
		// a request may start, so it is the token's own or the identity
		// cache's horizon.
		Expiry:         now.Add(DefaultIdentityTTL),
		AbsoluteExpiry: now.Add(DefaultIdentityTTL),
	}
	if !expiry.IsZero() {
		p.Expiry, p.AbsoluteExpiry = expiry, expiry
	}
	until := now.Add(knownBearerTTL)
	if !expiry.IsZero() && expiry.Before(until) {
		until = expiry
	}
	h.knownBearers.remember(tok, u.Name, until, now)
	return p, nil
}

// reviewCached reports whether the identity of tok is cached, so that
// reviewing it again sends nothing to the API server.
func (h *Handler) reviewCached(tok string) bool {
	c, ok := h.identity.(*CachedReviewer)
	return ok && c.Cached(tok)
}

// BearerReference is the audit reference of a raw bearer token: a short
// digest, enough to tell two tokens apart in the log and to revoke one by
// name, and useless to present.
func BearerReference(token string) string {
	sum := sha256.Sum256([]byte(token))
	return bearerReferencePrefix + hex.EncodeToString(sum[:8])
}

// bearerTimes reads iat and exp from a JWT's claims; zero when the token
// is not a JWT or carries neither. The signature is the API server's
// business: this runs after the API server accepted the token.
func bearerTimes(tok string) (issued, expiry time.Time) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return time.Time{}, time.Time{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(raw) > maxPastedToken {
		return time.Time{}, time.Time{}
	}
	var c struct {
		Iat json.Number `json:"iat"`
		Exp json.Number `json:"exp"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return time.Time{}, time.Time{}
	}
	if n, err := c.Iat.Int64(); err == nil && n > 0 {
		issued = time.Unix(n, 0).UTC()
	}
	if n, err := c.Exp.Int64(); err == nil && n > 0 {
		expiry = time.Unix(n, 0).UTC()
	}
	return issued, expiry
}
