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
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync"
	"testing"
	"time"
)

// stepClock is a clock the limiters share, advanced by the test.
type stepClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stepClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func useClock(c *stepClock, ls ...*RateLimiter) {
	for _, l := range ls {
		l.now = c.Now
	}
}

// The three ways a console sees its clients. In every case here the
// attacker and the victim reach the console with one address key: the
// shared proxy (httptest's peer 192.0.2.1), or one forwarded address.
var proxyModes = []struct {
	name string
	// shared: TRUSTED_PROXY_HOPS without CIDRs, the key is the proxy's.
	shared bool
	tweak  func(*Config)
	hdr    map[string]string
}{
	{name: "direct, hops 0", tweak: nil},
	{
		name: "trusted proxy, one forwarded address",
		tweak: func(c *Config) {
			c.TrustedProxy = ProxyTrust{Hops: 1, CIDRs: []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32")}}
		},
		hdr: map[string]string{"X-Forwarded-For": "203.0.113.7"},
	},
	{
		name: "hops without CIDRs, the proxy's address shared", shared: true,
		tweak: func(c *Config) { c.TrustedProxy = ProxyTrust{Hops: 1} },
	},
}

func withHeaders(a, b map[string]string) map[string]string {
	out := maps.Clone(a)
	if out == nil {
		out = map[string]string{}
	}
	maps.Copy(out, b)
	return out
}

// junkCallback is the attack of round 4's P1-3: start a flow of one's
// own, which costs nothing, and call its callback with a junk code. The
// cookie and state match, so it passes every flow check once.
func junkCallback(b *browser, hdr map[string]string, i int) *httptest.ResponseRecorder {
	b.t.Helper()
	w := b.do(http.MethodGet, "/auth/oidc/login?rd=/clusters", nil, hdr)
	if w.Code != http.StatusFound {
		b.t.Fatalf("login start = %d", w.Code)
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		b.t.Fatal(err)
	}
	// Never authorised at the IdP: the exchange fails (PKCE mismatch).
	return b.do(http.MethodGet, fmt.Sprintf("/auth/oidc/callback?code=junk-%d&state=%s",
		i, url.QueryEscape(loc.Query().Get("state"))), nil, hdr)
}

func legitCallback(b *browser, hdr map[string]string) *httptest.ResponseRecorder {
	b.t.Helper()
	w := b.do(http.MethodGet, "/auth/oidc/login?rd=/clusters", nil, hdr)
	if w.Code != http.StatusFound {
		b.t.Fatalf("login start = %d", w.Code)
	}
	state := b.env.idp.authorize(b.t, w.Header().Get("Location"))
	return b.do(http.MethodGet, "/auth/oidc/callback?code=c1&state="+url.QueryEscape(state), nil, hdr)
}

// TestJunkCallbacksOnSelfStartedFlows: behind a proxy the console does
// not trust, junk callbacks on flows the attacker started himself no
// longer lock everyone out of OIDC sign-in, the process-wide bucket
// still bounds the code exchanges they cost, and a client address the
// console does know keeps its per-address budget.
func TestJunkCallbacksOnSelfStartedFlows(t *testing.T) {
	t.Parallel()
	for _, mode := range proxyModes {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			env := newTestEnvWith(t, mode.tweak, ProviderOIDC)
			clock := &stepClock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
			useClock(clock, env.h.attempts, env.h.codeExchanges)
			attacker := env.browser(t)
			// 50 junk callbacks at 20 a minute: twice the per-address refill,
			// far under the process-wide one.
			for i := range 50 {
				clock.Advance(3 * time.Second)
				if w := junkCallback(attacker, mode.hdr, i); w.Code == http.StatusFound {
					t.Fatalf("junk callback %d signed in", i)
				}
			}
			w := legitCallback(env.browser(t), mode.hdr)
			switch {
			case mode.shared && w.Code != http.StatusFound:
				t.Fatalf("legitimate sign-in behind the shared address = %d, want 302: %s", w.Code, w.Body)
			case !mode.shared && w.Code != http.StatusTooManyRequests:
				t.Fatalf("same client address over its budget = %d, want 429 (per-address budget kept)", w.Code)
			}

			// A flood with the clock stopped: the code exchanges stop at the
			// process-wide burst, whatever the address budgets.
			clock.Advance(time.Hour)
			exchanges := env.idp.tokens
			refused := 0
			for i := range 3 * codeExchangesBurst {
				b := env.browser(t)
				if !mode.shared {
					b.addr = fmt.Sprintf("10.2.%d.%d:4711", i/256, i%256)
				}
				hdr := mode.hdr
				if mode.hdr != nil {
					hdr = withHeaders(mode.hdr, map[string]string{"X-Forwarded-For": fmt.Sprintf("198.18.%d.%d", i/256, i%256)})
				}
				if junkCallback(b, hdr, i).Code == http.StatusTooManyRequests {
					refused++
				}
			}
			if n := env.idp.tokens - exchanges; n > codeExchangesBurst {
				t.Fatalf("%d junk callbacks made %d code exchanges, want at most %d", 3*codeExchangesBurst, n, codeExchangesBurst)
			}
			if refused != 2*codeExchangesBurst {
				t.Fatalf("%d refused, want %d", refused, 2*codeExchangesBurst)
			}
		})
	}
}

