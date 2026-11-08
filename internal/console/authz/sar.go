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

package authz

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	authorizationv1 "k8s.io/api/authorization/v1"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// User is the subject of a SubjectAccessReview, exactly as the API server
// reported it in a SelfSubjectReview for the user's token. Nothing in it
// is ever read from a request header or a client-side store.
type User struct {
	Name   string              `json:"username"`
	UID    string              `json:"uid,omitempty"`
	Groups []string            `json:"groups,omitempty"`
	Extra  map[string][]string `json:"-"`
}

// Attributes is one question: may the user <Verb> <Group>/<Resource>
// [/<Subresource>] [<Name>] in <Namespace>? With Path set it is a
// non-resource URL question (Verb on Path) and the resource fields are
// ignored.
type Attributes struct {
	Namespace   string
	Verb        string
	Group       string
	Resource    string
	Subresource string
	Name        string
	Path        string
}

func (a Attributes) String() string {
	if a.Path != "" {
		return a.Verb + " " + a.Path
	}
	r := a.Resource
	if a.Group != "" {
		r += "." + a.Group
	}
	if a.Subresource != "" {
		r += "/" + a.Subresource
	}
	return fmt.Sprintf("%s %s %s/%s", a.Verb, r, a.Namespace, a.Name)
}

// Decision is the outcome of one question. Err is set when the API server
// could not be asked (timeout, 5xx, throttling): the question is then
// undecided, which callers render as "could not check", and which grants
// nothing.
type Decision struct {
	Allowed bool
	Reason  string
	Err     error
}

// Reviewer answers a SubjectAccessReview. In the console it is asked with
// the subject's own token, as a SelfSubjectAccessReview (auth.SelfReviewer),
// so the console holds no cluster-scoped grant; the question
// keeps the SubjectAccessReview shape so the subject is part of it and of
// the cache key.
type Reviewer interface {
	Review(ctx context.Context, sar *authorizationv1.SubjectAccessReview) (*authorizationv1.SubjectAccessReview, error)
}

// CredentialScoped is implemented by a Reviewer whose answer depends on a
// credential the context carries (auth.SelfReviewer: the asker's token).
// Concurrent callers share one review only when the digests of their
// credentials agree, so a session whose token the API server rejects
// never answers that rejection to another session of the same subject.
// The digest is a hash: no credential is ever part of a key.
type CredentialScoped interface {
	CredentialDigest(ctx context.Context) [32]byte
}

// Options tunes the Authorizer. Zero values take the plan's defaults.
type Options struct {
	// TTL of a positive answer (10 s). Negative answers and
	// errors are never cached, so a grant shows up on the next request
	// and a revocation within TTL; writes go to the API server as the
	// user, so a stale positive can never grant a write.
	TTL time.Duration
	// Workers bounds concurrent reviews across all users; PerUser bounds
	// one subject's share, so one user listing many clusters in a loop
	// cannot starve everyone else's checks.
	Workers int
	PerUser int
	// Timeout bounds one review.
	Timeout time.Duration
	// MaxEntries bounds the cache.
	MaxEntries int
}

// Defaults of Options.
const (
	DefaultTTL        = 10 * time.Second
	DefaultWorkers    = 16
	DefaultPerUser    = 4
	DefaultTimeout    = 5 * time.Second
	DefaultMaxEntries = 20000
)

// Authorizer answers access questions with parallel, cached
// SubjectAccessReviews. It is safe for concurrent use.
type Authorizer struct {
	reviewer Reviewer
	opts     Options
	sem      chan struct{}
	users    userSlots
	now      func() time.Time

	flightMu sync.Mutex
	flights  map[string]*flight

	mu    sync.RWMutex
	cache map[[32]byte]time.Time
	// narrow holds namespace-wide questions the API server denied: the
	// named question is asked instead until the entry expires. It never
	// denies anything by itself (see Allowed).
	narrow map[[32]byte]time.Time
}

