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
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"

	"github.com/fyannk/pgToolBox/internal/evidence/evidenceapi"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

const (
	consoleUser   = "system:serviceaccount:team-a:pgtoolbox-console"
	consoleToken  = "eyJhbGciOiJSUzI1NiJ9.Y29uc29sZQ.c2ln"
	apiToken      = "eyJhbGciOiJSUzI1NiJ9.YXBpc2VydmVy.c2ln"
	strangerToken = "eyJhbGciOiJSUzI1NiJ9.c3RyYW5nZXI.c2ln"
	// teamAAudience is the only audience the test agent (team-a/orders)
	// accepts.
	teamAAudience = "pgtoolbox-evidence-agent/team-a"
	// otherNamespaceToken carries the claim {"aud":["pgtoolbox-evidence-agent/team-b"]}:
	// the console's token for the agents of team-b.
	otherNamespaceToken = "eyJhbGciOiJSUzI1NiJ9.eyJhdWQiOiBbInBndG9vbGJveC1ldmlkZW5jZS1hZ2VudC90ZWFtLWIiXX0.c2ln"
	// clusterWideToken carries the claim {"aud":"pgtoolbox-evidence-agent"}: the
	// bare cluster-wide audience no agent accepts.
	clusterWideToken = "eyJhbGciOiJSUzI1NiJ9.eyJhdWQiOiAicGd0b29sYm94LWV2aWRlbmNlLWFnZW50In0.c2ln"
)

