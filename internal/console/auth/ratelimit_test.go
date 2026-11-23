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
	"testing"
	"time"
)

// A client that fills the table with fresh addresses must not reset its own
// throttle (#111): the table evicts the least recently used bucket, and
// never a bucket that is still in use.
func TestRateLimiterFullTableKeepsThrottledBuckets(t *testing.T) {
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	l := NewRateLimiter(1, 2, 4).Clock(func() time.Time { return now })

	for range 2 {
		if !l.Allow("victim") {
			t.Fatal("the burst is refused")
		}
	}
	if l.Allow("victim") {
		t.Fatal("over the burst is allowed")
	}
	// The sprayer spends one token per address, so none of its buckets is
	// full and evicting full buckets frees nothing.
	for i := range 20 {
		now = now.Add(time.Millisecond)
		l.Allow(fmt.Sprintf("spray-%d", i))
		l.Allow("victim") // still trying: its bucket stays the most recent
	}
	if l.Allow("victim") {
		t.Fatal("a table full of sprayed addresses reset the throttled bucket")
	}
	if n := len(l.buckets); n > 4 {
		t.Fatalf("the table holds %d keys, want at most 4", n)
	}
}

// Full buckets are forgotten before any bucket in use.
func TestRateLimiterFullTableForgetsFullBucketsFirst(t *testing.T) {
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	l := NewRateLimiter(60, 1, 2).Clock(func() time.Time { return now })
	l.Allow("a") // spent, refills in a second
	now = now.Add(2 * time.Second)
	l.Allow("b") // spent now; a has refilled
	l.Allow("c") // table full: a goes, b stays
	if _, ok := l.buckets["b"]; !ok {
		t.Fatal("the bucket in use was evicted before the full one")
	}
	if _, ok := l.buckets["a"]; ok {
		t.Fatal("the full bucket was kept")
	}
}
