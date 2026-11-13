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
	"encoding/json"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// docGrants is the grants document the API server holds.
func docGrants(t *testing.T, api *apiServer) map[string]grantEntry {
	t.Helper()
	raw := api.data(KeyGrants)
	if raw == "" {
		return nil
	}
	var doc grantsDoc
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("grants document: %v: %s", err, raw)
	}
	return doc.Grants
}

// standing reports whether s would accept grant id: known, not revoked,
// not expired.
func standing(s *Store, id string) bool {
	g, ok := s.Grant(id)
	return ok && g.Revoked.IsZero() && g.Expiry.After(s.opts.Now())
}

// A take-over's re-merge of the same newer document (its write conflicting
// with a replica of this version that left the document alone) does not
// reset the index again: a spend written through in that window is kept,
// and the value cannot be spent twice (round 15, go P3-1).
func TestASpendDuringATakeOverIsSpentOnce(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	newerState(t, api, "sid")
	a := loaded(t, api, clock)
	clock.Add(takeOverAfter)
	a.tick()
	if !a.takeOver {
		t.Fatal("the test needs the take-over decided")
	}
	api.set(func(x *apiServer) { x.conflicts = 1 })
	first := a.SpendCode(t.Context(), "c", uh("alice"), clock.Now().Add(time.Minute))
	second := a.SpendCode(t.Context(), "c", uh("alice"), clock.Now().Add(time.Minute))
	switch {
	case first == nil && second == nil:
		t.Fatal("one value spent twice during the take-over")
	case first != nil && second != nil:
		t.Fatalf("neither spend recorded: %v, %v", first, second)
	case first == nil && !errors.Is(second, ErrSpent):
		t.Errorf("the second spend: %v, want ErrSpent", second)
	}
	if first == nil && (!api.holds("c") || !a.CodeUsed("c")) {
		t.Errorf("SpendCode returned nil; the ConfigMap holds it: %v, the replica: %v", api.holds("c"), a.CodeUsed("c"))
	}
}

// writeThrough refuses a write whose reservation a conflict's merge
// removed: it would land without the spend.
func TestAWriteThroughWithoutItsReservationIsNotRecorded(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	a := loaded(t, api, clock)
	api.set(func(x *apiServer) { x.conflicts = 1 })
	a.mu.Lock()
	desired, gen := a.desiredLocked(), a.gen
	a.mu.Unlock()
	err := a.writeThrough(t.Context(), func(*corev1.ConfigMap) bool { return false }, func() bool { return false }, desired, gen)
	if !errors.Is(err, ErrNotRecorded) {
		t.Fatalf("got %v, want ErrNotRecorded", err)
	}
}

// A forged annotation of this version (MaxInt64) and one of another that
// never changes: the stamp still changes the value, and the replica sends
// a bounded number of heartbeats, then none.
func TestForgedAnnotationsCostABoundedNumberOfWrites(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	api.cm.Annotations = map[string]string{writerAnnotation(formatVersion): "9223372036854775807", writerAnnotation(7): "x"}
	a := loaded(t, api, clock)
	a.RecordSignOut("sid", tag("alice"), t0.Add(time.Hour))
	flushed(t, a)
	if got := annotation(api, writerAnnotation(formatVersion)); got == "9223372036854775807" {
		t.Fatal("the stamp did not change the forged value")
	}
	before := api.updates
	for range 120 {
		clock.Add(time.Minute)
		watchMerge(t, api, a)
		a.tick()
		flushed(t, a)
	}
	if n := api.updates - before; n > int(takeOverAfter/heartbeatEvery)+1 {
		t.Errorf("%d writes in two hours over a foreign annotation that never changes", n)
	}
}

// A ConfigMap older than the one a replica last merged (a watch event
// still queued when a conflict's re-read, or the replica's own write,
// showed a newer one) is not merged: the base stays the newer one, which
// the next write is built on.
func TestAnOlderConfigMapIsNotMerged(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	a, b := loaded(t, api, clock), loaded(t, api, clock)
	stale, err := api.Get(t.Context(), "console-state", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AddGrant(testGrant(1, "alice", t0)); err != nil {
		t.Fatal(err)
	}
	flushed(t, a)
	watchMerge(t, api, b) // the newer document first (a re-read)
	newer := b.base.ResourceVersion
	b.mu.Lock()
	b.mergeLocked(stale) // then the older one (the queued watch event)
	b.mu.Unlock()
	if b.base.ResourceVersion != newer {
		t.Fatalf("an older ConfigMap set the base back from %s to %s", newer, b.base.ResourceVersion)
	}
	// b's own write, then a late event of the document before it: same.
	b.InstanceSince("pod-b", "ok", t0)
	before, err := api.Get(t.Context(), "console-state", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	flushed(t, b)
	written := b.base.ResourceVersion
	b.mu.Lock()
	b.mergeLocked(before)
	b.mu.Unlock()
	if b.base.ResourceVersion != written {
		t.Fatalf("an event older than b's own write set the base back from %s to %s", written, b.base.ResourceVersion)
	}
}
