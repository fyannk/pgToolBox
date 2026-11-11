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
	"fmt"
	"math/rand/v2"
	"strings"
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

// A replica cut off from the API server for longer than RevokedKeep, while
// another revokes a grant and then prunes the revocation, does not write
// the grant back on reconnecting: it read the grant in a document, the
// document no longer holds it, so it was dropped elsewhere (round 15,
// security P2-1).
func TestAPartitionedReplicaDoesNotResurrectARevokedGrant(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	a := loaded(t, api, clock)
	if err := a.AddGrant(testGrant(1, "alice", t0)); err != nil {
		t.Fatal(err)
	}
	flushed(t, a)
	b := loaded(t, api, clock)
	if _, ok := b.Grant(grantID(1)); !ok {
		t.Fatal("the test needs the grant shared")
	}
	// b is cut off from here on (no merges, no writes).
	clock.Add(time.Minute)
	if !a.RevokeGrant(grantID(1), uh("alice"), clock.Now(), clock.Now().Add(12*time.Hour)) {
		t.Fatal("revoke")
	}
	flushed(t, a)
	clock.Add(RevokedKeep + time.Minute)
	a.InstanceSince("pod", "ok", t0) // any change: a prunes and writes
	flushed(t, a)
	if strings.Contains(api.data(KeyGrants), grantID(1)) {
		t.Fatal("the test needs the revocation pruned")
	}
	// b reconnects: re-read, merge, write.
	watchMerge(t, api, b)
	b.InstanceSince("pod-b", "ok", t0)
	flushed(t, b)
	watchMerge(t, api, a)
	for name, s := range map[string]*Store{"a": a, "b": b} {
		if standing(s, grantID(1)) {
			t.Errorf("%s accepts the revoked grant again", name)
		}
	}
	if strings.Contains(api.data(KeyGrants), grantID(1)) {
		t.Errorf("the revoked grant is back in the document: %s", api.data(KeyGrants))
	}
}

// The same through a take-over: b never saw the newer grants document
// (which revoked the grant), and drops the grant on finding the reset
// one.
func TestATakeOverMissedByAReplicaDoesNotResurrectAGrant(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	a := loaded(t, api, clock)
	if err := a.AddGrant(testGrant(1, "alice", t0)); err != nil {
		t.Fatal(err)
	}
	flushed(t, a)
	b := loaded(t, api, clock) // cut off from here
	api.set(func(x *apiServer) {
		cm := x.cm.DeepCopy()
		var doc grantsDoc
		if err := json.Unmarshal([]byte(cm.Data[KeyGrants]), &doc); err != nil {
			t.Fatal(err)
		}
		e := doc.Grants[grantID(1)]
		e.Revoked = t0.Unix()
		doc.Grants[grantID(1)] = e
		raw, _ := json.Marshal(doc.Grants)
		cm.Data[KeyGrants] = `{"version":3,"grants":` + string(raw) + `,"codes":{}}`
		if cm.Annotations == nil {
			cm.Annotations = map[string]string{}
		}
		cm.Annotations[writerAnnotation(3)] = itoa(t0.Unix())
		x.bumpLocked(cm)
	})
	watchMerge(t, api, a)
	clock.Add(takeOverAfter)
	a.tick()
	flushed(t, a)
	if version(t, api.data(KeyGrants)) != formatVersion {
		t.Fatal("the test needs the take-over")
	}
	watchMerge(t, api, b)
	b.InstanceSince("pod-b", "ok", t0)
	flushed(t, b)
	watchMerge(t, api, a)
	for name, s := range map[string]*Store{"a": a, "b": b} {
		if standing(s, grantID(1)) {
			t.Errorf("%s accepts the grant the newer console revoked", name)
		}
	}
	if strings.Contains(api.data(KeyGrants), grantID(1)) {
		t.Errorf("the grant came back in the document: %s", api.data(KeyGrants))
	}
}

// What the three-way merge keeps: a grant this replica recorded and has
// not seen in a document yet survives a merge of a document that lacks it
// (its own write has not landed; the replication grace of a brand-new
// grant), for localKeep; past it, it is dropped. A grant recorded here
// whose write landed is dropped once the document no longer holds it.
func TestTheMergeKeepsOnlyRecentLocalGrants(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	a, b := loaded(t, api, clock), loaded(t, api, clock)
	if err := a.AddGrant(testGrant(1, "alice", t0)); err != nil {
		t.Fatal(err)
	}
	// b writes first: a merges a document that lacks the new grant.
	b.InstanceSince("pod-b", "ok", t0)
	flushed(t, b)
	watchMerge(t, api, a)
	if !standing(a, grantID(1)) {
		t.Fatal("a brand-new grant was dropped before its own write")
	}
	clock.Add(localKeep - time.Second)
	watchMerge(t, api, a)
	if !standing(a, grantID(1)) {
		t.Fatal("a grant recorded less than localKeep ago was dropped")
	}
	clock.Add(time.Second)
	watchMerge(t, api, a)
	if _, ok := a.Grant(grantID(1)); ok {
		t.Fatal("a grant recorded here and never written was kept past localKeep")
	}

	// Written, then the key deleted: the reset reaches the replica that
	// recorded it, at once.
	if err := a.AddGrant(testGrant(2, "alice", clock.Now())); err != nil {
		t.Fatal(err)
	}
	flushed(t, a)
	api.set(func(x *apiServer) {
		cm := x.cm.DeepCopy()
		delete(cm.Data, KeyGrants)
		x.bumpLocked(cm)
	})
	watchMerge(t, api, a)
	if _, ok := a.Grant(grantID(2)); ok {
		t.Fatal("a grant whose write landed survived the key's deletion")
	}
}