var agentAttributes = authorizationv1.ResourceAttributes{
	Namespace: "team-a", Name: "orders", Verb: "get", Group: "pgtoolbox.fyannk.dev", Resource: "evidenceagents",
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func mustToken(t *testing.T, value string) evidencev1alpha1.BearerToken {
	t.Helper()
	token, err := evidencev1alpha1.ParseBearerToken([]byte(value))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func newTestAuthenticator(t *testing.T, reviewer Reviewer, now *clock) *Authenticator {
	t.Helper()
	authenticator, err := NewAuthenticator(AuthenticatorOptions{Reviewer: reviewer, Name: "orders", Namespace: "team-a", Now: now.Now})
	if err != nil {
		t.Fatal(err)
	}
	return authenticator
}

func TestAuthenticatorAgainstFakeAPIServer(t *testing.T) {
	fake := newFakeAPIServer(t)
	fake.addToken(consoleToken, consoleUser, teamAAudience)
	// A default ServiceAccount token: valid for the API server, not for us.
	fake.addToken(apiToken, consoleUser, "https://kubernetes.default.svc")
	fake.addToken(strangerToken, "system:serviceaccount:team-b:pgtoolbox-console", teamAAudience)
	fake.addToken(otherNamespaceToken, consoleUser, "pgtoolbox-evidence-agent/team-b")
	fake.addToken(clusterWideToken, consoleUser, "pgtoolbox-evidence-agent")
	fake.grant(consoleUser, agentAttributes)
	fake.grant("system:serviceaccount:team-b:pgtoolbox-console", authorizationv1.ResourceAttributes{
		Namespace: "team-b", Name: "orders", Verb: "get", Group: "pgtoolbox.fyannk.dev", Resource: "evidenceagents",
	})
	now := &clock{now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	authenticator := newTestAuthenticator(t, fake.reviewer(t), now)
	ctx := context.Background()

	tests := []struct {
		name    string
		token   string
		outcome evidenceapi.AuthOutcome
	}{
		{"granted console with the agent audience", consoleToken, evidenceapi.AuthAllowed},
		{"API-server audience is refused", apiToken, evidenceapi.AuthUnauthenticated},
		{"unknown token", "eyJ.dW5rbm93bg.c2ln", evidenceapi.AuthUnauthenticated},
		{"valid token, grant on another namespace's agent", strangerToken, evidenceapi.AuthForbidden},
		// The console's own, valid token for team-b, held by a subject that
		// may get this agent: refused all the same, so a token for one
		// namespace's agents opens no other namespace's.
		{"another namespace's audience is refused", otherNamespaceToken, evidenceapi.AuthAudienceMismatch},
		{"the bare cluster-wide audience is refused", clusterWideToken, evidenceapi.AuthAudienceMismatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := authenticator.Authenticate(ctx, mustToken(t, test.token))
			if decision.Outcome != test.outcome {
				t.Fatalf("outcome = %v, want %v", decision.Outcome, test.outcome)
			}
		})
	}

	fake.mu.Lock()
	for _, review := range fake.reviews {
		if len(review.Spec.Audiences) != 1 || review.Spec.Audiences[0] != teamAAudience {
			t.Errorf("TokenReview audiences = %v", review.Spec.Audiences)
		}
	}
	for _, access := range fake.accesses {
		if access.Spec.ResourceAttributes.Name != "orders" || access.Spec.ResourceAttributes.Namespace != "team-a" ||
			access.Spec.ResourceAttributes.Verb != "get" || access.Spec.ResourceAttributes.Resource != "evidenceagents" ||
			access.Spec.UID == "" || len(access.Spec.Groups) == 0 {
			t.Errorf("SubjectAccessReview spec = %+v", access.Spec)
		}
	}
	fake.mu.Unlock()
	if reviews, accesses := fake.counts(); reviews != 6 || accesses != 2 {
		t.Fatalf("reviews = %d, accesses = %d: a refused token must not reach the SAR", reviews, accesses)
	}
}

func TestAuthenticatorCachesOnlyPositiveDecisionsForTenSeconds(t *testing.T) {
	fake := newFakeAPIServer(t)
	fake.addToken(consoleToken, consoleUser, teamAAudience)
	now := &clock{now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	authenticator := newTestAuthenticator(t, fake.reviewer(t), now)
	ctx := context.Background()
	token := mustToken(t, consoleToken)

	// Denied, then granted: no negative cache, the grant applies at once.
	if decision := authenticator.Authenticate(ctx, token); decision.Outcome != evidenceapi.AuthForbidden {
		t.Fatalf("before grant: %v", decision.Outcome)
	}
	fake.grant(consoleUser, agentAttributes)
	if decision := authenticator.Authenticate(ctx, token); decision.Outcome != evidenceapi.AuthAllowed || decision.User != consoleUser {
		t.Fatalf("after grant: %+v", decision)
	}
	reviews, _ := fake.counts()

	// Within the TTL the decision is reused; the API server is not asked.
	now.Advance(9 * time.Second)
	fake.revoke(consoleUser)
	if decision := authenticator.Authenticate(ctx, token); decision.Outcome != evidenceapi.AuthAllowed {
		t.Fatalf("cached: %v", decision.Outcome)
	}
	if again, _ := fake.counts(); again != reviews {
		t.Fatal("a cached decision spent a review")
	}
	// Past the TTL the revocation takes effect.
	now.Advance(2 * time.Second)
	if decision := authenticator.Authenticate(ctx, token); decision.Outcome != evidenceapi.AuthForbidden {
		t.Fatalf("after TTL: %v", decision.Outcome)
	}
}

func TestAuthenticatorFailsClosedWhenReviewsAreUnavailable(t *testing.T) {
	fake := newFakeAPIServer(t)
	fake.addToken(consoleToken, consoleUser, teamAAudience)
	fake.grant(consoleUser, agentAttributes)
	now := &clock{now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	reviewer := fake.reviewer(t)
	authenticator := newTestAuthenticator(t, reviewer, now)

	for _, status := range []int{http.StatusInternalServerError, http.StatusForbidden, http.StatusTooManyRequests} {
		fake.failWith(status)
		if decision := authenticator.Authenticate(context.Background(), mustToken(t, consoleToken)); decision.Outcome != evidenceapi.AuthUnavailable {
			t.Fatalf("API server %d: outcome %v", status, decision.Outcome)
		}
	}
	fake.failWith(0)

	// The agent's own credential is unreadable.
	if err := os.Remove(reviewer.tokenFile); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := reviewer.ReviewToken(context.Background(), consoleToken, []string{teamAAudience})
	if !errors.Is(err, ErrReviewUnavailable) || strings.Contains(err.Error(), consoleToken) || strings.Contains(err.Error(), reviewer.tokenFile) {
		t.Fatalf("missing SA token error = %v", err)
	}
	if decision := authenticator.Authenticate(context.Background(), mustToken(t, consoleToken)); decision.Outcome != evidenceapi.AuthUnavailable {
		t.Fatalf("unreadable SA token: %v", decision.Outcome)
	}

	// A canceled request never authenticates.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fresh := newTestAuthenticator(t, fake.reviewer(t), now)
	if decision := fresh.Authenticate(ctx, mustToken(t, consoleToken)); decision.Outcome != evidenceapi.AuthUnavailable {
		t.Fatalf("canceled: %v", decision.Outcome)
	}
}

// stubReviewer lets a test play an API server that is not audience-aware.
type stubReviewer struct {
	audiences []string
	allowed   bool
}

func (s stubReviewer) ReviewToken(context.Context, string, []string) (User, []string, bool, error) {
	return User{Username: consoleUser}, s.audiences, true, nil
}

func (s stubReviewer) Allowed(context.Context, User, authorizationv1.ResourceAttributes) (bool, error) {
	return s.allowed, nil
}

func TestAuthenticatorRequiresAnAudienceAwareAnswer(t *testing.T) {
	now := &clock{now: time.Now()}
	for _, audiences := range [][]string{nil, {}, {"https://kubernetes.default.svc"}} {
		authenticator := newTestAuthenticator(t, stubReviewer{audiences: audiences, allowed: true}, now)
		if decision := authenticator.Authenticate(context.Background(), mustToken(t, consoleToken)); decision.Outcome != evidenceapi.AuthUnauthenticated {
			t.Fatalf("audiences %v: %v", audiences, decision.Outcome)
		}
	}
	if decision := newTestAuthenticator(t, stubReviewer{audiences: []string{teamAAudience}, allowed: true}, now).
		Authenticate(context.Background(), evidencev1alpha1.BearerToken{}); decision.Outcome != evidenceapi.AuthUnauthenticated {
		t.Fatal("the zero token authenticated")
	}
}

func TestAuthenticatorCacheIsBounded(t *testing.T) {
	now := &clock{now: time.Now()}
	authenticator := newTestAuthenticator(t, stubReviewer{audiences: []string{teamAAudience}, allowed: true}, now)
	for index := range maximumCacheEntries + 10 {
		token := mustToken(t, "eyJ."+strings.Repeat("a", index%50+1)+".c2ln"+strings.Repeat("b", index/50+1))
		_ = authenticator.Authenticate(context.Background(), token)
	}
	authenticator.mu.Lock()
	size := len(authenticator.allowed)
	authenticator.mu.Unlock()
	if size > maximumCacheEntries {
		t.Fatalf("cache holds %d entries", size)
	}
}

func TestNewAuthenticatorRejectsIncompleteOptions(t *testing.T) {
	for _, options := range []AuthenticatorOptions{
		{Name: "orders", Namespace: "team-a"},
		{Reviewer: stubReviewer{}, Namespace: "team-a"},
		{Reviewer: stubReviewer{}, Name: "orders"},
		{Reviewer: stubReviewer{}, Name: "orders", Namespace: "Team-A"},
		{Reviewer: stubReviewer{}, Name: "orders", Namespace: "team-a/b"},
	} {
		if _, err := NewAuthenticator(options); err == nil {
			t.Fatalf("accepted %+v", options)
		}
	}
	if _, err := NewAPIServerReviewer("http://plaintext", nil, "/token"); err == nil {
		t.Fatal("a plaintext API server URL was accepted")
	}
}

// countingReviewer counts reviews and authenticates nothing.
type countingReviewer struct{ reviews int }

func (c *countingReviewer) ReviewToken(context.Context, string, []string) (User, []string, bool, error) {
	c.reviews++
	return User{}, nil, false, nil
}

func (c *countingReviewer) Allowed(context.Context, User, authorizationv1.ResourceAttributes) (bool, error) {
	return true, nil
}

// TestAuthenticatorRemembersRejectedTokens: one bogus token costs one
// TokenReview per minute, not one per request.
func TestAuthenticatorRemembersRejectedTokens(t *testing.T) {
	now := &clock{now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	reviewer := &countingReviewer{}
	authenticator := newTestAuthenticator(t, reviewer, now)
	token := mustToken(t, "eyJ.Ym9ndXM.c2ln")
	for range 1000 {
		if decision := authenticator.Authenticate(context.Background(), token); decision.Outcome != evidenceapi.AuthUnauthenticated {
			t.Fatalf("outcome = %v", decision.Outcome)
		}
	}
	if reviewer.reviews != 1 {
		t.Fatalf("reviews = %d, want 1", reviewer.reviews)
	}
	now.Advance(rejectedCacheTTL + time.Second)
	_ = authenticator.Authenticate(context.Background(), token)
	if reviewer.reviews != 2 {
		t.Fatalf("reviews after the TTL = %d, want 2", reviewer.reviews)
	}
}

// TestAuthenticatorRemembersAudienceMismatch: the cached refusal keeps its
// reason, so the second request of a wrong-namespace token is refused with
// the same code without a second TokenReview.
func TestAuthenticatorRemembersAudienceMismatch(t *testing.T) {
	now := &clock{now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	reviewer := &countingReviewer{}
	authenticator := newTestAuthenticator(t, reviewer, now)
	for range 3 {
		if decision := authenticator.Authenticate(context.Background(), mustToken(t, otherNamespaceToken)); decision.Outcome != evidenceapi.AuthAudienceMismatch {
			t.Fatalf("outcome = %v", decision.Outcome)
		}
	}
	if reviewer.reviews != 1 {
		t.Fatalf("reviews = %d, want 1", reviewer.reviews)
	}
	if authenticator.Audience() != teamAAudience {
		t.Fatalf("Audience() = %q", authenticator.Audience())
	}
}

// TestAuthenticatorClaimNeverAccepts: a token whose unverified claim names
// the agent's own audience but that the API server does not authenticate
// for it is unauthenticated; the claim chooses a refusal, never an
// acceptance.
func TestAuthenticatorClaimNeverAccepts(t *testing.T) {
	now := &clock{now: time.Now()}
	forged := "eyJhbGciOiJub25lIn0.eyJhdWQiOiAicGd0b29sYm94LWV2aWRlbmNlLWFnZW50L3RlYW0tYSJ9.c2ln"
	authenticator := newTestAuthenticator(t, &countingReviewer{}, now)
	if decision := authenticator.Authenticate(context.Background(), mustToken(t, forged)); decision.Outcome != evidenceapi.AuthUnauthenticated {
		t.Fatalf("outcome = %v", decision.Outcome)
	}
}
