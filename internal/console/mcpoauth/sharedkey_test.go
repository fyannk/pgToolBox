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

package mcpoauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/internal/console/auth"
)

// sharedProxy is TRUSTED_PROXY_HOPS without CIDRs: every client has the
// proxy's address, so no per-address budget may be charged.
func sharedReplica(t *testing.T) (*fakeSessions, *replica) {
	t.Helper()
	c := &clock{now: t0}
	f := newSessions(t, c, secretA)
	return f, newReplica(t, f, memoryStore(c), c, func(o *Options) { o.TrustedProxy = auth.ProxyTrust{Hops: 1} })
}

const proxyAddr = "10.128.0.5:4000"

// TestSharedAddressRegistrations: behind an untrusted proxy one sender
// cannot spend everyone's registration budget; the process-wide bucket
// still bounds a flood.
func TestSharedAddressRegistrations(t *testing.T) {
	t.Parallel()
	_, r := sharedReplica(t)
	body := `{"redirect_uris":["http://127.0.0.1/cb"]}`
	created, refused := 0, 0
	for range 4 * registrationsBurst {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, console+PathRegister, strings.NewReader(body))
		req.RemoteAddr = proxyAddr
		switch w := r.do(req); w.Code {
		case http.StatusCreated:
			created++
		case http.StatusTooManyRequests:
			refused++
		default:
			t.Fatalf("registration = %d %s", w.Code, w.Body)
		}
		if created+refused == 2*registersBurst && refused != 0 {
			t.Fatalf("%d registrations through the shared address refused after %d: the per-address budget was charged", refused, created)
		}
	}
	// The bucket refills at 10 a second; the loop takes far less than 5 s.
	if refused == 0 || created > registrationsBurst+50 {
		t.Fatalf("a flood of %d made %d registrations, want about the process-wide burst %d", 4*registrationsBurst, created, registrationsBurst)
	}
}

// TestSharedAddressTokenRequests: junk token requests from one sender
// behind an untrusted proxy do not throttle the others at the
// per-address budget; the process-wide bucket bounds them.
func TestSharedAddressTokenRequests(t *testing.T) {
	t.Parallel()
	_, r := sharedReplica(t)
	junk := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"junk"}, "client_id": {"junk"}}.Encode()
	throttledAt := -1
	for i := range 4 * tokenRequestsBurst {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, console+PathToken, strings.NewReader(junk))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = proxyAddr
		if r.do(req).Code == http.StatusTooManyRequests {
			throttledAt = i
			break
		}
	}
	if throttledAt >= 0 && throttledAt < tokenRequestsBurst {
		t.Fatalf("token request %d through the shared address throttled, before the process-wide burst %d", throttledAt, tokenRequestsBurst)
	}
	if throttledAt < 0 {
		t.Fatalf("%d junk token requests were never throttled", 4*tokenRequestsBurst)
	}
}

// TestJunkTokenRequestsDoNotStarveARefresh is round 5's P1-1 on the
// token endpoint: a flood of junk refresh tokens through the shared
// address, far past every bucket's burst, does not refuse a connected
// client's refresh. Junk spends the bucket of tokens that do not open;
// only a token this console sealed spends the redemption bucket. The
// negative: junk is still throttled once its own bucket is empty.
func TestJunkTokenRequestsDoNotStarveARefresh(t *testing.T) {
	t.Parallel()
	c := &clock{now: t0}
	f := newSessions(t, c, secretA)
	r := newReplica(t, f, memoryStore(c), c, func(o *Options) { o.TrustedProxy = auth.ProxyTrust{Hops: 1} })
	id, tok := r.connect(t)
	junk := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"pgc_rt_junk"}, "client_id": {id}}.Encode()
	throttled := 0
	for range 3 * (tokenRequestsBurst + unknownTokenRequestsBurst) {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, console+PathToken, strings.NewReader(junk))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = proxyAddr
		if r.do(req).Code == http.StatusTooManyRequests {
			throttled++
		}
	}
	if throttled == 0 {
		t.Fatal("a junk flood was never throttled")
	}
	form := refreshForm(id, tok.Refresh)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, console+PathToken, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = proxyAddr
	if w := r.do(req); w.Code != http.StatusOK {
		t.Fatalf("a connected client's refresh after the flood = %d, want 200: %s", w.Code, w.Body)
	}
}

// TestSharedAddressAuthorize: anonymous authorization requests through
// the shared address (they only reach the sign-in) do not throttle a
// signed-in person behind the same proxy.
func TestSharedAddressAuthorize(t *testing.T) {
	t.Parallel()
	f, r := sharedReplica(t)
	q := authorizeQuery(r.clientID(t), loopback)
	f.mu.Lock()
	sess := f.session
	f.session = nil
	f.mu.Unlock()
	for i := range 3 * requestsBurst {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, console+PathAuthorize+"?"+q.Encode(), nil)
		req.RemoteAddr = proxyAddr
		if w := r.do(req); w.Code == http.StatusTooManyRequests {
			t.Fatalf("anonymous authorization %d throttled: they cost nothing but a redirect", i)
		}
	}
	f.mu.Lock()
	f.session = sess
	f.mu.Unlock()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, console+PathAuthorize+"?"+q.Encode(), nil)
	req.RemoteAddr = proxyAddr
	if w := r.do(req); w.Code != http.StatusOK {
		t.Fatalf("a signed-in person behind the shared address = %d, want the consent page: %s", w.Code, w.Body)
	}
	// The person's own budget still applies.
	throttled := false
	for range 2 * requestsBurst {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, console+PathAuthorize+"?"+q.Encode(), nil)
		req.RemoteAddr = proxyAddr
		if r.do(req).Code == http.StatusTooManyRequests {
			throttled = true
			break
		}
	}
	if !throttled {
		t.Fatal("one person's authorization requests were never throttled")
	}
}