// A revocation made on a replica cut off from the ConfigMap is not pruned
// before it reaches it: pruned at RevokedKeep, it would be lost, and the
// grant (standing on the others) never revoked.
func TestARevocationMadeWhileCutOffIsNotLost(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	a := loaded(t, api, clock)
	if err := a.AddGrant(testGrant(1, "alice", t0)); err != nil {
		t.Fatal(err)
	}
	flushed(t, a)
	b := loaded(t, api, clock)
	// a is cut off; it revokes the grant, and cannot write.
	if !a.RevokeGrant(grantID(1), uh("alice"), clock.Now(), clock.Now().Add(12*time.Hour)) {
		t.Fatal("revoke")
	}
	clock.Add(RevokedKeep + time.Minute)
	a.InstanceSince("pod", "ok", t0) // prunes
	a.mu.Lock()
	a.pruneLocked()
	a.mu.Unlock()
	if !a.GrantRevoked(grantID(1)) {
		t.Fatal("a revocation that never reached the ConfigMap was pruned")
	}
	// b wrote meanwhile; a reconnects.
	b.InstanceSince("pod-b", "ok", t0)
	flushed(t, b)
	watchMerge(t, api, a)
	flushed(t, a)
	watchMerge(t, api, b)
	if standing(b, grantID(1)) {
		t.Fatal("the revocation made while cut off never reached b")
	}
	// Written now: pruned as usual.
	a.InstanceSince("pod", "ok2", t0)
	flushed(t, a)
	if a.GrantRevoked(grantID(1)) {
		t.Error("a written revocation past RevokedKeep was kept")
	}
}

// A property of the merge rule: N replicas, random partitions, restarts,
// grants recorded and revoked anywhere (on a replica cut off too, as in
// its staleGrace), time passing; once every replica is reconnected, no
// grant revoked anywhere is standing on any replica nor in the document.
func TestNoPartitionBringsBackARevokedGrant(t *testing.T) {
	const replicas, steps = 3, 80
	for seed := range uint64(300) {
		rng := rand.New(rand.NewPCG(seed, 15))
		clock := &testClock{now: t0}
		api := newAPIServer()
		stores := make([]*Store, replicas)
		for i := range stores {
			stores[i] = loaded(t, api, clock)
		}
		cut := make([]bool, replicas)
		revoked := map[string]bool{}
		var trace []string
		sync := func(i int) {
			watchMerge(t, api, stores[i])
			flushed(t, stores[i])
		}
		next := 0
		for range steps {
			i := rng.IntN(replicas)
			s := stores[i]
			switch rng.IntN(8) {
			case 0:
				cut[i] = !cut[i]
				trace = append(trace, fmt.Sprintf("%d cut=%v", i, cut[i]))
			case 1:
				next++
				if err := s.AddGrant(testGrant(next, "alice", clock.Now())); err == nil {
					trace = append(trace, fmt.Sprintf("%d add %d", i, next))
				}
			case 2:
				if next == 0 {
					continue
				}
				n := 1 + rng.IntN(next)
				s.RevokeGrant(grantID(n), uh("alice"), clock.Now(), clock.Now().Add(12*time.Hour))
				if s.GrantRevoked(grantID(n)) {
					revoked[grantID(n)] = true
				}
				trace = append(trace, fmt.Sprintf("%d revoke %d", i, n))
			case 3:
				d := time.Duration(rng.IntN(16)) * time.Minute
				clock.Add(d)
				trace = append(trace, fmt.Sprintf("+%v", d))
			case 4:
				if cut[i] {
					continue
				}
				// A restart: the final write of Run, then a new replica.
				sync(i)
				stores[i] = loaded(t, api, clock)
				trace = append(trace, fmt.Sprintf("%d restart", i))
			default:
				if !cut[i] {
					sync(i)
					trace = append(trace, fmt.Sprintf("%d sync", i))
				}
			}
		}
		for range 3 {
			for i := range stores {
				sync(i)
			}
		}
		doc := docGrants(t, api)
		for id := range revoked {
			for i, s := range stores {
				if standing(s, id) {
					t.Fatalf("seed %d: %s revoked, standing again on replica %d after %v", seed, id, i, trace)
				}
			}
			if e, ok := doc[id]; ok && e.Revoked == 0 && e.Expiry > clock.Now().Unix() {
				t.Fatalf("seed %d: %s revoked, standing again in the document after %v", seed, id, trace)
			}
		}
	}
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
// showed a newer one) is not merged: under the three-way rule its lack of
// a grant the newer document holds would drop the grant, and a revocation
// of it would then find no such grant.
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
	if _, ok := b.Grant(grantID(1)); !ok {
		t.Fatal("the test needs b to learn the grant")
	}
	b.mu.Lock()
	b.mergeLocked(stale) // then the older one (the queued watch event)
	b.mu.Unlock()
	if !b.RevokeGrant(grantID(1), uh("alice"), clock.Now(), clock.Now().Add(12*time.Hour)) {
		t.Fatal("an older ConfigMap merged late dropped a grant b had learned")
	}
	// b's own write, then a late event of the document before it: same.
	b.InstanceSince("pod-b", "ok", t0)
	before, err := api.Get(t.Context(), "console-state", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AddGrant(testGrant(2, "alice", t0)); err != nil {
		t.Fatal(err)
	}
	flushed(t, a)
	watchMerge(t, api, b)
	flushed(t, b)
	b.mu.Lock()
	b.mergeLocked(before)
	b.mu.Unlock()
	if !standing(b, grantID(2)) {
		t.Fatal("an event older than b's own write dropped a grant")
	}
}
