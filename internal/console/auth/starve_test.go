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
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// sharedModes are the proxy modes in which no per-address budget is
// charged: only the per-replica buckets stand between a flood and the
// genuine users behind the same address.
func sharedModes() []int {
	var out []int
	for i, m := range proxyModes {
		if m.shared {
			out = append(out, i)
		}
	}
	return out
}

// floods are the junk rates a test rides out: the round-5 reproduction
// (25 a second for 30 s) and one near the exchange and review buckets'
// refill (80 a second for 10 s).
var floods = []struct{ perSecond, seconds int }{{25, 30}, {80, 10}}

// TestJunkCallbackFloodDoesNotStarveSignIns is round 5's P1-1, ported
// from the security review's reproduction: behind a shared address one
// sender starts flows of its own and calls their callbacks with junk
// codes, many a second. Before the fix the junk spent the per-replica
// bucket that genuine sign-ins needed (20 a second) and 24 of 30 genuine
// sign-ins were refused. Every genuine sign-in now succeeds, and the
// junk codes cost no SelfSubjectReview: the review is charged only
// after a successful exchange, from a bucket junk never reaches.
func TestJunkCallbackFloodDoesNotStarveSignIns(t *testing.T) {
	t.Parallel()
	for _, i := range sharedModes() {
		mode := proxyModes[i]
		for _, fl := range floods {
			t.Run(fmt.Sprintf("%s/%d a second", mode.name, fl.perSecond), func(t *testing.T) {
				t.Parallel()
				env := newTestEnvWith(t, mode.tweak, ProviderOIDC)
				clock := &stepClock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
				useClock(clock, env.h.attempts, env.h.codeExchanges, env.h.exchangeReviews)
				attacker := env.browser(t)
				reviews := env.rev.calls
				refused, legit, n := 0, 0, 0
				for range fl.seconds {
					for range fl.perSecond {
						clock.Advance(time.Second / time.Duration(fl.perSecond))
						if w := junkCallback(attacker, mode.hdr, n); w.Code == http.StatusFound {
							t.Fatalf("junk callback %d signed in", n)
						}
						n++
					}
					w := legitCallback(env.browser(t), mode.hdr)
					legit++
					if w.Code != http.StatusFound {
						refused++
					}
				}
				if refused != 0 {
					t.Fatalf("%d of %d genuine sign-ins refused under %d junk callbacks a second", refused, legit, fl.perSecond)
				}
				if got := env.rev.calls - reviews; got != legit {
					t.Fatalf("%d SelfSubjectReviews for %d genuine sign-ins and %d junk callbacks, want one per genuine sign-in", got, legit, n)
				}
			})
		}
	}
}

// TestCodeExchangesStayBounded: the exchange bucket is larger, not
// gone. A burst of junk callbacks far above it makes at most its burst
// of IdP calls, the rest are refused before the exchange and counted in
// pgtoolbox_console_throttled_requests_total.
func TestCodeExchangesStayBounded(t *testing.T) {
	t.Parallel()
	env := newTestEnvWith(t, proxyModes[2].tweak, ProviderOIDC)
	clock := &stepClock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	useClock(clock, env.h.attempts, env.h.codeExchanges, env.h.exchangeReviews)
	throttled := testutil.ToFloat64(throttledRequests.WithLabelValues(BucketCodeExchanges))
	attacker := env.browser(t)
	exchanges := env.idp.tokens
	for i := range codeExchangesBurst + 50 {
		junkCallback(attacker, nil, i)
	}
	if got := env.idp.tokens - exchanges; got > codeExchangesBurst {
		t.Fatalf("%d junk callbacks made %d code exchanges, want at most %d", codeExchangesBurst+50, got, codeExchangesBurst)
	}
	if d := testutil.ToFloat64(throttledRequests.WithLabelValues(BucketCodeExchanges)) - throttled; d < 50 {
		t.Fatalf("throttled_requests_total{bucket=code_exchanges} rose by %v, want at least 50", d)
	}
}

