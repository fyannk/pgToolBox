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

package mcpauth

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/fyannk/pgToolBox/internal/console/auth"
)

// TestSharedAddressBearers: behind a proxy the console does not trust
// (hops without CIDRs) every client has the proxy's address. Junk
// bearers from one client must not lock out another's first use of a
// good token; each bearer keeps a budget of its own.
func TestSharedAddressBearers(t *testing.T) {
	t.Parallel()
	env := newGuardEnv(t, func(o *Options) {
		o.Attempts = auth.NewRateLimiter(1, 2, 100)
		o.TrustedProxy = auth.ProxyTrust{Hops: 1}
	})
	for i := range 10 {
		if w := env.do(t, bearerHdr(fmt.Sprintf("forged-%d", i))); w.Code != http.StatusUnauthorized {
			t.Fatalf("junk bearer %d = %d, want 401 (no shared address budget)", i, w.Code)
		}
	}
	if w := env.do(t, bearerHdr("good-dba")); w.Code != http.StatusOK {
		t.Fatalf("a good token behind the shared address after the junk = %d, want 200", w.Code)
	}
	// One bearer retried is still bounded.
	codes := make([]int, 0, 3)
	for range 3 {
		codes = append(codes, env.do(t, bearerHdr("forged-0")).Code)
	}
	if codes[2] != http.StatusTooManyRequests {
		t.Fatalf("one junk bearer retried = %v, want the last over its own budget", codes)
	}
}
