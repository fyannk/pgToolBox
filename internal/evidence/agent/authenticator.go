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

package agent

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fyannk/pgToolBox/internal/evidence/evidenceapi"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

const (
	// positiveCacheTTL bounds how long an allowed decision is reused, the
	// same 10 s as the console's SubjectAccessReview cache. There is
	// no negative authorization cache: a revoked grant takes effect within
	// the TTL, a new grant immediately.
	positiveCacheTTL = 10 * time.Second
	// maximumCacheEntries bounds memory however many distinct tokens arrive.
	maximumCacheEntries = 1024
	// rejectedCacheTTL: a token the API server did not authenticate for the
	// agent's audience never becomes valid, so remembering the rejection is
	// safe and stops one bogus token from costing a TokenReview per request.
	// Authorization (SAR) denials are never cached.
	rejectedCacheTTL = time.Minute
)

// AuthenticatorOptions names the EvidenceAgent whose "get" authorizes a
// caller. Its namespace also fixes the only audience accepted:
// evidencev1alpha1.TokenAudience(Namespace).
type AuthenticatorOptions struct {
	Reviewer  Reviewer
	Name      string
	Namespace string
	Logger    *slog.Logger
	Now       func() time.Time
}

// Authenticator is the agent's TokenReview (for its own namespace's
// audience) + SubjectAccessReview ("get" on its EvidenceAgent) decision,
// with a bounded positive cache keyed by the token's SHA-256.
type Authenticator struct {
	reviewer   Reviewer
	audience   string
	namespace  string
	attributes authorizationv1.ResourceAttributes
	logger     *slog.Logger
	now        func() time.Time

	mu       sync.Mutex
	allowed  map[[sha256.Size]byte]cachedDecision
	rejected map[[sha256.Size]byte]cachedRejection
}

type cachedRejection struct {
	outcome evidenceapi.AuthOutcome
	expires time.Time
}

type cachedDecision struct {
	user    string
	expires time.Time
}

// NewAuthenticator validates the options.
func NewAuthenticator(options AuthenticatorOptions) (*Authenticator, error) {
	if options.Reviewer == nil || options.Name == "" || options.Namespace == "" {
		return nil, errors.New("agent authenticator configuration is invalid")
	}
	audience, err := evidencev1alpha1.TokenAudience(options.Namespace)
	if err != nil {
		return nil, errors.New("agent authenticator configuration is invalid: the namespace is not a DNS-1123 label")
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Authenticator{
		reviewer:  options.Reviewer,
		audience:  audience,
		namespace: options.Namespace,
		attributes: authorizationv1.ResourceAttributes{
			Namespace: options.Namespace, Name: options.Name,
			Verb: evidencev1alpha1.AuthorizationVerb, Group: evidencev1alpha1.AuthorizationGroup,
			Resource: evidencev1alpha1.AuthorizationResource,
		},
		logger:   logger,
		now:      now,
		allowed:  make(map[[sha256.Size]byte]cachedDecision),
		rejected: make(map[[sha256.Size]byte]cachedRejection),
	}, nil
}

// Audience is the only token audience this authenticator accepts.
func (a *Authenticator) Audience() string { return a.audience }

// Authenticate implements evidenceapi.Authenticator.
func (a *Authenticator) Authenticate(ctx context.Context, token evidencev1alpha1.BearerToken) evidenceapi.AuthDecision {
	if !token.Valid() {
		return evidenceapi.AuthDecision{Outcome: evidenceapi.AuthUnauthenticated}
	}
	key := sha256.Sum256([]byte(token.Value()))
	if user, ok := a.cached(key); ok {
		return evidenceapi.AuthDecision{Outcome: evidenceapi.AuthAllowed, User: user}
	}
	if outcome, ok := a.knownRejected(key); ok {
		return evidenceapi.AuthDecision{Outcome: outcome}
	}

	user, audiences, authenticated, err := a.reviewer.ReviewToken(ctx, token.Value(), []string{a.audience})
	if err != nil {
		a.logger.WarnContext(ctx, "token review failed", slog.String("cause", err.Error()))
		return evidenceapi.AuthDecision{Outcome: evidenceapi.AuthUnavailable}
	}
	// An audience-aware API server returns the intersection of the
	// requested and the token's audiences; an empty answer means it did not
	// check the audience, which this agent must not accept.
	if !authenticated || !slices.Contains(audiences, a.audience) || user.Username == "" {
		outcome := a.refusal(token)
		a.reject(key, outcome)
		return evidenceapi.AuthDecision{Outcome: outcome}
	}

	allowed, err := a.reviewer.Allowed(ctx, user, a.attributes)
	if err != nil {
		a.logger.WarnContext(ctx, "subject access review failed", slog.String("cause", err.Error()))
		return evidenceapi.AuthDecision{Outcome: evidenceapi.AuthUnavailable, User: user.Username}
	}
	if !allowed {
		return evidenceapi.AuthDecision{Outcome: evidenceapi.AuthForbidden, User: user.Username}
	}
	a.remember(key, user.Username)
	return evidenceapi.AuthDecision{Outcome: evidenceapi.AuthAllowed, User: user.Username}
}

func (a *Authenticator) cached(key [sha256.Size]byte) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.allowed[key]
	if !ok {
		return "", false
	}
	if !a.now().Before(entry.expires) {
		delete(a.allowed, key)
		return "", false
	}
	return entry.user, true
}

func (a *Authenticator) remember(key [sha256.Size]byte, user string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if len(a.allowed) >= maximumCacheEntries {
		for candidate, entry := range a.allowed {
			if !now.Before(entry.expires) {
				delete(a.allowed, candidate)
			}
		}
		if len(a.allowed) >= maximumCacheEntries {
			clear(a.allowed)
		}
	}
	a.allowed[key] = cachedDecision{user: user, expires: now.Add(positiveCacheTTL)}
}

// refusal names a refused token's outcome. The token's unverified "aud"
// claim only chooses between two refusals: a token minted for another
// namespace's agents, or for the bare cluster-wide audience, is
// AuthAudienceMismatch so an honest console can say which token it should
// present; anything else is AuthUnauthenticated. A forged claim gains
// nothing: both outcomes refuse the request, and neither reveals anything
// the caller did not write itself.
func (a *Authenticator) refusal(token evidencev1alpha1.BearerToken) evidenceapi.AuthOutcome {
	switch evidencev1alpha1.ClassifyAudienceClaim(token, a.namespace) {
	case evidencev1alpha1.AudienceOtherNamespace, evidencev1alpha1.AudienceClusterWide:
		return evidenceapi.AuthAudienceMismatch
	default:
		return evidenceapi.AuthUnauthenticated
	}
}

func (a *Authenticator) knownRejected(key [sha256.Size]byte) (evidenceapi.AuthOutcome, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.rejected[key]
	if !ok {
		return 0, false
	}
	if !a.now().Before(entry.expires) {
		delete(a.rejected, key)
		return 0, false
	}
	return entry.outcome, true
}

func (a *Authenticator) reject(key [sha256.Size]byte, outcome evidenceapi.AuthOutcome) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if len(a.rejected) >= maximumCacheEntries {
		for candidate, entry := range a.rejected {
			if !now.Before(entry.expires) {
				delete(a.rejected, candidate)
			}
		}
		if len(a.rejected) >= maximumCacheEntries {
			clear(a.rejected)
		}
	}
	a.rejected[key] = cachedRejection{outcome: outcome, expires: now.Add(rejectedCacheTTL)}
}

func typeMeta(apiVersion, kind string) metav1.TypeMeta {
	return metav1.TypeMeta{APIVersion: apiVersion, Kind: kind}
}