// TestJunkTokenFloodDoesNotStarveTheTokenForm: the same flood on the
// token form, with distinct junk tokens (each has a budget of its own)
// from the shared address: every genuine pasted token still signs in.
func TestJunkTokenFloodDoesNotStarveTheTokenForm(t *testing.T) {
	t.Parallel()
	for _, i := range sharedModes() {
		mode := proxyModes[i]
		for _, fl := range floods {
			t.Run(fmt.Sprintf("%s/%d a second", mode.name, fl.perSecond), func(t *testing.T) {
				t.Parallel()
				env := newTestEnvWith(t, mode.tweak, ProviderToken)
				clock := &stepClock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
				useClock(clock, env.h.tokenAttempts, env.h.tokenAddresses, env.h.tokenReviews)
				attacker := env.browser(t)
				hdr := withHeaders(sameOriginForm, mode.hdr)
				refused, n := 0, 0
				for sec := range fl.seconds {
					for range fl.perSecond {
						clock.Advance(time.Second / time.Duration(fl.perSecond))
						attacker.do(http.MethodPost, "/auth/token", url.Values{"token": {fmt.Sprintf("junk-%d", n)}}, hdr)
						n++
					}
					w := env.browser(t).do(http.MethodPost, "/auth/token", url.Values{"token": {fmt.Sprintf("opaque-access-%d", sec)}}, hdr)
					if w.Code != http.StatusFound {
						refused++
					}
				}
				if refused != 0 {
					t.Fatalf("%d of %d genuine token sign-ins refused under %d junk tokens a second", refused, fl.seconds, fl.perSecond)
				}
			})
		}
	}
}

// TestJunkBearersDoNotFailAKnownReReview: an MCP client's raw token,
// accepted once, is re-reviewed when its 30 s cached answer ends. A
// flood of distinct junk bearers that empties the bucket of unknown
// tokens does not fail that re-review: it spends a bucket of its own.
// The negative: a valid token this replica never accepted is refused
// while the flood lasts, so the bucket really is empty.
func TestJunkBearersDoNotFailAKnownReReview(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderToken)
	cached := NewCachedReviewer(env.rev, 0, 0)
	cacheClock := &stepClock{now: time.Now()}
	cached.now = cacheClock.Now
	env.h.identity = cached
	frozen := &stepClock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	useClock(frozen, env.h.tokenReviews, env.h.knownReviews)
	if _, err := env.h.BearerPrincipal(t.Context(), "sa-token"); err != nil {
		t.Fatalf("a valid bearer: %v", err)
	}
	throttled := testutil.ToFloat64(throttledRequests.WithLabelValues(BucketTokenReviews))
	busy := 0
	for i := range tokenReviewsBurst + 20 {
		if _, err := env.h.BearerPrincipal(t.Context(), fmt.Sprintf("junk-%d", i)); errors.Is(err, ErrReviewsBusy) {
			busy++
		}
	}
	// The first review of sa-token, unknown then, spent one of the burst.
	if busy != 21 {
		t.Fatalf("%d junk bearers refused as busy, want 21 (the bucket's burst is %d, one spent by sa-token)", busy, tokenReviewsBurst)
	}
	if d := testutil.ToFloat64(throttledRequests.WithLabelValues(BucketTokenReviews)) - throttled; d < 20 {
		t.Fatalf("throttled_requests_total{bucket=token_reviews} rose by %v, want at least 20", d)
	}
	cacheClock.Advance(DefaultIdentityTTL + time.Second)
	if cached.Cached("sa-token") {
		t.Fatal("the cached answer did not expire")
	}
	reviews := env.rev.calls
	if _, err := env.h.BearerPrincipal(t.Context(), "sa-token"); err != nil {
		t.Fatalf("the known bearer's re-review under a junk flood: %v", err)
	}
	if env.rev.calls != reviews+1 {
		t.Fatal("the known bearer was not re-reviewed by the API server")
	}
	if _, err := env.h.BearerPrincipal(t.Context(), "opaque-access-new"); !errors.Is(err, ErrReviewsBusy) {
		t.Fatalf("a never-accepted token while the bucket is empty: %v, want ErrReviewsBusy", err)
	}
}

// TestARejectedBearerIsForgotten: a known bearer the API server stops
// accepting leaves the known set, so a stolen-then-revoked token cannot
// keep spending the known bucket.
func TestARejectedBearerIsForgotten(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderToken)
	if _, err := env.h.BearerPrincipal(t.Context(), "sa-token"); err != nil {
		t.Fatal(err)
	}
	if !env.h.knownBearers.known("sa-token", time.Now()) {
		t.Fatal("an accepted bearer is not remembered")
	}
	env.rev.mu.Lock()
	delete(env.rev.users, "sa-token")
	env.rev.mu.Unlock()
	if _, err := env.h.BearerPrincipal(t.Context(), "sa-token"); !errors.Is(err, ErrTokenRejected) {
		t.Fatalf("a rejected bearer: %v", err)
	}
	if env.h.knownBearers.known("sa-token", time.Now()) {
		t.Fatal("a rejected bearer is still remembered")
	}
}

