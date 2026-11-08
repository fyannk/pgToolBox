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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// rbac is a fake authorizer: the set of "user verb resource/sub ns/name"
// strings that are allowed.
type rbac struct {
	mu       sync.Mutex
	allowed  map[string]bool
	fail     map[string]bool
	calls    int
	inflight atomic.Int32
	peak     atomic.Int32
	delay    time.Duration
	seen     []authorizationv1.SubjectAccessReviewSpec
}

func key(user string, at Attributes) string { return user + " " + at.String() }

func (r *rbac) Review(ctx context.Context, sar *authorizationv1.SubjectAccessReview) (*authorizationv1.SubjectAccessReview, error) {
	n := r.inflight.Add(1)
	defer r.inflight.Add(-1)
	for {
		p := r.peak.Load()
		if n <= p || r.peak.CompareAndSwap(p, n) {
			break
		}
	}
	if r.delay > 0 {
		select {
		case <-time.After(r.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s := sar.Spec
	var at Attributes
	if s.NonResourceAttributes != nil {
		at = Attributes{Verb: s.NonResourceAttributes.Verb, Path: s.NonResourceAttributes.Path}
	} else {
		ra := s.ResourceAttributes
		at = Attributes{Namespace: ra.Namespace, Verb: ra.Verb, Group: ra.Group, Resource: ra.Resource, Subresource: ra.Subresource, Name: ra.Name}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.seen = append(r.seen, s)
	k := key(s.User, at)
	if r.fail[k] {
		return nil, errors.New("apiserver: 503")
	}
	out := sar.DeepCopy()
	out.Status.Allowed = r.allowed[k]
	return out, nil
}

var jane = User{Name: "jane", UID: "u1", Groups: []string{"dba", "system:authenticated"}, Extra: map[string][]string{"scopes": {"b", "a"}}}

func TestAllowedCachesPositiveOnly(t *testing.T) {
	t.Parallel()
	view := ClusterView("team-a", "orders")
	f := &rbac{allowed: map[string]bool{key("jane", view): true}}
	a := NewAuthorizer(f, Options{TTL: 10 * time.Second})
	now := time.Now()
	a.now = func() time.Time { return now }

	for range 3 {
		if d := a.Allowed(t.Context(), jane, view); !d.Allowed || d.Err != nil {
			t.Fatalf("Allowed = %+v", d)
		}
	}
	// jane is allowed on orders by name only: the namespace-wide question
	// is asked once (denied, remembered for TTL), then the named one.
	if f.calls != 2 {
		t.Fatalf("calls = %d, want 2 (positive answers are cached)", f.calls)
	}
	other := ClusterView("team-a", "billing")
	for range 3 {
		if d := a.Allowed(t.Context(), jane, other); d.Allowed {
			t.Fatal("billing allowed")
		}
	}
	if f.calls != 5 {
		t.Fatalf("calls = %d, want 5 (no negative cache: a new grant shows on the next request)", f.calls)
	}
	// A revocation shows within the TTL.
	f.allowed[key("jane", view)] = false
	now = now.Add(10 * time.Second)
	if d := a.Allowed(t.Context(), jane, view); d.Allowed {
		t.Fatal("a revoked grant outlived the TTL")
	}
}

func TestCacheKeySeparatesSubjects(t *testing.T) {
	t.Parallel()
	view := ClusterView("team-a", "orders")
	f := &rbac{allowed: map[string]bool{key("jane", view): true}}
	a := NewAuthorizer(f, Options{})
	if !a.Allowed(t.Context(), jane, view).Allowed {
		t.Fatal("jane denied")
	}
	// Another user, and jane with other groups, are separate entries:
	// the cache can never hand one subject's answer to another.
	for _, u := range []User{
		{Name: "bob"},
		{Name: "jane", UID: "u1", Groups: []string{"system:authenticated"}},
		{Name: "jane", UID: "u2", Groups: jane.Groups, Extra: jane.Extra},
	} {
		before := f.calls
		a.Allowed(t.Context(), u, view)
		if f.calls != before+2 { // the namespace-wide question, then the named one
			t.Fatalf("%+v was answered from jane's cache entry", u)
		}
	}
	// Group and extra order do not matter.
	before := f.calls
	reordered := User{Name: "jane", UID: "u1", Groups: []string{"system:authenticated", "dba"}, Extra: map[string][]string{"scopes": {"a", "b"}}}
	if !a.Allowed(t.Context(), reordered, view).Allowed || f.calls != before {
		t.Fatal("a reordered group list missed the cache")
	}
}

func TestSARCarriesTheFullSubject(t *testing.T) {
	t.Parallel()
	f := &rbac{}
	a := NewAuthorizer(f, Options{})
	a.Allowed(t.Context(), jane, Attributes{Verb: "get", Path: "/metrics"})
	s := f.seen[0]
	if s.User != "jane" || s.UID != "u1" || !slices.Equal(s.Groups, jane.Groups) || s.Extra["scopes"][0] != "b" {
		t.Fatalf("SAR spec = %+v", s)
	}
	if s.NonResourceAttributes == nil || s.ResourceAttributes != nil || s.NonResourceAttributes.Path != "/metrics" {
		t.Fatalf("non-resource SAR = %+v", s)
	}
}

func TestErrorsAreUndecidedNotCached(t *testing.T) {
	t.Parallel()
	view := ClusterView("team-a", "orders")
	f := &rbac{allowed: map[string]bool{key("jane", view): true}, fail: map[string]bool{key("jane", view): true}}
	a := NewAuthorizer(f, Options{})
	d := a.Allowed(t.Context(), jane, view)
	if d.Allowed || d.Err == nil {
		t.Fatalf("an API failure must be undecided, not allowed: %+v", d)
	}
	f.fail = nil
	if d := a.Allowed(t.Context(), jane, view); !d.Allowed {
		t.Fatalf("after recovery = %+v", d)
	}
	if d := a.Allowed(t.Context(), User{}, view); d.Err == nil {
		t.Fatal("a question without a user was answered")
	}
}

func TestCheckIsParallelAndBounded(t *testing.T) {
	t.Parallel()
	f := &rbac{allowed: map[string]bool{}, delay: 20 * time.Millisecond}
	a := NewAuthorizer(f, Options{Workers: 4, PerUser: 4})
	ats := make([]Attributes, 0, 40)
	for i := range 40 {
		at := ClusterView("team-a", "c"+string(rune('a'+i%26))+string(rune('a'+i/26)))
		ats = append(ats, at)
		if i%2 == 0 {
			f.allowed[key("jane", at)] = true
		}
	}
	start := time.Now()
	ds := a.Check(t.Context(), jane, ats)
	elapsed := time.Since(start)
	for i, d := range ds {
		if d.Allowed != (i%2 == 0) {
			t.Fatalf("decision %d = %+v (order must follow input)", i, d)
		}
	}
	if p := f.peak.Load(); p > 4 || p < 2 {
		t.Fatalf("peak concurrency = %d, want 2..4", p)
	}
	if elapsed > 600*time.Millisecond {
		t.Fatalf("40 reviews took %s: not parallel", elapsed)
	}
}

func TestCapabilities(t *testing.T) {
	t.Parallel()
	ns, name := "team-a", "orders"
	viewer := map[string]bool{
		key("jane", ClusterView(ns, name)): true,
		key("jane", Attributes{Namespace: ns, Verb: "get", Resource: "pods", Subresource: "log"}): true,
	}
	operator := map[string]bool{}
	for k, v := range viewer {
		operator[k] = v
	}
	operator[key("jane", Attributes{Namespace: ns, Verb: "patch", Group: CNPGGroup, Resource: "clusters", Name: name})] = true
	operator[key("jane", Attributes{Namespace: ns, Verb: "patch", Group: CNPGGroup, Resource: "clusters", Subresource: "status", Name: name})] = true
	operator[key("jane", Attributes{Namespace: ns, Verb: "create", Group: CNPGGroup, Resource: "backups"})] = true
	almost := map[string]bool{}
	for k, v := range operator {
		almost[k] = v
	}
	delete(almost, key("jane", Attributes{Namespace: ns, Verb: "create", Group: CNPGGroup, Resource: "backups"}))

	cases := []struct {
		name string
		rbac map[string]bool
		fail map[string]bool
		want Capabilities
	}{
		{"nothing", nil, nil, Capabilities{}},
		{"viewer", viewer, nil, Capabilities{View: true, Logs: true}},
		{"operator", operator, nil, Capabilities{View: true, Logs: true, Operations: true}},
		{"operations need all three verbs", almost, nil, Capabilities{View: true, Logs: true}},
		{
			"undecided", viewer,
			map[string]bool{key("jane", ClusterView(ns, name)): true},
			Capabilities{Logs: true, Undecided: []string{"view"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := NewAuthorizer(&rbac{allowed: tc.rbac, fail: tc.fail}, Options{})
			got := a.ClusterCapabilities(t.Context(), jane, ns, name)
			if got.View != tc.want.View || got.Logs != tc.want.Logs || got.Operations != tc.want.Operations || !slices.Equal(got.Undecided, tc.want.Undecided) {
				t.Fatalf("capabilities = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestVisibleClusters(t *testing.T) {
	t.Parallel()
	refs := []ObjectRef{{"a", "one"}, {"a", "two"}, {"b", "three"}}
	f := &rbac{
		allowed: map[string]bool{key("jane", ClusterView("a", "one")): true, key("jane", ClusterView("b", "three")): true},
		fail:    map[string]bool{key("jane", ClusterView("a", "two")): true},
	}
	vis, und, err := NewAuthorizer(f, Options{}).VisibleClusters(t.Context(), jane, refs)
	if !slices.Equal(vis, []ObjectRef{{"a", "one"}, {"b", "three"}}) || !slices.Equal(und, []ObjectRef{{"a", "two"}}) || err == nil {
		t.Fatalf("visible=%v undecided=%v err=%v", vis, und, err)
	}
}

// The authorizer against a fake API server through a real clientset:
// the wire shape of the review and the console's own credential.
func TestAuthorizerAgainstFakeAPIServer(t *testing.T) {
	t.Parallel()
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/apis/authorization.k8s.io/v1/subjectaccessreviews" {
			http.NotFound(w, r)
			return
		}
		auth.Store(r.Header.Get("Authorization"))
		var sar authorizationv1.SubjectAccessReview
		if err := json.NewDecoder(r.Body).Decode(&sar); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ra := sar.Spec.ResourceAttributes
		sar.Status.Allowed = sar.Spec.User == "jane" && ra != nil && ra.Verb == "get" && ra.Group == CNPGGroup && ra.Name == "orders"
		sar.APIVersion, sar.Kind = "authorization.k8s.io/v1", "SubjectAccessReview"
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sar)
	}))
	defer srv.Close()
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL, BearerToken: "console-sa", ContentConfig: rest.ContentConfig{ContentType: "application/json"}})
	if err != nil {
		t.Fatal(err)
	}
	a := NewAuthorizer(sarCreator{cs}, Options{})
	if d := a.Allowed(t.Context(), jane, ClusterView("team-a", "orders")); !d.Allowed || d.Err != nil {
		t.Fatalf("orders = %+v", d)
	}
	if d := a.Allowed(t.Context(), jane, ClusterView("team-a", "billing")); d.Allowed {
		t.Fatal("billing allowed")
	}
	if got, _ := auth.Load().(string); got != "Bearer console-sa" {
		t.Fatalf("reviews must be sent as the console's ServiceAccount, got %q", got)
	}
}

type sarCreator struct{ cs kubernetes.Interface }

func (s sarCreator) Review(ctx context.Context, sar *authorizationv1.SubjectAccessReview) (*authorizationv1.SubjectAccessReview, error) {
	return s.cs.AuthorizationV1().SubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
}

func TestPerUserShareLeavesRoomForOthers(t *testing.T) {
	t.Parallel()
	f := &rbac{allowed: map[string]bool{}, delay: 30 * time.Millisecond}
	a := NewAuthorizer(f, Options{Workers: 8, PerUser: 2})
	ats := make([]Attributes, 0, 20)
	for i := range 20 {
		ats = append(ats, ClusterView("team-a", "c"+string(rune('a'+i))))
	}
	done := make(chan struct{})
	go func() { a.Check(t.Context(), User{Name: "noisy"}, ats); close(done) }()
	time.Sleep(10 * time.Millisecond)
	if p := f.peak.Load(); p > 2 {
		t.Fatalf("one user had %d reviews in flight, want at most 2", p)
	}
	start := time.Now()
	a.Allowed(t.Context(), jane, ClusterView("team-a", "orders"))
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("another user's check waited %s behind the noisy one", d)
	}
	<-done
}

// TestAllowedAsksTheNamespaceOnceForNamedObjects: a user allowed on every
// Cluster of a namespace costs one review for the namespace, however many
// Clusters it holds; a grant by name still works and a denied wide question
// never denies by itself.
func TestAllowedAsksTheNamespaceOnceForNamedObjects(t *testing.T) {
	t.Parallel()
	wide := ClusterView("team-a", "")
	f := &rbac{allowed: map[string]bool{
		key("jane", wide): true,
		key("bob", ClusterView("team-b", "orders")): true,
	}, fail: map[string]bool{key("eve", ClusterView("team-a", "")): true}}
	f.allowed[key("eve", ClusterView("team-a", "orders"))] = true
	a := NewAuthorizer(f, Options{TTL: 10 * time.Second})
	now := time.Now()
	a.now = func() time.Time { return now }

	for _, name := range []string{"orders", "billing", "ledger", "orders"} {
		if d := a.Allowed(t.Context(), jane, ClusterView("team-a", name)); !d.Allowed || d.Err != nil {
			t.Fatalf("jane on %s = %+v", name, d)
		}
	}
	if f.calls != 1 {
		t.Fatalf("calls = %d, want 1 (one namespace-wide review covers every Cluster)", f.calls)
	}
	if d := a.Allowed(t.Context(), jane, ClusterView("team-b", "orders")); d.Allowed {
		t.Fatal("a grant on team-a was used in team-b")
	}

	// bob is allowed on one Cluster by name: the wide question is denied
	// once per TTL, the named questions decide.
	before := f.calls
	if d := a.Allowed(t.Context(), bob, ClusterView("team-b", "orders")); !d.Allowed {
		t.Fatalf("a grant by name was refused: %+v", d)
	}
	if d := a.Allowed(t.Context(), bob, ClusterView("team-b", "billing")); d.Allowed {
		t.Fatal("a grant by name allowed another Cluster")
	}
	if f.calls != before+3 {
		t.Fatalf("calls = %d, want %d (one wide denial, then two named reviews)", f.calls, before+3)
	}

	// The wide question failing is not a denial: the named one decides.
	if d := a.Allowed(t.Context(), User{Name: "eve"}, ClusterView("team-a", "orders")); !d.Allowed || d.Err != nil {
		t.Fatalf("eve = %+v, want allowed by name after the wide review failed", d)
	}

	// A wide grant revoked is gone within TTL.
	f.allowed[key("jane", wide)] = false
	now = now.Add(10 * time.Second)
	if d := a.Allowed(t.Context(), jane, ClusterView("team-a", "billing")); d.Allowed {
		t.Fatal("a revoked namespace-wide grant outlived the TTL")
	}
}

var bob = User{Name: "bob"}

func TestCheckAsksEachNamespaceOnceThenOnlyTheDeniedNames(t *testing.T) {
	t.Parallel()
	f := &rbac{allowed: map[string]bool{
		key("jane", ClusterView("team-a", "")):       true,
		key("jane", ClusterView("team-b", "orders")): true,
	}}
	a := NewAuthorizer(f, Options{})
	ats := make([]Attributes, 0, 52)
	for i := range 50 {
		ats = append(ats, ClusterView("team-a", fmt.Sprintf("c-%02d", i)))
	}
	ats = append(ats, ClusterView("team-b", "orders"), ClusterView("team-b", "billing"))

	ds := a.Check(t.Context(), jane, ats)
	for i, d := range ds[:50] {
		if !d.Allowed {
			t.Fatalf("team-a cluster %d = %+v, want allowed by the namespace grant", i, d)
		}
	}
	if !ds[50].Allowed || ds[51].Allowed {
		t.Fatalf("team-b = %+v, %+v; want orders allowed by name, billing denied", ds[50], ds[51])
	}
	f.mu.Lock()
	calls := f.calls
	f.mu.Unlock()
	if calls != 4 {
		t.Fatalf("reviews = %d, want 4: one per namespace, then the two names of team-b", calls)
	}
}

// gateReviewer allows everything, but every review of the "slow"
// resource waits for the gate; entered, when set, is signalled as such a
// review starts (so a test knows the worker is held).
type gateReviewer struct {
	gate    chan struct{}
	entered chan struct{}
}

func (g gateReviewer) Review(ctx context.Context, sar *authorizationv1.SubjectAccessReview) (*authorizationv1.SubjectAccessReview, error) {
	if sar.Spec.ResourceAttributes != nil && sar.Spec.ResourceAttributes.Resource == "slow" {
		if g.entered != nil {
			g.entered <- struct{}{}
		}
		select {
		case <-g.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	sar.Status.Allowed = true
	return sar, nil
}

// TestJoinedQuestionSurvivesTheFirstCallerLeaving is the regression test of
// the shared-context bug: two callers share one review of the same
// question; the first one's request ends while the review still queues for
// a worker. The second caller must get the answer, not the first caller's
// cancellation.
func TestJoinedQuestionSurvivesTheFirstCallerLeaving(t *testing.T) {
	gate, entered := make(chan struct{}), make(chan struct{}, 1)
	a := NewAuthorizer(gateReviewer{gate: gate, entered: entered}, Options{Workers: 1})
	u := User{Name: "alice"}
	// Hold the only worker.
	go a.Allowed(context.Background(), u, Attributes{Verb: "get", Resource: "slow", Namespace: "x"})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow review never started")
	}

	q := Attributes{Verb: "get", Resource: "pods", Namespace: "x"}
	ctxA, cancelA := context.WithCancel(context.Background())
	resA := make(chan Decision, 1)
	go func() { resA <- a.Allowed(ctxA, u, q) }()
	if !waitFor(t, 5*time.Second, func() bool { return flightWaiters(context.Background(), a, u, q) == 1 }) {
		t.Fatal("the first caller never joined the review")
	}
	resB := make(chan Decision, 1)
	go func() { resB <- a.Allowed(context.Background(), u, q) }()
	if !waitFor(t, 5*time.Second, func() bool { return flightWaiters(context.Background(), a, u, q) == 2 }) {
		t.Fatal("the second caller never joined the review")
	}

	cancelA()
	// The leaving caller is answered at once, with its own cancellation.
	select {
	case d := <-resA:
		if !errors.Is(d.Err, context.Canceled) {
			t.Errorf("the cancelled caller got %+v, want its context's error", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the cancelled caller was not released")
	}
	// The leaving caller has given up its share before it returned.
	if got := flightWaiters(context.Background(), a, u, q); got != 1 {
		t.Fatalf("%d callers wait after the first left, want 1", got)
	}
	close(gate)
	select {
	case d := <-resB:
		if d.Err != nil || !d.Allowed {
			t.Fatalf("the remaining caller got %+v, want allowed", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the remaining caller was never answered")
	}
}

// TestSharedReviewIsStillBounded: the review shared by joined callers is
// detached from their cancellation, not from the review timeout, and its
// error is not cached.
func TestSharedReviewIsStillBounded(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	a := NewAuthorizer(gateReviewer{gate: gate}, Options{Timeout: 100 * time.Millisecond})
	u := User{Name: "alice"}
	slow := Attributes{Verb: "get", Resource: "slow", Namespace: "x"}
	d := a.Allowed(context.Background(), u, slow)
	if !errors.Is(d.Err, context.DeadlineExceeded) {
		t.Fatalf("a review past its timeout: %+v, want deadline exceeded", d)
	}
	if a.cached(cacheKey(u, slow)) {
		t.Error("an undecided question was cached")
	}
}

// blockingReviewer holds every review until its context ends, and counts
// the reviews it was asked and the ones that ended by cancellation.
type blockingReviewer struct {
	started, cancelled atomic.Int32
}

func (b *blockingReviewer) Review(ctx context.Context, _ *authorizationv1.SubjectAccessReview) (*authorizationv1.SubjectAccessReview, error) {
	b.started.Add(1)
	<-ctx.Done()
	b.cancelled.Add(1)
	return nil, ctx.Err()
}

// flightWaiters is the test's view of the number of callers waiting for
// the shared review of the question u asks with ctx's credential: 0 when
// no review of it is in flight.
func flightWaiters(ctx context.Context, a *Authorizer, u User, at Attributes) int {
	key := a.flightKey(ctx, cacheKey(u, at))
	a.flightMu.Lock()
	defer a.flightMu.Unlock()
	if f, ok := a.flights[key]; ok {
		return f.waiters
	}
	return 0
}

// waitFor polls condition until it holds or the deadline passes.
func waitFor(t *testing.T, within time.Duration, condition func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return condition()
}

// TestAbandonedQuestionsReleaseTheirWork: a client
// that aborts thousands of distinct questions must not leave their shared
// reviews queued for the subject's slots and the workers until the queue
// wait and the review timeout expire.
func TestAbandonedQuestionsReleaseTheirWork(t *testing.T) {
	reviewer := &blockingReviewer{}
	a := NewAuthorizer(reviewer, Options{Workers: 4, PerUser: 2, Timeout: time.Minute})
	baseline := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	const questions = 2000
	var callers sync.WaitGroup
	for index := range questions {
		callers.Go(func() {
			a.Allowed(ctx, User{Name: "mallory"}, Attributes{Verb: "get", Resource: fmt.Sprintf("r%d", index), Namespace: "x"})
		})
	}
	if !waitFor(t, 5*time.Second, func() bool { return reviewer.started.Load() == 2 }) {
		t.Fatalf("%d reviews started, want the subject's share of 2", reviewer.started.Load())
	}
	cancel()
	callers.Wait()
	if !waitFor(t, 5*time.Second, func() bool { return runtime.NumGoroutine() <= baseline+5 }) {
		t.Fatalf("%d goroutines remain after every caller left (baseline %d): the abandoned reviews still queue", runtime.NumGoroutine(), baseline)
	}
	if !waitFor(t, 5*time.Second, func() bool { return reviewer.cancelled.Load() == reviewer.started.Load() }) {
		t.Fatalf("%d of %d in-flight reviews were cancelled", reviewer.cancelled.Load(), reviewer.started.Load())
	}
	a.flightMu.Lock()
	remaining := len(a.flights)
	a.flightMu.Unlock()
	if remaining != 0 {
		t.Fatalf("%d flights remain registered", remaining)
	}
	// The worker pool is free again: a new question is reviewed at once.
	// (A queued review whose callers have not all noticed their
	// cancellation yet may still take a freed slot, so the count before is
	// read, not assumed.)
	before := reviewer.started.Load()
	fresh, stop := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer stop()
	a.Allowed(fresh, User{Name: "jane"}, Attributes{Verb: "get", Resource: "after", Namespace: "x"})
	if got := reviewer.started.Load(); got != before+1 {
		t.Fatalf("%d reviews started after %d, want the new question's to have started", got, before)
	}
}

type credentialKey struct{}

// tokenReviewer answers with the token in the context, as auth.SelfReviewer
// does: "expired" is rejected, any other token allowed. The review of the
// expired token waits for gate, so the valid one would join it.
type tokenReviewer struct {
	gate  chan struct{}
	calls atomic.Int32
}

var errRejected = errors.New("token rejected")

func (r *tokenReviewer) CredentialDigest(ctx context.Context) [32]byte {
	token, _ := ctx.Value(credentialKey{}).(string)
	return sha256.Sum256([]byte(token))
}

func (r *tokenReviewer) Review(ctx context.Context, sar *authorizationv1.SubjectAccessReview) (*authorizationv1.SubjectAccessReview, error) {
	r.calls.Add(1)
	if ctx.Value(credentialKey{}) == "expired" {
		<-r.gate
		return nil, errRejected
	}
	sar.Status.Allowed = true
	return sar, nil
}

// TestOneSessionsRejectedTokenStaysItsOwn: two
// sessions of one subject ask the same question at once; the first one's
// token is expired. The second must be answered with its own token.
func TestOneSessionsRejectedTokenStaysItsOwn(t *testing.T) {
	reviewer := &tokenReviewer{gate: make(chan struct{})}
	a := NewAuthorizer(reviewer, Options{})
	u, q := User{Name: "jane", UID: "u1"}, Attributes{Verb: "get", Resource: "pods", Namespace: "x"}
	expired := make(chan Decision, 1)
	go func() {
		expired <- a.Allowed(context.WithValue(context.Background(), credentialKey{}, "expired"), u, q)
	}()
	if !waitFor(t, 2*time.Second, func() bool { return reviewer.calls.Load() == 1 }) {
		t.Fatal("the expired session's review never started")
	}
	valid := a.Allowed(context.WithValue(context.Background(), credentialKey{}, "valid"), u, q)
	if valid.Err != nil || !valid.Allowed {
		t.Fatalf("the valid session got %+v, want allowed", valid)
	}
	close(reviewer.gate)
	if d := <-expired; !errors.Is(d.Err, errRejected) {
		t.Fatalf("the expired session got %+v, want its own rejection", d)
	}
	// One session's identical questions still share one review.
	reviewer.calls.Store(0)
	a = NewAuthorizer(reviewer, Options{})
	reviewer.gate = make(chan struct{})
	var joined sync.WaitGroup
	session := context.WithValue(context.Background(), credentialKey{}, "expired")
	for range 10 {
		joined.Go(func() { a.Allowed(session, u, q) })
	}
	if !waitFor(t, 5*time.Second, func() bool { return flightWaiters(session, a, u, q) == 10 }) {
		t.Fatalf("%d of ten identical questions joined the review", flightWaiters(session, a, u, q))
	}
	close(reviewer.gate)
	joined.Wait()
	if got := reviewer.calls.Load(); got != 1 {
		t.Fatalf("%d reviews for ten identical questions of one session, want 1", got)
	}
}