// NewAuthorizer builds an Authorizer over r.
func NewAuthorizer(r Reviewer, o Options) *Authorizer {
	if o.TTL <= 0 {
		o.TTL = DefaultTTL
	}
	if o.Workers <= 0 {
		o.Workers = DefaultWorkers
	}
	if o.PerUser <= 0 {
		o.PerUser = DefaultPerUser
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.MaxEntries <= 0 {
		o.MaxEntries = DefaultMaxEntries
	}
	return &Authorizer{
		reviewer: r,
		opts:     o,
		sem:      make(chan struct{}, o.Workers),
		users:    userSlots{per: o.PerUser, m: map[string]*slot{}},
		now:      time.Now,
		cache:    map[[32]byte]time.Time{},
		narrow:   map[[32]byte]time.Time{},
		flights:  map[string]*flight{},
	}
}

// ErrNoUser is returned for a question without a subject.
var ErrNoUser = errors.New("no user to authorize")

// Allowed answers one question.
//
// A question about a named object is first asked without the name: in a
// SubjectAccessReview an empty name means every object of that resource,
// so a user allowed on all of them is allowed on each, and listing N
// Clusters of one namespace costs one review instead of N (with RBAC, only
// a rule without resourceNames allows the nameless question). When the
// wide question is denied or cannot be asked, the named one decides, so a
// grant limited to resourceNames still works.
func (a *Authorizer) Allowed(ctx context.Context, u User, at Attributes) Decision {
	if u.Name == "" {
		return Decision{Err: ErrNoUser}
	}
	if at.Path == "" && at.Name != "" && a.allowedOnAll(ctx, u, at) {
		return Decision{Allowed: true}
	}
	return a.decide(ctx, u, at)
}

// allowedOnAll asks at without its name. A denial is remembered for TTL
// in narrow, so a user with only named grants costs one extra review per
// TTL, not one per question.
func (a *Authorizer) allowedOnAll(ctx context.Context, u User, at Attributes) bool {
	at.Name = ""
	key := cacheKey(u, at)
	if a.cached(key) {
		cacheLookups.WithLabelValues("hit").Inc()
		return true
	}
	if a.cachedIn(a.narrow, key) {
		return false
	}
	d := a.decide(ctx, u, at)
	if d.Err == nil && !d.Allowed {
		a.storeIn(a.narrow, key)
	}
	return d.Allowed
}

// decide answers at as asked, from the cache or with one review.
func (a *Authorizer) decide(ctx context.Context, u User, at Attributes) Decision {
	key := cacheKey(u, at)
	if a.cached(key) {
		cacheLookups.WithLabelValues("hit").Inc()
		return Decision{Allowed: true}
	}
	cacheLookups.WithLabelValues("miss").Inc()
	d, err := a.shared(ctx, a.flightKey(ctx, key), func(shared context.Context) (Decision, error) {
		d, err := a.review(shared, u, at)
		if err == nil && d.Allowed {
			// Stored by the shared work, so a caller that gave up while
			// others still waited is served from the cache on its retry.
			a.store(key)
		}
		return d, err
	})
	if err != nil {
		return Decision{Err: err}
	}
	return d
}

// flight is one review shared by concurrent identical questions.
type flight struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	d       Decision
	err     error
}

// flightKey is the question's cache key, plus the digest of the caller's
// credential when the reviewer answers with it (CredentialScoped).
func (a *Authorizer) flightKey(ctx context.Context, key [32]byte) string {
	if scoped, ok := a.reviewer.(CredentialScoped); ok {
		digest := scoped.CredentialDigest(ctx)
		return string(key[:]) + string(digest[:])
	}
	return string(key[:])
}

// shared runs run once for concurrent callers of one key (a list rendering
// while the stream filters the same clusters). The shared work belongs to
// none of them: the caller that happens to start it leaving must not
// answer "could not check" to the others, so it runs detached from that
// caller and bounded on its own (queue wait and review timeout). It is
// cancelled when the last caller waiting for it leaves: a request aborted
// by its client releases its share of the subject's slots and the workers
// instead of keeping them for an answer nobody reads. Each caller waits only as long as its own context allows.
func (a *Authorizer) shared(ctx context.Context, key string, run func(context.Context) (Decision, error)) (Decision, error) {
	a.flightMu.Lock()
	f, ok := a.flights[key]
	if !ok {
		detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), maxQueueWait+a.opts.Timeout)
		f = &flight{done: make(chan struct{}), cancel: cancel}
		a.flights[key] = f
		go func() {
			d, err := run(detached)
			a.flightMu.Lock()
			if a.flights[key] == f {
				delete(a.flights, key)
			}
			f.d, f.err = d, err
			a.flightMu.Unlock()
			cancel()
			close(f.done)
		}()
	}
	f.waiters++
	a.flightMu.Unlock()
	select {
	case <-f.done:
		return f.d, f.err
	case <-ctx.Done():
		a.flightMu.Lock()
		f.waiters--
		if f.waiters == 0 {
			// Nobody waits any more: a later caller starts afresh rather
			// than joining a cancelled review.
			if a.flights[key] == f {
				delete(a.flights, key)
			}
			f.cancel()
		}
		a.flightMu.Unlock()
		return Decision{}, ctx.Err()
	}
}

