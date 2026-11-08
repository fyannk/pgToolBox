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

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeSyncer is synced after a delay, or never.
type fakeSyncer struct {
	syncedAfter time.Duration
	never       bool
}

func (f fakeSyncer) WaitForCacheSync(ctx context.Context) bool {
	if f.never {
		<-ctx.Done()
		return false
	}
	select {
	case <-time.After(f.syncedAfter):
		return true
	case <-ctx.Done():
		return false
	}
}

// TestCacheSyncedCheck: readiness is the informer cache's sync, answered
// within the timeout either way — never Ping, never a hang.
func TestCacheSyncedCheck(t *testing.T) {
	tests := []struct {
		name  string
		cache fakeSyncer
		ready bool
	}{
		{"synced", fakeSyncer{}, true},
		{"syncing within the timeout", fakeSyncer{syncedAfter: 10 * time.Millisecond}, true},
		{"still syncing", fakeSyncer{syncedAfter: time.Hour}, false},
		{"never syncs (no RBAC, no API server)", fakeSyncer{never: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check := cacheSyncedCheck(tt.cache, 100*time.Millisecond)
			start := time.Now()
			err := check(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
			if (err == nil) != tt.ready {
				t.Fatalf("ready = %v, want %v (%v)", err == nil, tt.ready, err)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("the check took %s, over its timeout", elapsed)
			}
		})
	}
}