// TestKnownTokensAreBounded: the set never grows past its bound; expired
// entries make room first, then the least recently used.
func TestKnownTokensAreBounded(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	k := newKnownTokens(3, 3)
	for i := range 3 {
		k.remember(fmt.Sprintf("t%d", i), fmt.Sprint("user", i), now.Add(time.Duration(i+1)*time.Minute), now)
	}
	later := now.Add(90 * time.Second) // t0 expired
	k.remember("t3", "user3", later.Add(time.Hour), later)
	if k.len() != 3 || !k.known("t3", later) || k.known("t0", later) || !k.known("t1", later) {
		t.Fatalf("after expiry: len %d, t3 known %v, t0 known %v; want 3, true, false", k.len(), k.known("t3", later), k.known("t0", later))
	}
	// None expired: the least recently used goes (t2: t1 and t3 were
	// just looked up).
	k.remember("t4", "user4", later.Add(time.Hour), later)
	if k.len() != 3 || !k.known("t4", later) || k.known("t2", later) || !k.known("t1", later) || !k.known("t3", later) {
		t.Fatalf("full set: len %d; want t1, t3, t4 kept and t2 evicted", k.len())
	}
	k.remember("expired", "user5", now, now)
	if k.known("expired", now) {
		t.Fatal("a token already expired was remembered")
	}
	k.forget("t4")
	if k.len() != 2 || k.known("t4", later) {
		t.Fatal("a forgotten token is still remembered")
	}
}

// TestOnePersonCannotFillTheKnownBearers is round 6's P2: one person
// minting thousands of ServiceAccount tokens filled the set of known
// bearers, so other clients' re-reviews fell back to the shared bucket.
// A person now keeps their own most recently used tokens only.
func TestOnePersonCannotFillTheKnownBearers(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	k := newKnownTokens(maxKnownBearers, maxKnownBearersPerSubject)
	k.remember("alice-token", "alice", now.Add(time.Hour), now)
	for i := range maxKnownBearers + 10 {
		k.remember(fmt.Sprint("mallory-", i), "system:serviceaccount:team-a:mallory", now.Add(time.Hour), now)
	}
	if k.len() != maxKnownBearersPerSubject+1 {
		t.Fatalf("known set holds %d tokens, want mallory's %d and alice's", k.len(), maxKnownBearersPerSubject)
	}
	if !k.known("alice-token", now) {
		t.Fatal("another person's token was evicted")
	}
	if !k.known(fmt.Sprint("mallory-", maxKnownBearers+9), now) || k.known("mallory-0", now) {
		t.Fatal("a person must keep their most recently used tokens")
	}
}

// TestHopsZeroBehindAPublicURL is round 5's P2-1: with no proxy
// declared but a PUBLIC_URL other clients reach (a clusterIP exposure
// behind an Ingress of one's own), the peer may be the Ingress
// controller, so its address is not taken for a client's.
func TestHopsZeroBehindAPublicURL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, url, hops, cidrs string
		shared                 bool
	}{
		{name: "public URL, no hops", url: "https://console.example.com", shared: true},
		{name: "public URL, hops 0 explicitly", url: "https://console.example.com", hops: "0", shared: true},
		{name: "loopback URL", url: "http://127.0.0.1:8080"},
		{name: "localhost URL", url: "http://localhost:8080"},
		{name: "no public URL", url: ""},
		{name: "hops and CIDRs", url: "https://console.example.com", hops: "1", cidrs: "10.0.0.0/8"},
		{name: "hops without CIDRs", url: "https://console.example.com", hops: "1", shared: true},
	} {
		env := map[string]string{"AUTH_PROVIDER": "token", "SESSION_KEY_FILE": "/k", "POD_NAMESPACE": "team-a"}
		if strings.HasPrefix(tc.url, "http://") {
			env["COOKIE_SECURE"] = "false"
		}
		for k, v := range map[string]string{"PUBLIC_URL": tc.url, "TRUSTED_PROXY_HOPS": tc.hops, "TRUSTED_PROXY_CIDRS": tc.cidrs} {
			if v != "" {
				env[k] = v
			}
		}
		cfg, err := LoadConfig(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := cfg.TrustedProxy.SharedClientKey(); got != tc.shared {
			t.Errorf("%s: SharedClientKey = %v, want %v", tc.name, got, tc.shared)
		}
		if got := cfg.Info()["trusted_proxy_shared"]; got != strconv.FormatBool(tc.shared) {
			t.Errorf("%s: config info trusted_proxy_shared = %q", tc.name, got)
		}
	}
}
