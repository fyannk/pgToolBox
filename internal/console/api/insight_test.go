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

package api

import (
	"testing"
	"time"
)

// TestRefreshLimiter: each user has refreshRequests on-demand reads per
// window, across Clusters; one user's use does not spend another's.
func TestRefreshLimiter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	l := newRefreshLimiter(func() time.Time { return now })
	for i := range refreshRequests {
		if !l.allow("alice") {
			t.Fatalf("read %d refused under the limit", i+1)
		}
	}
	if l.allow("alice") {
		t.Fatal("a read over the limit was allowed")
	}
	if !l.allow("bob") {
		t.Fatal("another user's limit was spent")
	}
	now = now.Add(refreshWindow)
	if !l.allow("alice") {
		t.Fatal("the next window must allow reads again")
	}
	// The users tracked are bounded; expired windows make room.
	for i := range maxRefreshUsers {
		l.allow(time.Duration(i).String())
	}
	if len(l.m) > maxRefreshUsers {
		t.Fatalf("%d users tracked", len(l.m))
	}
	now = now.Add(refreshWindow)
	if !l.allow("carol") || len(l.m) > maxRefreshUsers {
		t.Fatalf("expired windows must make room: %d users", len(l.m))
	}
}
