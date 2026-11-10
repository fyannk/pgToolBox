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

package mcp

import (
	"context"
	"net/http"
	"testing"

	"github.com/fyannk/pgToolBox/internal/console/api"
	"github.com/fyannk/pgToolBox/internal/console/auth"
)

// TestConfirmPageBehindASharedAddress: behind a proxy the console does
// not trust every client has the proxy's address (here every request
// comes from 127.0.0.1). Anonymous loads of the confirmation page must
// not spend the person's page budget; the person's own budget applies.
func TestConfirmPageBehindASharedAddress(t *testing.T) {
	t.Parallel()
	h, _ := writeHarness(t, func(o *Options, _ *api.Deps) { o.TrustedProxy = auth.ProxyTrust{Hops: 1} })
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(as string) int {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, h.url+PathConfirm+"?r=garbage", nil)
		if as != "" {
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: as})
		}
		res, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	for i := range 2 * pageViewsPerMinute {
		if code := get(""); code != http.StatusFound {
			t.Fatalf("anonymous load %d = %d, want the sign-in redirect", i, code)
		}
	}
	if code := get("dba"); code != http.StatusBadRequest {
		t.Fatalf("the person's load after anonymous ones = %d, want 400 (a forged link), not 429", code)
	}
	throttled := false
	for range 2 * pageViewsPerMinute {
		if get("dba") == http.StatusTooManyRequests {
			throttled = true
			break
		}
	}
	if !throttled {
		t.Fatal("one person's page loads were never throttled")
	}
}
