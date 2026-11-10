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
	"time"

	"github.com/fyannk/pgToolBox/internal/console/auth"
)

// tokenViaProxy posts a token request from the shared proxy address, where
// no per-address budget applies.
func (r *replica) tokenViaProxy(form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, console+PathToken, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = proxyAddr
	return r.do(req)
}

func sharedProxyReplica(t *testing.T) (*clock, *replica) {
	t.Helper()
	c := &clock{now: t0}
	f := newSessions(t, c, secretA)
	r := newReplica(t, f, memoryStore(c), c, func(o *Options) { o.TrustedProxy = auth.ProxyTrust{Hops: 1} })
	return c, r.unthrottled()
}

// A code once held (redeemed, then expired, or spent and its grant
// revoked) replayed in a loop behind a shared proxy spends the bucket of
// unknown tokens, never the one connected clients refresh from.
func TestReplayedCodeDoesNotStarveRefresh(t *testing.T) {
	for _, tc := range []struct {
		name    string
		advance time.Duration
	}{
		{"expired code", 10 * time.Minute},
		{"spent code within its minute", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, r := sharedProxyReplica(t)
			aid := r.clientID(t)
			code := r.consent(t, authorizeQuery(aid, loopback), "approve").Get("code")
			if w := r.tokenViaProxy(codeForm(aid, code)); w.Code != http.StatusOK {
				t.Fatalf("first exchange: %d %s", w.Code, w.Body)
			}
			vid, vtok := r.connect(t)
			c.Add(tc.advance)
			throttled := 0
			for range 5 * (tokenRequestsBurst + unknownTokenRequestsBurst) {
				if r.tokenViaProxy(codeForm(aid, code)).Code == http.StatusTooManyRequests {
					throttled++
				}
			}
			if throttled == 0 {
				t.Error("a replay flood was never throttled")
			}
			if w := r.tokenViaProxy(refreshForm(vid, vtok.Refresh)); w.Code != http.StatusOK {
				t.Fatalf("a connected client's refresh after a replay flood: %d %s", w.Code, w.Body)
			}
		})
	}
}

// A revoked grant's refresh token replayed in a loop is refused from the
// bucket of unknown tokens too.
func TestReplayedRefreshDoesNotStarveRefresh(t *testing.T) {
	_, r := sharedProxyReplica(t)
	aid, atok := r.connect(t)
	if w := r.tokenViaProxy(refreshForm(aid, atok.Refresh)); w.Code != http.StatusOK {
		t.Fatalf("first refresh: %d %s", w.Code, w.Body)
	}
	vid, vtok := r.connect(t)
	// atok.Refresh is now spent: its first replay revokes the grant, the
	// rest are refused as of a revoked grant.
	for range 5 * (tokenRequestsBurst + unknownTokenRequestsBurst) {
		r.tokenViaProxy(refreshForm(aid, atok.Refresh))
	}
	if w := r.tokenViaProxy(refreshForm(vid, vtok.Refresh)); w.Code != http.StatusOK {
		t.Fatalf("a connected client's refresh after a replay flood: %d %s", w.Code, w.Body)
	}
}

// One live grant rotating its own refresh token as fast as it can is
// bounded by its grant's bucket, before it empties the shared one.
func TestOneGrantCannotEmptyTheSharedBucket(t *testing.T) {
	c, r := sharedProxyReplica(t)
	aid, atok := r.connect(t)
	vid, vtok := r.connect(t)
	refresh, ok := atok.Refresh, 0
	for range 2 * tokenRequestsBurst {
		w, tok, _ := r.token(t, refreshForm(aid, refresh))
		if w.Code != http.StatusOK {
			if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "this grant") {
				t.Fatalf("refresh %d: %d %s", ok, w.Code, w.Body)
			}
			break
		}
		refresh = tok.Refresh
		ok++
	}
	if ok != grantTokenRequestsBurst-1 { // connect spent one on the code
		t.Errorf("a grant refreshed %d times in a burst, want %d", ok, grantTokenRequestsBurst-1)
	}
	if w := r.tokenViaProxy(refreshForm(vid, vtok.Refresh)); w.Code != http.StatusOK {
		t.Fatalf("another grant's refresh: %d %s", w.Code, w.Body)
	}
	// The grant's bucket refills on the console's clock.
	c.Add(time.Minute)
	if w, _, _ := r.token(t, refreshForm(aid, refresh)); w.Code != http.StatusOK {
		t.Fatalf("refresh after a minute: %d %s", w.Code, w.Body)
	}
}
