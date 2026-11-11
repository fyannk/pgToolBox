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
	"maps"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

func prefixes(t *testing.T, cidrs ...string) []netip.Prefix {
	t.Helper()
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		out = append(out, netip.MustParsePrefix(c))
	}
	return out
}

func TestClientIP(t *testing.T) {
	t.Parallel()
	router := []string{"10.128.0.0/14"}
	anyPeer := []string{"0.0.0.0/0", "::/0"}
	for _, tc := range []struct {
		name  string
		peer  string
		xff   []string
		hops  int
		cidrs []string
		want  string
	}{
		{name: "no hops: the peer", peer: "10.128.0.5:4000", xff: []string{"203.0.113.7"}, cidrs: router, want: "10.128.0.5"},
		{name: "hops without CIDRs: the header is ignored", peer: "10.244.3.17:4000", xff: []string{"198.18.0.1"}, hops: 1, want: "10.244.3.17"},
		{name: "untrusted peer: its header is ignored", peer: "10.244.3.17:4000", xff: []string{"198.18.0.1"}, hops: 1, cidrs: router, want: "10.244.3.17"},
		{name: "trusted peer: the proxy's entry", peer: "10.128.0.5:4000", xff: []string{"203.0.113.7"}, hops: 1, cidrs: router, want: "203.0.113.7"},
		{name: "trusted peer: entries the client wrote are skipped", peer: "10.128.0.5:4000", xff: []string{"1.2.3.4, 203.0.113.7"}, hops: 1, cidrs: router, want: "203.0.113.7"},
		{name: "two hops: a trusted inner proxy is walked past", peer: "10.128.0.5:4000", xff: []string{"9.9.9.9, 203.0.113.7, 10.129.0.9"}, hops: 2, cidrs: router, want: "203.0.113.7"},
		{name: "two hops: the walk stops at the first untrusted entry", peer: "10.128.0.5:4000", xff: []string{"9.9.9.9", "203.0.113.7"}, hops: 2, cidrs: router, want: "203.0.113.7"},
		{name: "any peer trusted: the hops-th entry", peer: "10.244.3.17:4000", xff: []string{"9.9.9.9, 203.0.113.7, 198.51.100.2"}, hops: 2, cidrs: anyPeer, want: "203.0.113.7"},
		{name: "short chain: the last trusted hop", peer: "10.128.0.5:4000", hops: 1, cidrs: router, want: "10.128.0.5"},
		{name: "garbage entry: the last trusted hop", peer: "10.128.0.5:4000", xff: []string{"evil"}, hops: 1, cidrs: router, want: "10.128.0.5"},
		{name: "entry with a port", peer: "10.128.0.5:4000", xff: []string{"[2001:db8::1]:443"}, hops: 1, cidrs: router, want: "2001:db8::1"},
		{name: "IPv6 peer", peer: "[fd00::5]:4000", xff: []string{"203.0.113.7"}, hops: 1, cidrs: []string{"fd00::/64"}, want: "203.0.113.7"},
		{name: "IPv4-mapped peer", peer: "[::ffff:10.128.0.5]:4000", xff: []string{"203.0.113.7"}, hops: 1, cidrs: router, want: "203.0.113.7"},
		{name: "unparsable peer is returned as is", peer: "pipe", xff: []string{"203.0.113.7"}, hops: 1, cidrs: anyPeer, want: "pipe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			r.RemoteAddr = tc.peer
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := clientIP(r, ProxyTrust{Hops: tc.hops, CIDRs: prefixes(t, tc.cidrs...)}); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseProxyCIDRs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in, want string
		err      bool
	}{
		{in: "10.128.0.0/14", want: "10.128.0.0/14"},
		{in: "10.128.3.4/14, 10.0.0.1", want: "10.128.0.0/14,10.0.0.1/32"},
		{in: "0.0.0.0/0 ::/0", want: "0.0.0.0/0,::/0"},
		{in: "::ffff:10.0.0.0/104", want: "10.0.0.0/8"},
		{in: "10.0.0.0/8,10.0.0.0/8", want: "10.0.0.0/8"},
		{in: "fe80::1%eth0/64", err: true},
		{in: "::ffff:0.0.0.0/80", err: true},
		{in: "router", err: true},
		{in: "10.0.0.0/33", err: true},
		{in: strings.Repeat("10.0.0.1,", 0) + func() string {
			parts := make([]string, maxProxyCIDRs+1)
			for i := range parts {
				parts[i] = fmt.Sprintf("10.0.%d.0/24", i)
			}
			return strings.Join(parts, ",")
		}(), err: true},
	} {
		got, err := ParseProxyCIDRs(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("ParseProxyCIDRs(%q) = %v, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseProxyCIDRs(%q): %v", tc.in, err)
			continue
		}
		if s := (ProxyTrust{CIDRs: got}).String(); s != tc.want {
			t.Errorf("ParseProxyCIDRs(%q) = %s, want %s", tc.in, s, tc.want)
		}
	}
}

func TestLoadConfigTrustedProxy(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"AUTH_PROVIDER": "token", "SESSION_KEY_FILE": "/k", "POD_NAMESPACE": "team-a",
		"TRUSTED_PROXY_HOPS": "2", "TRUSTED_PROXY_CIDRS": "10.128.0.0/14,fd00::/64",
	}
	cfg, err := LoadConfig(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TrustedProxy.Hops != 2 || cfg.TrustedProxy.String() != "10.128.0.0/14,fd00::/64" {
		t.Fatalf("TrustedProxy = %+v", cfg.TrustedProxy)
	}
	if got := cfg.Info()["trusted_proxy_cidrs"]; got != "10.128.0.0/14,fd00::/64" {
		t.Fatalf("config info trusted_proxy_cidrs = %q", got)
	}
	env["TRUSTED_PROXY_CIDRS"] = "the-router"
	if _, err := LoadConfig(func(k string) (string, bool) { v, ok := env[k]; return v, ok }); err == nil || !strings.Contains(err.Error(), "TRUSTED_PROXY_CIDRS") {
		t.Fatalf("a bad CIDR = %v, want an error naming TRUSTED_PROXY_CIDRS", err)
	}
}

func TestConfigWarnings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		trust ProxyTrust
		url   string
		want  string
	}{
		{name: "hops without CIDRs", trust: ProxyTrust{Hops: 1}, url: "https://c.example", want: "X-Forwarded-For is ignored"},
		{name: "no hops behind a public URL", url: "https://c.example", want: "treated as shared"},
		{name: "no hops on loopback", url: "http://localhost:8080"},
		{name: "no hops on a loopback address", url: "http://127.0.0.1:8080"},
		{name: "no hops and no public URL", url: ""},
		{name: "hops with CIDRs", trust: ProxyTrust{Hops: 1, CIDRs: prefixes(t, "10.0.0.0/8")}, url: "https://c.example"},
	} {
		c := Config{TrustedProxy: tc.trust, PublicURL: tc.url}
		got := c.Warnings()
		if tc.want == "" {
			if len(got) != 0 {
				t.Errorf("%s: warnings %q, want none", tc.name, got)
			}
			continue
		}
		if !slices.ContainsFunc(got, func(w string) bool { return strings.Contains(w, tc.want) }) {
			t.Errorf("%s: warnings %q, want one with %q", tc.name, got, tc.want)
		}
	}
}

