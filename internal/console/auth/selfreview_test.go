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
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/client-go/rest"

	"github.com/fyannk/pgToolBox/internal/console/authz"
)

// selfReviewServer answers SelfSubjectAccessReviews: "get" is allowed for
// the token "jane-token", any other token is rejected (401). It records the
// tokens it was asked with.
func selfReviewServer(t *testing.T) (*SelfReviewer, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews" {
			http.NotFound(w, r)
			return
		}
		tok := r.Header.Get("Authorization")
		mu.Lock()
		tokens = append(tokens, tok)
		mu.Unlock()
		if tok != "Bearer jane-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Unauthorized","code":401}`)
			return
		}
		var ssar authorizationv1.SelfSubjectAccessReview
		_ = json.NewDecoder(r.Body).Decode(&ssar)
		ssar.Status.Allowed = ssar.Spec.ResourceAttributes != nil && ssar.Spec.ResourceAttributes.Verb == "get"
		_ = json.NewEncoder(w).Encode(ssar)
	}))
	t.Cleanup(srv.Close)
	f, err := NewClientFactory(&rest.Config{Host: srv.URL, BearerToken: "console-sa", ContentConfig: rest.ContentConfig{ContentType: "application/json"}})
	if err != nil {
		t.Fatal(err)
	}
	return &SelfReviewer{Factory: f}, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), tokens...)
	}
}

func sarFor(user, verb string) *authorizationv1.SubjectAccessReview {
	return &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User:               user,
		ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: "team-a", Verb: verb, Group: "postgresql.cnpg.io", Resource: "clusters", Name: "orders"},
	}}
}

func TestSelfReviewerAsksWithThePrincipalsToken(t *testing.T) {
	t.Parallel()
	r, asked := selfReviewServer(t)
	ctx := WithPrincipal(context.Background(), &Principal{User: authz.User{Name: "jane"}, Token: "jane-token"})

	res, err := r.Review(ctx, sarFor("jane", "get"))
	if err != nil || !res.Status.Allowed || res.Spec.User != "jane" {
		t.Fatalf("Review = %+v, %v; want allowed, subject kept", res, err)
	}
	res, err = r.Review(ctx, sarFor("jane", "delete"))
	if err != nil || res.Status.Allowed {
		t.Fatalf("Review(delete) = %+v, %v; want a denial", res, err)
	}
	for _, tok := range asked() {
		if tok != "Bearer jane-token" {
			t.Fatalf("asked with %q: the user's own token must be sent, never the console's", tok)
		}
	}
}

func TestSelfReviewerRefusesOtherSubjects(t *testing.T) {
	t.Parallel()
	r, asked := selfReviewServer(t)
	ctx := WithPrincipal(context.Background(), &Principal{User: authz.User{Name: "jane"}, Token: "jane-token"})
	if _, err := r.Review(ctx, sarFor("mallory", "get")); !errors.Is(err, errOtherSubject) {
		t.Fatalf("Review(mallory) error = %v, want errOtherSubject", err)
	}
	if _, err := r.Review(context.Background(), sarFor("jane", "get")); !errors.Is(err, ErrNoUserToken) {
		t.Fatalf("Review without a token error = %v, want ErrNoUserToken", err)
	}
	if got := asked(); len(got) != 0 {
		t.Fatalf("the API server was asked %d times; refusals must not leave the process", len(got))
	}
}

func TestSelfReviewerUsesTheContextTokenWithoutAPrincipal(t *testing.T) {
	t.Parallel()
	r, _ := selfReviewServer(t)
	// MetricsFilter's path: the scraper's token, no session.
	res, err := r.Review(WithUserToken(context.Background(), "jane-token"), sarFor("jane", "get"))
	if err != nil || !res.Status.Allowed {
		t.Fatalf("Review = %+v, %v; want allowed", res, err)
	}
}

func TestSelfReviewerReportsARejectedToken(t *testing.T) {
	t.Parallel()
	r, _ := selfReviewServer(t)
	ctx := WithPrincipal(context.Background(), &Principal{User: authz.User{Name: "jane"}, Token: "expired"})
	if _, err := r.Review(ctx, sarFor("jane", "get")); !errors.Is(err, ErrTokenRejected) {
		t.Fatalf("error = %v, want ErrTokenRejected", err)
	}
}

// TestSelfReviewerScopesSharedReviewsToTheToken:
// the authorizer shares a review between callers only when their tokens'
// digests agree; two sessions of jane with different tokens must not.
func TestSelfReviewerScopesSharedReviewsToTheToken(t *testing.T) {
	t.Parallel()
	var reviewer authz.Reviewer = SelfReviewer{}
	scoped, ok := reviewer.(authz.CredentialScoped)
	if !ok {
		t.Fatal("SelfReviewer does not scope shared reviews to the asker's token")
	}
	jane := authz.User{Name: "jane"}
	expired := scoped.CredentialDigest(WithPrincipal(context.Background(), &Principal{User: jane, Token: "expired"}))
	valid := scoped.CredentialDigest(WithPrincipal(context.Background(), &Principal{User: jane, Token: "jane-token"}))
	again := scoped.CredentialDigest(WithPrincipal(context.Background(), &Principal{User: jane, Token: "jane-token"}))
	if expired == valid || valid != again {
		t.Fatal("the digest does not follow the token")
	}
	// The principal's token is the one a review is sent with, over the
	// context's.
	both := WithPrincipal(WithUserToken(context.Background(), "expired"), &Principal{User: jane, Token: "jane-token"})
	if scoped.CredentialDigest(both) != valid {
		t.Fatal("the digest is not the principal's token's")
	}
	if scoped.CredentialDigest(WithUserToken(context.Background(), "expired")) != expired {
		t.Fatal("without a principal, the digest is not the context token's")
	}
}

// TestTwoSessionsOfOneSubjectAreAnsweredWithTheirOwnTokens runs the real
// authorizer over the self reviewer: an expired session and a valid one
// ask the same question at once.
func TestTwoSessionsOfOneSubjectAreAnsweredWithTheirOwnTokens(t *testing.T) {
	t.Parallel()
	r, _ := selfReviewServer(t)
	jane := authz.User{Name: "jane"}
	q := authz.Attributes{Namespace: "team-a", Verb: "get", Group: "postgresql.cnpg.io", Resource: "clusters"}
	// Whichever session asks first, the valid one is allowed. (The expired
	// one may be answered from the subject's positive cache when the valid
	// one finished first: its identity was reviewed when it signed in.)
	for range 20 {
		a := authz.NewAuthorizer(r, authz.Options{})
		var wg sync.WaitGroup
		decisions := make([]authz.Decision, 2)
		for index, token := range []string{"expired", "jane-token"} {
			wg.Go(func() {
				decisions[index] = a.Allowed(WithPrincipal(context.Background(), &Principal{User: jane, Token: token}), jane, q)
			})
		}
		wg.Wait()
		if decisions[1].Err != nil || !decisions[1].Allowed {
			t.Fatalf("the valid session got %+v, want allowed", decisions[1])
		}
		if decisions[0].Err != nil && !errors.Is(decisions[0].Err, ErrTokenRejected) {
			t.Fatalf("the expired session got %+v", decisions[0])
		}
	}
}