// maxQueueWait bounds how long a shared review waits for the subject's
// share and a worker before it is undecided.
const maxQueueWait = 30 * time.Second

// Check answers several questions in parallel, bounded by the worker
// pool; the result is in the order of ats. It decides as Allowed does,
// but asks each distinct namespace-wide question once up front: a list
// of 1,600 Clusters in 16 namespaces is then 16 cache lookups, not 1,600
// goroutines hashing the user and queueing on the cache lock, and only
// the names in namespaces where the wide question is denied are asked
// one by one.
func (a *Authorizer) Check(ctx context.Context, u User, ats []Attributes) []Decision {
	out := make([]Decision, len(ats))
	named := func(at Attributes) bool { return u.Name != "" && at.Path == "" && at.Name != "" }
	wide := map[Attributes]bool{}
	var questions []Attributes
	for _, at := range ats {
		if !named(at) {
			continue
		}
		at.Name = ""
		if _, ok := wide[at]; !ok {
			wide[at] = false
			questions = append(questions, at)
		}
	}
	answers := make([]bool, len(questions))
	var wg sync.WaitGroup
	for i, at := range questions {
		wg.Go(func() { answers[i] = a.allowedOnAll(ctx, u, at) })
	}
	wg.Wait()
	for i, at := range questions {
		wide[at] = answers[i]
	}

	for i, at := range ats {
		if !named(at) {
			wg.Go(func() { out[i] = a.Allowed(ctx, u, at) })
			continue
		}
		all := at
		all.Name = ""
		if wide[all] {
			out[i] = Decision{Allowed: true}
			continue
		}
		wg.Go(func() { out[i] = a.decide(ctx, u, at) })
	}
	wg.Wait()
	return out
}

func (a *Authorizer) review(ctx context.Context, u User, at Attributes) (Decision, error) {
	release, err := a.users.acquire(ctx, u.Name+"\x00"+u.UID)
	if err != nil {
		return Decision{}, err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		// Abandoned while it queued for the subject's share.
		return Decision{}, err
	}
	select {
	case a.sem <- struct{}{}:
	case <-ctx.Done():
		return Decision{}, ctx.Err()
	}
	defer func() { <-a.sem }()
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	// ctx is the shared flight's: cancelled only when no caller waits.
	ctx, cancel := context.WithTimeout(ctx, a.opts.Timeout)
	defer cancel()

	start := a.now()
	res, err := a.reviewer.Review(ctx, newSAR(u, at))
	reviewDuration.Observe(a.now().Sub(start).Seconds())
	if err != nil {
		reviews.WithLabelValues("error").Inc()
		return Decision{}, fmt.Errorf("SubjectAccessReview for %s: %w", at, err)
	}
	if res.Status.EvaluationError != "" && !res.Status.Allowed {
		// An authorizer in the chain failed to evaluate; RBAC may still
		// have answered. The denial stands, but it is reported as such.
		reviews.WithLabelValues("denied").Inc()
		return Decision{Reason: res.Status.Reason}, nil
	}
	if res.Status.Allowed && !res.Status.Denied {
		reviews.WithLabelValues("allowed").Inc()
		return Decision{Allowed: true, Reason: res.Status.Reason}, nil
	}
	reviews.WithLabelValues("denied").Inc()
	return Decision{Reason: res.Status.Reason}, nil
}

func newSAR(u User, at Attributes) *authorizationv1.SubjectAccessReview {
	spec := authorizationv1.SubjectAccessReviewSpec{
		User:   u.Name,
		UID:    u.UID,
		Groups: slices.Clone(u.Groups),
	}
	if len(u.Extra) > 0 {
		spec.Extra = make(map[string]authorizationv1.ExtraValue, len(u.Extra))
		for k, v := range u.Extra {
			spec.Extra[k] = slices.Clone(v)
		}
	}
	if at.Path != "" {
		spec.NonResourceAttributes = &authorizationv1.NonResourceAttributes{Path: at.Path, Verb: at.Verb}
	} else {
		spec.ResourceAttributes = &authorizationv1.ResourceAttributes{
			Namespace:   at.Namespace,
			Verb:        at.Verb,
			Group:       at.Group,
			Resource:    at.Resource,
			Subresource: at.Subresource,
			Name:        at.Name,
		}
	}
	return &authorizationv1.SubjectAccessReview{Spec: spec}
}

