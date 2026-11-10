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
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"

	"github.com/fyannk/pgToolBox/internal/console/authz"
)

// ClientFactory builds the client that acts as the signed-in user: token
// passthrough, the only write mode. There is no
// impersonating variant and the console's ServiceAccount holds no
// impersonate verb; an impersonation opt-in, if it ever ships, is a
// separate, separately granted client, never a branch of this one.
//
// The client is built once from an anonymous copy of the console's REST
// configuration (the API server address and CA, none of the console's
// credentials) and sends the bearer token carried by each request's
// context. A request whose context carries no user token is refused
// before it leaves the process, so this client can never fall back to
// acting as the console or as anonymous.
type ClientFactory struct {
	client kubernetes.Interface
}

type userTokenKey struct{}

// WithUserToken returns ctx carrying the user's bearer token for the
// ClientFactory's client.
func WithUserToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, userTokenKey{}, token)
}

func userToken(ctx context.Context) string {
	t, _ := ctx.Value(userTokenKey{}).(string)
	return t
}

// ErrNoUserToken is returned by the user client for a context without a
// user token.
var ErrNoUserToken = errors.New("user client called without a user token in the context")

// UserConfig is base stripped of every credential and of impersonation,
// with a transport that sets the user's token from the request context.
func UserConfig(base *rest.Config, wraps ...transport.WrapperFunc) *rest.Config {
	cfg := rest.AnonymousClientConfig(base)
	// Instrumentation (tracing) first, so it sees the request the token
	// wrapper below sends; nil entries are skipped.
	for _, w := range wraps {
		if w != nil {
			cfg.Wrap(w)
		}
	}
	cfg.Impersonate = rest.ImpersonationConfig{}
	cfg.UserAgent = "pgtoolbox-console (as user)"
	// Its own client-side budget, never shared with the console's
	// informers or reviews: users' calls must not queue behind them.
	cfg.RateLimiter = nil
	cfg.QPS, cfg.Burst = 100, 200
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			tok := userToken(req.Context())
			if tok == "" {
				return nil, ErrNoUserToken
			}
			req = req.Clone(req.Context())
			req.Header.Set("Authorization", "Bearer "+tok)
			return rt.RoundTrip(req)
		})
	})
	return cfg
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// NewClientFactory builds the user client over base (the console's
// in-cluster or kubeconfig configuration).
func NewClientFactory(base *rest.Config, wraps ...transport.WrapperFunc) (*ClientFactory, error) {
	c, err := kubernetes.NewForConfig(UserConfig(base, wraps...))
	if err != nil {
		return nil, fmt.Errorf("building the user client: %w", err)
	}
	return &ClientFactory{client: c}, nil
}

// NewClientFactoryFor wraps an existing clientset (tests).
func NewClientFactoryFor(c kubernetes.Interface) *ClientFactory { return &ClientFactory{client: c} }

// Client returns the user client. Every call must pass a context built
// with WithUserToken.
func (f *ClientFactory) Client() kubernetes.Interface { return f.client }

// IdentityReviewer resolves a token to the identity the API server sees.
type IdentityReviewer interface {
	Review(ctx context.Context, token string) (authz.User, error)
}

// ErrTokenRejected means the API server does not accept the token (401):
// expired, revoked, or minted for another audience. It ends the session.
var ErrTokenRejected = errors.New("the API server does not accept this token")

// SelfSubjectReviewer asks the API server who a token belongs to with a
// SelfSubjectReview (authentication.k8s.io/v1, GA since Kubernetes 1.28),
// sent with that token. It needs no permission on the console's side
// (every authenticated user may create one through system:basic-user),
// and its answer is the username and groups the API server's
// authenticator derived: OIDC claim mappings and prefixes, OpenShift
// users and groups, ServiceAccounts.
type SelfSubjectReviewer struct {
	Factory *ClientFactory
	// sem bounds concurrent reviews; nil means unbounded (tests).
	sem chan struct{}
}

// Bounds of identity reviews: concurrent calls, and each call's deadline.
const (
	maxConcurrentIdentityReviews = 32
	identityReviewTimeout        = 5 * time.Second
)

// NewSelfSubjectReviewer bounds concurrency and time of every review, so
// a flood of bogus tokens queues at most maxConcurrentIdentityReviews
// calls and every caller gives up after identityReviewTimeout.
func NewSelfSubjectReviewer(f *ClientFactory) *SelfSubjectReviewer {
	return &SelfSubjectReviewer{Factory: f, sem: make(chan struct{}, maxConcurrentIdentityReviews)}
}

// Review implements IdentityReviewer.
func (r *SelfSubjectReviewer) Review(ctx context.Context, token string) (authz.User, error) {
	if token == "" {
		return authz.User{}, ErrTokenRejected
	}
	ctx, cancel := context.WithTimeout(ctx, identityReviewTimeout)
	defer cancel()
	if r.sem != nil {
		select {
		case r.sem <- struct{}{}:
			defer func() { <-r.sem }()
		case <-ctx.Done():
			return authz.User{}, fmt.Errorf("SelfSubjectReview: %w", ctx.Err())
		}
	}
	res, err := r.Factory.Client().AuthenticationV1().SelfSubjectReviews().
		Create(WithUserToken(ctx, token), &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsUnauthorized(err) {
			return authz.User{}, ErrTokenRejected
		}
		return authz.User{}, fmt.Errorf("SelfSubjectReview: %w", err)
	}
	ui := res.Status.UserInfo
	if ui.Username == "" || ui.Username == "system:anonymous" {
		return authz.User{}, ErrTokenRejected
	}
	u := authz.User{Name: ui.Username, UID: ui.UID, Groups: ui.Groups}
	if len(ui.Extra) > 0 {
		u.Extra = make(map[string][]string, len(ui.Extra))
		for k, v := range ui.Extra {
			u.Extra[k] = []string(v)
		}
	}
	return u, nil
}