// TestJunkTokensBehindASharedAddress: on the token form, one sender of
// junk tokens behind an untrusted proxy cannot lock the others out; the
// process-wide bucket alone bounds what the flood costs. A known client
// address keeps its per-address budget.
func TestJunkTokensBehindASharedAddress(t *testing.T) {
	t.Parallel()
	for _, mode := range proxyModes {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			env := newTestEnvWith(t, mode.tweak, ProviderToken)
			clock := &stepClock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
			useClock(clock, env.h.tokenAttempts, env.h.tokenAddresses, env.h.tokenReviews)
			hdr := withHeaders(sameOriginForm, mode.hdr)
			attacker := env.browser(t)
			// 60 distinct junk tokens at one a second: three times the
			// per-address refill, far under the process-wide one.
			for i := range 60 {
				clock.Advance(time.Second)
				w := attacker.do(http.MethodPost, "/auth/token", url.Values{"token": {fmt.Sprintf("junk-%d", i)}}, hdr)
				if w.Code == http.StatusFound {
					t.Fatalf("junk token %d signed in", i)
				}
			}
			w := env.browser(t).do(http.MethodPost, "/auth/token", url.Values{"token": {"sa-token"}}, hdr)
			switch {
			case mode.shared && w.Code != http.StatusFound:
				t.Fatalf("legitimate token behind the shared address = %d, want 302: %s", w.Code, w.Body)
			case !mode.shared && w.Code != http.StatusTooManyRequests:
				t.Fatalf("same client address over its budget = %d, want 429 (per-address budget kept)", w.Code)
			}

			if !mode.shared {
				return
			}
			// The flood with the clock stopped: at most the shared burst of
			// SelfSubjectReviews.
			clock.Advance(time.Hour)
			reviews := env.rev.calls
			for i := range 3 * tokenReviewsBurst {
				attacker.do(http.MethodPost, "/auth/token", url.Values{"token": {fmt.Sprintf("flood-%d", i)}}, hdr)
			}
			if n := env.rev.calls - reviews; n > tokenReviewsBurst {
				t.Fatalf("a junk-token flood made %d reviews, want at most %d", n, tokenReviewsBurst)
			}
		})
	}
}

func TestSharedClientKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		trust ProxyTrust
		want  bool
	}{
		{ProxyTrust{}, false},
		{ProxyTrust{Hops: 1}, true},
		{ProxyTrust{Hops: 2, CIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}, false},
		{ProxyTrust{CIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}, false},
	} {
		if got := tc.trust.SharedClientKey(); got != tc.want {
			t.Errorf("%+v.SharedClientKey() = %v, want %v", tc.trust, got, tc.want)
		}
	}
}
