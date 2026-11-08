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

// Package agent is the transport of the evidence agent runtime mode: the
// HTTPS listener with a reloading serving certificate, the bound-token
// authenticator (TokenReview + SubjectAccessReview), and the pgtools
// chart's shared-token authenticator and plain listener.
//
// With the operator, the agent's only Kubernetes API access is this file's
// two POSTs: it cannot read, list, watch or write any object, which keeps
// the agent — the one process holding object-store credentials — away from
// everything else in the cluster. Its ClusterRole grants exactly create on
// tokenreviews and subjectaccessreviews. The pgtools team agent makes no
// review call (its callers present a shared token); its only API access is
// internal/evidence/selfresolve's namespaced reads of its own Cluster,
// ObjectStore and the Secrets named in the chart values.
package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
)

const (
	tokenReviewPath          = "/apis/authentication.k8s.io/v1/tokenreviews"
	subjectAccessReviewPath  = "/apis/authorization.k8s.io/v1/subjectaccessreviews"
	maximumReviewBodyBytes   = 64 * 1024
	maximumServiceTokenBytes = 16 * 1024
	reviewRequestTimeout     = 3 * time.Second

	// The kubelet's in-cluster projection of the Pod's own credential.
	// #nosec G101 -- mount paths, not credentials.
	inClusterTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	inClusterCAFile    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// ErrReviewUnavailable wraps every failure to obtain a review decision. It
// never carries a token or a response body.
var ErrReviewUnavailable = errors.New("kubernetes review unavailable")

// User is the identity a TokenReview returned.
type User struct {
	Username string
	UID      string
	Groups   []string
	Extra    map[string][]string
}

// Reviewer is the consumer-owned view of the two review APIs.
type Reviewer interface {
	// ReviewToken returns the user and the audiences the API server
	// validated the token for, or authenticated=false.
	ReviewToken(ctx context.Context, token string, audiences []string) (user User, authenticatedAudiences []string, authenticated bool, err error)
	// Allowed asks whether user may perform the resource attributes.
	Allowed(ctx context.Context, user User, attributes authorizationv1.ResourceAttributes) (bool, error)
}

// APIServerReviewer posts reviews to the API server with the Pod's own
// ServiceAccount token, re-read from its file on every call because the
// kubelet rotates it.
type APIServerReviewer struct {
	baseURL   string
	client    *http.Client
	tokenFile string
}

// NewInClusterReviewer builds the reviewer from the standard in-cluster
// environment (KUBERNETES_SERVICE_HOST/PORT and the projected token and CA).
func NewInClusterReviewer() (*APIServerReviewer, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not running in a cluster: KUBERNETES_SERVICE_HOST/PORT unset")
	}
	caPEM, err := os.ReadFile(inClusterCAFile)
	if err != nil {
		return nil, fmt.Errorf("read the API server CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("the API server CA file holds no certificate")
	}
	return NewAPIServerReviewer("https://"+net.JoinHostPort(host, port), pool, inClusterTokenFile)
}

// NewAPIServerReviewer is the explicit constructor tests use with a fake
// API server.
func NewAPIServerReviewer(baseURL string, roots *x509.CertPool, tokenFile string) (*APIServerReviewer, error) {
	if !strings.HasPrefix(baseURL, "https://") || roots == nil || tokenFile == "" {
		return nil, errors.New("API server reviewer configuration is invalid")
	}
	transport := &http.Transport{
		TLSClientConfig:       &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   reviewRequestTimeout,
		ResponseHeaderTimeout: reviewRequestTimeout,
	}
	return &APIServerReviewer{
		baseURL:   strings.TrimSuffix(baseURL, "/"),
		client:    &http.Client{Transport: transport, Timeout: reviewRequestTimeout},
		tokenFile: tokenFile,
	}, nil
}

// ReviewToken creates a TokenReview for token with the given audiences.
func (r *APIServerReviewer) ReviewToken(ctx context.Context, token string, audiences []string) (User, []string, bool, error) {
	request := authenticationv1.TokenReview{
		TypeMeta: typeMeta("authentication.k8s.io/v1", "TokenReview"),
		Spec:     authenticationv1.TokenReviewSpec{Token: token, Audiences: audiences},
	}
	var response authenticationv1.TokenReview
	if err := r.post(ctx, tokenReviewPath, request, &response); err != nil {
		return User{}, nil, false, err
	}
	if !response.Status.Authenticated {
		return User{}, nil, false, nil
	}
	info := response.Status.User
	user := User{Username: info.Username, UID: info.UID, Groups: info.Groups}
	if len(info.Extra) > 0 {
		user.Extra = make(map[string][]string, len(info.Extra))
		for key, values := range info.Extra {
			user.Extra[key] = []string(values)
		}
	}
	return user, response.Status.Audiences, true, nil
}

// Allowed creates a SubjectAccessReview for user.
func (r *APIServerReviewer) Allowed(ctx context.Context, user User, attributes authorizationv1.ResourceAttributes) (bool, error) {
	extra := make(map[string]authorizationv1.ExtraValue, len(user.Extra))
	for key, values := range user.Extra {
		extra[key] = authorizationv1.ExtraValue(values)
	}
	request := authorizationv1.SubjectAccessReview{
		TypeMeta: typeMeta("authorization.k8s.io/v1", "SubjectAccessReview"),
		Spec: authorizationv1.SubjectAccessReviewSpec{
			ResourceAttributes: &attributes,
			User:               user.Username, UID: user.UID, Groups: user.Groups, Extra: extra,
		},
	}
	var response authorizationv1.SubjectAccessReview
	if err := r.post(ctx, subjectAccessReviewPath, request, &response); err != nil {
		return false, err
	}
	return response.Status.Allowed && !response.Status.Denied, nil
}

func (r *APIServerReviewer) post(ctx context.Context, path string, body, into any) error {
	credential, err := r.serviceAccountToken()
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("%w: encode review", ErrReviewUnavailable)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("%w: build review request", ErrReviewUnavailable)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+credential)
	response, err := r.client.Do(request)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrReviewUnavailable, transportCause(err))
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maximumReviewBodyBytes))
		return fmt.Errorf("%w: API server answered %d", ErrReviewUnavailable, response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maximumReviewBodyBytes))
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("%w: undecodable review response", ErrReviewUnavailable)
	}
	return nil
}

func (r *APIServerReviewer) serviceAccountToken() (string, error) {
	// #nosec G304 -- fixed in-cluster path or a test fixture.
	file, err := os.Open(r.tokenFile)
	if err != nil {
		return "", fmt.Errorf("%w: service account token unreadable", ErrReviewUnavailable)
	}
	defer func() { _ = file.Close() }()
	content, err := io.ReadAll(io.LimitReader(file, maximumServiceTokenBytes+1))
	value := strings.TrimSpace(string(content))
	if err != nil || value == "" || len(content) > maximumServiceTokenBytes {
		return "", fmt.Errorf("%w: service account token invalid", ErrReviewUnavailable)
	}
	return value, nil
}

// transportCause names a transport failure without echoing a URL that could
// one day carry a query.
func transportCause(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return "timeout"
		}
		return "transport error"
	}
}