func (a *Authorizer) cached(key [32]byte) bool { return a.cachedIn(a.cache, key) }

func (a *Authorizer) store(key [32]byte) { a.storeIn(a.cache, key) }

func (a *Authorizer) cachedIn(entries map[[32]byte]time.Time, key [32]byte) bool {
	a.mu.RLock()
	exp, ok := entries[key]
	a.mu.RUnlock()
	if !ok {
		return false
	}
	if a.now().Before(exp) {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// Delete only the entry seen expired: a fresher one may have been
	// stored between the two locks.
	if exp2, ok := entries[key]; ok && !a.now().Before(exp2) {
		delete(entries, key)
	}
	return false
}

func (a *Authorizer) storeIn(entries map[[32]byte]time.Time, key [32]byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if len(entries) >= a.opts.MaxEntries {
		for k, exp := range entries {
			if !now.Before(exp) {
				delete(entries, k)
			}
		}
		if len(entries) >= a.opts.MaxEntries {
			clear(entries)
		}
	}
	entries[key] = now.Add(a.opts.TTL)
}

// cacheKey is a digest of the full subject (name, uid, groups and extra,
// order-independent) and the question: two users never share an entry,
// and neither do two group memberships of one user.
func cacheKey(u User, at Attributes) [32]byte {
	h := sha256.New()
	w := func(s string) { _, _ = fmt.Fprintf(h, "%d:%s|", len(s), s) }
	w(u.Name)
	w(u.UID)
	groups := slices.Clone(u.Groups)
	slices.Sort(groups)
	w(strconv.Itoa(len(groups)))
	for _, g := range groups {
		w(g)
	}
	keys := make([]string, 0, len(u.Extra))
	for k := range u.Extra {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	w(strconv.Itoa(len(keys)))
	for _, k := range keys {
		w(k)
		vals := slices.Clone(u.Extra[k])
		slices.Sort(vals)
		w(strconv.Itoa(len(vals)))
		for _, v := range vals {
			w(v)
		}
	}
	for _, f := range []string{at.Namespace, at.Verb, at.Group, at.Resource, at.Subresource, at.Name, at.Path} {
		w(f)
	}
	var k [32]byte
	copy(k[:], h.Sum(nil))
	return k
}

// Metrics of the SAR authorizer, served on the console's /metrics.
var (
	cacheLookups = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pgtoolbox_console_sar_cache_lookups_total",
		Help: "SubjectAccessReview cache lookups by result (hit, miss); only positive answers are cached.",
	}, []string{"result"})
	reviews = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pgtoolbox_console_sar_reviews_total",
		Help: "SubjectAccessReviews sent to the API server by outcome (allowed, denied, error).",
	}, []string{"outcome"})
	reviewDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "pgtoolbox_console_sar_review_duration_seconds",
		Help:    "Latency of SubjectAccessReviews sent to the API server.",
		Buckets: prometheus.ExponentialBuckets(0.005, 2, 10),
	})
)

func init() {
	crmetrics.Registry.MustRegister(cacheLookups, reviews, reviewDuration)
}

// userSlots bounds concurrent reviews per subject.
type userSlots struct {
	per int
	mu  sync.Mutex
	m   map[string]*slot
}

type slot struct {
	ch   chan struct{}
	refs int
}

func (s *userSlots) acquire(ctx context.Context, key string) (func(), error) {
	s.mu.Lock()
	sl, ok := s.m[key]
	if !ok {
		sl = &slot{ch: make(chan struct{}, s.per)}
		s.m[key] = sl
	}
	sl.refs++
	s.mu.Unlock()
	done := func() {
		s.mu.Lock()
		sl.refs--
		if sl.refs == 0 {
			delete(s.m, key)
		}
		s.mu.Unlock()
	}
	select {
	case sl.ch <- struct{}{}:
		return func() { <-sl.ch; done() }, nil
	case <-ctx.Done():
		done()
		return nil, ctx.Err()
	}
}
