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
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// RateLimiter is a token bucket per key (a client address), bounded in
// the number of keys it remembers. It protects the paths where an
// unauthenticated caller makes the console ask the API server something:
// every pasted token and every OAuth callback costs a SelfSubjectReview,
// every /metrics scrape one more.
type RateLimiter struct {
	rate  float64 // tokens per second
	burst float64
	max   int
	now   func() time.Time
	// throttled counts refusals (pgtoolbox_console_throttled_requests_total)
	// when the limiter is Named; nil counts nothing.
	throttled prometheus.Counter

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter allows burst requests per key, refilled at perMinute.
func NewRateLimiter(perMinute, burst, maxKeys int) *RateLimiter {
	return &RateLimiter{
		rate: float64(perMinute) / 60, burst: float64(burst), max: maxKeys,
		now: time.Now, buckets: map[string]*bucket{},
	}
}

// Named labels the limiter's refusals with bucket in
// pgtoolbox_console_throttled_requests_total. bucket is one of the
// Bucket* constants: the label set is fixed, never derived from a
// request. It returns l, for chaining after NewRateLimiter.
func (l *RateLimiter) Named(bucket string) *RateLimiter {
	l.throttled = throttledRequests.WithLabelValues(bucket)
	return l
}

// Allow takes one token for key.
func (l *RateLimiter) Allow(key string) bool {
	if l == nil {
		return true
	}
	if l.take(key) {
		return true
	}
	if l.throttled != nil {
		l.throttled.Inc()
	}
	return false
}

func (l *RateLimiter) take(key string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.max {
			// Forget full buckets first; when every key is mid-burst the
			// table is reset rather than grown: an address spray cannot
			// buy memory, only a fresh burst per address.
			for k, v := range l.buckets {
				if v.tokens+now.Sub(v.last).Seconds()*l.rate >= l.burst {
					delete(l.buckets, k)
				}
			}
			if len(l.buckets) >= l.max {
				clear(l.buckets)
			}
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// addressKey groups client addresses for budgets: IPv4 as is, IPv6 by
// its /64, which one client controls.
func addressKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	addr = addr.Unmap()
	if addr.Is4() {
		return addr.String()
	}
	p, err := addr.Prefix(64)
	if err != nil {
		return ip
	}
	return p.String()
}

// BudgetKey is the per-address budget key of r's client, and false when
// the address is shared by every client behind proxies the console does
// not trust (ProxyTrust.SharedClientKey): such a key must not be
// charged, since one sender would spend it for everyone. Callers bound
// that case per token, person or flow, and by a process-wide bucket.
func BudgetKey(r *http.Request, trust ProxyTrust) (string, bool) {
	if trust.SharedClientKey() {
		return "", false
	}
	return ClientKey(r, trust), true
}

// ClientKey is the rate-limit key of r's client, honouring the trusted
// proxies.
func ClientKey(r *http.Request, trust ProxyTrust) string {
	return addressKey(clientIP(r, trust))
}
