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

package sharedstate

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// A failed write-through rolls back its reservation when the ConfigMap
// holds only an expired copy of the code (the pruner dropped it here,
// its write failed): an expired entry is no spend another replica made.
// Kept, the reservation would refuse the retry as spent and the next
// write would put the code back.
func TestAFailedSpendOverAnExpiredBaseCopyLeavesNothing(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	alice := uh("alice")
	a := loaded(t, api, clock)
	if err := a.SpendCode(t.Context(), "confirm-1", alice, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	hc := &hookClient{apiServer: api}
	b := newStore(hc, clock, nil)
	watchMerge(t, api, b)
	b.mu.Lock()
	b.setModeLocked(modeLive, "")
	b.mu.Unlock()
	clock.Add(2 * time.Minute)
	b.mu.Lock()
	b.pruneGrantsLocked(clock.Now().Unix()) // the pruner ran; its write failed
	b.mu.Unlock()
	hc.beforeUpdate = func() error { return context.Canceled }
	until := clock.Now().Add(time.Minute)
	if err := b.SpendCode(t.Context(), "confirm-1", alice, until); !errors.Is(err, ErrNotRecorded) {
		t.Fatalf("the failed write: %v, want ErrNotRecorded", err)
	}
	b.mu.Lock()
	_, kept := b.codes[codeHash("confirm-1")]
	b.setModeLocked(modeLive, "")
	b.mu.Unlock()
	if kept {
		t.Fatal("ErrNotRecorded left the reservation behind")
	}
	if b.CodeUsed("confirm-1") {
		t.Fatal("the value is refused as spent after ErrNotRecorded")
	}
	if err := b.SpendCode(t.Context(), "confirm-1", alice, until); err != nil {
		t.Fatalf("the retry: %v, want nil", err)
	}
}

// The same for a mark: an expired copy of the code in the ConfigMap
// carrying the mark does not keep a mark whose write failed.
func TestAFailedMarkOverAnExpiredBaseCopyLeavesNothing(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	alice := uh("alice")
	h := codeHash("confirm-1")
	// The ConfigMap holds an earlier, expired spend of the value, marked.
	old, err := json.Marshal(grantsDoc{Version: formatVersion, Grants: map[string]grantEntry{}, Codes: map[string]codeEntry{
		h: {Expiry: t0.Add(-time.Minute).Unix(), User: alice, Marks: []string{"run"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	api.cm.Data = map[string]string{KeyGrants: string(old)}
	hc := &hookClient{apiServer: api}
	b := newStore(hc, clock, nil)
	watchMerge(t, api, b)
	// This replica holds the value spent again, unmarked, not yet
	// written (the prune of the expired copy and the new spend).
	b.mu.Lock()
	b.setModeLocked(modeLive, "")
	b.codes[h] = codeEntry{Expiry: t0.Add(time.Minute).Unix(), User: alice}
	b.mu.Unlock()
	hc.beforeUpdate = func() error { return context.Canceled }
	if err := b.MarkCode(t.Context(), "confirm-1", "run", alice); !errors.Is(err, ErrNotRecorded) {
		t.Fatalf("the failed write: %v, want ErrNotRecorded", err)
	}
	if b.CodeMarked("confirm-1", "run") {
		t.Fatal("ErrNotRecorded left the mark behind")
	}
	b.mu.Lock()
	b.setModeLocked(modeLive, "")
	b.mu.Unlock()
	if err := b.MarkCode(t.Context(), "confirm-1", "run", alice); err != nil {
		t.Fatalf("the retry: %v, want nil", err)
	}
}
