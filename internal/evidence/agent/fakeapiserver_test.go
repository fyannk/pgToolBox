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
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
)

// fakeAPIServer answers TokenReviews and SubjectAccessReviews the way an
// audience-aware API server does: status.audiences is the intersection of
// the requested audiences and the token's.
type fakeAPIServer struct {
	server *httptest.Server
	// serviceAccountToken is what the agent must present to the API server.
	serviceAccountToken string

	mu       sync.Mutex
	tokens   map[string]fakeToken
	grants   map[string]authorizationv1.ResourceAttributes // username -> the one allowed attribute set
	status   int                                           // non-zero: answer every request with this status
	reviews  []authenticationv1.TokenReview
	accesses []authorizationv1.SubjectAccessReview
}

type fakeToken struct {
	user      authenticationv1.UserInfo
	audiences []string
}

func newFakeAPIServer(t *testing.T) *fakeAPIServer {
	t.Helper()
	fake := &fakeAPIServer{
		serviceAccountToken: "agent-sa-token",
		tokens:              map[string]fakeToken{},
		grants:              map[string]authorizationv1.ResourceAttributes{},
	}
	fake.server = httptest.NewTLSServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeAPIServer) serve(writer http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer "+f.serviceAccountToken {
		writer.WriteHeader(http.StatusUnauthorized)
		return
	}
	if f.status != 0 {
		writer.WriteHeader(f.status)
		return
	}
	switch request.URL.Path {
	case tokenReviewPath:
		var review authenticationv1.TokenReview
		if err := json.NewDecoder(request.Body).Decode(&review); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		f.reviews = append(f.reviews, review)
		if token, ok := f.tokens[review.Spec.Token]; ok {
			var audiences []string
			for _, audience := range review.Spec.Audiences {
				if slices.Contains(token.audiences, audience) {
					audiences = append(audiences, audience)
				}
			}
			if len(audiences) > 0 || len(review.Spec.Audiences) == 0 {
				review.Status = authenticationv1.TokenReviewStatus{Authenticated: true, User: token.user, Audiences: audiences}
			} else {
				review.Status = authenticationv1.TokenReviewStatus{Error: "token audiences do not match"}
			}
		}
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(review)
	case subjectAccessReviewPath:
		var review authorizationv1.SubjectAccessReview
		if err := json.NewDecoder(request.Body).Decode(&review); err != nil || review.Spec.ResourceAttributes == nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		f.accesses = append(f.accesses, review)
		grant, ok := f.grants[review.Spec.User]
		review.Status.Allowed = ok && grant == *review.Spec.ResourceAttributes
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(review)
	default:
		writer.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeAPIServer) addToken(token, username string, audiences ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[token] = fakeToken{user: authenticationv1.UserInfo{Username: username, UID: "uid-" + username, Groups: []string{"system:serviceaccounts"}}, audiences: audiences}
}

func (f *fakeAPIServer) grant(username string, attributes authorizationv1.ResourceAttributes) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.grants[username] = attributes
}

func (f *fakeAPIServer) revoke(username string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.grants, username)
}

func (f *fakeAPIServer) failWith(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

func (f *fakeAPIServer) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reviews), len(f.accesses)
}

// reviewer returns an APIServerReviewer pointed at the fake, with the Pod's
// service account token in a temporary file.
func (f *fakeAPIServer) reviewer(t *testing.T) *APIServerReviewer {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(f.serviceAccountToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(f.server.Certificate())
	reviewer, err := NewAPIServerReviewer(f.server.URL, pool, tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	return reviewer
}