// TestSpoofedForwardedForFromAnUntrustedPeer is round 3's P1-5: a Pod
// that reaches the console's Service directly, past the router, names a
// fresh X-Forwarded-For on every request. Its header is not believed, so
// its junk tokens stop at its own address's budget and leave the shared
// review bucket to a real client coming through the router.
func TestSpoofedForwardedForFromAnUntrustedPeer(t *testing.T) {
	t.Parallel()
	env := newTestEnvWith(t, func(c *Config) {
		c.TrustedProxy = ProxyTrust{Hops: 1, CIDRs: prefixes(t, "10.128.0.0/14")}
	}, ProviderToken)
	// A frozen clock: at 20 a minute the address budget refills a token
	// every 3 s, which a slow -race run would otherwise spend.
	useClock(&stepClock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}, env.h.tokenAttempts, env.h.tokenAddresses, env.h.tokenReviews)
	attacker := env.browser(t)
	attacker.addr = "10.244.3.17:40000"
	reviews := env.rev.calls
	for i := range 3 * tokenReviewsBurst {
		hdr := maps.Clone(sameOriginForm)
		hdr["X-Forwarded-For"] = fmt.Sprintf("198.18.%d.%d", i/256, i%256)
		attacker.do(http.MethodPost, "/auth/token", url.Values{"token": {fmt.Sprintf("junk-%d", i)}}, hdr)
	}
	if n := env.rev.calls - reviews; n > tokenAddressBurst {
		t.Fatalf("one peer forging X-Forwarded-For made %d TokenReviews, want at most its address budget %d", n, tokenAddressBurst)
	}
	victim := env.browser(t)
	victim.addr = "10.128.0.5:5000" // the router
	hdr := maps.Clone(sameOriginForm)
	hdr["X-Forwarded-For"] = "192.0.2.10"
	if w := victim.do(http.MethodPost, "/auth/token", url.Values{"token": {"sa-token"}}, hdr); w.Code != http.StatusFound {
		t.Fatalf("a valid token through the router after the spray = %d, want 302: %s", w.Code, w.Body)
	}
	if !strings.Contains(env.logs.String(), `"client_ip":"10.244.3.17"`) {
		t.Fatalf("the audit trail names a forged address, want the peer's: %s", env.logs)
	}
	if strings.Contains(env.logs.String(), `"client_ip":"198.18.`) {
		t.Fatal("the audit trail carries a forged client_ip")
	}
}