// CachedReviewer caches positive identity answers for a short TTL, keyed
// by a digest of the token (the token itself is never a map key, so a
// heap dump of the cache holds no credential). A rejected token is never
// cached: the next request asks again.
type CachedReviewer struct {
	next IdentityReviewer
	ttl  time.Duration
	max  int
	now  func() time.Time

	mu      sync.Mutex
	entries map[[32]byte]identityEntry
}

type identityEntry struct {
	user authz.User
	exp  time.Time
}

// DefaultIdentityTTL bounds how long a token revoked at the API server
// (a deleted OpenShift OAuth token, a removed ServiceAccount) keeps
// working for views; writes are refused immediately because they are
// sent with the token.
const DefaultIdentityTTL = 30 * time.Second

// NewCachedReviewer wraps next.
func NewCachedReviewer(next IdentityReviewer, ttl time.Duration, maxEntries int) *CachedReviewer {
	if ttl <= 0 {
		ttl = DefaultIdentityTTL
	}
	if maxEntries <= 0 {
		maxEntries = 4096
	}
	return &CachedReviewer{next: next, ttl: ttl, max: maxEntries, now: time.Now, entries: map[[32]byte]identityEntry{}}
}

// Review implements IdentityReviewer.
func (c *CachedReviewer) Review(ctx context.Context, token string) (authz.User, error) {
	key := sha256.Sum256([]byte(token))
	now := c.now()
	c.mu.Lock()
	e, ok := c.entries[key]
	c.mu.Unlock()
	if ok && now.Before(e.exp) {
		return e.user, nil
	}
	u, err := c.next.Review(ctx, token)
	if err != nil {
		c.mu.Lock()
		delete(c.entries, key)
		c.mu.Unlock()
		return authz.User{}, err
	}
	c.mu.Lock()
	if len(c.entries) >= c.max {
		for k, v := range c.entries {
			if !now.Before(v.exp) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= c.max {
			clear(c.entries)
		}
	}
	c.entries[key] = identityEntry{user: u, exp: now.Add(c.ttl)}
	c.mu.Unlock()
	return u, nil
}

// Cached reports whether token's identity is cached and fresh.
func (c *CachedReviewer) Cached(token string) bool {
	key := sha256.Sum256([]byte(token))
	now := c.now()
	c.mu.Lock()
	e, ok := c.entries[key]
	c.mu.Unlock()
	return ok && now.Before(e.exp)
}

// Forget drops the cached identity of token (sign-out).
func (c *CachedReviewer) Forget(token string) {
	key := sha256.Sum256([]byte(token))
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

// SelfReviewer answers the authorizer's questions with
// SelfSubjectAccessReviews sent with the asker's own token: every
// authenticated user may create those (system:basic-user), so the console
// needs no cluster-scoped grant to authorize anyone. Every
// question the console asks is about the person making the request: the
// signed-in user (the request's Principal) or a metrics scraper (the token
// MetricsFilter puts in the context). A question about anyone else, or
// without a token, is refused before it leaves the process.
type SelfReviewer struct {
	Factory *ClientFactory
}

// errOtherSubject is SelfReviewer's answer to a question about someone
// other than the request's principal.
var errOtherSubject = errors.New("a self review answers only for the request's own principal")

// askerToken is the token a self review is sent with: the principal's,
// else the context's (a metrics scraper's).
func askerToken(ctx context.Context) string {
	if p, ok := PrincipalFrom(ctx); ok {
		return p.Token
	}
	return userToken(ctx)
}

// CredentialDigest implements authz.CredentialScoped: the answer depends on
// the asker's token, so two sessions of one subject share a review only
// with the same token, and an expired token in one browser tab is not
// answered to another. A SHA-256 digest, like the
// identity cache's key: the token itself is never a key.
func (r SelfReviewer) CredentialDigest(ctx context.Context) [32]byte {
	return sha256.Sum256([]byte(askerToken(ctx)))
}

// Review implements authz.Reviewer.
func (r SelfReviewer) Review(ctx context.Context, sar *authorizationv1.SubjectAccessReview) (*authorizationv1.SubjectAccessReview, error) {
	if p, ok := PrincipalFrom(ctx); ok && (p.User.Name != sar.Spec.User || p.User.UID != sar.Spec.UID) {
		return nil, errOtherSubject
	}
	token := askerToken(ctx)
	if token == "" {
		return nil, ErrNoUserToken
	}
	self := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
		ResourceAttributes:    sar.Spec.ResourceAttributes,
		NonResourceAttributes: sar.Spec.NonResourceAttributes,
	}}
	res, err := r.Factory.Client().AuthorizationV1().SelfSubjectAccessReviews().
		Create(WithUserToken(ctx, token), self, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsUnauthorized(err) {
			return nil, ErrTokenRejected
		}
		return nil, fmt.Errorf("SelfSubjectAccessReview: %w", err)
	}
	out := sar.DeepCopy()
	out.Status = res.Status
	return out, nil
}