// TestForwardedForFromATrustedProxyIsHonoured: through the router every
// forwarded address has a budget of its own, so more tokens than one
// address's budget all reach the API server.
func TestForwardedForFromATrustedProxyIsHonoured(t *testing.T) {
	t.Parallel()
	env := newTestEnvWith(t, func(c *Config) {
		c.TrustedProxy = ProxyTrust{Hops: 1, CIDRs: prefixes(t, "10.128.0.0/14")}
	}, ProviderToken)
	b := env.browser(t)
	b.addr = "10.128.0.5:5000"
	for i := range tokenAddressBurst + 1 {
		hdr := maps.Clone(sameOriginForm)
		hdr["X-Forwarded-For"] = fmt.Sprintf("203.0.113.%d", i)
		// Distinct junk tokens, so that only the address budget applies:
		// each is refused by the API server, none by a budget.
		if w := b.do(http.MethodPost, "/auth/token", url.Values{"token": {fmt.Sprintf("junk-%d", i)}}, hdr); w.Code != http.StatusForbidden {
			t.Fatalf("token %d from its own forwarded address = %d, want 403", i, w.Code)
		}
	}
	if !strings.Contains(env.logs.String(), `"client_ip":"203.0.113.3"`) {
		t.Fatalf("the audit trail does not name the forwarded client: %s", env.logs)
	}
}

// TestBearerReviewsShareTheProcessBucket: raw bearers the MCP endpoint
// presents spend the token form's process-wide review bucket, so a spray
// of distinct junk bearers (each from an address of its own) cannot make
// the API server review more than the bucket allows; a bearer whose
// answer is cached spends nothing.
func TestBearerReviewsShareTheProcessBucket(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, ProviderToken)
	cached := NewCachedReviewer(env.rev, 0, 0)
	env.h.identity = cached
	// The bucket's clock stands still: at 100 a second a slow run would
	// refill it during the spray.
	useClock(&stepClock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}, env.h.tokenReviews)
	if _, err := env.h.BearerPrincipal(t.Context(), "sa-token"); err != nil {
		t.Fatalf("a valid bearer: %v", err)
	}
	reviews := env.rev.calls
	busy := 0
	for i := range 3 * tokenReviewsBurst {
		_, err := env.h.BearerPrincipal(t.Context(), fmt.Sprintf("junk-%d", i))
		if errors.Is(err, ErrReviewsBusy) {
			busy++
		}
	}
	if n := env.rev.calls - reviews; n > tokenReviewsBurst+1 {
		t.Fatalf("distinct junk bearers made %d reviews, want at most the bucket's burst %d (+1 refill)", n, tokenReviewsBurst)
	}
	if busy == 0 {
		t.Fatal("no bearer was refused with ErrReviewsBusy")
	}
	if _, err := env.h.BearerPrincipal(t.Context(), "sa-token"); err != nil {
		t.Fatalf("a cached bearer while the bucket is empty: %v", err)
	}
	// The form shares the bucket: it is empty for a pasted token too.
	if w := env.browser(t).do(http.MethodPost, "/auth/token", url.Values{"token": {"fresh-token"}}, sameOriginForm); w.Code != http.StatusTooManyRequests {
		t.Fatalf("the token form after a bearer spray = %d, want 429", w.Code)
	}
}

// FuzzClientIP: X-Forwarded-For is attacker-written. Whatever it holds,
// a peer outside the trusted CIDRs is the client, and otherwise the
// answer is the peer or an address taken from the header.
func FuzzClientIP(f *testing.F) {
	f.Add("10.128.0.5:4000", "203.0.113.7", 1)
	f.Add("10.244.3.17:4000", "198.18.0.1, 10.129.0.9", 2)
	f.Add("[::ffff:10.128.0.5]:1", "[2001:db8::1]:443,evil,,", 8)
	trust := ProxyTrust{CIDRs: []netip.Prefix{netip.MustParsePrefix("10.128.0.0/14")}}
	f.Fuzz(func(t *testing.T, peer, xff string, hops int) {
		trust := trust
		trust.Hops = hops
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		r.RemoteAddr = peer
		r.Header.Set("X-Forwarded-For", xff)
		got := clientIP(r, trust)
		untrusted := clientIP(r, ProxyTrust{Hops: hops})
		if p, ok := parseHop(peer); ok && (!trust.Enabled() || !trust.trusts(p)) && got != untrusted {
			t.Fatalf("untrusted peer %q: clientIP = %q, want the peer %q", peer, got, untrusted)
		}
		if got == untrusted {
			return
		}
		if _, err := netip.ParseAddr(got); err != nil {
			t.Fatalf("clientIP = %q, not an address", got)
		}
		if !strings.Contains(xff, strings.TrimPrefix(got, "::ffff:")) && !strings.Contains(strings.ToLower(xff), got) {
			// The answer is normalised (unmapped, lower case, zone dropped);
			// it must still come from the header.
			if a, ok := parseHop(got); !ok || !slices.ContainsFunc(strings.Split(xff, ","), func(e string) bool {
				h, ok := parseHop(e)
				return ok && h == a
			}) {
				t.Fatalf("clientIP = %q, neither the peer nor an entry of %q", got, xff)
			}
		}
	})
}
